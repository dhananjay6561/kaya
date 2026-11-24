package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/dhananjay6561/kaya/kit/logging"
)

// RequestIDHeader carries the id that ties one request's logs together across services.
const RequestIDHeader = "X-Request-Id"

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func loggerFrom(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	return logging.FromContext(ctx, fallback)
}

func withRequestID(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := logging.WithLogger(r.Context(), logger.With("request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

func withLogging(next http.Handler, fallback *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		aborted := true
		defer func() {
			status := rec.status
			if aborted {
				status = http.StatusInternalServerError
			}
			level := slog.LevelInfo
			switch {
			case r.URL.Path == "/health" && !aborted:
				level = slog.LevelDebug
			case status >= 500:
				level = slog.LevelError
			}
			loggerFrom(r.Context(), fallback).Log(r.Context(), level, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"aborted", aborted,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}()
		next.ServeHTTP(rec, r)
		aborted = false
	})
}

func withRecover(next http.Handler, fallback *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec, ok := w.(*statusRecorder)
		if !ok {
			rec = &statusRecorder{ResponseWriter: w}
		}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			loggerFrom(r.Context(), fallback).Error("panic in handler",
				"panic", fmt.Sprint(v),
				"path", r.URL.Path,
				"stack", string(debug.Stack()),
			)
			if rec.wrote {
				panic(http.ErrAbortHandler)
			}
			writeJSON(rec, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}()
		next.ServeHTTP(rec, r)
	})
}
