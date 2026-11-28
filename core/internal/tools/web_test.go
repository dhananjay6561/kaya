package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/agent"
	"github.com/dhananjay6561/kaya/core/internal/masking"
	"github.com/dhananjay6561/kaya/core/internal/web"
)

type searcher struct {
	query, within string
	limit         int
	results       []web.Result
	err           error
}

func (s *searcher) Search(_ context.Context, query, within string, limit int) ([]web.Result, error) {
	s.query, s.within, s.limit = query, within, limit
	return s.results, s.err
}

func private(s string) bool { return masking.NewSession().Mask(s, "") != s }

func TestWebSearchListsResultsWithTheirLinks(t *testing.T) {
	s := &searcher{results: []web.Result{
		{
			Title: "iPhone 17 price in India", URL: "https://shop.example/iphone-17", Snippet: "Starts at Rs 82,900.",
			Published: time.Date(2026, 11, 26, 20, 0, 0, 0, time.UTC),
		},
		{Title: "Untitled", URL: "https://www.news.example/a"},
	}}
	got, err := run(t, WebSearch(s, private, kolkata(t)), `{"query":"  iphone 17 price india ","recent":"week"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := "[1] iPhone 17 price in India (shop.example, 27 Nov 2026)\nStarts at Rs 82,900.\n" +
		"[2] Untitled (news.example)"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if s.query != "iphone 17 price india" || s.within != "week" || s.limit != webResults {
		t.Fatalf("asked %+v", s)
	}
}

func TestWebSearchSaysWhenNothingWasFound(t *testing.T) {
	got, err := run(t, WebSearch(&searcher{}, private, time.UTC), `{"query":"zzqx"}`)
	if err != nil || got != `Nothing found for "zzqx".` {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestWebSearchNeverSendsAPrivateDetail(t *testing.T) {
	for _, q := range []string{"who owns +91 98765 43210", "priya.sharma@example.com linkedin"} {
		s := &searcher{}
		_, err := run(t, WebSearch(s, private, time.UTC), `{"query":"`+q+`"}`)
		var problem *agent.Problem
		if !errors.As(err, &problem) || s.query != "" {
			t.Errorf("%q: searched %q, err %v", q, s.query, err)
		}
	}
}

func TestWebSearchTellsTheModelWhenSearchIsDown(t *testing.T) {
	for _, cause := range []error{web.ErrNoEngines, web.ErrUnavailable} {
		_, err := run(t, WebSearch(&searcher{err: cause}, private, time.UTC), `{"query":"news"}`)
		var problem *agent.Problem
		if !errors.As(err, &problem) || !strings.Contains(problem.Message, "not working") {
			t.Errorf("%v: got %v", cause, err)
		}
	}
	other := errors.New("boom")
	if _, err := run(t, WebSearch(&searcher{err: other}, private, time.UTC), `{"query":"news"}`); !errors.Is(err, other) {
		t.Errorf("got %v", err)
	}
}

func TestWebSearchNumbersResultsForTheWholeRun(t *testing.T) {
	ctx, cited := agent.CollectSources(bg)
	agent.Cite(ctx, "https://earlier.example/")
	s := &searcher{results: []web.Result{{Title: "A", URL: "https://a.example/"}, {Title: "Earlier", URL: "https://earlier.example/"}}}
	tool := WebSearch(s, private, time.UTC)
	got, err := tool.Run(ctx, []byte(`{"query":"q"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "[2] A (a.example)\n[1] Earlier (earlier.example)" {
		t.Fatalf("got %q", got)
	}
	if links := cited(); len(links) != 2 || links[1] != "https://a.example/" {
		t.Fatalf("cited %v", links)
	}
}
