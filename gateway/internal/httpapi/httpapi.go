// Package httpapi serves the gateway's HTTP endpoints.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/dhananjay6561/kaya/gateway/internal/logging"
)

// Checker reports whether one dependency works.
type Checker interface {
	Check(ctx context.Context) error
}

// Options configures the gateway's routes.
type Options struct {
	Logger       *slog.Logger
	Service      string
	Version      string
	Checks       map[string]Checker
	CheckTimeout time.Duration
	// Info adds state that /health reports without failing on, such as the WhatsApp connection.
	Info map[string]func() any
}

type checkResult struct {
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
}

type healthBody struct {
	Status  string                 `json:"status"`
	Service string                 `json:"service"`
	Version string                 `json:"version"`
	Checks  map[string]checkResult `json:"checks"`
	Info    map[string]any         `json:"info,omitempty"`
}

// NewHandler returns the gateway's routes, wrapped with request ids, logging and panic recovery.
func NewHandler(o Options) http.Handler {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.CheckTimeout == 0 {
		o.CheckTimeout = 2 * time.Second
	}
	mux := http.NewServeMux()
	mux.Handle("GET /health", health(o))
	return withRequestID(withLogging(withRecover(mux, o.Logger), o.Logger), o.Logger)
}

func health(o Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), o.CheckTimeout)
		defer cancel()

		body := healthBody{Status: "ok", Service: o.Service, Version: o.Version, Checks: map[string]checkResult{}}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for name, checker := range o.Checks {
			wg.Go(func() {
				start := time.Now()
				err := checker.Check(ctx)
				result := checkResult{Status: "ok", DurationMS: time.Since(start).Milliseconds()}
				if err != nil {
					result.Status = "down"
					loggerFrom(r.Context(), o.Logger).Warn("health check failed", "check", name, logging.Err(err))
				}
				mu.Lock()
				defer mu.Unlock()
				body.Checks[name] = result
				if err != nil {
					body.Status = "unhealthy"
				}
			})
		}
		wg.Wait()
		for name, info := range o.Info {
			if body.Info == nil {
				body.Info = map[string]any{}
			}
			body.Info[name] = info()
		}

		status := http.StatusOK
		if body.Status != "ok" {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, body)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// NewServer returns a server for h with timeouts set.
func NewServer(h http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// Serve runs srv on ln until ctx is done, then shuts it down, waiting up to grace for open requests.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, grace time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down http server: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Check returns nil when url answers 200.
func Check(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", url, resp.StatusCode)
	}
	return nil
}
