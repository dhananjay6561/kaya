package whatsapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"

	"github.com/dhananjay6561/kaya/kit/logging"
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

func TestLoggerMasksJIDsAndRawNodes(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(logging.New(&buf, "info"))
	node := &waBinary.Node{Tag: "failure", Attrs: waBinary.Attrs{"from": "919876543210@s.whatsapp.net"}}

	log.Warnf("Ignoring message %s from %s: %v", "3EB0C767D26B5A7F2A1F", "919876543210.0:12@s.whatsapp.net", "bad mac")
	log.Warnf("lid %s, group %s, old group %s, bot %s, hosted %s", "100000000000001@lid", "120363000000000000@g.us",
		"911111111111-1600000000@g.us", "13135550002@bot", "200000000000001@hosted.lid")
	log.Warnf("Unknown connect failure: %s", node)
	log.Errorf("value node %v", *node)

	out := buf.String()
	for _, leaked := range []string{"919876543210", "100000000000001", "120363000000000000", "911111111111", "13135550002", "200000000000001"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("%s reached the log: %s", leaked, out)
		}
	}
	got := lines(t, &buf)
	if got[0]["msg"] != "Ignoring message 3EB0C767D26B5A7F2A1F from <jid>: bad mac" {
		t.Fatalf("got %q", got[0]["msg"])
	}
	if got[2]["msg"] != "Unknown connect failure: <failure node>" || got[3]["msg"] != "value node <failure node>" {
		t.Fatalf("nodes must be reduced to their tag: %v", got[2:])
	}
}

func TestLoggerMasksDownloadAddresses(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(logging.New(&buf, "info"))
	err := &url.Error{Op: "Get", URL: "https://mmg.whatsapp.net/v/t62.7118-24/12345_678_n.enc?ccb=11-4&oh=01_abc&oe=6770&hash=Zm9vYmFy&mms-type=image", Err: errors.New("EOF")}
	log.Warnf("Failed to download media: %v, trying with next host...", err)
	out := buf.String()
	for _, leaked := range []string{"mmg.whatsapp.net", "t62.7118", "Zm9vYmFy", "oh=01_abc"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("%s reached the log: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "<url>") || !strings.Contains(out, "trying with next host") {
		t.Fatalf("the rest of the line stays readable: %s", out)
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
