// Package search keeps the search index over DJ's chats: messages grouped into chunks, each
// embedded as one vector.
package search

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dhananjay6561/kaya/core/internal/text"
)

// Line is one message as it goes into a chunk.
type Line struct {
	MessageID int64
	ChatID    int64
	SentAt    time.Time
	UpdatedAt time.Time
	Sender    string
	Text      string
}

// Shape bounds a chunk: messages more than Gap apart start a new one, and a chunk holds at most
// MaxLines messages or about MaxChars characters. A single message is cut at MaxChars.
type Shape struct {
	Gap      time.Duration
	MaxChars int
	MaxLines int
}

// DefaultShape groups messages sent within 10 minutes, up to 30 of them or 1,500 characters.
var DefaultShape = Shape{Gap: 10 * time.Minute, MaxChars: 1500, MaxLines: 30}

// Group splits one chat's lines, oldest first, into the lines of each chunk.
func Group(lines []Line, s Shape) [][]Line {
	var out [][]Line
	var group []Line
	size := 0
	for _, line := range lines {
		length := min(utf8.RuneCountInString(line.Text), s.MaxChars) + utf8.RuneCountInString(line.Sender) + 8
		if len(group) > 0 {
			last := group[len(group)-1]
			if line.SentAt.Sub(last.SentAt) > s.Gap || size+length > s.MaxChars || len(group) >= s.MaxLines {
				out = append(out, group)
				group, size = nil, 0
			}
		}
		group = append(group, line)
		size += length
	}
	if len(group) > 0 {
		out = append(out, group)
	}
	return out
}

// Masker hides private details in a text; context is the text before it, which can say what a
// number is. One masker covers one chunk, so a detail gets the same placeholder throughout.
type Masker interface {
	Mask(text, context string) string
}

var (
	codeWords = regexp.MustCompile(`\b(?:otp|code|pin|mpin|passcode|verification|verify)\b`)
	justCode  = regexp.MustCompile(`^\d(?:[ -]?\d){3,7}$`)
)

// Render writes a chunk as its chat's name, then each day and "time sender: text" lines in zone.
//
// With a masker, the messages are masked together, one to a line, so a word in one message counts
// for a number in the next; no masking rule crosses a line, so each message comes back on its own
// line. before is the text of the messages just before the chunk in the same chat. When the chunk
// or before mentions an OTP, a code or a PIN, or a message is nothing but a 4 to 8 digit number,
// every code-shaped number in the chunk is masked as an OTP: the masking rules want the word right
// before the number, and chat puts it a verb or a message earlier ("code bhej" ... "482913"), so for
// text leaving the machine it is safer to mask a little too much. Names are masked apart from the
// text, and nothing is cut until it is masked, so a cut never splits a detail.
func Render(title string, lines []Line, zone *time.Location, s Shape, m Masker, before string) string {
	texts := make([]string, len(lines))
	codeShaped := false
	for i, line := range lines {
		texts[i] = oneLine(line.Text)
		codeShaped = codeShaped || justCode.MatchString(texts[i])
	}
	mask := func(text string) string { return text }
	if m != nil {
		mask = func(text string) string { return m.Mask(text, "") }
		joined := strings.Join(texts, "\n")
		hint := ""
		if codeShaped || codeWords.MatchString(strings.ToLower(before+"\n"+joined)) {
			hint = "otp"
		}
		if masked := strings.Split(m.Mask(joined, hint), "\n"); len(masked) == len(texts) {
			texts = masked
		} else {
			for i := range texts {
				texts[i] = m.Mask(texts[i], "")
			}
		}
	}
	var b strings.Builder
	b.WriteString("Chat: " + cut(oneLine(mask(title)), 80))
	day := ""
	for i, line := range lines {
		at := line.SentAt.In(zone)
		if d := at.Format("2 Jan 2006"); d != day {
			b.WriteString("\n" + d)
			day = d
		}
		b.WriteString("\n" + at.Format("15:04") + " " + cut(oneLine(mask(line.Sender)), 60) + ": " + cut(texts[i], s.MaxChars))
	}
	return b.String()
}

// Hash is the sha256 of a chunk's unmasked text, to notice when it changes.
func Hash(rendered string) string {
	sum := sha256.Sum256([]byte(rendered))
	return hex.EncodeToString(sum[:])
}

// oneLine joins a text's lines and runs of white space into single spaces.
func oneLine(s string) string { return strings.Join(strings.FieldsFunc(s, text.IsSpace), " ") }

var placeholder = regexp.MustCompile(`\[[A-Z]+_\d+\]`)

// cut keeps about max characters: it stops at the last space in the second half when there is
// one, never inside a placeholder, and marks the cut with an ellipsis.
func cut(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	limit := len(s)
	for i := range s {
		if max == 0 {
			limit = i
			break
		}
		max--
	}
	end := limit
	if space := strings.LastIndexByte(s[:limit], ' '); space > limit/2 {
		end = space
	}
	for _, loc := range placeholder.FindAllStringIndex(s, -1) {
		if loc[0] < end && end < loc[1] {
			end = loc[0]
		}
	}
	return strings.TrimRightFunc(s[:end], text.IsSpace) + "…"
}
