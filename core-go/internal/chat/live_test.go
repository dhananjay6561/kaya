//go:build live

package chat

import (
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/memory"
)

// TestLiveGroq asks the real Groq one made-up question from testdata/live.json, chosen by
// KAYA_LIVE_CASE, and logs what the model was sent and answered. It runs only with the live build
// tag and GROQ_API_KEY set: go test -tags live -run TestLiveGroq ./internal/chat/
func TestLiveGroq(t *testing.T) {
	key := os.Getenv("GROQ_API_KEY")
	if key == "" {
		t.Skip("GROQ_API_KEY not set")
	}
	raw, err := os.ReadFile("testdata/live.json")
	if err != nil {
		t.Fatal(err)
	}
	var live struct {
		Now   time.Time `json:"now"`
		Zone  string    `json:"zone"`
		Cases []struct {
			Name     string            `json:"name"`
			Question string            `json:"question"`
			History  []memory.Exchange `json:"history"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &live); err != nil {
		t.Fatal(err)
	}
	i, err := strconv.Atoi(os.Getenv("KAYA_LIVE_CASE"))
	if err != nil || i < 0 || i >= len(live.Cases) {
		t.Fatalf("set KAYA_LIVE_CASE to 0..%d", len(live.Cases)-1)
	}
	c := live.Cases[i]
	groq, err := llm.NewGroq(config.Groq{APIKey: key, HeavyModel: "openai/gpt-oss-120b", RoutineModel: "openai/gpt-oss-20b", ContextTokens: 2000}, llm.GroqOptions{})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := New(Options{
		LLM:            groq,
		Logger:         slog.New(slog.NewJSONHandler(os.Stdout, nil)),
		Zone:           zone(t, live.Zone),
		Now:            func() time.Time { return live.Now },
		History:        history(c.History...),
		DatabaseAway:   func(error) bool { return false },
		NewMaskSession: func() MaskSession { return identity{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := chat.Reply(bg, Question{ID: "3EB0LIVE", Text: c.Question, SentAt: live.Now})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{"core": "go", "case": c.Name, "answer": answer.Text})
	t.Log(string(out))
}
