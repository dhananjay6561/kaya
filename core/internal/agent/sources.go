package agent

import (
	"context"
	"slices"
	"sync"
)

type sourcesKey struct{}

type sources struct {
	mu    sync.Mutex
	links []string
}

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

func withSources(ctx context.Context) (context.Context, *sources) {
	s := &sources{}
	return context.WithValue(ctx, sourcesKey{}, s), s
}

func (s *sources) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.links)
}

// CollectSources returns ctx numbering cited links, and a function that reads them, for running
// a tool outside the loop, as tests do.
func CollectSources(ctx context.Context) (context.Context, func() []string) {
	ctx, s := withSources(ctx)
	return ctx, s.all
}
