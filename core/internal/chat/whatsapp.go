package chat

import (
	"strings"

	"github.com/dhananjay6561/kaya/core/internal/text"
)

// ToWhatsApp turns the Markdown models write into what WhatsApp shows: headings become bold,
// **bold** and __italic__ become *bold* and _italic_, links show their address, and * or +
// bullets become dashes. Code is left alone. It follows the TypeScript core's regular expressions
// exactly, written out by hand because Go's have no look-around.
func ToWhatsApp(s string) string {
	var b strings.Builder
	for _, part := range splitCode(s) {
		if part.code {
			b.WriteString(part.text)
			continue
		}
		r := []rune(part.text)
		r = headings(r)
		r = emphasis(r, []rune("***"), "*_", "_*")
		r = emphasis(r, []rune("**"), "*", "*")
		r = emphasis(r, []rune("__"), "_", "_")
		r = links(r)
		r = bullets(r)
		b.WriteString(string(r))
	}
	lines := strings.Split(b.String(), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRightFunc(line, text.IsSpace)
	}
	return text.Trim(strings.Join(lines, "\n"))
}

type part struct {
	text string
	code bool
}

// splitCode cuts out ```fenced``` blocks and `inline` code, as /(```[\s\S]*?```|`[^`\n]+`)/ did.
func splitCode(s string) []part {
	var parts []part
	last := 0
	for i := 0; i < len(s); {
		end := codeAt(s, i)
		if end < 0 {
			i++
			continue
		}
		parts = append(parts, part{text: s[last:i]}, part{text: s[i:end], code: true})
		last, i = end, end
	}
	return append(parts, part{text: s[last:]})
}

func codeAt(s string, i int) int {
	if strings.HasPrefix(s[i:], "```") {
		if j := strings.Index(s[i+3:], "```"); j >= 0 {
			return i + 3 + j + 3
		}
	}
	if s[i] != '`' {
		return -1
	}
	j := i + 1
	for j < len(s) && s[j] != '`' && s[j] != '\n' {
		j++
	}
	if j < len(s) && s[j] == '`' && j > i+1 {
		return j + 1
	}
	return -1
}

func lineBreak(r rune) bool { return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029 }

func blank(r rune) bool { return r == ' ' || r == '\t' }

// word is [\w*_] with JavaScript's ASCII \w.
func word(r rune) bool {
	return r == '_' || r == '*' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// eachLine calls fn on every line, split where JavaScript's ^ and $ match in multiline mode, and
// joins the results back with the original breaks.
func eachLine(r []rune, fn func(line []rune) []rune) []rune {
	out := make([]rune, 0, len(r))
	start := 0
	for i := 0; i <= len(r); i++ {
		if i < len(r) && !lineBreak(r[i]) {
			continue
		}
		out = append(out, fn(r[start:i])...)
		if i < len(r) {
			out = append(out, r[i])
		}
		start = i + 1
	}
	return out
}

// headings replaces /^[ \t]*#{1,6}[ \t]+(.+?)(?:[ \t]+#+)?[ \t]*$/gm with the title in bold,
// without its stars or runs of underscores.
func headings(r []rune) []rune {
	return eachLine(r, func(line []rune) []rune {
		i := 0
		for i < len(line) && blank(line[i]) {
			i++
		}
		hashes := i
		for i < len(line) && line[i] == '#' {
			i++
		}
		if n := i - hashes; n < 1 || n > 6 {
			return line
		}
		gap := i
		for i < len(line) && blank(line[i]) {
			i++
		}
		if i == gap {
			return line
		}
		var title []rune
		if i == len(line) {
			// Only blanks follow: [ \t]+ gives its last one back so (.+?) has a character.
			if i-gap < 2 {
				return line
			}
			title = line[i-1:]
		} else {
			title = headingTitle(line[i:])
		}
		return []rune("*" + stripMarkers(string(title)) + "*")
	})
}

// headingTitle is the shortest title (.+?) can take from rest, which starts with a character that
// is not blank: the rest is cut before closing hashes ([ \t]+#+) and trailing blanks.
func headingTitle(rest []rune) []rune {
	end := len(rest)
	for end > 0 && blank(rest[end-1]) {
		end--
	}
	hashes := end
	for hashes > 0 && rest[hashes-1] == '#' {
		hashes--
	}
	gap := hashes
	for gap > 0 && blank(rest[gap-1]) {
		gap--
	}
	if hashes < end && gap < hashes && gap > 0 {
		return rest[:gap]
	}
	return rest[:end]
}

// stripMarkers removes /\*+|_{2,}/g.
func stripMarkers(s string) string {
	var b strings.Builder
	r := []rune(s)
	for i := 0; i < len(r); {
		switch {
		case r[i] == '*':
			i++
		case r[i] == '_' && i+1 < len(r) && r[i+1] == '_':
			for i < len(r) && r[i] == '_' {
				i++
			}
		default:
			b.WriteRune(r[i])
			i++
		}
	}
	return b.String()
}

// emphasis replaces (?<![\w*_])M(?=\S)(.+?)(?<=\S)M(?![\w*_]) with open + $1 + close, where M is
// the marker. Like JavaScript, it looks behind and ahead at the text as it was and carries on
// after each match; the nearest closer and line break are found ahead of time so it stays linear.
func emphasis(r []rune, marker []rune, open, close string) []rune {
	m, n := len(marker), len(r)
	at := func(i int) bool {
		if i+m > n {
			return false
		}
		for k := range m {
			if r[i+k] != marker[k] {
				return false
			}
		}
		return true
	}
	nextCloser := make([]int, n+1)
	nextBreak := make([]int, n+1)
	nextCloser[n], nextBreak[n] = n, n
	for j := n - 1; j >= 0; j-- {
		nextCloser[j], nextBreak[j] = nextCloser[j+1], nextBreak[j+1]
		if j > 0 && at(j) && !text.IsSpace(r[j-1]) && (j+m == n || !word(r[j+m])) {
			nextCloser[j] = j
		}
		if lineBreak(r[j]) {
			nextBreak[j] = j
		}
	}
	out := make([]rune, 0, n)
	for i := 0; i < n; {
		if at(i) && (i == 0 || !word(r[i-1])) && i+m < n && !text.IsSpace(r[i+m]) {
			if j := nextCloser[i+m+1]; j < n && j <= nextBreak[i+m] {
				out = append(out, []rune(open)...)
				out = append(out, r[i+m:j]...)
				out = append(out, []rune(close)...)
				i = j + m
				continue
			}
		}
		out = append(out, r[i])
		i++
	}
	return out
}

// links replaces /\[([^\]]+)\]\((https?:\/\/[^\s]+?)\)(?!\))/g with "$1 ($2)".
func links(r []rune) []rune {
	n := len(r)
	nextBracket := make([]int, n+1)
	nextSpace := make([]int, n+1)
	nextParen := make([]int, n+1)
	nextBracket[n], nextSpace[n], nextParen[n] = n, n, n
	for k := n - 1; k >= 0; k-- {
		nextBracket[k], nextSpace[k], nextParen[k] = nextBracket[k+1], nextSpace[k+1], nextParen[k+1]
		if r[k] == ']' {
			nextBracket[k] = k
		}
		if text.IsSpace(r[k]) {
			nextSpace[k] = k
		}
		if r[k] == ')' && (k+1 == n || r[k+1] != ')') {
			nextParen[k] = k
		}
	}
	out := make([]rune, 0, n)
	for i := 0; i < n; {
		if r[i] == '[' {
			j := nextBracket[i+1]
			if j > i+1 && j+1 < n && r[j+1] == '(' {
				if body := j + 2 + scheme(r[j+2:]); body > j+2 && body < n {
					if k := nextParen[body+1]; k < n && nextSpace[body] >= k {
						out = append(out, r[i+1:j]...)
						out = append(out, ' ', '(')
						out = append(out, r[j+2:k]...)
						out = append(out, ')')
						i = k + 1
						continue
					}
				}
			}
		}
		out = append(out, r[i])
		i++
	}
	return out
}

// scheme is the length of an http:// or https:// prefix, or 0.
func scheme(r []rune) int {
	s := string(r[:min(len(r), 8)])
	switch {
	case strings.HasPrefix(s, "https://"):
		return 8
	case strings.HasPrefix(s, "http://"):
		return 7
	}
	return 0
}

// bullets replaces /^([ \t]*)[*+][ \t]+/gm with "$1- ".
func bullets(r []rune) []rune {
	return eachLine(r, func(line []rune) []rune {
		i := 0
		for i < len(line) && blank(line[i]) {
			i++
		}
		if i == len(line) || line[i] != '*' && line[i] != '+' {
			return line
		}
		j := i + 1
		for j < len(line) && blank(line[j]) {
			j++
		}
		if j == i+1 {
			return line
		}
		out := append([]rune{}, line[:i]...)
		out = append(out, '-', ' ')
		return append(out, line[j:]...)
	})
}

// ClipTo keeps the first length characters and marks a cut with an ellipsis.
func ClipTo(s string, length int) string { return text.Clip(s, length) }
