package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/gateway/internal/logging"
)

type checkFunc func(ctx context.Context) error

func (f checkFunc) Check(ctx context.Context) error { return f(ctx) }

var (
	healthy   = checkFunc(func(context.Context) error { return nil })
	unhealthy = checkFunc(func(context.Context) error { return errors.New("connection refused") })
)

func get(t *testing.T, h http.Handler, path string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeHealth(t *testing.T, rec *httptest.ResponseRecorder) healthBody {
	t.Helper()
	var body healthBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	return body
}

func TestHealthOK(t *testing.T) {
	h := NewHandler(Options{Service: "gateway", Version: "v1", Checks: map[string]Checker{"db": healthy}})
	rec := get(t, h, "/health", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type %q", got)
	}
	body := decodeHealth(t, rec)
	if body.Status != "ok" || body.Service != "gateway" || body.Version != "v1" || body.Checks["db"].Status != "ok" {
		t.Errorf("unexpected body %+v", body)
	}
}

func TestHealthReportsDownDependency(t *testing.T) {
	var logs bytes.Buffer
	h := NewHandler(Options{
		Logger: logging.New(&logs, "info"),
		Checks: map[string]Checker{"db": unhealthy},
	})
	rec := get(t, h, "/health", nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	body := decodeHealth(t, rec)
	if body.Status != "unhealthy" || body.Checks["db"].Status != "down" {
		t.Errorf("unexpected body %+v", body)
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Error("the response must not expose the error")
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Error("the error must be logged")
	}
}

func TestHealthTimesOutSlowChecks(t *testing.T) {
	slow := checkFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	h := NewHandler(Options{Checks: map[string]Checker{"db": slow}, CheckTimeout: 50 * time.Millisecond})
	start := time.Now()
	rec := get(t, h, "/health", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if time.Since(start) > time.Second {
		t.Fatal("health did not respect the check timeout")
	}
}

func TestHealthRejectsOtherMethods(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler(Options{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health", http.NoBody))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}

func TestRequestID(t *testing.T) {
	var logs bytes.Buffer
	h := NewHandler(Options{Logger: logging.New(&logs, "debug")})

	rec := get(t, h, "/health", http.Header{RequestIDHeader: {"abc-123"}})
	if got := rec.Header().Get(RequestIDHeader); got != "abc-123" {
		t.Errorf("kept id %q, want abc-123", got)
	}
	if !strings.Contains(logs.String(), `"request_id":"abc-123"`) {
		t.Errorf("request id missing from logs: %s", logs.String())
	}

	rec = get(t, h, "/health", http.Header{RequestIDHeader: {"bad id\nwith newline"}})
	if got := rec.Header().Get(RequestIDHeader); !validRequestID.MatchString(got) || strings.Contains(got, " ") {
		t.Errorf("invalid incoming id must be replaced, got %q", got)
	}

	rec = get(t, h, "/health", nil)
	if rec.Header().Get(RequestIDHeader) == "" {
		t.Error("a missing id must be generated")
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	var logs bytes.Buffer
	logger := logging.New(&logs, "info")
	h := withRequestID(withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}), logger), logger)

	rec := get(t, h, "/x", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "panic in handler") {
		t.Error("panic must be logged")
	}
}

func TestServeAnswersThenStopsOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	logger := logging.New(&bytes.Buffer{}, "info")
	go func() { done <- Serve(ctx, NewServer(NewHandler(Options{}), logger), ln, time.Second) }()

	url := "http://" + ln.Addr().String() + "/health"
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer checkCancel()
	if err := Check(checkCtx, url); err != nil {
		t.Fatalf("check while running: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not stop after cancel")
	}
	if err := Check(checkCtx, url); err == nil {
		t.Fatal("check passed after shutdown")
	}
}

func TestCheckFailsOnBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if err := Check(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error for 503")
	}
}
