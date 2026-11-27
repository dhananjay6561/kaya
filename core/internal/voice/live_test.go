//go:build live

package voice

import (
	"os"
	"strings"
	"testing"
)

// TestLiveGroq transcribes the files in KAYA_VOICE_FILES (comma separated) with the real Groq:
// go test -tags live -run TestLiveGroq -v ./internal/voice/
func TestLiveGroq(t *testing.T) {
	key := os.Getenv("GROQ_API_KEY")
	files := os.Getenv("KAYA_VOICE_FILES")
	if key == "" || files == "" {
		t.Skip("GROQ_API_KEY or KAYA_VOICE_FILES not set")
	}
	g := NewGroq(key, "whisper-large-v3-turbo", "")
	for _, f := range strings.Split(files, ",") {
		got, err := g.Transcribe(bg, f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		t.Logf("%s -> %q (%s, %.1fs)", f, got.Text, got.Language, got.Duration)
	}
}
