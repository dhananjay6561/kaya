package agent

import (
	"context"
	"slices"
	"strings"
	"sync"
)

type sourcesKey struct{}

type sources struct {
	mu       sync.Mutex
	links    []string
	evidence []Shown
}

// Shown is public text a tool showed the model, such as a search result's title and snippet, and
// the link it came from.
type Shown struct{ Link, Text string }

// Cite numbers a link for this run, so a tool can label a result [n] and the answer can cite it
// by that number: the same link always gets the same number, and results of two calls never share
// one. Outside a run it returns 0.
func Cite(ctx context.Context, link string) int {
	s, ok := ctx.Value(sourcesKey{}).(*sources)
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.Index(s.links, link); i >= 0 {
		return i + 1
	}
	s.links = append(s.links, link)
	return len(s.links)
}

// Witness keeps public text a tool showed the model, such as a search result's title and
// snippet, with the link it came from, so the answer can be checked against it: a name or a
// price the model gives that no tool showed it was made up. Outside a run it does nothing.
func Witness(ctx context.Context, link, text string) {
	s, ok := ctx.Value(sourcesKey{}).(*sources)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evidence = append(s.evidence, Shown{Link: link, Text: text})
}

func withSources(ctx context.Context) (context.Context, *sources) {
	s := &sources{}
	return context.WithValue(ctx, sourcesKey{}, s), s
}

func (s *sources) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.links)
}

func (s *sources) witnessed() []Shown {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.evidence)
}

// CollectSources returns ctx numbering cited links, and a function that reads them, for running
// a tool outside the loop, as tests do.
func CollectSources(ctx context.Context) (context.Context, func() []string) {
	ctx, s := withSources(ctx)
	return ctx, s.all
}

// unknownLink is the first Links argument that is neither a link DJ wrote nor one a tool
// returned in this run; "" when every one is known.
func unknownLink(ctx context.Context, tool Tool, args any, owner string) string {
	fields, _ := args.(map[string]any)
	var known []string
	if s, ok := ctx.Value(sourcesKey{}).(*sources); ok {
		known = s.all()
	}
	said := sameLink(owner)
	for _, name := range tool.Links {
		value, _ := fields[name].(string)
		link := sameLink(value)
		if link == "" || (!strings.Contains(said, link) && !slices.ContainsFunc(known, func(k string) bool { return sameLink(k) == link })) {
			return value
		}
	}
	return ""
}

// sameLink is a link written the way two copies of it compare: lower case, with no scheme, no
// "www.", no fragment and no trailing slash. Applied to a whole message, it lets a link the
// model wrote be found in what DJ typed, with or without https://.
func sameLink(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, "#"); i >= 0 && !strings.Contains(s, " ") {
		s = s[:i]
	}
	s = strings.NewReplacer("https://", "", "http://", "", "www.", "").Replace(s)
	return strings.TrimRight(s, "/")
}
