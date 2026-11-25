package o200k

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type golden struct {
	Curated []struct {
		Text  string `json:"text"`
		IDs   []int  `json:"ids"`
		Count *int   `json:"count"`
	} `json:"curated"`
	Fuzz []struct {
		Text  string `json:"text"`
		Count int    `json:"count"`
	} `json:"fuzz"`
	Sweep []*int `json:"sweep"`
}

func readGolden(t *testing.T) golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Curated) < 100 || len(g.Fuzz) < 1000 {
		t.Fatalf("golden file is too small: %d curated, %d fuzz", len(g.Curated), len(g.Fuzz))
	}
	return g
}

// encode gives the token ids, to show the ranks are the same file gpt-tokenizer uses.
func encode(text string) []int {
	e := load()
	var ids []int
	for start := 0; start < len(text); {
		end := e.next(text, start)
		piece := text[start:end]
		if id, ok := e.ranks[piece]; ok {
			ids = append(ids, id)
		} else {
			next, _ := e.merge(piece)
			for i := 0; i < len(piece); i = next[i] {
				ids = append(ids, e.ranks[piece[i:next[i]]])
			}
		}
		start = end
	}
	return ids
}

func TestTheRanksAreTheOfficialFile(t *testing.T) {
	sum := sha256.Sum256(ranksFile)
	if got := hex.EncodeToString(sum[:]); got != "446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d" {
		t.Fatalf("o200k_base.tiktoken sha256 = %s", got)
	}
	if n := len(load().ranks); n != 199_998 {
		t.Fatalf("ranks = %d, want 199998", n)
	}
}

func TestCountsMatchTheTypeScriptCore(t *testing.T) {
	g := readGolden(t)
	for _, c := range g.Curated {
		want := len(c.IDs)
		if c.Count != nil {
			want = *c.Count
		}
		if got := Count(c.Text); got != want {
			t.Errorf("Count(%q) = %d, want %d", clip(c.Text), got, want)
		}
		if c.IDs != nil {
			if got := encode(c.Text); !equal(got, c.IDs) {
				t.Errorf("ids of %q = %v, want %v", clip(c.Text), got, c.IDs)
			}
		}
	}
	for _, c := range g.Fuzz {
		if got := Count(c.Text); got != c.Count {
			t.Errorf("Count(%q) = %d, want %d", c.Text, got, c.Count)
		}
	}
}

func TestCountsEveryCharacterLikeTheTypeScriptCore(t *testing.T) {
	g := readGolden(t)
	if len(g.Sweep) != 0x110000/64 {
		t.Fatalf("sweep has %d entries", len(g.Sweep))
	}
	checked := 0
	for i, want := range g.Sweep {
		if want == nil {
			continue
		}
		runes := make([]rune, 64)
		for k := range runes {
			runes[k] = rune(i*64 + k)
		}
		if got := Count(string(runes)); got != *want {
			t.Errorf("code points U+%04X to U+%04X: count %d, want %d", i*64, i*64+63, got, *want)
		}
		checked++
	}
	if checked != 0x110000/64-32 {
		t.Fatalf("checked %d chunks", checked)
	}
}

func TestCountsWhiteSpaceAndContractionsLikeTiktoken(t *testing.T) {
	for text, want := range map[string]int{
		"":                  0,
		"hello how are you": 4,
		"\n \n":             1,
		"a  b":              3,
	} {
		if got := Count(text); got != want {
			t.Errorf("Count(%q) = %d, want %d", text, got, want)
		}
	}
	for _, text := range []string{
		"\u3000word", "\u00a0word", "x\u3000\u3000y", "trailing   ", "a\r\n\r\nb", "itſ fine", "it'ſ fine",
		"\u0085", "x\ufeffy", "a \n", "😀 x",
	} {
		if Count(text) == 0 {
			t.Errorf("Count(%q) = 0", text)
		}
	}
}

func TestSplitsLikeTiktoken(t *testing.T) {
	e := load()
	for text, want := range map[string][]string{
		"a  b":           {"a", " ", " b"},
		"\n \n":          {"\n \n"},
		"x\u3000\u3000y": {"x", "\u3000", "\u3000y"},
		"  leading":      {" ", " leading"},
		"trailing  ":     {"trailing", "  "},
		"I'M here":       {"I'M", " here"},
		"itſ":            {"itſ"},
		"it'ſ fine":      {"it'ſ", " fine"},
		"12345":          {"123", "45"},
		"a\r\n\r\nb":     {"a", "\r\n\r\n", "b"},
		"x \u0085y":      {"x", " ", "\u0085y"},
		"path/to\n":      {"path", "/to", "\n"},
		"wow!!\n\n":      {"wow", "!!\n\n"},
	} {
		var got []string
		for start := 0; start < len(text); {
			end := e.next(text, start)
			got = append(got, text[start:end])
			start = end
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("pieces of %q = %q, want %q", text, got, want)
		}
	}
}

var raceDetector bool

func TestStaysFastOnLongText(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("timing means nothing in short mode or under the race detector")
	}
	Load()
	const size = 64 << 10
	for name, text := range map[string]string{
		"spaces then x":   strings.Repeat(" ", size-1) + "x",
		"blank lines":     strings.Repeat("\n \n", size/3),
		"emoji":           strings.Repeat("😀", size/4),
		"one letter":      strings.Repeat("a", size),
		"digits":          strings.Repeat("7", size),
		"ideographic":     strings.Repeat("\u3000", size/3) + "x",
		"devanagari":      strings.Repeat("क्ष", size/9),
		"punctuation run": strings.Repeat("!?", size/2),
	} {
		started := time.Now()
		Count(text)
		took := time.Since(started)
		t.Logf("%s: %v", name, took)
		if took > 200*time.Millisecond {
			t.Errorf("%s: %v", name, took)
		}
	}
}

func FuzzCount(f *testing.F) {
	for _, seed := range []string{"", "a  b", "\n \n", "it's", "\u3000x", "<|endoftext|>", "a\xffb", "😀👍🏽"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		n := Count(text)
		if n < 0 || n > len(text) {
			t.Fatalf("Count(%q) = %d", text, n)
		}
		if text != "" && n == 0 {
			t.Fatalf("Count(%q) = 0", text)
		}
	})
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func clip(s string) string {
	if utf8.RuneCountInString(s) <= 60 {
		return s
	}
	return string([]rune(s)[:60]) + "…"
}
