package chat

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/dhananjay6561/kaya/core/internal/agent"
)

var (
	citation      = regexp.MustCompile(`[ \t]*[\[【](\d{1,3}(?:\s*,\s*\d{1,3})*)(?:†[^\]】]*)?[\]】]`)
	spaceBeforeIt = regexp.MustCompile(`[ \t]+([.,;:!?)])`)
	// roundCitation is a number list in round brackets at the end of a sentence or line, which the
	// model sometimes writes instead of square ones; mid-sentence, "(2)" is left alone.
	roundCitation = regexp.MustCompile(`[ \t\x{202f}]*\((\d{1,3}(?:[\s\x{202f}]*,[\s\x{202f}]*\d{1,3})*)\)([.!?]?)(\s*(?:\n|$))`)
)

// cite takes the model's citations ([n], or gpt-oss's own 【n】 and 【n†source】) out of an
// answer, since a friend does not write them, and returns the one link to put under it: the
// first source cited. An answer that cites none gets the result that best matches its words,
// or no link when none does, since the top result may be unrelated. With no sources the answer is
// left as it is.
func cite(text string, sources []string, shown []agent.Shown) (string, string) {
	if len(sources) == 0 {
		return text, ""
	}
	link := ""
	out := citation.ReplaceAllStringFunc(text, func(m string) string {
		for _, part := range strings.Split(citation.FindStringSubmatch(m)[1], ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if link == "" && err == nil && n >= 1 && n <= len(sources) {
				link = sources[n-1]
			}
		}
		return ""
	})
	out = roundCitation.ReplaceAllStringFunc(out, func(m string) string {
		g := roundCitation.FindStringSubmatch(m)
		for _, part := range strings.Split(g[1], ",") {
			n, err := strconv.Atoi(strings.Trim(part, " \t\n\u202f"))
			if err != nil || n < 1 || n > len(sources) {
				return m
			}
			if link == "" {
				link = sources[n-1]
			}
		}
		return g[2] + g[3]
	})
	if out != text {
		out = spaceBeforeIt.ReplaceAllString(out, "$1")
	}
	if link == "" {
		link = bestMatch(out, shown)
	}
	return out, link
}
