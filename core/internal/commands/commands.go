// Package commands answers what DJ types to Kaya: slash commands itself, everything else through
// the chat model, with the timing and failure rules of decisions 64 to 69.
package commands

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/core/internal/text"
)

// ChatAnswer is the model's reply to a plain message. Remember is false for a canned reply that
// must not become part of the conversation.
type ChatAnswer struct {
	Text     string
	Remember bool
}

// Options configure the command handler. Zero durations take their defaults.
type Options struct {
	Channel channel.Channel
	Logger  *slog.Logger
	Pause   pause.Switch
	Status  func(ctx context.Context) (string, error)
	// Facts keeps the facts /facts, /remember and /forget work on; nil answers them with a notice.
	Facts FactBook
	Chat  func(ctx context.Context, m channel.IncomingMessage) (ChatAnswer, error)
	// DatabaseAway says whether an error means the database is down, so the command will run again
	// and nothing is said; it must agree with the inbox, so pass channel.DatabaseAway.
	DatabaseAway func(err error) bool

	// MaxAge is how late a command may be taken in and still be acted on.
	MaxAge time.Duration
	// SilentAfter is how late a command may be before Kaya says nothing about it at all.
	SilentAfter time.Duration
	// MinReplyTTL is the least time an answer is worth sending.
	MinReplyTTL time.Duration
	// NoticeTTL is how long a notice stays worth sending.
	NoticeTTL time.Duration
	// Now reads the clock; nil means time.Now.
	Now func() time.Time
}

// Clip cuts a quoted command for a notice.
func Clip(s string) string { return text.Clip(s, 1000) }

func failureNotice(err error) string {
	var failed *llm.Error
	if !errors.As(err, &failed) {
		return "Something went wrong with that. Send /status to see how things stand."
	}
	switch failed.Code {
	case llm.RateLimited:
		return "I have hit my limit with the AI service for now. Try again in a minute."
	case llm.Auth:
		return "The AI service refused my key, so I could not answer. Check GROQ_API_KEY."
	case llm.TooLong:
		return "That is too long for the AI service. Try a shorter message."
	case llm.Misconfigured:
		return "The AI service does not know a model I am set to use. Check GROQ_MODEL_HEAVY and GROQ_MODEL_ROUTINE."
	case llm.Rejected:
		return "The AI service turned this message down. Try asking another way."
	}
	return "The AI service is not answering right now. Try again in a bit."
}

func neverSent(err error) (*channel.SendFailedError, bool) {
	var failed *channel.SendFailedError
	if errors.As(err, &failed) && (failed.Code == "expired" || failed.Code == "no_answer") {
		return failed, true
	}
	return nil, false
}

// Start registers the command handler on the channel and returns a function that removes it.
func Start(o Options) func() {
	if o.DatabaseAway == nil {
		panic("commands: Options.DatabaseAway is required")
	}
	if o.MaxAge == 0 {
		o.MaxAge = 10 * time.Minute
	}
	if o.SilentAfter == 0 {
		o.SilentAfter = 24 * time.Hour
	}
	if o.MinReplyTTL == 0 {
		o.MinReplyTTL = 30 * time.Second
	}
	if o.NoticeTTL == 0 {
		o.NoticeTTL = time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	h := &handler{o: o}
	return o.Channel.OnMessage(h.handle)
}

type handler struct {
	o Options
}

func (h *handler) handle(ctx context.Context, m channel.IncomingMessage) (channel.Handled, error) {
	o := h.o
	// How late Kaya took the command in, not how late it is being run: the same command gets the
	// same answer however many times it is started.
	late := m.ReceivedAt.Sub(m.SentAt)
	if late > o.SilentAfter {
		o.Logger.Warn("a command more than a day old was skipped", "command", m.ID)
		return channel.Handled{}, nil
	}
	slash := ParseSlash(m.Text)
	if slash == nil {
		st, err := o.Pause.State(ctx)
		if err != nil {
			return channel.Handled{}, err
		}
		if st.Paused {
			o.Logger.Info("Kaya is paused; a message was left unanswered", "command", m.ID)
			return channel.Handled{}, nil
		}
	}
	if late > o.MaxAge {
		_, err := o.Channel.Send(ctx, channel.OutgoingMessage{
			IdempotencyKey: "command:" + m.ID,
			ReplyTo:        m.ID,
			Text:           "I was away when you sent this, so I did not act on it. Send it again if you still want it:\n" + Clip(m.Text),
		}, channel.SendOptions{TTL: o.NoticeTTL})
		return channel.Handled{}, err
	}

	key := "command:" + m.ID + ":chat"
	if slash != nil {
		key = "command:" + m.ID + ":" + slash.Kind + ":" + slash.Name
	}
	canned := "command:" + m.ID + ":chat:canned"
	handled, err := h.answer(ctx, m, slash, key, canned)
	if failed, ok := neverSent(err); ok {
		o.Logger.Warn("a reply could not be sent in time", "command", m.ID, "error_code", failed.Code)
		notice := "I could not answer this in time, so nothing was done. Send it again if you still want it:\n" + Clip(m.Text)
		if slash != nil {
			notice = "I could not answer this in time. Send /status to see how things stand."
		}
		if _, sendErr := o.Channel.Send(ctx, channel.OutgoingMessage{
			IdempotencyKey: "command:" + m.ID + ":unanswered",
			ReplyTo:        m.ID,
			Text:           notice,
		}, channel.SendOptions{TTL: o.NoticeTTL}); sendErr != nil {
			return handled, sendErr
		}
	}
	return handled, err
}

func (h *handler) answer(ctx context.Context, m channel.IncomingMessage, slash *Slash, key, canned string) (channel.Handled, error) {
	o := h.o
	earlier, err := o.Channel.Follow(ctx, key)
	if err != nil {
		return channel.Handled{}, err
	}
	if earlier != nil {
		if slash != nil {
			return channel.Handled{}, nil
		}
		return channel.Handled{AnswerID: earlier.ID}, nil
	}
	if slash == nil {
		cannedEarlier, err := o.Channel.Follow(ctx, canned)
		if err != nil {
			return channel.Handled{}, err
		}
		if cannedEarlier != nil {
			return channel.Handled{}, nil
		}
	}
	if o.Now().Sub(m.SentAt) > o.MaxAge {
		return channel.Handled{}, &channel.SendFailedError{RequestID: key, Code: "expired"}
	}

	produced, err := h.produce(ctx, m, slash)
	if err != nil {
		return channel.Handled{}, err
	}
	// The answer is worth sending until the command itself would count as too old.
	ttl := max(o.MinReplyTTL, o.MaxAge-o.Now().Sub(m.SentAt))
	sendKey := key
	if slash == nil && !produced.Remember {
		sendKey = canned
	}
	sent, err := o.Channel.Send(ctx, channel.OutgoingMessage{IdempotencyKey: sendKey, ReplyTo: m.ID, Text: produced.Text},
		channel.SendOptions{TTL: ttl, TextMayChange: true})
	if err != nil {
		return channel.Handled{}, err
	}
	if slash == nil && produced.Remember {
		return channel.Handled{AnswerID: sent.ID}, nil
	}
	return channel.Handled{}, nil
}

// produce works out the answer. When that fails for a reason of its own, DJ is told once and the
// command fails; when core is stopping or the database is away, nothing is said and it runs again.
func (h *handler) produce(ctx context.Context, m channel.IncomingMessage, slash *Slash) (ChatAnswer, error) {
	o := h.o
	var answer ChatAnswer
	var err error
	if slash != nil {
		answer.Text, err = answerSlash(ctx, slash, m, o)
	} else {
		answer, err = o.Chat(ctx, m)
	}
	if err == nil {
		return answer, nil
	}
	if ctx.Err() != nil || o.DatabaseAway(err) {
		return ChatAnswer{}, err
	}
	if _, sendErr := o.Channel.Send(ctx, channel.OutgoingMessage{
		IdempotencyKey: "command:" + m.ID + ":failed",
		ReplyTo:        m.ID,
		Text:           failureNotice(err),
	}, channel.SendOptions{TTL: o.NoticeTTL}); sendErr != nil {
		return ChatAnswer{}, sendErr
	}
	return ChatAnswer{}, err
}
