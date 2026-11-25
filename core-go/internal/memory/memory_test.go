package memory

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/core/internal/llm"
)

var at = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func count(text string) int { return utf8.RuneCountInString(text) }

func exchange(n string, size int) Exchange {
	return Exchange{
		Question: "question " + n + " " + strings.Repeat("q", size),
		Answer:   "answer " + n + " " + strings.Repeat("a", size),
		At:       at,
	}
}

var sentMarker = regexp.MustCompile(`^\(sent [^)]*\) `)

func firstWords(text string) string {
	words := strings.Split(sentMarker.ReplaceAllString(text, ""), " ")
	return strings.Join(words[:min(2, len(words))], " ")
}

func TestShortenKeepsTheStartAndTheEndAndCutsTheMiddle(t *testing.T) {
	text := "start " + strings.Repeat("x", 500) + " end"
	short := Shorten(text, 60, count)
	if count(short) > 60 || !strings.HasPrefix(short, "start") || !strings.HasSuffix(short, "end") || !strings.Contains(short, " … ") {
		t.Fatalf("short = %q", short)
	}
}

func TestShortenLeavesTextThatFitsAndGivesUpWithAnEllipsis(t *testing.T) {
	if got := Shorten("short", 60, count); got != "short" {
		t.Fatalf("got %q", got)
	}
	if got := Shorten(strings.Repeat("x", 100), 2, count); got != "…" {
		t.Fatalf("got %q", got)
	}
}

func roles(p Package) []string {
	var out []string
	for _, m := range p.Messages {
		out = append(out, m.Role+": "+firstWords(m.Content))
	}
	return out
}

func TestBuildPutsTheSystemPromptFirstPastExchangesInOrderThenTheQuestion(t *testing.T) {
	p := Build(Input{System: "be brief", History: []Exchange{exchange("1", 10), exchange("2", 10)},
		Question: "and in Hindi?", Budget: 10_000, Count: count, Now: at})
	want := []string{"system: be brief", "user: question 1", "assistant: answer 1", "user: question 2",
		"assistant: answer 2", "user: and in"}
	if got := roles(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if p.Exchanges != 2 || p.LeftOut != 0 || p.Messages[0].Content != "be brief" {
		t.Fatalf("package %+v", p)
	}
}

func TestBuildKeepsTheNewestThatFitDropsOlderOnesWholeAndSaysSo(t *testing.T) {
	var history []Exchange
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		history = append(history, exchange(n, 200))
	}
	p := Build(Input{System: "s", History: history, Question: "now?", Budget: 1100, Count: count, Now: at})
	if p.Exchanges != 2 || p.LeftOut != 3 || p.Tokens > 1100 {
		t.Fatalf("package %d kept, %d left out, %d tokens", p.Exchanges, p.LeftOut, p.Tokens)
	}
	if got := roles(p)[1:5]; !reflect.DeepEqual(got, []string{"user: question 4", "assistant: answer 4", "user: question 5", "assistant: answer 5"}) {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(p.Messages[0].Content, "Older parts of this conversation were left out") {
		t.Fatal("no note")
	}
}

func TestBuildShortensTheNewestExchangeRatherThanDropIt(t *testing.T) {
	big := Exchange{Question: "paste " + strings.Repeat("p", 5000) + " end", Answer: "summary here", At: at}
	p := Build(Input{System: "s", History: []Exchange{big}, Question: "make it shorter", Budget: 600, Count: count, Now: at})
	if p.Exchanges != 1 || p.Tokens > 600 {
		t.Fatalf("package %+v", p)
	}
	q, a := p.Messages[1].Content, p.Messages[2].Content
	if !strings.HasPrefix(q, "(sent ") || !strings.Contains(q, "paste") || !strings.HasSuffix(q, "end") || a != "summary here" {
		t.Fatalf("question %q, answer %q", q, a)
	}
}

func TestBuildSendsJustTheQuestionWhenThereIsNoRoomAtAll(t *testing.T) {
	p := Build(Input{System: "s", History: []Exchange{exchange("1", 10)}, Question: "q", Budget: 10, Count: count, Now: at})
	if p.Exchanges != 0 || len(p.Messages) != 2 || p.LeftOut != 1 {
		t.Fatalf("package %+v", p)
	}
}

func TestBuildDoesNotSkipPastAnExchangeThatDoesNotFit(t *testing.T) {
	history := []Exchange{exchange("1", 5), exchange("2", 5000), exchange("3", 5)}
	p := Build(Input{System: "s", History: history, Question: "q", Budget: 300, Count: count, Now: at})
	if p.Exchanges != 1 || firstWords(p.Messages[1].Content) != "question 3" {
		t.Fatalf("package %+v", p)
	}
}

func TestBuildCountsWithTheCounterItIsGiven(t *testing.T) {
	var calls []string
	Build(Input{System: "s", History: []Exchange{exchange("1", 10)}, Question: "q", Budget: 10_000, Now: at,
		Count: func(text string) int { calls = append(calls, text); return 1 }})
	if !strings.Contains(strings.Join(calls, "|"), "question 1") || calls[1] != "q" {
		t.Fatalf("calls %q", calls)
	}
}

func TestHowLongAgoReadsLikeAPersonWouldSayIt(t *testing.T) {
	const minute = 60_000
	for ms, want := range map[int64]string{-5: "just now", 30_000: "just now", 5 * minute: "5 min ago",
		3 * 60 * minute: "3 h ago", 26 * 60 * minute: "1 day ago", 9 * 24 * 60 * minute: "9 days ago"} {
		if got := HowLongAgo(ms); got != want {
			t.Errorf("HowLongAgo(%d) = %q, want %q", ms, got, want)
		}
	}
}

func TestBuildMarksEachEarlierQuestionWithItsAgeButNotTheAnswersOrTheNewQuestion(t *testing.T) {
	p := Build(Input{System: "s", History: []Exchange{exchange("1", 10)}, Question: "and today?", Budget: 10_000,
		Count: count, Now: at.Add(3 * 24 * time.Hour)})
	if !strings.HasPrefix(p.Messages[1].Content, "(sent 3 days ago) question 1") ||
		!strings.HasPrefix(p.Messages[2].Content, "answer 1") || p.Messages[3].Content != "and today?" {
		t.Fatalf("messages %+v", p.Messages)
	}
}

func TestBuildMeasuresAgesInWholeMilliseconds(t *testing.T) {
	now := time.Date(2026, 11, 25, 9, 1, 0, 999_000, time.UTC)
	sent := time.Date(2026, 11, 25, 9, 0, 0, 999_999, time.UTC)
	p := Build(Input{System: "s", History: []Exchange{{Question: "q", Answer: "a", At: sent}}, Question: "x",
		Budget: 10_000, Count: count, Now: now})
	if p.Messages[1].Content != "(sent 1 min ago) q" {
		t.Fatalf("got %q: 59.999 ms of microseconds must not count", p.Messages[1].Content)
	}
}

type golden struct {
	Now    time.Time `json:"now"`
	System string    `json:"system"`
	Cases  []struct {
		Name    string `json:"name"`
		History []struct {
			Question string    `json:"question"`
			Answer   string    `json:"answer"`
			At       time.Time `json:"at"`
		} `json:"history"`
		Question string `json:"question"`
		Budget   int    `json:"budget"`
		Want     struct {
			Messages  []llm.Message `json:"messages"`
			Tokens    int           `json:"tokens"`
			Exchanges int           `json:"exchanges"`
			LeftOut   int           `json:"leftOut"`
		} `json:"want"`
	} `json:"cases"`
	Shorten []struct {
		Text string `json:"text"`
		Max  int    `json:"max"`
		Want string `json:"want"`
	} `json:"shorten"`
	Ago []struct {
		MS   float64 `json:"ms"`
		Want string  `json:"want"`
	} `json:"ago"`
}

func readGolden(t *testing.T) golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/context.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Cases) < 10 {
		t.Fatalf("only %d cases", len(g.Cases))
	}
	return g
}

// gptOss is the Groq provider's counter, which the golden file was written with.
func gptOss(t *testing.T) func(string) int {
	t.Helper()
	g, err := llm.NewGroq(config.Groq{HeavyModel: "openai/gpt-oss-120b", RoutineModel: "openai/gpt-oss-20b"}, llm.GroqOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return g.CountTokens
}

func TestBuildMatchesTheTypeScriptCore(t *testing.T) {
	g := readGolden(t)
	count := gptOss(t)
	for _, c := range g.Cases {
		var history []Exchange
		for _, e := range c.History {
			history = append(history, Exchange{Question: e.Question, Answer: e.Answer, At: e.At})
		}
		got := Build(Input{System: g.System, History: history, Question: c.Question, Budget: c.Budget, Count: count, Now: g.Now})
		if got.Tokens != c.Want.Tokens || got.Exchanges != c.Want.Exchanges || got.LeftOut != c.Want.LeftOut {
			t.Errorf("%s: tokens %d, exchanges %d, left out %d; want %d, %d, %d", c.Name,
				got.Tokens, got.Exchanges, got.LeftOut, c.Want.Tokens, c.Want.Exchanges, c.Want.LeftOut)
		}
		if !reflect.DeepEqual(got.Messages, c.Want.Messages) {
			t.Errorf("%s: messages differ", c.Name)
		}
	}
	for _, c := range g.Shorten {
		if got := Shorten(c.Text, c.Max, count); got != c.Want {
			t.Errorf("Shorten(%.20q, %d) = %.40q, want %.40q", c.Text, c.Max, got, c.Want)
		}
	}
	for _, c := range g.Ago {
		if got := HowLongAgo(int64(c.MS)); got != c.Want {
			t.Errorf("HowLongAgo(%v) = %q, want %q", c.MS, got, c.Want)
		}
	}
}
