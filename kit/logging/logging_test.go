package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
)

func decode(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	dec := json.NewDecoder(buf)
	for dec.More() {
		var line map[string]any
		if err := dec.Decode(&line); err != nil {
			t.Fatalf("log line is not JSON: %v", err)
		}
		lines = append(lines, line)
	}
	return lines
}

func TestParseLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"trace": LevelTrace, "debug": slog.LevelDebug, "info": slog.LevelInfo,
		"warn": slog.LevelWarn, "error": slog.LevelError, "fatal": LevelFatal,
		"WARN": slog.LevelWarn, "": slog.LevelInfo, "loud": slog.LevelInfo,
	}
	for name, want := range tests {
		if got := ParseLevel(name); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestNewWritesJSONWithLowercaseLevels(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, "trace", "service", "gateway")
	logger.Log(context.Background(), LevelTrace, "a")
	logger.Info("b", "count", 2)
	logger.Log(context.Background(), LevelFatal, "c")

	lines := decode(t, &buf)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	for i, want := range []string{"trace", "info", "fatal"} {
		if lines[i]["level"] != want {
			t.Errorf("line %d level %v, want %s", i, lines[i]["level"], want)
		}
		if lines[i]["service"] != "gateway" {
			t.Errorf("line %d missing service", i)
		}
		if _, ok := lines[i]["time"]; !ok {
			t.Errorf("line %d missing time", i)
		}
	}
	if lines[1]["msg"] != "b" || lines[1]["count"] != float64(2) {
		t.Errorf("unexpected line %v", lines[1])
	}
}

func TestNewFiltersBelowLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, "warn")
	logger.Info("hidden")
	logger.Warn("shown")
	lines := decode(t, &buf)
	if len(lines) != 1 || lines[0]["msg"] != "shown" {
		t.Fatalf("got %v", lines)
	}
}

func TestContextLogger(t *testing.T) {
	fallback := slog.New(slog.DiscardHandler)
	if FromContext(context.Background(), fallback) != fallback {
		t.Fatal("expected the fallback without a logger in context")
	}
	inCtx := slog.New(slog.DiscardHandler)
	if FromContext(WithLogger(context.Background(), inCtx), fallback) != inCtx {
		t.Fatal("expected the logger from context")
	}
}

func TestErrUsesTheSameKeyAsCore(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, "info").Error("failed", Err(errors.New("boom")))
	lines := decode(t, &buf)
	errField, ok := lines[0]["err"].(map[string]any)
	if !ok || errField["message"] != "boom" || errField["type"] != "*errors.errorString" {
		t.Fatalf("unexpected err field: %v", lines[0])
	}
}
