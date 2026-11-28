package chat

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dhananjay6561/kaya/core/internal/agent"
)

var (
	amount      = regexp.MustCompile(`(?i)(₹|rs\.?\s?|inr\s?)?(\d[\d,]*(?:\.\d+)?)(\s?(?:%|lakhs?|lacs?|crores?|cr|k)\b|%)?`)
	highlighted = regexp.MustCompile(`\*{1,2}([^*\n]{2,80}?)\*{1,2}`)
	// capitalised is a run of two or more Capitalised Words, which in Hinglish is nearly always a
	// name: a place, a shop, a product.
	capitalised = regexp.MustCompile(`(?:^|[^\p{L}\p{N}])(\p{Lu}[\p{L}\p{N}'’&.]*(?:[ \x{00a0}\x{202f}]+\p{Lu}[\p{L}\p{N}'’&.]*)+)`)
	blankLines  = regexp.MustCompile(`\n{3,}`)
)

// ungrounded lists the prices, numbers and names in an answer that nothing in the
// evidence (what tools showed the model, and what DJ wrote) contains: a price with ₹ or Rs, a
// percentage or any number of three digits or more must appear in it, written with or without
// commas, and a bold name of two words or more needs at least half its words of three letters or
// more in it (one bold word is usually a label, such as "Tip:"), and so does a run of two or more
// Capitalised Words. Amounts in lakh, crore or k are left alone, since the model may have
// converted them.
func ungrounded(answer string, evidence []string) []string {
	hay := plain(strings.Join(evidence, "\n"))
	numbers := map[string]bool{}
	for _, m := range amount.FindAllStringSubmatch(hay, -1) {
		numbers[strings.ReplaceAll(m[2], ",", "")] = true
	}
	words := map[string]bool{}
	for _, w := range wordsOf(hay) {
		words[w] = true
	}
	var out []string
	add := func(claim string) {
		claim = strings.TrimSpace(claim)
		if claim != "" && !slices.ContainsFunc(out, func(c string) bool { return strings.Contains(plain(c), plain(claim)) }) {
			out = append(out, claim)
		}
	}
	text := plain(answer)
	for _, m := range amount.FindAllStringSubmatch(text, -1) {
		unit := strings.TrimSpace(strings.ToLower(m[3]))
		if unit != "" && unit != "%" {
			continue
		}
		n := strings.TrimRight(strings.ReplaceAll(m[2], ",", ""), ".")
		digits := strings.Count(strings.ReplaceAll(n, ".", ""), "") - 1
		if m[1] == "" && unit == "" && digits < 3 {
			continue
		}
		if !numbers[n] {
			add(strings.TrimSpace(m[0]))
		}
	}
	names := []string{}
	for _, m := range highlighted.FindAllStringSubmatch(answer, -1) {
		names = append(names, m[1])
	}
	for _, m := range capitalised.FindAllStringSubmatchIndex(answer, -1) {
		run := answer[m[2]:m[3]]
		if sentenceStart(answer[:m[2]]) {
			if i := strings.IndexFunc(run, unicode.IsSpace); i >= 0 {
				run = strings.TrimLeftFunc(run[i:], unicode.IsSpace)
			}
		}
		names = append(names, run)
	}
	for _, name := range names {
		var long []string
		for _, w := range wordsOf(plain(name)) {
			if len([]rune(w)) >= 3 {
				long = append(long, w)
			}
		}
		if len(long) < 2 {
			continue
		}
		found := 0
		for _, w := range long {
			if words[w] {
				found++
			}
		}
		if found*2 < len(long) {
			add(name)
		}
	}
	return out
}

// dropClaims removes every line of an answer that mentions one of the claims.
func dropClaims(answer string, claims []string) string {
	var kept []string
	for _, line := range strings.Split(answer, "\n") {
		l := plain(line)
		if !slices.ContainsFunc(claims, func(c string) bool { return strings.Contains(l, plain(c)) }) {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(kept, "\n"), "\n\n"))
}

// plain is text in lower case with every kind of space a plain space and every dash a hyphen,
// so "₹82,900" and "Rs 82,900" or "120–130" and "120-130" read the same.
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.Is(unicode.Pd, r):
			return '-'
		}
		return unicode.ToLower(r)
	}, s)
}

func wordsOf(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

// bestMatch is the first shown link whose site the answer names, such as "Zomato pe …" for a
// zomato.com page; "" when it names none, since a link chosen by shared words is often unrelated
// (every result of a search shares the place it was about).
func bestMatch(answer string, shown []agent.Shown) string {
	said := map[string]bool{}
	for _, w := range wordsOf(plain(answer)) {
		said[w] = true
	}
	for _, s := range shown {
		if name := siteName(s.Link); len([]rune(name)) >= 3 && said[name] {
			return s.Link
		}
	}
	return ""
}

// siteName is the name part of a link's host: "zomato" for www.zomato.com, "magicpin" for
// magicpin.in, "91mobiles" for www.91mobiles.com.
func siteName(link string) string {
	parts := strings.Split(siteOf(link), ".")
	if len(parts) < 2 {
		return ""
	}
	name := parts[len(parts)-2]
	if (name == "co" || name == "com" || name == "org" || name == "net") && len(parts) >= 3 {
		name = parts[len(parts)-3]
	}
	return strings.ToLower(name)
}

func texts(shown []agent.Shown) []string {
	out := make([]string, len(shown))
	for i, s := range shown {
		out[i] = s.Text
	}
	return out
}

// sentenceStart says whether text ends where a sentence begins: nothing, a line break, or the
// end of a sentence, a list marker or a colon, then spaces.
func sentenceStart(before string) bool {
	t := strings.TrimRightFunc(before, func(r rune) bool { return unicode.IsSpace(r) && r != '\n' })
	if t == "" || strings.HasSuffix(t, "\n") {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(t)
	return strings.ContainsRune(".!?:-–•*>", r)
}
