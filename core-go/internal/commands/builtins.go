package commands

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/core/internal/text"
)

var builtinNames = []string{"help", "status", "pause", "resume"}

// Slash kinds.
const (
	Builtin   = "builtin"
	ExtraText = "extra_text"
	Unknown   = "unknown"
)

// Slash is a message that starts with a slash: a built-in, a built-in with words after it, or an
// unknown command.
type Slash struct {
	Kind string
	Name string
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
	if len(words) > 1 {
		return &Slash{Kind: ExtraText, Name: name}
	}
	return &Slash{Kind: Builtin, Name: name}
}

// IsBuiltin reports whether message is exactly one of the four built-ins; only these are urgent.
func IsBuiltin(message string) bool {
	s := ParseSlash(message)
	return s != nil && s.Kind == Builtin
}

const helpText = "Commands:\n" +
	"/help: this list\n" +
	"/status: how Kaya is doing\n" +
	"/pause: stop answering until /resume\n" +
	"/resume: start answering again\n" +
	"Anything else you send here goes to an AI model, which answers with your recent conversation in mind. It does not see your other chats yet."

// answerSlash works out the reply to a slash command; /pause and /resume flip the switch.
func answerSlash(ctx context.Context, s *Slash, commandID string, sw pause.Switch, status func(context.Context) (string, error)) (string, error) {
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
