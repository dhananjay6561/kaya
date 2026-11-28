// Package web reaches the public web for Kaya: searching through the self-hosted SearXNG.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// Result is one search result.
type Result struct {
	Title     string
	URL       string
	Snippet   string
	Published time.Time
}

// Ranges a search can be limited to, by when pages were published.
var Ranges = []string{"day", "week", "month", "year"}

// ErrUnavailable is SearXNG not answering, or answering with an error.
var ErrUnavailable = errors.New("web search is unavailable")

// ErrNoEngines is a search every engine behind SearXNG failed to answer.
var ErrNoEngines = errors.New("no search engine answered")

// SearchOptions configure a Searcher. Zero values take the defaults.
type SearchOptions struct {
	BaseURL string
	Timeout time.Duration
	Client  *http.Client
}

const (
	maxAnswer  = 4 << 20
	maxSnippet = 300
	maxTitle   = 150
)

// Searcher asks SearXNG.
type Searcher struct{ o SearchOptions }

// NewSearcher returns a Searcher for the SearXNG at o.BaseURL.
func NewSearcher(o SearchOptions) *Searcher {
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.Timeout == 0 {
		o.Timeout = 8 * time.Second
	}
	if o.Client == nil {
		o.Client = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	}
	return &Searcher{o: o}
}

type answer struct {
	Results []struct {
		Title         string `json:"title"`
		URL           string `json:"url"`
		Content       string `json:"content"`
		PublishedDate string `json:"publishedDate"`
	} `json:"results"`
	Unresponsive [][]string `json:"unresponsive_engines"`
}

// Search returns up to limit results for query, the best first, each link once. within limits
// them to pages from the last day, week, month or year; "" means any time.
func (s *Searcher) Search(ctx context.Context, query, within string, limit int) ([]Result, error) {
	q := url.Values{"q": {query}, "format": {"json"}, "safesearch": {"0"}}
	if within != "" {
		q.Set("time_range", within)
	}
	ctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.o.BaseURL+"/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.o.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil && context.Cause(ctx) != context.DeadlineExceeded {
			return nil, context.Cause(ctx)
		}
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	var a answer
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&a); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	out := make([]Result, 0, limit)
	seen := map[string]bool{}
	for _, r := range a.Results {
		if len(out) == limit {
			break
		}
		link, ok := webLink(r.URL)
		if !ok || seen[link] {
			continue
		}
		seen[link] = true
		res := Result{Title: clip(tidy(r.Title), maxTitle), URL: link, Snippet: clip(tidy(r.Content), maxSnippet)}
		if res.Title == "" {
			res.Title = link
		}
		if t, err := time.Parse(time.RFC3339, r.PublishedDate); err == nil {
			res.Published = t
		} else if t, err := time.Parse("2006-01-02T15:04:05", r.PublishedDate); err == nil {
			res.Published = t
		}
		out = append(out, res)
	}
	if len(out) == 0 && len(a.Unresponsive) > 0 {
		return nil, ErrNoEngines
	}
	return out, nil
}

// webLink is raw when it is an http or https link with a host.
func webLink(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", false
	}
	return u.String(), true
}

// tidy puts text on one line with single spaces and no control characters.
func tidy(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}
