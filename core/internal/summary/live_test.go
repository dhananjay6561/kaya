//go:build live

package summary

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/core/internal/llm"
)

// TestLiveSummary summarises a made-up busy group with the real Groq heavy model and logs it. It
// runs only with the live build tag, GROQ_API_KEY and TEST_DATABASE_URL set:
// go test -tags live -run TestLiveSummary -v ./internal/summary/
func TestLiveSummary(t *testing.T) {
	key := os.Getenv("GROQ_API_KEY")
	if key == "" {
		t.Skip("GROQ_API_KEY not set")
	}
	groq, err := llm.NewGroq(config.Groq{APIKey: key, HeavyModel: "openai/gpt-oss-120b", RoutineModel: "openai/gpt-oss-20b", ContextTokens: 4000}, llm.GroqOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t)
	r.exec("UPDATE contacts SET push_name = 'Dhananjay' WHERE id = $1", r.me)
	chatter := []string{
		"good morning all 🌞", "gm", "gm gm", "anyone up for goa trip in jan?", "count me in!!",
		"dates? 15-18 jan works for me", "15-18 is fine", "same", "I can't do 15, 16-19?",
		"ok let's do 16-19 jan then", "who's booking flights?", "Dhananjay can you book the flights? you have the card points",
		"yes DJ pls 🙏", "budget per head?", "15k max", "15k is fine", "lol rahul will spend 15k on drinks alone",
		"😂😂", "hotel near baga beach?", "Priya found one, 3k a night", "send link", "https://example.com/hotel",
		"looks good, book it Priya", "booking tonight", "also need someone for the car rental", "I'll do the car",
		"cool so DJ flights, Priya hotel, Amit car", "DJ please confirm flights by friday, prices are going up",
		"forwarded: 10 tips for a healthy morning routine", "ok done for today, bye",
	}
	for i, body := range chatter {
		r.say(r.group, time.Duration(len(chatter)-i)*time.Minute, body)
	}
	s := r.summarizer(&model{})
	s.o.LLM = groq
	s.o.InputTokens = groq.ContextTokens()
	got, err := s.Chat(bg, "college gang")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("summary (%d characters):\n%s", len([]rune(got)), got)
	lower := strings.ToLower(got)
	if strings.Contains(lower, "no direct tasks") || strings.Contains(lower, "nothing for you") {
		t.Error("the summary missed that the flights are the owner's to book")
	}
	for _, want := range []string{"flight", "16", "hotel", "car", "15k"} {
		if !strings.Contains(lower, want) {
			t.Errorf("the summary leaves out %q", want)
		}
	}
	if len([]rune(got)) > 900 {
		t.Errorf("the summary is long: %d characters", len([]rune(got)))
	}
}
