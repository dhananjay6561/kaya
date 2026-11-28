//go:build live

package actions

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/masking"
)

// TestLiveJudge reads many ways of answering "send this?" with the real Groq:
// go test -tags live -run TestLiveJudge -v ./internal/actions/
func TestLiveJudge(t *testing.T) {
	key := os.Getenv("GROQ_API_KEY")
	if key == "" {
		t.Skip("GROQ_API_KEY not set")
	}
	groq, err := llm.NewGroq(config.Groq{APIKey: key, HeavyModel: "openai/gpt-oss-120b", RoutineModel: "openai/gpt-oss-20b", ContextTokens: 4000}, llm.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	judge := NewJudge(groq, func() Masker { return masking.NewSession() }, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	asked := "Send to Rahul (contact, +919876543210):\nrunning 10 min late"
	for reply, want := range map[string]string{
		"Yess": Yes, "haan bhej de yaar": Yes, "sure go": Yes, "👍🏻": Yes, "ha bhai": Yes, "theek hai bhejo": Yes, "sí, mándalo": Yes, "ok cool": Yes,
		"nah leave it": No, "ruk abhi mat bhej": No, "no": No, "rehne de": No, "cancel kar": No,
		"haan but say 20 min": Change, "send it to priya instead": Change, "add sorry too": Change,
		"btw what time is it": Other, "who is rahul": Other, "lol": Other,
	} {
		time.Sleep(2 * time.Second)
		got, err := judge(bg, asked, reply)
		if err != nil {
			t.Errorf("%q: %v", reply, err)
			continue
		}
		if got != want {
			t.Errorf("%q: got %s, want %s", reply, got, want)
		}
	}
}
