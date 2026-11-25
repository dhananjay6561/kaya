// Package memory builds the context package for a question: the system prompt, as much of the
// conversation with DJ as fits the model's token budget, and the question itself.
package memory

import (
	"strconv"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/llm"
)

// Exchange is one past question from DJ and Kaya's answer to it.
type Exchange struct {
	Question string
	Answer   string
	At       time.Time
}

// Input is what Build needs. Count is the model's own token counter.
type Input struct {
	System   string
	History  []Exchange
	Question string
	Budget   int
	Count    func(text string) int
	Now      time.Time
}

// Package is the context sent to the model, with how many tokens it was counted at and how many
// past exchanges it kept and left out.
type Package struct {
	Messages  []llm.Message
	Tokens    int
	Exchanges int
	LeftOut   int
}

const (
	requestOverhead = 48
	minShortened    = 64
	leftOutNote     = "Older parts of this conversation were left out to save space."
)

// HowLongAgo says an age the way a person would.
func HowLongAgo(ms int64) string {
	minutes := max(0, ms) / 60_000
	switch {
	case minutes < 1:
		return "just now"
	case minutes < 60:
		return strconv.FormatInt(minutes, 10) + " min ago"
	case minutes/60 < 24:
		return strconv.FormatInt(minutes/60, 10) + " h ago"
	case minutes/60/24 == 1:
		return "1 day ago"
	default:
		return strconv.FormatInt(minutes/60/24, 10) + " days ago"
	}
}

// Shorten cuts text from the middle, keeping its start and its end, until it fits maxTokens.
func Shorten(text string, maxTokens int, count func(string) int) string {
	if count(text) <= maxTokens {
		return text
	}
	characters := []rune(text)
	keep := len(characters)
	for keep > 16 {
		keep = int(float64(keep) * 0.7)
		half := keep / 2
		candidate := string(characters[:half]) + " … " + string(characters[len(characters)-half:])
		if count(candidate) <= maxTokens {
			return candidate
		}
	}
	return "…"
}

// Build fills the budget with the newest exchanges first, stops at the first that does not fit,
// and shortens the newest one rather than drop it when there is room for part of it.
func Build(in Input) Package {
	count := in.Count
	marked := func(e Exchange, question string) string {
		return "(sent " + HowLongAgo(in.Now.UnixMilli()-e.At.UnixMilli()) + ") " + question
	}
	system := in.System
	if len(in.History) > 0 {
		system = in.System + "\n" + leftOutNote
	}
	tokens := requestOverhead + count(system) + count(in.Question)
	var kept [][2]string
	for i := range in.History {
		e := in.History[len(in.History)-1-i]
		question := marked(e, e.Question)
		if cost := count(question) + count(e.Answer); tokens+cost <= in.Budget {
			tokens += cost
			kept = append(kept, [2]string{question, e.Answer})
			continue
		}
		if room := in.Budget - tokens; i == 0 && room >= minShortened {
			marker := count(marked(e, ""))
			question = marked(e, Shorten(e.Question, room/2-marker, count))
			answer := Shorten(e.Answer, room-count(question), count)
			tokens += count(question) + count(answer)
			kept = append(kept, [2]string{question, answer})
		}
		break
	}
	leftOut := len(in.History) - len(kept)
	if leftOut == 0 {
		system = in.System
	}
	messages := make([]llm.Message, 0, 2*len(kept)+2)
	messages = append(messages, llm.Message{Role: llm.System, Content: system})
	for i := len(kept) - 1; i >= 0; i-- {
		messages = append(messages,
			llm.Message{Role: llm.User, Content: kept[i][0]},
			llm.Message{Role: llm.Assistant, Content: kept[i][1]})
	}
	messages = append(messages, llm.Message{Role: llm.User, Content: in.Question})
	return Package{Messages: messages, Tokens: tokens, Exchanges: len(kept), LeftOut: leftOut}
}
