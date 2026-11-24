// Package text holds the small text rules core shares, written to behave like the TypeScript core's.
package text

import (
	"strings"

	"github.com/rivo/uniseg"
)

// Trim cuts the characters JavaScript's trim() does, so text compares the same way it did in the
// TypeScript core.
func Trim(s string) string { return strings.TrimFunc(s, IsSpace) }

// IsSpace reports whether r is white space or a line break to JavaScript.
func IsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Clip keeps the first max characters of s, as code points, and marks a cut with an ellipsis.
// Invalid UTF-8 is replaced first, so the result is always safe to store and send.
func Clip(s string, max int) string {
	s = strings.ToValidUTF8(s, "�")
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// FirstGraphemes keeps the first n characters of s as a person sees them, so a joined emoji is
// never split.
func FirstGraphemes(s string, n int) string {
	var b strings.Builder
	g := uniseg.NewGraphemes(s)
	for i := 0; i < n && g.Next(); i++ {
		b.WriteString(g.Str())
	}
	return b.String()
}
