package chat

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dhananjay6561/kaya/core/internal/actions"
	"github.com/dhananjay6561/kaya/core/internal/agent"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/memory"
	"github.com/dhananjay6561/kaya/core/internal/search"
	"github.com/dhananjay6561/kaya/core/internal/text"
	"github.com/dhananjay6561/kaya/kit/logging"
)

// Question is a plain message from DJ to Kaya.
type Question struct {
	ID     string
	Text   string
	SentAt time.Time
}

// Answer is Kaya's reply. Remember is false for a canned reply that must not become part of the
// conversation.
type Answer struct {
	Text     string
	Remember bool
	// ActionID is the action the answer asks DJ to confirm; it waits until the answer is sent.
	ActionID string
}

// MaskSession hides private details in text bound for a cloud model and puts them back in its
// answer. One session covers one question, every attempt included, so placeholders stay the same.
type MaskSession interface {
	// Mask hides details in text; context is the text it answers, which can say what a number is.
	Mask(text, context string) string
	Restore(text string) string
	// Counts is how many details of each kind were hidden.
	Counts() map[string]int
}

// Options configure a Chat.
type Options struct {
	LLM    llm.Provider
	Logger *slog.Logger
	Zone   *time.Location
	// History reads the conversation so far for a question; nil means none.
	History func(ctx context.Context, q Question) ([]memory.Exchange, error)
	// Recall finds older exchanges that bear on query (the question, or what the search plan says
	// it is about), best first, leaving out exclude (the recent exchanges the context carries, by
	// question id) and anything from before on; nil means none. A failure is handled like
	// History's.
	Recall func(ctx context.Context, query string, exclude []int64, before time.Time) ([]memory.Exchange, error)
	// Excerpts finds passages of DJ's other chats for a search plan, which the routine model works
	// out from the question; nil means the chats are not searched. A failure is handled like
	// History's, and the model is told the search failed.
	Excerpts func(ctx context.Context, p search.Plan) (search.Found, error)
	// Facts reads the lasting facts about DJ, most important first; nil means none. A failure is
	// handled like History's.
	Facts func(ctx context.Context) ([]string, error)
	// DatabaseAway says whether a History error means the database is down, in which case the
	// question fails and runs again later; any other error is logged and the question is answered
	// without the conversation.
	DatabaseAway func(err error) bool
	// NewMaskSession is required for a cloud model.
	NewMaskSession func() MaskSession
	// Tools are offered to the model with every question; nil means none. A tool error that
	// DatabaseAway recognises fails the question like a History error does.
	Tools *agent.Registry
	// Propose stores an action an outward tool proposes, which waits ActionTTL for DJ's yes; nil
	// refuses outward tools.
	Propose        func(ctx context.Context, p actions.Proposal) (actions.Action, error)
	ActionTTL      time.Duration
	Now            func() time.Time
	MaxReplyLength int
}

// Chat answers DJ's plain messages.
type Chat struct {
	o Options
	// toolTokens is what offering the tools adds to every request, and the room kept for their
	// results.
	toolTokens int
}

const (
	maxRecalled     = 3
	newestLabel     = "(Among the newest messages found for the question.)"
	maxAnswerTokens = 3072
	cutShort        = "\n\n(Cut short here. Ask for less, or for the rest.)"
	goingInCircles  = "I went round in circles with my tools on that one. Try asking another way."
	maxProposalNote = 300
)

// New checks the options and returns a Chat.
func New(o Options) (*Chat, error) {
	switch {
	case o.LLM == nil || o.Logger == nil || o.Zone == nil:
		return nil, errors.New("chat needs a model, a logger and a time zone")
	case (o.History != nil || o.Recall != nil || o.Excerpts != nil || o.Facts != nil) && o.DatabaseAway == nil:
		return nil, errors.New("chat needs DatabaseAway to read history")
	case o.LLM.Locality() == llm.Cloud && o.NewMaskSession == nil:
		return nil, errors.New("chat needs masking to send the conversation to a cloud model")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxReplyLength == 0 {
		o.MaxReplyLength = 8000
	}
	c := &Chat{o: o}
	if o.Tools != nil {
		c.toolTokens = o.Tools.Tokens(o.LLM.CountTokens) + o.LLM.ContextTokens()/8
	}
	return c, nil
}

// Reply answers q with the model, carrying as much of the conversation as fits its budget.
func (c *Chat) Reply(ctx context.Context, q Question) (Answer, error) {
	o := c.o
	now := o.Now()
	var privacy MaskSession
	if o.LLM.Locality() == llm.Cloud {
		privacy = o.NewMaskSession()
	}
	var excerpts, unknown, known []string
	var chatSearch memory.ChatSearch
	tag := strings.ToLower(rand.Text()[:10])
	input := func(history, recalled []memory.Exchange) memory.Input {
		in := memory.Input{
			Excerpts:    excerpts,
			Tag:         tag,
			ChatSearch:  chatSearch,
			Unknown:     unknown,
			System:      SystemPrompt(now, o.Zone, o.LLM.Locality()),
			Facts:       known,
			History:     history,
			Recalled:    recalled,
			MaxRecalled: maxRecalled,
			Question:    q.Text,
			Budget:      o.LLM.ContextTokens() - c.toolTokens,
			Count:       o.LLM.CountTokens,
			Now:         now,
		}
		if len(recalled) > 0 {
			in.RecallReserve = in.Budget / 4
		}
		return in
	}
	var proposed *actions.Action
	var notes []string
	var owner string
	ask := func(history, recalled []memory.Exchange, effort llm.Effort) (llm.Completion, error) {
		built := memory.Build(input(history, recalled))
		started := time.Now()
		res, err := c.complete(ctx, llm.Request{
			Tier: llm.Heavy, Effort: effort, MaxTokens: maxAnswerTokens, Messages: built.Messages,
		}, privacy, owner)
		completion := res.Completion
		if res.Proposed != nil {
			proposed = res.Proposed
		}
		for _, n := range res.Notes {
			if !slices.Contains(notes, n) {
				notes = append(notes, n)
			}
		}
		if err != nil {
			return completion, err
		}
		calls := make([]string, len(res.Calls))
		for i, call := range res.Calls {
			calls[i] = call.Tool + ":" + call.Outcome
		}
		o.Logger.Info("the model answered",
			"model", completion.Model,
			"finish_reason", completion.FinishReason,
			"input_tokens", res.Usage.InputTokens,
			"output_tokens", res.Usage.OutputTokens,
			"counted_tokens", built.Tokens,
			"tool_rounds", res.Rounds,
			"tool_calls", calls,
			"exchanges", built.Exchanges,
			"left_out", built.LeftOut,
			"recalled", built.Recalled,
			"excerpts", built.Excerpts,
			"facts", built.Facts,
			"ms", time.Since(started).Milliseconds(),
		)
		return completion, nil
	}

	mask := func(exchanges []memory.Exchange) []memory.Exchange {
		if privacy == nil {
			return exchanges
		}
		masked := make([]memory.Exchange, len(exchanges))
		for i, e := range exchanges {
			// An answer may repeat a code from an excerpt, which was masked there because of the
			// messages around it; the question and answer alone often do not say it is one, so
			// every answer is masked as if a code were mentioned.
			e.Answer = privacy.Mask(e.Answer, e.Question+"\notp")
			masked[i] = e
		}
		return masked
	}
	known, err := c.facts(ctx)
	if err != nil {
		return Answer{}, err
	}
	if privacy != nil {
		// A fact goes with every question for good, so details in it, such as a friend's number,
		// are hidden like everything else DJ did not type in this message.
		for i, f := range known {
			known[i] = privacy.Mask(f, f)
		}
	}
	history, err := c.read(ctx, q, o.History, "could not read the conversation; answering without it")
	if err != nil {
		return Answer{}, err
	}
	owner = ownerWords(q, history, now)
	history = mask(history)
	query := q.Text
	if o.Excerpts != nil {
		p, searchIt, err := c.plan(ctx, q, history, now)
		if err != nil {
			return Answer{}, err
		}
		if searchIt {
			if strings.TrimSpace(p.Query) != "" {
				query = p.Query
			}
			if excerpts, unknown, chatSearch, err = c.excerpts(ctx, p, privacy); err != nil {
				return Answer{}, err
			}
			unknown = written(unknown, q, history)
		}
	}
	var recalled []memory.Exchange
	if o.Recall != nil {
		// What the recent conversation will carry decides what recall looks beyond, so a recent
		// talk on the same topic neither crowds out an older one nor comes back twice.
		recent := memory.Build(input(history, nil))
		recalled, err = c.read(ctx, q, func(ctx context.Context, _ Question) ([]memory.Exchange, error) {
			return o.Recall(ctx, query, recent.Kept, recent.Oldest)
		}, "could not recall older exchanges; answering without them")
		if err != nil {
			return Answer{}, err
		}
		recalled = mask(recalled)
	}
	sent, sentRecalled := history, recalled
	completion, err := ask(sent, sentRecalled, llm.Medium)
	var failed *llm.Error
	tooLong := func(err error) bool { return errors.As(err, &failed) && failed.Code == llm.TooLong }
	if err != nil && tooLong(err) && len(history)+len(recalled) > 0 {
		o.Logger.Warn("the request was too long with the conversation; asking without it")
		sent, sentRecalled = nil, nil
		completion, err = ask(sent, sentRecalled, llm.Medium)
	}
	if err != nil && tooLong(err) && len(excerpts) > 0 {
		o.Logger.Warn("the request was too long with the excerpts; asking without them")
		excerpts, chatSearch = nil, memory.DidNotFit
		completion, err = ask(sent, sentRecalled, llm.Medium)
	}
	if text.Trim(completion.Text) == "" && completion.FinishReason == llm.FinishLength && err == nil {
		o.Logger.Warn("the model spent its room thinking; asking again with less effort")
		completion, err = ask(sent, sentRecalled, llm.Low)
	}
	if errors.Is(err, agent.ErrTooManyRounds) {
		o.Logger.Warn("the model kept calling tools; giving up on the question")
		return withNotes(Answer{Text: goingInCircles}, notes), nil
	}
	if err != nil {
		return Answer{}, err
	}

	reply := completion.Text
	if privacy != nil {
		if masked := privacy.Counts(); len(masked) > 0 {
			o.Logger.Info("private details were masked", "masked", masked)
		}
		reply = privacy.Restore(reply)
	}
	answer := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) && r != 0x200d {
			return -1
		}
		return r
	}, ToWhatsApp(reply))
	ranOut := completion.FinishReason == llm.FinishLength
	if garbled(answer) {
		o.Logger.Warn("the model's answer was garbled; it is not shown")
		if len(notes) > 0 || proposed != nil {
			answer = "Done."
		} else {
			return Answer{Text: "My answer came out garbled. Ask me again."}, nil
		}
	}
	if answer == "" {
		if ranOut {
			return withNotes(Answer{Text: "I ran out of room before I could answer. Try a narrower question."}, notes), nil
		}
		return withNotes(Answer{Text: "I had no answer to that. Try asking another way."}, notes), nil
	}
	cut := utf8.RuneCountInString(answer) > o.MaxReplyLength
	answer = ClipTo(answer, o.MaxReplyLength)
	if ranOut || cut {
		answer += cutShort
	}
	return withNotes(c.withProposal(Answer{Text: answer, Remember: true}, proposed), notes), nil
}

// excerpts renders the passages Excerpts finds, masked for a cloud model with the question's mask
// session, so placeholders match the rest of the request and are put back in the answer, and says
// what the search did.
func (c *Chat) excerpts(ctx context.Context, p search.Plan, privacy MaskSession) ([]string, []string, memory.ChatSearch, error) {
	found, err := c.o.Excerpts(ctx, p)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil, nil, memory.NotSearched, context.Cause(ctx)
	case c.o.DatabaseAway(err):
		return nil, nil, memory.NotSearched, err
	default:
		c.o.Logger.Warn("could not search the chats; answering without them", logging.Err(err))
		return nil, nil, memory.SearchFailed, nil
	}
	var masker search.Masker
	if privacy != nil {
		masker = privacy
	}
	out := make([]string, len(found.Excerpts))
	for i, e := range found.Excerpts {
		out[i] = search.Render(e.Title, e.Lines, c.o.Zone, search.DefaultShape, masker, e.Before)
		if e.Newest {
			out[i] = newestLabel + "\n" + out[i]
		}
	}
	return out, found.Unknown, memory.Searched, nil
}

// written keeps the names DJ wrote himself, in this question or an earlier one: a name the model
// took from elsewhere, such as an answer quoting someone's message, is never repeated to it.
func written(names []string, q Question, history []memory.Exchange) []string {
	var his strings.Builder
	his.WriteString(strings.ToLower(q.Text))
	for _, e := range history {
		his.WriteString("\n" + strings.ToLower(e.Question))
	}
	var out []string
	for _, n := range names {
		if strings.Contains(his.String(), strings.ToLower(n)) {
			out = append(out, n)
		}
	}
	return out
}

// facts reads the facts about DJ, handling a failure like read does.
func (c *Chat) facts(ctx context.Context) ([]string, error) {
	if c.o.Facts == nil {
		return nil, nil
	}
	known, err := c.o.Facts(ctx)
	switch {
	case err == nil:
		return known, nil
	case ctx.Err() != nil:
		return nil, context.Cause(ctx)
	case c.o.DatabaseAway(err):
		return nil, err
	default:
		c.o.Logger.Warn("could not read the facts; answering without them", logging.Err(err))
		return nil, nil
	}
}

// read gets exchanges from source. When the database is away the question fails and runs again
// later; any other failure is logged and the question is answered without them.
func (c *Chat) read(ctx context.Context, q Question, source func(context.Context, Question) ([]memory.Exchange, error), failed string) ([]memory.Exchange, error) {
	if source == nil {
		return nil, nil
	}
	exchanges, err := source(ctx, q)
	switch {
	case err == nil:
		return exchanges, nil
	case ctx.Err() != nil:
		return nil, context.Cause(ctx)
	case c.o.DatabaseAway(err):
		return nil, err
	default:
		c.o.Logger.Warn(failed, logging.Err(err))
		return nil, nil
	}
}

// complete asks the model once, or, with tools, until it stops calling them; a nil privacy means a
// local model.
func (c *Chat) complete(ctx context.Context, r llm.Request, privacy MaskSession, owner string) (agent.Result, error) {
	if c.o.Tools == nil {
		completion, err := c.o.LLM.Complete(ctx, r)
		return agent.Result{Completion: completion, Usage: completion.Usage}, err
	}
	var hide agent.Privacy
	if privacy != nil {
		hide = privacy
	}
	return agent.Run(ctx, agent.Options{
		LLM: c.o.LLM, Tools: c.o.Tools, Logger: c.o.Logger, Privacy: hide, Budget: c.o.LLM.ContextTokens(), Propose: c.o.Propose, OwnerWords: owner,
		Fatal: func(err error) bool { return c.o.DatabaseAway != nil && c.o.DatabaseAway(err) },
	}, r)
}

// ownerWords is what DJ wrote himself lately: this question and his questions of the last 30
// minutes, never what Kaya sent unasked.
func ownerWords(q Question, history []memory.Exchange, now time.Time) string {
	words := []string{q.Text}
	for _, e := range history {
		if !e.Unasked && now.Sub(e.At) <= 30*time.Minute {
			words = append(words, e.Question)
		}
	}
	return strings.Join(words, "\n")
}

// withNotes puts the lines tools wrote for DJ under an answer, as they are, such as the exact time
// a reminder was set for; an answer with notes is remembered, so a follow-up has them.
func withNotes(a Answer, notes []string) Answer {
	if len(notes) == 0 {
		return a
	}
	a.Text = strings.TrimSpace(a.Text) + "\n\n" + strings.Join(notes, "\n")
	a.Remember = true
	return a
}

// withProposal puts the exact question for DJ at the top of an answer that proposed an action,
// with only a short note from the model under it, so what a yes does is always the first thing he
// reads, whatever the model wrote; the answer is then always remembered, so his yes or no makes
// sense in the conversation.
func (c *Chat) withProposal(a Answer, proposed *actions.Action) Answer {
	if proposed == nil {
		return a
	}
	text := actions.ConfirmLine(*proposed, c.o.ActionTTL)
	if note := ClipTo(strings.TrimSpace(a.Text), maxProposalNote); note != "" {
		text += "\n\n" + note
	}
	return Answer{Text: text, Remember: true, ActionID: proposed.ID}
}

// garbled is an answer that is mostly not words, such as dots and odd spaces, which a model
// sometimes writes instead of an answer.
func garbled(s string) bool {
	letters, others := 0, 0
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			letters++
		case unicode.IsSpace(r) || r == 0xa0 || r == 0x202f:
		case unicode.Is(unicode.So, r) || r == 0x200d || unicode.Is(unicode.Sk, r):
			letters++
		default:
			others++
		}
	}
	return letters+others >= 8 && letters < others
}
