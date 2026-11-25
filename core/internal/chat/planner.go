package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/memory"
	"github.com/dhananjay6561/kaya/core/internal/search"
	"github.com/dhananjay6561/kaya/core/internal/text"
	"github.com/dhananjay6561/kaya/kit/logging"
)

// planSchema is the JSON a plan must follow; with a strict schema every field is required, so
// "no date" is an empty string.
const planSchema = `{"type":"object","properties":{` +
	`"search":{"type":"boolean"},"query":{"type":"string"},` +
	`"people":{"type":"array","items":{"type":"string"}},"groups":{"type":"array","items":{"type":"string"}},` +
	`"newest":{"type":"boolean"},"from":{"type":"string"},"to":{"type":"string"}},` +
	`"required":["search","query","people","groups","newest","from","to"],"additionalProperties":false}`

const (
	planExchanges    = 2
	planQuoteLength  = 400
	planMaxTokens    = 1024
	planTimeout      = 8 * time.Second
	planEffort       = llm.Low
	plannerInstructs = `You plan a search of the WhatsApp chats of the person who owns this account, for the assistant that answers them. Work out what their latest message needs; the earlier messages are only there so you know who or what it refers to. Answer in JSON:
- search: true if answering needs anything from their chats with other people (what someone said, sent, asked, shared or planned, any message or conversation); false for anything else, such as greetings, thanks, general knowledge, writing, maths, or questions about the assistant itself.
- query: what to look for, in a few words, in the same language and the same alphabet as their message (never translated or transliterated), with words like "he", "she" or "they" replaced by who they mean.
- people: the names of the people the message is about, spelled exactly as they wrote them (same alphabet, never translated or transliterated), with words of address or respect left out. Empty if it names nobody.
- groups: the names of any group chats it mentions, spelled as they wrote them. Empty if none.
- newest: true only if it asks for the most recent, latest or new messages, without a topic and without saying when ("what did she say recently", "latest from Sam"); false when it asks about something in particular, even with "recently".
- from and to: the first and last day (YYYY-MM-DD) the messages it asks about were sent, only if the message says when (today, yesterday, last week, in March); otherwise empty strings.

Examples, if today were Wednesday 25 November 2026:
"what did Priya say about the trip?" → search true, query "Priya trip", people ["Priya"], newest false, no dates.
"aur usne kal kya bola?" after a question about Rahul → search true, query "Rahul kya bola", people ["Rahul"], newest false, from and to "2026-11-24".
"any new messages in Office Team?" → search true, query "new messages Office Team", groups ["Office Team"], newest true.
"¿qué dijo mamá sobre la cena?" → search true, query "mamá cena", people ["mamá"], newest false.
"Sam ne recently rent ke baare mein kya bola?" → search true, query "Sam rent", people ["Sam"], newest false.
"Was hat Lena gestern geschrieben?" → search true, query "Lena geschrieben", people ["Lena"], newest false, from and to "2026-11-24".
"thanks!" or "what is 15% of 80?" → search false, query "", no names.`
)

type plan struct {
	Search bool     `json:"search"`
	Query  string   `json:"query"`
	People []string `json:"people"`
	Groups []string `json:"groups"`
	Newest bool     `json:"newest"`
	From   string   `json:"from"`
	To     string   `json:"to"`
}

var errBadPlan = errors.New("the model's search plan was not valid")

// plan asks the routine model what to search DJ's other chats for, from his question and the last
// exchanges before it, already masked for a cloud model. It says whether to search at all. When the
// model fails or answers nonsense, the question itself is searched for.
func (c *Chat) plan(ctx context.Context, q Question, history []memory.Exchange, now time.Time) (search.Plan, bool, error) {
	var b strings.Builder
	b.WriteString(plannerInstructs)
	b.WriteString("\n\nToday is " + now.In(c.o.Zone).Format("Monday 2 January 2006") + " (" + c.o.Zone.String() + ").")
	var earlier strings.Builder
	for _, e := range history[max(0, len(history)-planExchanges):] {
		earlier.WriteString("Them: " + quote(e.Question) + "\nYou: " + quote(e.Answer) + "\n")
	}
	content := "Their latest message: " + quote(q.Text)
	if earlier.Len() > 0 {
		content = "Earlier:\n" + earlier.String() + "\n" + content
	}
	started := time.Now()
	planCtx, cancel := context.WithTimeout(ctx, planTimeout)
	defer cancel()
	completion, err := c.o.LLM.Complete(planCtx, llm.Request{
		Tier: llm.Routine, Effort: planEffort, MaxTokens: planMaxTokens,
		Messages: []llm.Message{{Role: llm.System, Content: b.String()}, {Role: llm.User, Content: content}},
		Schema:   &llm.Schema{Name: "search_plan", Schema: json.RawMessage(planSchema)},
	})
	if err == nil {
		var p search.Plan
		var searchIt bool
		if p, searchIt, err = c.parse(completion.Text); err == nil {
			c.o.Logger.Info("the search was planned",
				"search", searchIt, "people", len(p.People), "groups", len(p.Groups), "newest", p.Newest,
				"dated", !p.From.IsZero() || !p.To.IsZero(), "ms", time.Since(started).Milliseconds())
			return p, searchIt, nil
		}
	}
	if ctx.Err() != nil {
		return search.Plan{}, false, context.Cause(ctx)
	}
	c.o.Logger.Warn("could not plan the search; searching for the question as asked", logging.Err(err))
	return search.Plan{Query: q.Text}, true, nil
}

// parse checks a plan the model wrote: dates are days in DJ's time zone, to covers its whole day,
// and a range the wrong way round is dropped.
func (c *Chat) parse(answer string) (search.Plan, bool, error) {
	var raw plan
	if err := json.Unmarshal([]byte(strings.TrimSpace(answer)), &raw); err != nil {
		return search.Plan{}, false, errors.Join(errBadPlan, err)
	}
	p := search.Plan{Query: raw.Query, People: raw.People, Groups: raw.Groups, Newest: raw.Newest}
	day := func(s string) (time.Time, error) {
		if s = strings.TrimSpace(s); s == "" {
			return time.Time{}, nil
		}
		return time.ParseInLocation("2006-01-02", s, c.o.Zone)
	}
	var err error
	if p.From, err = day(raw.From); err != nil {
		return search.Plan{}, false, errors.Join(errBadPlan, err)
	}
	if p.To, err = day(raw.To); err != nil {
		return search.Plan{}, false, errors.Join(errBadPlan, err)
	}
	if !p.To.IsZero() {
		p.To = p.To.AddDate(0, 0, 1).Add(-time.Nanosecond)
	}
	if !p.From.IsZero() && !p.To.IsZero() && p.From.After(p.To) {
		p.From, p.To = time.Time{}, time.Time{}
	}
	return p, raw.Search, nil
}

// quote is text on one line, cut to planQuoteLength characters.
func quote(s string) string {
	s = strings.Join(strings.FieldsFunc(s, text.IsSpace), " ")
	if r := []rune(s); len(r) > planQuoteLength {
		return string(r[:planQuoteLength]) + "…"
	}
	return s
}
