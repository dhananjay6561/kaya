package whatsapp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dhananjay6561/kaya/gateway/internal/logging"
)

func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestLoggerMapsLevelsAndTagsTheModule(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(logging.New(&buf, "debug")).Sub("Client")

	log.Debugf("message body %s", "secret")
	log.Infof("connected to %s", "server")
	log.Warnf("slow")
	log.Errorf("failed: %d", 3)

	got := lines(t, &buf)
	if len(got) != 3 {
		t.Fatalf("want 3 lines (debug dropped), got %d: %v", len(got), got)
	}
	for i, level := range []string{"info", "warn", "error"} {
		if got[i]["level"] != level || got[i]["module"] != "whatsmeow" || got[i]["submodule"] != "Client" {
			t.Fatalf("line %d: %v", i, got[i])
		}
	}
	if got[0]["msg"] != "connected to server" || got[2]["msg"] != "failed: 3" {
		t.Fatalf("messages not formatted: %v", got)
	}
	if strings.Contains(buf.String(), "secret") {
		t.Fatal("a debug line reached the log")
	}
}

func TestLoggerRespectsTheLevel(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(logging.New(&buf, "warn"))
	log.Infof("hidden")
	log.Warnf("shown")
	if got := lines(t, &buf); len(got) != 1 || got[0]["msg"] != "shown" {
		t.Fatalf("got %v", got)
	}
}
