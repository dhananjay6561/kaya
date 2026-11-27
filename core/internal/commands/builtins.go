package commands

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/core/internal/text"
)

var builtinNames = []string{"help", "status", "pause", "resume", "facts", "remember", "forget", "tasks", "done", "summary", "alerts"}

// takesText are the built-ins that act on the words after them.
var takesText = []string{"remember", "forget", "done", "summary", "alerts"}

// Slash kinds.
const (
	Builtin   = "builtin"
	ExtraText = "extra_text"
	Unknown   = "unknown"
)

// Slash is a message that starts with a slash: a built-in, a built-in with words after it, or an
// unknown command. Args is what follows a built-in that takes text, as DJ wrote it.
type Slash struct {
	Kind string
	Name string
	Args string
}

var trailingPunctuation = regexp.MustCompile(`[.!?]+$`)

// ParseSlash reads a slash command, seeing through what phones and keyboards add: full-width
// characters, invisible marks (the emoji joiner kept) and a full stop from autocorrect. It returns
// nil for anything that is not a slash command.
func ParseSlash(message string) *Slash {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) && r != 0x200d {
			return -1
		}
		return r
	}, norm.NFKC.String(message))
	cleaned = text.Trim(cleaned)
	rest, ok := strings.CutPrefix(cleaned, "/")
	if !ok {
		return nil
	}
	if first, _ := utf8.DecodeRuneInString(rest); rest == "" || text.IsSpace(first) {
		return &Slash{Kind: Unknown}
	}
	words := strings.FieldsFunc(rest, text.IsSpace)
	name := trailingPunctuation.ReplaceAllString(strings.ToLower(words[0]), "")
	if !slices.Contains(builtinNames, name) {
		return &Slash{Kind: Unknown, Name: text.FirstGraphemes(words[0], 30)}
	}
	if slices.Contains(takesText, name) {
		return &Slash{Kind: Builtin, Name: name, Args: argsOf(message)}
	}
	if len(words) > 1 {
		return &Slash{Kind: ExtraText, Name: name}
	}
	return &Slash{Kind: Builtin, Name: name}
}

// argsOf is what follows the first word of message, taken from the message as written rather than
// normalized, so a fact keeps its own characters.
func argsOf(message string) string {
	message = text.Trim(strings.TrimLeftFunc(message, func(r rune) bool { return unicode.Is(unicode.Cf, r) || text.IsSpace(r) }))
	i := strings.IndexFunc(message, text.IsSpace)
	if i < 0 {
		return ""
	}
	return text.Trim(message[i:])
}

// IsBuiltin reports whether message is exactly a built-in with nothing after it that makes no model
// call; only these are urgent, so they never wait behind questions and questions never wait behind
// a summary.
func IsBuiltin(message string) bool {
	s := ParseSlash(message)
	return s != nil && s.Kind == Builtin && s.Args == "" && s.Name != "summary"
}

const helpText = "Commands:\n" +
	"/help: this list\n" +
	"/status: how Kaya is doing\n" +
	"/pause: stop answering until /resume\n" +
	"/resume: start answering again\n" +
	"/facts: what I know about you\n" +
	"/remember <fact>: keep a fact about you\n" +
	"/forget <number>: drop a fact, by its number in /facts\n" +
	"/tasks: open tasks and reminders found in your chats\n" +
	"/done <number>: close a task, by its number in /tasks\n" +
	"/summary: what came in today; /summary <chat>: one chat's last day\n" +
	"/alerts: whether I message you when something needs you; /alerts on, /alerts off\n" +
	"Anything else you send here goes to an AI model, which answers with what it knows about you, your conversation and what a search of your other chats finds."

// answerSlash works out the reply to a slash command; /pause and /resume flip the switch.
func answerSlash(ctx context.Context, s *Slash, m channel.IncomingMessage, o Options) (string, error) {
	sw, status := o.Pause, o.Status
	commandID := m.ID
	switch s.Kind {
	case Unknown:
		if s.Name == "" {
			return "Send /help to see what I can do.", nil
		}
		return "/" + s.Name + " is not a command I know. Send /help to see what I can do.", nil
	case ExtraText:
		return "/" + s.Name + " takes nothing after it, so nothing was done. Send just /" + s.Name + ".", nil
	}
	switch s.Name {
	case "help":
		st, err := sw.State(ctx)
		if err != nil {
			return "", err
		}
		if st.Paused {
			return helpText + "\nI am paused: only slash commands get an answer.", nil
		}
		return helpText, nil
	case "status":
		return status(ctx)
	case "facts":
		return listFacts(ctx, o.Facts)
	case "remember":
		return remember(ctx, o.Facts, s.Args, m.ID)
	case "forget":
		return forget(ctx, o.Facts, s.Args)
	case "summary":
		if o.Summary == nil {
			return "Summaries are not set up here.", nil
		}
		return o.Summary(ctx, s.Args)
	case "tasks":
		return listTasks(ctx, o.Tasks, o.Zone, o.Now())
	case "alerts":
		return switchAlerts(ctx, o.Alerts, s.Args)
	case "done":
		return done(ctx, o.Tasks, s.Args, o.Zone, o.Now())
	case "pause":
		paused, err := sw.Pause(ctx, commandID)
		if err != nil {
			return "", err
		}
		if paused {
			return "Paused. Until you send /resume I answer only slash commands, like /status.", nil
		}
		return "Already paused. Send /resume to start again.", nil
	default:
		resumed, err := sw.Resume(ctx, commandID)
		if err != nil {
			return "", err
		}
		if resumed {
			return "Resumed. I am answering again.", nil
		}
		return "I was not paused.", nil
	}
}
