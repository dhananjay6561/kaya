package chat

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

var (
	amount      = regexp.MustCompile(`(?i)(₹|rs\.?\s?|inr\s?)?(\d[\d,]*(?:\.\d+)?)(\s?(?:%|lakhs?|lacs?|crores?|cr|k)\b|%)?`)
	highlighted = regexp.MustCompile(`\*{1,2}([^*\n]{2,80}?)\*{1,2}`)
	blankLines  = regexp.MustCompile(`\n{3,}`)
)

// ungrounded lists the prices, numbers and highlighted names in an answer that nothing in the
// evidence (what tools showed the model, and what DJ wrote) contains: a price with ₹ or Rs, a
// percentage or any number of three digits or more must appear in it, written with or without
// commas, and a bold name of two words or more needs at least half its words of three letters or
// more in it (one bold word is usually a label, such as "Tip:"). Amounts in lakh, crore or k are
// left alone, since the model may have converted them.
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
		if claim = strings.TrimSpace(claim); claim != "" && !slices.Contains(out, claim) {
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
	for _, m := range highlighted.FindAllStringSubmatch(answer, -1) {
		var long []string
		for _, w := range wordsOf(plain(m[1])) {
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
			add(m[1])
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
