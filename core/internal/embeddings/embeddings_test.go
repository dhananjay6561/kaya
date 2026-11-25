package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/core/internal/llm"
)

var bg = context.Background()

type fake struct {
	mu      sync.Mutex
	bodies  []map[string]any
	auth    []string
	replies []http.HandlerFunc
	server  *httptest.Server
}

func newFake(t *testing.T, replies ...http.HandlerFunc) *fake {
	t.Helper()
	f := &fake{replies: replies}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		var next http.HandlerFunc
		if len(f.replies) > 0 {
			next, f.replies = f.replies[0], f.replies[1:]
		}
		f.mu.Unlock()
		if next == nil {
			http.Error(w, "no more replies", http.StatusTeapot)
			return
		}
		next(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fake) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fake) voyage(o VoyageOptions) *Voyage {
	o.BaseURL = f.server.URL + "/v1/"
	if o.RetryBase == 0 {
		o.RetryBase = time.Millisecond
	}
	return NewVoyage(config.Voyage{APIKey: "test-key-not-real", Model: "voyage-4"}, o)
}

func vector(seed float32) []float32 {
	v := make([]float32, Dimensions)
	for i := range v {
		v[i] = seed
	}
	return v
}

// answer replies with one vector per input, numbered by position, listed in reverse order.
func answer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input []string `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	type item struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	var data []item
	for i := len(req.Input) - 1; i >= 0; i-- {
		data = append(data, item{Index: i, Embedding: vector(float32(i + 1))})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]int{"total_tokens": 42}})
}

func status(code int, headers ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"detail":"the text you sent: secret"}`)
	}
}

func raw(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }
}

func embedError(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want an *embeddings.Error", err)
	}
	return e
}

func TestVoyageSendsTheTextsAndPutsTheVectorsBackInOrder(t *testing.T) {
	f := newFake(t, answer, answer)
	v := f.voyage(VoyageOptions{})
	got, err := v.Embed(bg, []string{"kal milte hain", "see you tomorrow"}, Document)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tokens != 42 || got.Vectors[0][0] != 1 || got.Vectors[1][0] != 2 || len(got.Vectors[0]) != Dimensions {
		t.Fatalf("got %d tokens, first values %v %v", got.Tokens, got.Vectors[0][0], got.Vectors[1][0])
	}
	want := map[string]any{"input": []any{"kal milte hain", "see you tomorrow"}, "model": "voyage-4", "input_type": "document", "output_dimension": float64(1024)}
	if !reflect.DeepEqual(f.bodies[0], want) || f.auth[0] != "Bearer test-key-not-real" {
		t.Fatalf("body %v, auth %q", f.bodies[0], f.auth[0])
	}
	if _, err := v.Embed(bg, []string{"q"}, Query); err != nil || f.bodies[1]["input_type"] != "query" {
		t.Fatalf("query: %v, %v", err, f.bodies[1])
	}
}

func TestVoyageDoesNotCallOutForNothingOrTooMuch(t *testing.T) {
	f := newFake(t)
	v := f.voyage(VoyageOptions{})
	if got, err := v.Embed(bg, nil, Document); err != nil || len(got.Vectors) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
	if _, err := v.Embed(bg, make([]string, 129), Document); err == nil {
		t.Fatal("129 texts were sent")
	}
	if f.calls() != 0 {
		t.Fatalf("%d calls", f.calls())
	}
}

func TestVoyageRetriesWhenDownOrSlowButHandsARateLimitBack(t *testing.T) {
	f := newFake(t, status(503), status(408), answer)
	if _, err := f.voyage(VoyageOptions{}).Embed(bg, []string{"x"}, Document); err != nil || f.calls() != 3 {
		t.Fatalf("err %v after %d calls", err, f.calls())
	}
	f = newFake(t, status(429, "Retry-After", "7"), answer)
	_, err := f.voyage(VoyageOptions{}).Embed(bg, []string{"x"}, Document)
	if e := embedError(t, err); e.Code != RateLimited || e.RetryAfter == nil || *e.RetryAfter != 7*time.Second || f.calls() != 1 {
		t.Fatalf("err %+v after %d calls", e, f.calls())
	}
	f = newFake(t, status(429))
	_, err = f.voyage(VoyageOptions{}).Embed(bg, []string{"x"}, Document)
	if e := embedError(t, err); e.RetryAfter != nil {
		t.Fatalf("retry after %v without a header", *e.RetryAfter)
	}
}

func TestVoyageGivesUpAfterItsRetriesAndNamesTheProblemWithoutTheText(t *testing.T) {
	f := newFake(t, status(500), status(500), status(500))
	_, err := f.voyage(VoyageOptions{}).Embed(bg, []string{"secret"}, Document)
	e := embedError(t, err)
	if e.Code != Unavailable || f.calls() != 3 || strings.Contains(e.Error(), "secret") {
		t.Fatalf("err %v after %d calls", e, f.calls())
	}
	for code, want := range map[int]string{401: Auth, 403: Auth, 404: Misconfigured, 400: Rejected, 413: Rejected, 422: Rejected} {
		f := newFake(t, status(code))
		_, err := f.voyage(VoyageOptions{}).Embed(bg, []string{"x"}, Document)
		if e := embedError(t, err); e.Code != want || e.Status != code || f.calls() != 1 {
			t.Fatalf("%d: %+v after %d calls", code, e, f.calls())
		}
	}
}

func TestVoyageCallsAWrongAnswerWhatItIs(t *testing.T) {
	two := func(v1, v2 string) string {
		return `{"data":[{"index":0,"embedding":` + v1 + `},{"index":1,"embedding":` + v2 + `}]}`
	}
	good, _ := json.Marshal(vector(1))
	short, _ := json.Marshal(vector(1)[:3])
	for _, body := range []string{
		"<html>",
		`{"data":[]}`,
		two(string(good), string(short)),
		`{"data":[{"index":0,"embedding":` + string(good) + `},{"index":0,"embedding":` + string(good) + `}]}`,
		`{"data":[{"index":0,"embedding":` + string(good) + `},{"index":2,"embedding":` + string(good) + `}]}`,
		`{"data":[{"embedding":` + string(good) + `},{"index":1,"embedding":` + string(good) + `}]}`,
		`{"data":[null,{"index":1,"embedding":` + string(good) + `}]}`,
	} {
		f := newFake(t, raw(body))
		_, err := f.voyage(VoyageOptions{}).Embed(bg, []string{"a", "b"}, Document)
		if e := embedError(t, err); e.Code != BadResponse {
			t.Fatalf("%.60s: %+v", body, e)
		}
	}
}

func TestVoyageTimesOutAndStopsForTheCaller(t *testing.T) {
	hang := func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	f := newFake(t, hang)
	_, err := f.voyage(VoyageOptions{Timeout: 20 * time.Millisecond, MaxRetries: -1}).Embed(bg, []string{"x"}, Document)
	if e := embedError(t, err); e.Code != Timeout {
		t.Fatalf("err %+v", e)
	}
	stopping := errors.New("core is stopping")
	ctx, cancel := context.WithCancelCause(bg)
	time.AfterFunc(20*time.Millisecond, func() { cancel(stopping) })
	f = newFake(t, hang)
	if _, err := f.voyage(VoyageOptions{}).Embed(ctx, []string{"x"}, Document); !errors.Is(err, stopping) {
		t.Fatalf("err %v", err)
	}
}

func TestStubPointsTextsThatShareWordsTheSameWay(t *testing.T) {
	s := NewStub()
	got, err := s.Embed(bg, []string{"dinner on Saturday", "Saturday dinner plans", "invoice due", ""}, Document)
	if err != nil {
		t.Fatal(err)
	}
	cos := func(a, b []float32) float32 {
		var dot float32
		for i := range a {
			dot += a[i] * b[i]
		}
		return dot
	}
	if cos(got.Vectors[0], got.Vectors[1]) <= cos(got.Vectors[0], got.Vectors[2]) || len(got.Vectors[3]) != Dimensions {
		t.Fatalf("similarities %v %v", cos(got.Vectors[0], got.Vectors[1]), cos(got.Vectors[0], got.Vectors[2]))
	}
	if s.Locality() != llm.Local || s.Model() == "" {
		t.Fatal("stub must be local and name its model")
	}
}

func TestNewBuildsTheProviderConfigNames(t *testing.T) {
	for name, want := range map[string]string{"voyage": "voyage", "stub": "stub"} {
		p, err := New(config.Embeddings{Provider: name, Voyage: config.Voyage{APIKey: "k", Model: "voyage-4"}})
		if err != nil || p.Name() != want {
			t.Fatalf("%s: %v, %v", name, p, err)
		}
	}
	if _, err := New(config.Embeddings{Provider: "other"}); err == nil {
		t.Fatal("an unknown provider was built")
	}
	if v, _ := New(config.Embeddings{Provider: "voyage", Voyage: config.Voyage{APIKey: "k", Model: "voyage-4"}}); v.Locality() != llm.Cloud {
		t.Fatal(fmt.Sprint("voyage must be a cloud provider, got ", v.Locality()))
	}
}
