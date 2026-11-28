package chat

import (
	"net/url"
	"regexp"
	"slices"
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
// answer, since a friend does not write them, and returns the links to put under it: the sources
// it cites, in the order cited, at most two and each from a different site, so a comparison can
// show both. An answer that cites none gets the result that best matches its words, or no link
// when none does, since the top result may be unrelated. With no sources the answer is left as
// it is.
func cite(text string, sources []string, shown []agent.Shown) (string, []string) {
	if len(sources) == 0 {
		return text, nil
	}
	var cited []int
	note := func(n int) {
		if n >= 1 && n <= len(sources) && !slices.Contains(cited, n) {
			cited = append(cited, n)
		}
	}
	out := citation.ReplaceAllStringFunc(text, func(m string) string {
		for _, part := range strings.Split(citation.FindStringSubmatch(m)[1], ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
				note(n)
			}
		}
		return ""
	})
	out = roundCitation.ReplaceAllStringFunc(out, func(m string) string {
		g := roundCitation.FindStringSubmatch(m)
		var ns []int
		for _, part := range strings.Split(g[1], ",") {
			n, err := strconv.Atoi(strings.Trim(part, " \t\n\u202f"))
			if err != nil || n < 1 || n > len(sources) {
				return m
			}
			ns = append(ns, n)
		}
		for _, n := range ns {
			note(n)
		}
		return g[2] + g[3]
	})
	if out != text {
		out = spaceBeforeIt.ReplaceAllString(out, "$1")
	}
	var links, sites []string
	for _, n := range cited {
		if s := siteOf(sources[n-1]); !slices.Contains(sites, s) && len(links) < maxLinks {
			links, sites = append(links, sources[n-1]), append(sites, s)
		}
	}
	if len(links) == 0 {
		if best := bestMatch(out, shown); best != "" {
			links = []string{best}
		}
	}
	return out, links
}

const maxLinks = 2

// siteOf is a link's host without "www.".
func siteOf(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}
