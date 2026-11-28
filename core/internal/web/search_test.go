package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serve(t *testing.T, h http.HandlerFunc) *Searcher {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewSearcher(SearchOptions{BaseURL: srv.URL + "/"})
}

func TestSearchAsksForJSONAndReadsTheResults(t *testing.T) {
	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/search" || q.Get("q") != "iphone 17 price" || q.Get("format") != "json" || q.Get("time_range") != "month" {
			t.Errorf("asked %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"results": [
			{"title": "  iPhone 17\n price ", "url": "https://shop.example/iphone-17", "content": "Starts at\tRs 82,900", "publishedDate": "2026-11-26T10:00:00"},
			{"title": "Same page again", "url": "https://shop.example/iphone-17", "content": "dup"},
			{"title": "A local file", "url": "file:///etc/passwd", "content": "no"},
			{"title": "With a login", "url": "https://someone@shop.example/", "content": "no"},
			{"title": "", "url": "http://news.example/a", "content": ""},
			{"title": "Third", "url": "https://third.example/", "content": "c"}
		], "unresponsive_engines": [["bing", "timeout"]]}`))
	})
	got, err := s.Search(context.Background(), "iphone 17 price", "month", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results: %+v", len(got), got)
	}
	want := Result{Title: "iPhone 17 price", URL: "https://shop.example/iphone-17", Snippet: "Starts at Rs 82,900", Published: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)}
	if got[0] != want {
		t.Fatalf("got %+v\nwant %+v", got[0], want)
	}
	if got[1].Title != "http://news.example/a" || got[1].Snippet != "" {
		t.Fatalf("an untitled result is named by its link: %+v", got[1])
	}
}

func TestSearchClipsLongText(t *testing.T) {
	long := strings.Repeat("word ", 200)
	s := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results": [{"title": "` + long + `", "url": "https://a.example/", "content": "` + long + `"}]}`))
	})
	got, err := s.Search(context.Background(), "q", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got[0].Snippet)); n > maxSnippet || !strings.HasSuffix(got[0].Snippet, "…") {
		t.Fatalf("snippet of %d runes: %q", n, got[0].Snippet)
	}
	if n := len([]rune(got[0].Title)); n > maxTitle {
		t.Fatalf("title of %d runes", n)
	}
}

func TestSearchSaysWhenNoEngineAnswered(t *testing.T) {
	s := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results": [], "unresponsive_engines": [["google", "access denied"]]}`))
	})
	if _, err := s.Search(context.Background(), "q", "", 5); !errors.Is(err, ErrNoEngines) {
		t.Fatalf("got %v", err)
	}
	empty := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"results": []}`)) })
	got, err := empty.Search(context.Background(), "q", "", 5)
	if err != nil || len(got) != 0 {
		t.Fatalf("nothing found is no error: %v %v", got, err)
	}
}

func TestSearchFailsAsUnavailable(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"error status": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
		"not json":     func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
	}
	for name, h := range cases {
		if _, err := serve(t, h).Search(context.Background(), "q", "", 5); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	down := NewSearcher(SearchOptions{BaseURL: "http://127.0.0.1:1"})
	if _, err := down.Search(context.Background(), "q", "", 5); !errors.Is(err, ErrUnavailable) {
		t.Errorf("down: got %v", err)
	}
	slow := serve(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	slow.o.Timeout = 50 * time.Millisecond
	if _, err := slow.Search(context.Background(), "q", "", 5); !errors.Is(err, ErrUnavailable) {
		t.Errorf("slow: got %v", err)
	}
}

func TestSearchStopsWhenTheCallerGivesUp(t *testing.T) {
	s := serve(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Search(ctx, "q", "", 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
