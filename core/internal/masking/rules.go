package masking

import (
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/dhananjay6561/kaya/core/internal/text"
)

// space is JavaScript's \s. Go's \s is ASCII only, so it is never used here.
const space = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// surrogate stands in for each half of a character outside the BMP, so one rune is one UTF-16
// unit, as in JavaScript. Like a surrogate, it is not a letter, digit, word character or space.
const surrogate = ''

var digitZeros = []rune{0x660, 0x6f0, 0x966, 0x9e6, 0xa66, 0xae6, 0xb66, 0xbe6, 0xc66, 0xce6, 0xd66, 0xff10}

type span struct {
	start, end int
	kind       string
}

// scan is the text a mask call works on, indexed in UTF-16 units.
type scan struct {
	units []uint16 // the escaped input
	ascii []uint16 // units with Indian, Arabic and full-width digits read as ASCII
	runes []rune   // ascii, one rune per unit, surrogates replaced, for the regular expressions
	text  string   // runes as UTF-8
	lower string   // text with ASCII letters lowered, for rules JavaScript ran with the i flag
	at    []int    // byte offset in text of each unit, and len(text) at the end
	unit  []int    // unit at each byte offset that starts a rune
	// context is the text around this one, digits read as ASCII and ASCII letters lowered.
	context string
	// contextSays caches whether the context names an OTP or an account, which every candidate asks.
	contextSays map[*regexp.Regexp]bool
	taken       []bool
	spans       []span
	// places holds, for each unit, the lengths of the address words that start there.
	places [][]int
	upi    *upiRuns
}

func newScan(escaped, context string) *scan {
	units := utf16.Encode([]rune(escaped))
	n := len(units)
	sc := &scan{units: units, ascii: make([]uint16, n), runes: make([]rune, n), at: make([]int, n+1), taken: make([]bool, n)}
	var b, lower strings.Builder
	for i, u := range units {
		sc.ascii[i] = u
		if d := asciiDigit(rune(u)); d != rune(u) {
			sc.ascii[i] = uint16(d) //nolint:gosec // an ASCII digit
		}
		r := rune(sc.ascii[i])
		if utf16.IsSurrogate(r) {
			r = surrogate
		}
		sc.runes[i] = r
		sc.at[i] = b.Len()
		b.WriteRune(r)
		lower.WriteRune(asciiLower(r))
	}
	sc.at[n] = b.Len()
	sc.text, sc.lower = b.String(), lower.String()
	sc.unit = make([]int, len(sc.text)+1)
	for i, off := range sc.at {
		sc.unit[off] = i
	}
	sc.context = lowered(digitsToASCII(context))
	sc.contextSays = map[*regexp.Regexp]bool{}
	return sc
}

func asciiDigit(r rune) rune {
	for _, zero := range digitZeros {
		if r >= zero && r <= zero+9 {
			return '0' + r - zero
		}
	}
	return r
}

func digitsToASCII(s string) string { return strings.Map(asciiDigit, s) }

func asciiLower(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + 'a' - 'A'
	}
	return r
}

func lowered(s string) string { return strings.Map(asciiLower, s) }

// slice is the units from i to j of the ASCII-digit text, clamped like JavaScript's slice.
func (sc *scan) slice(i, j int) string {
	i, j = max(0, min(i, len(sc.ascii))), max(0, min(j, len(sc.ascii)))
	if i >= j {
		return ""
	}
	return decode(sc.ascii[i:j])
}

// around is the text before and after a match, joined by a space, lowered for the word lists.
func (sc *scan) around(start, end, before, after int) string {
	return lowered(sc.slice(start-before, start) + " " + sc.slice(end, end+after))
}

// contextHas reports whether the context matches words, checking it once per mask call.
func (sc *scan) contextHas(words *regexp.Regexp) bool {
	has, ok := sc.contextSays[words]
	if !ok {
		has = words.MatchString(sc.context)
		sc.contextSays[words] = has
	}
	return has
}

func (sc *scan) runeAt(i int) rune {
	if i < 0 || i >= len(sc.runes) {
		return -1
	}
	return sc.runes[i]
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isWord(r rune) bool {
	return isDigit(r) || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

func isLineBreak(r rune) bool { return r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029 }

// rule is one of the TypeScript rules. find locates where a match may start; exact, with the
// character before the start when there is one, decides whether it does and where it ends.
type rule struct {
	kind  string
	lower bool
	clip  bool
	// longest is the most units a match can take, or 0 when it has no limit. The exact pattern
	// then sees only that much text (and a little more for what follows), so Go uses its fast
	// matcher instead of running over the whole rest of the text.
	longest int
	find    *regexp.Regexp
	exact   [2]*regexp.Regexp
	// before checks a look-behind or a leading \b at the start; after a look-ahead that is checked
	// by hand at the end.
	before func(sc *scan, start int) bool
	after  func(sc *scan, end int) bool
	// candidate, when set, finds where a match may start instead of find, and exactAt, when set,
	// decides a match at start instead of exact.
	candidate func(sc *scan, hay string, p int) (int, bool)
	exactAt   func(sc *scan, start int) (int, bool)
	accept    func(sc *scan, value string, start, end int) bool
}

// newRule builds a rule from core, the match itself, and trail, a look-ahead written as what may
// follow, which is matched but not kept. find is core with its \b assertions removed, so it finds
// every place exact can match and maybe more.
func newRule(kind, core, trail string) rule {
	return rule{
		kind: kind,
		find: regexp.MustCompile(strings.ReplaceAll(core, `\b`, "")),
		exact: [2]*regexp.Regexp{
			regexp.MustCompile(`^(` + core + `)` + trail),
			regexp.MustCompile(`^(?s:.)(` + core + `)` + trail),
		},
	}
}

// match finds the first match at or after unit p, as JavaScript's exec would.
func (r *rule) match(sc *scan, p int) (start, end int, ok bool) {
	hay := sc.text
	if r.lower {
		hay = sc.lower
	}
	for p < len(sc.units) {
		if start, ok = r.candidateAt(sc, hay, p); !ok {
			return 0, 0, false
		}
		if end, ok = r.matchAt(sc, hay, start); ok {
			return start, end, true
		}
		p = start + 1
	}
	return 0, 0, false
}

func (r *rule) candidateAt(sc *scan, hay string, p int) (int, bool) {
	if r.candidate != nil {
		return r.candidate(sc, hay, p)
	}
	return firstAt(r.find, sc, hay, p)
}

// firstAt is the unit where re first matches at or after unit p.
func firstAt(re *regexp.Regexp, sc *scan, hay string, p int) (int, bool) {
	loc := re.FindStringIndex(hay[sc.at[p]:])
	if loc == nil {
		return 0, false
	}
	return sc.unit[sc.at[p]+loc[0]], true
}

func (r *rule) matchAt(sc *scan, hay string, start int) (int, bool) {
	if r.before != nil && !r.before(sc, start) {
		return 0, false
	}
	if r.exactAt != nil {
		return r.exactAt(sc, start)
	}
	stop := len(hay)
	if r.longest > 0 {
		stop = sc.at[min(len(sc.units), start+r.longest+3)]
	}
	var m []int
	if start == 0 {
		m = r.exact[0].FindStringSubmatchIndex(hay[:stop])
	} else {
		m = r.exact[1].FindStringSubmatchIndex(hay[sc.at[start-1]:stop])
		if m != nil {
			m[3] += sc.at[start-1]
		}
	}
	if m == nil {
		return 0, false
	}
	end := sc.unit[m[3]]
	if r.after != nil && !r.after(sc, end) {
		return 0, false
	}
	return end, true
}

func trailing(r rune) bool { return text.IsSpace(r) || strings.ContainsRune(",.;:", r) }

// run finds every match of the rule, as the TypeScript find did: a match is trimmed, an address
// is cut short at a detail already found, and a match that overlaps one or is not accepted is
// tried again one unit later.
func (r *rule) run(sc *scan) {
	for p := 0; p < len(sc.units); {
		index, matchEnd, ok := r.match(sc, p)
		if !ok {
			return
		}
		start, end := index, matchEnd
		for start < end && text.IsSpace(sc.runes[start]) {
			start++
		}
		for end > start && trailing(sc.runes[end-1]) {
			end--
		}
		if r.clip {
			if first := sc.firstTaken(start, end); first > start {
				end = first
				for end > start && trailing(sc.runes[end-1]) {
					end--
				}
			}
		}
		if end > start {
			// A span never splits a character made of two UTF-16 units, such as an emoji.
			if start > 0 && utf16.IsSurrogate(rune(sc.units[start])) && sc.units[start] >= 0xdc00 && sc.units[start-1] < 0xdc00 && utf16.IsSurrogate(rune(sc.units[start-1])) {
				start--
			}
			if end < len(sc.units) && sc.units[end-1] >= 0xd800 && sc.units[end-1] < 0xdc00 && sc.units[end] >= 0xdc00 && sc.units[end] <= 0xdfff {
				end++
			}
		}
		if end <= start || sc.firstTaken(start, end) >= 0 ||
			(r.accept != nil && !r.accept(sc, sc.slice(start, end), start, end)) {
			p = index + 1
			continue
		}
		sc.spans = append(sc.spans, span{start: start, end: end, kind: r.kind})
		for i := start; i < end; i++ {
			sc.taken[i] = true
		}
		p = matchEnd
	}
}

func (sc *scan) firstTaken(start, end int) int {
	for i := start; i < end; i++ {
		if sc.taken[i] {
			return i
		}
	}
	return -1
}

func utf16Encode(s string) []uint16 { return utf16.Encode([]rune(s)) }

func utf16Decode(u []uint16) []rune { return utf16.Decode(u) }
