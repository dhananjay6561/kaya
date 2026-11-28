package chat

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	citation      = regexp.MustCompile(`\[(\d{1,3}(?:\s*,\s*\d{1,3})*)\]`)
	spaceBeforeIt = regexp.MustCompile(`[ \t]+([.,;:!?)])`)
	doubleSpace   = regexp.MustCompile(`[ \t]{2,}`)
)

// cite turns an answer's [n] citations into links under it: the sources it cites are numbered
// again in the order they first appear, numbers that name no source are dropped, and the links
// are returned as "[n] link" lines. With no sources the answer is left as it is.
func cite(text string, sources []string) (string, []string) {
	if len(sources) == 0 {
		return text, nil
	}
	renumbered := map[int]int{}
	var lines []string
	out := citation.ReplaceAllStringFunc(text, func(m string) string {
		var kept []string
		for _, part := range strings.Split(strings.Trim(m, "[]"), ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 || n > len(sources) {
				continue
			}
			k, ok := renumbered[n]
			if !ok {
				lines = append(lines, fmt.Sprintf("[%d] %s", len(lines)+1, sources[n-1]))
				k = len(lines)
				renumbered[n] = k
			}
			if label := "[" + strconv.Itoa(k) + "]"; !slices.Contains(kept, label) {
				kept = append(kept, label)
			}
		}
		return strings.Join(kept, "")
	})
	if out != text {
		out = doubleSpace.ReplaceAllString(spaceBeforeIt.ReplaceAllString(out, "$1"), " ")
	}
	return out, lines
}
