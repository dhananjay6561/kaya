// Package logging builds a Kaya service's structured JSON logger.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Levels beyond slog's four, matching the names used in LOG_LEVEL.
const (
	LevelTrace = slog.Level(-8)
	LevelFatal = slog.Level(12)
)

var levels = map[string]slog.Level{
	"trace": LevelTrace,
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
	"fatal": LevelFatal,
}

// ParseLevel maps a LOG_LEVEL value to a slog level. Unknown values fall back to info.
func ParseLevel(name string) slog.Level {
	if level, ok := levels[strings.ToLower(name)]; ok {
		return level
	}
	return slog.LevelInfo
}

// New returns a JSON logger writing to w at the given level, with attrs on every line.
func New(w io.Writer, level string, attrs ...any) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       ParseLevel(level),
		ReplaceAttr: renameLevel,
	})
	return slog.New(handler).With(attrs...)
}

func renameLevel(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 || a.Key != slog.LevelKey {
		return a
	}
	level, _ := a.Value.Any().(slog.Level)
	for name, l := range levels {
		if l == level {
			return slog.String(slog.LevelKey, name)
		}
	}
	return slog.String(slog.LevelKey, strings.ToLower(level.String()))
}

type ctxKey struct{}

// WithLogger returns a copy of ctx carrying logger.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, logger)
}

// FromContext returns the logger in ctx, or fallback when there is none.
func FromContext(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if logger, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return logger
	}
	return fallback
}

// Err logs err under the same "err" key and fields core uses: type and message.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.Group("err", slog.String("type", fmt.Sprintf("%T", err)), slog.String("message", err.Error()))
}
