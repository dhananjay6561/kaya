package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/config"
)

var bg = context.Background()

type call struct {
	path, auth string
	body       map[string]any
}

// fakeGroq answers each call with the next reply in turn.
type fakeGroq struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []call
	replies []http.HandlerFunc
	server  *httptest.Server
}

func newFake(t *testing.T, replies ...http.HandlerFunc) *fakeGroq {
	t.Helper()
	f := &fakeGroq{t: t, replies: replies}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.calls = append(f.calls, call{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
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

func (f *fakeGroq) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeGroq) call(i int) call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

var testGroq = config.Groq{
	APIKey:        "test-key-not-real",
	HeavyModel:    "openai/gpt-oss-120b",
	RoutineModel:  "openai/gpt-oss-20b",
	ContextTokens: 4000,
}

func (f *fakeGroq) llm(o GroqOptions) *Groq {
	f.t.Helper()
	if o.BaseURL == "" {
		o.BaseURL = f.server.URL + "/openai/v1"
	}
	if o.RetryBase == 0 {
		o.RetryBase = time.Millisecond
	}
	g, err := NewGroq(testGroq, o)
	if err != nil {
		f.t.Fatal(err)
	}
	return g
}

func ok(content any, extra ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		finish := `"stop"`
		if len(extra) > 0 {
			finish = extra[0]
		}
		c, _ := json.Marshal(content)
		_, _ = io.WriteString(w, `{"model":"openai/gpt-oss-20b","choices":[{"message":{"role":"assistant","content":`+
			string(c)+`},"finish_reason":`+finish+`}],"usage":{"prompt_tokens":83,"completion_tokens":19}}`)
	}
}

func raw(status int, body string, headers ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func failure(status int, code string, headers ...string) http.HandlerFunc {
	c := "null"
	if code != "" {
		b, _ := json.Marshal(code)
		c = string(b)
	}
	return raw(status, `{"error":{"message":"secret prompt text echoed back","code":`+c+`}}`, headers...)
}

func hang(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

var ask = Request{Tier: Routine, Messages: []Message{{Role: User, Content: "hi"}}}

func llmError(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want an *llm.Error", err)
	}
	return e
}

func TestGroqAsksTheTierAndReturnsTextModelReasonAndUsage(t *testing.T) {
	f := newFake(t, ok("Hello there!"))
	got, err := f.llm(GroqOptions{BaseURL: f.server.URL + "/openai/v1/"}).Complete(bg, ask)
	if err != nil {
		t.Fatal(err)
	}
	want := Completion{Text: "Hello there!", Model: "openai/gpt-oss-20b", FinishReason: FinishStop, Usage: Usage{83, 19}}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	c := f.call(0)
	if c.path != "/openai/v1/chat/completions" || c.auth != "Bearer test-key-not-real" {
		t.Fatalf("call = %s %q", c.path, c.auth)
	}
	wantBody := map[string]any{
		"model":                 "openai/gpt-oss-20b",
		"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
		"max_completion_tokens": float64(1024),
		"reasoning_effort":      "low",
		"include_reasoning":     false,
	}
	if !reflect.DeepEqual(c.body, wantBody) {
		t.Fatalf("body = %v", c.body)
	}
}

func TestGroqUsesTheHeavyModelWithMoreRoomAndEffortAndTheCallersLimits(t *testing.T) {
	f := newFake(t, ok("x"), ok("y"))
	g := f.llm(GroqOptions{})
	temp := 0.2
	if _, err := g.Complete(bg, Request{Tier: Heavy, Messages: ask.Messages, MaxTokens: 50, Temperature: &temp}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Complete(bg, Request{Tier: Heavy, Effort: Low, Messages: ask.Messages}); err != nil {
		t.Fatal(err)
	}
	first, second := f.call(0).body, f.call(1).body
	if first["model"] != "openai/gpt-oss-120b" || first["max_completion_tokens"] != float64(50) ||
		first["temperature"] != 0.2 || first["reasoning_effort"] != "medium" {
		t.Fatalf("first body = %v", first)
	}
	if second["reasoning_effort"] != "low" || second["max_completion_tokens"] != float64(8192) {
		t.Fatalf("second body = %v", second)
	}
	if _, sent := second["temperature"]; sent {
		t.Fatal("temperature sent when not asked for")
	}
}

func TestGroqReportsACutOffAnswerAndAnEmptyOneAsTheyAre(t *testing.T) {
	f := newFake(t, ok("partial", `"length"`), ok(nil), ok("x", `"weird"`), ok("x", "null"))
	g := f.llm(GroqOptions{})
	for _, want := range []Completion{{Text: "partial", FinishReason: FinishLength}, {Text: "", FinishReason: FinishStop},
		{Text: "x", FinishReason: FinishStop}, {Text: "x", FinishReason: FinishStop}} {
		got, err := g.Complete(bg, ask)
		if err != nil {
			t.Fatal(err)
		}
		if got.Text != want.Text || got.FinishReason != want.FinishReason {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
}

func TestGroqRetriesWhenBusyOrDownThenSucceeds(t *testing.T) {
	f := newFake(t, failure(429, "rate_limit_exceeded", "Retry-After", "0"), failure(503, ""), ok("finally"))
	got, err := f.llm(GroqOptions{}).Complete(bg, ask)
	if err != nil || got.Text != "finally" || f.count() != 3 {
		t.Fatalf("got %q, %v after %d calls", got.Text, err, f.count())
	}
}

func TestGroqGivesUpAfterTheRetriesAndSaysWhyWithoutThePromptOrGroqsMessage(t *testing.T) {
	f := newFake(t, failure(500, ""), failure(502, ""), failure(503, ""))
	_, err := f.llm(GroqOptions{}).Complete(bg, ask)
	e := llmError(t, err)
	if e.Code != Unavailable || e.Status != 503 || f.count() != 3 {
		t.Fatalf("err = %+v after %d calls", e, f.count())
	}
	if strings.Contains(e.Error(), "secret") || strings.Contains(e.Error(), "hi") {
		t.Fatalf("error leaks: %s", e)
	}
	if e.Error() != "groq could not answer: unavailable (HTTP 503)" {
		t.Fatalf("message = %q", e)
	}
}

func TestGroqDoesNotWaitOutALongRateLimit(t *testing.T) {
	for _, after := range []string{"60", "11", " 10.5 "} {
		f := newFake(t, failure(429, "rate_limit_exceeded", "Retry-After", after))
		_, err := f.llm(GroqOptions{}).Complete(bg, ask)
		e := llmError(t, err)
		if e.Code != RateLimited || e.Detail != "rate_limit_exceeded" || e.RetryAfter == nil || f.count() != 1 {
			t.Fatalf("%q: err = %+v after %d calls", after, e, f.count())
		}
	}
	f := newFake(t, failure(429, "", "Retry-After", "60"))
	_, err := f.llm(GroqOptions{}).Complete(bg, ask)
	if e := llmError(t, err); *e.RetryAfter != time.Minute {
		t.Fatalf("retry after = %v", *e.RetryAfter)
	}
}

func TestGroqDoesNotRetryARefusedKeyOrARefusedRequest(t *testing.T) {
	for status, code := range map[int]string{401: Auth, 403: Auth, 400: Rejected, 413: TooLong, 404: Misconfigured, 422: Rejected} {
		f := newFake(t, failure(status, "invalid_api_key"))
		_, err := f.llm(GroqOptions{}).Complete(bg, ask)
		if e := llmError(t, err); e.Code != code || e.Status != status || f.count() != 1 {
			t.Fatalf("%d: err = %+v after %d calls", status, e, f.count())
		}
	}
}

func TestGroqNamesATooLongMessageAndAnUnknownModelWhateverTheStatus(t *testing.T) {
	for detail, code := range map[string]string{
		"context_length_exceeded": TooLong,
		"request_too_large":       TooLong,
		"model_decommissioned":    Misconfigured,
		"model_not_found":         Misconfigured,
	} {
		f := newFake(t, failure(400, detail))
		_, err := f.llm(GroqOptions{}).Complete(bg, ask)
		if e := llmError(t, err); e.Code != code {
			t.Fatalf("%s: code = %s", detail, e.Code)
		}
	}
}

func TestGroqReadsARetryAfterDate(t *testing.T) {
	at := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	f := newFake(t, failure(429, "", "Retry-After", at))
	_, err := f.llm(GroqOptions{}).Complete(bg, ask)
	e := llmError(t, err)
	if e.RetryAfter == nil || *e.RetryAfter <= 20*time.Second || *e.RetryAfter > 30*time.Second {
		t.Fatalf("retry after = %v", e.RetryAfter)
	}
	if f.count() != 1 {
		t.Fatal("waited for a date too far off")
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	f = newFake(t, failure(429, "", "Retry-After", past), ok("soon"))
	if got, err := f.llm(GroqOptions{}).Complete(bg, ask); err != nil || got.Text != "soon" {
		t.Fatalf("a date in the past: %q, %v", got.Text, err)
	}
	f = newFake(t, failure(429, "", "Retry-After", "soon-ish"), failure(429, ""))
	_, err = f.llm(GroqOptions{MaxRetries: -1}).Complete(bg, ask)
	if e := llmError(t, err); e.RetryAfter != nil {
		t.Fatalf("an unreadable header gave %v", *e.RetryAfter)
	}
}

func TestGroqDoublesItsWaitAndGivesUpRatherThanWaitTooLong(t *testing.T) {
	f := newFake(t, failure(503, ""), failure(503, ""), ok("x"))
	_, err := f.llm(GroqOptions{RetryBase: 6 * time.Millisecond, MaxRetryWait: 10 * time.Millisecond}).Complete(bg, ask)
	if e := llmError(t, err); e.Code != Unavailable || f.count() != 2 {
		t.Fatalf("err = %+v after %d calls; the second wait (12 ms) is over the 10 ms limit", e, f.count())
	}
}

func TestGroqStopsRetryingAtTheOverallDeadline(t *testing.T) {
	f := newFake(t, hang, hang, hang, hang, hang, hang)
	started := time.Now()
	_, err := f.llm(GroqOptions{Timeout: time.Second, Deadline: 30 * time.Millisecond, MaxRetries: 5}).Complete(bg, ask)
	if e := llmError(t, err); e.Code != Timeout {
		t.Fatalf("code = %s", e.Code)
	}
	if took := time.Since(started); took > 500*time.Millisecond || f.count() > 2 {
		t.Fatalf("took %v and %d calls: the deadline, not the per-call timeout, should end it", took, f.count())
	}
}

func TestGroqStopsWaitingBetweenTriesWhenTheCallerGivesUp(t *testing.T) {
	f := newFake(t, failure(503, ""), ok("never"))
	stopping := errors.New("core is stopping")
	ctx, cancel := context.WithCancelCause(bg)
	time.AfterFunc(20*time.Millisecond, func() { cancel(stopping) })
	_, err := f.llm(GroqOptions{RetryBase: 5 * time.Second}).Complete(ctx, ask)
	if !errors.Is(err, stopping) || f.count() != 1 {
		t.Fatalf("err = %v after %d calls", err, f.count())
	}
}

func TestGroqCallsATimeoutWhileReadingTheAnswerATimeout(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"model":`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	_, err := f.llm(GroqOptions{Timeout: 50 * time.Millisecond, MaxRetries: -1}).Complete(bg, ask)
	if e := llmError(t, err); e.Code != Timeout {
		t.Fatalf("code = %s", e.Code)
	}
}

func TestGroqKeepsANetworkFailuresCode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	g, err := NewGroq(testGroq, GroqOptions{BaseURL: "http://" + addr, MaxRetries: -1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Complete(bg, ask)
	if e := llmError(t, err); e.Code != Unavailable || e.Detail != "econnrefused" {
		t.Fatalf("err = %+v", e)
	}

	reset := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	})
	_, err = reset.llm(GroqOptions{MaxRetries: -1}).Complete(bg, ask)
	if e := llmError(t, err); e.Code != Unavailable {
		t.Fatalf("err = %+v", e)
	}
}

func TestGroqKeepsOnlySafeErrorCodes(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"Bad Thing: <script>"}}`,
		`{"error":{"code":42}}`,
		`{"error":"just text"}`,
		`<html>oops</html>`,
		`{"error":{"code":"` + strings.Repeat("a", 62) + `"}}`,
	} {
		f := newFake(t, raw(400, body))
		_, err := f.llm(GroqOptions{}).Complete(bg, ask)
		if e := llmError(t, err); e.Detail != "" || e.Code != Rejected {
			t.Fatalf("%s: err = %+v", body, e)
		}
	}
}

func TestGroqCallsAMalformedAnswerWhatItIs(t *testing.T) {
	usage := `"usage":{"prompt_tokens":1,"completion_tokens":1}`
	for _, body := range []string{
		`<html>oops</html>`,
		`null`,
		`[]`,
		`{"model":"m","choices":[{"message":{"content":"x"}}]}`,
		`{"model":"m","choices":[],` + usage + `}`,
		`{"model":"m",` + usage + `}`,
		`{"model":"m","choices":[{"finish_reason":"stop"}],` + usage + `}`,
		`{"model":"m","choices":[{"message":"x"}],` + usage + `}`,
		`{"model":"m","choices":[null],` + usage + `}`,
		`{"model":"m","choices":[{"message":{"content":"x"}},{"message":null}],` + usage + `}`,
		`{"model":7,"choices":[{"message":{"content":"x"}}],` + usage + `}`,
		`{"choices":[{"message":{"content":"x"}}],` + usage + `}`,
		`{"model":"m","choices":[{"message":{"content":5}}],` + usage + `}`,
		`{"model":"m","choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":1}}`,
		`{"model":"m","choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":"1","completion_tokens":1}}`,
	} {
		f := newFake(t, raw(200, body))
		_, err := f.llm(GroqOptions{}).Complete(bg, ask)
		if e := llmError(t, err); e.Code != BadResponse || e.Status != 200 {
			t.Fatalf("%s: err = %+v", body, e)
		}
	}
}

func TestGroqAcceptsWhatTheTypeScriptSchemaAccepted(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","choices":[{"message":{}}],"usage":{"prompt_tokens":83.0,"completion_tokens":1}}`,
		`{"model":"m","choices":[{"message":{"content":null},"finish_reason":null}],"usage":{"prompt_tokens":1,"completion_tokens":1},"x":1}`,
	} {
		f := newFake(t, raw(200, body))
		got, err := f.llm(GroqOptions{}).Complete(bg, ask)
		if err != nil || got.Text != "" || got.FinishReason != FinishStop {
			t.Fatalf("%s: %+v, %v", body, got, err)
		}
	}
}

func TestGroqTimesOutACallThatHangsAndRetriesIt(t *testing.T) {
	f := newFake(t, hang, hang)
	_, err := f.llm(GroqOptions{Timeout: 20 * time.Millisecond, MaxRetries: 1}).Complete(bg, ask)
	if e := llmError(t, err); e.Code != Timeout || f.count() != 2 {
		t.Fatalf("err = %+v after %d calls", e, f.count())
	}
}

func TestGroqStopsAtOnceWhenTheCallerGivesUpWithTheCallersReason(t *testing.T) {
	f := newFake(t, hang)
	stopping := errors.New("core is stopping")
	ctx, cancel := context.WithCancelCause(bg)
	time.AfterFunc(10*time.Millisecond, func() { cancel(stopping) })
	if _, err := f.llm(GroqOptions{}).Complete(ctx, ask); !errors.Is(err, stopping) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.llm(GroqOptions{}).Complete(ctx, ask); !errors.Is(err, stopping) {
		t.Fatalf("an already stopped call: err = %v", err)
	}
}

func TestGroqCountsTokensTheWayGptOssDoesWithTheChatFraming(t *testing.T) {
	g := newFake(t).llm(GroqOptions{})
	if n := g.CountTokens("hello how are you"); n != 4+4 {
		t.Fatalf("count = %d", n)
	}
	if n := g.CountTokens("order id 4839201, txn 99381"); n <= 10 {
		t.Fatalf("numbers cost more than words, got %d", n)
	}
	for text, want := range map[string]int{"<|endoftext|>": 7, "say <|start|> now": 7, "<|reserved_200050|>": 8} {
		if n := g.CountTokens(text); n != want+4 {
			t.Fatalf("%s: count = %d", text, n)
		}
	}
}

func TestGroqDoesNotCallOutForAnEmptyConversation(t *testing.T) {
	f := newFake(t)
	if _, err := f.llm(GroqOptions{}).Complete(bg, Request{Tier: Routine}); err == nil || f.count() != 0 {
		t.Fatalf("err = %v after %d calls", err, f.count())
	}
}

func TestGroqRefusesModelsItCannotCountFor(t *testing.T) {
	for _, models := range [][2]string{{"llama-3.3-70b-versatile", "openai/gpt-oss-20b"}, {"openai/gpt-oss-120b", "qwen/qwen3-32b"}} {
		cfg := testGroq
		cfg.HeavyModel, cfg.RoutineModel = models[0], models[1]
		if _, err := NewGroq(cfg, GroqOptions{}); err == nil || !strings.Contains(err.Error(), "gpt-oss") {
			t.Fatalf("%v: err = %v", models, err)
		}
	}
}

func TestGroqBuildsTheSameRequestAsTheTypeScriptCore(t *testing.T) {
	file, err := os.ReadFile("testdata/request.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Provider []struct {
			Request struct {
				Tier        Tier      `json:"tier"`
				Effort      Effort    `json:"effort"`
				MaxTokens   int       `json:"maxTokens"`
				Temperature *float64  `json:"temperature"`
				Messages    []Message `json:"messages"`
			} `json:"request"`
			Body json.RawMessage `json:"body"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(file, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Provider) == 0 {
		t.Fatal("no cases")
	}
	g := newFake(t).llm(GroqOptions{})
	for _, c := range golden.Provider {
		r := c.Request
		body, err := g.body(Request{Tier: r.Tier, Effort: r.Effort, MaxTokens: r.MaxTokens, Temperature: r.Temperature, Messages: r.Messages})
		if err != nil {
			t.Fatal(err)
		}
		if !sameJSON(t, body, c.Body) {
			t.Errorf("body = %s\nwant   %s", body, c.Body)
		}
	}
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}
