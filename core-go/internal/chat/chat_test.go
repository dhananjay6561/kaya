package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/memory"
)

var (
	bg     = context.Background()
	now    = time.Date(2026, 11, 24, 19, 0, 0, 0, time.UTC)
	logger = slog.New(slog.DiscardHandler)
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func q(text string) Question { return Question{ID: "3EB0Q", Text: text, SentAt: now} }

// fakeMask stands in for the masking engine until G.6: it hides the values it is given and
// records the order of calls.
type fakeMask struct {
	mu     sync.Mutex
	values map[string]string
	calls  *[]string
	seen   map[string]int
}

func (f *fakeMask) Mask(text, context string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.calls = append(*f.calls, "mask")
	for value, placeholder := range f.values {
		if strings.Contains(text, value) {
			if placeholder == "[OTP_1]" && !strings.Contains(context, "OTP") {
				continue
			}
			text = strings.ReplaceAll(text, value, placeholder)
			f.seen[strings.Trim(strings.SplitN(placeholder, "_", 2)[0], "[")]++
		}
	}
	return text
}

func (f *fakeMask) Restore(text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.calls = append(*f.calls, "restore")
	for value, placeholder := range f.values {
		text = strings.ReplaceAll(text, placeholder, value)
	}
	return text
}

func (f *fakeMask) Counts() map[string]int { return f.seen }

func masking(values map[string]string, calls *[]string) func() MaskSession {
	return func() MaskSession {
		*calls = append(*calls, "session")
		return &fakeMask{values: values, calls: calls, seen: map[string]int{}}
	}
}

// cloudStub is the stub model, reporting itself as a cloud model.
type cloudStub struct{ *llm.Stub }

func (cloudStub) Locality() llm.Locality { return llm.Cloud }

// scripted answers each request with the next completion or error.
type scripted struct {
	cloud     bool
	mu        sync.Mutex
	requests  []llm.Request
	responses func(r llm.Request) (llm.Completion, error)
}

func (s *scripted) Name() string { return "scripted" }
func (s *scripted) Locality() llm.Locality {
	if s.cloud {
		return llm.Cloud
	}
	return llm.Local
}
func (s *scripted) ContextTokens() int          { return 4000 }
func (s *scripted) CountTokens(text string) int { return len(text) }
func (s *scripted) Complete(_ context.Context, r llm.Request) (llm.Completion, error) {
	s.mu.Lock()
	s.requests = append(s.requests, r)
	s.mu.Unlock()
	return s.responses(r)
}

func newChat(t *testing.T, o Options) *Chat {
	t.Helper()
	if o.Logger == nil {
		o.Logger = logger
	}
	if o.Zone == nil {
		o.Zone = time.UTC
	}
	if o.History != nil && o.DatabaseAway == nil {
		o.DatabaseAway = func(error) bool { return false }
	}
	if o.Now == nil {
		o.Now = func() time.Time { return now }
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func history(exchanges ...memory.Exchange) func(context.Context, Question) ([]memory.Exchange, error) {
	return func(context.Context, Question) ([]memory.Exchange, error) { return exchanges, nil }
}

func reply(t *testing.T, c *Chat, text string) Answer {
	t.Helper()
	a, err := c.Reply(bg, q(text))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSystemPromptGivesTheDateAndTimeWhereTheOwnerIs(t *testing.T) {
	if p := SystemPrompt(now, zone(t, "Asia/Kolkata"), llm.Cloud); !strings.Contains(p, "Wednesday 25 November 2026, 00:30 (Asia/Kolkata)") {
		t.Fatal(p)
	}
	if p := SystemPrompt(now, time.UTC, llm.Cloud); !strings.Contains(p, "Tuesday 24 November 2026, 19:00 (UTC)") {
		t.Fatal(p)
	}
}

func TestSystemPromptTellsTheTruthAboutWhereTheConversationGoes(t *testing.T) {
	cloud, local := SystemPrompt(now, time.UTC, llm.Cloud), SystemPrompt(now, time.UTC, llm.Local)
	if !strings.Contains(cloud, "sent to an AI service in the cloud") || strings.Contains(cloud, "run entirely on their own computer") ||
		!strings.Contains(cloud, "hidden behind placeholders such as [PHONE_1]") {
		t.Fatal(cloud)
	}
	if !strings.Contains(local, "run entirely on their own computer") || strings.Contains(local, "placeholders") || strings.Contains(local, "in the cloud") {
		t.Fatal(local)
	}
}

func TestSystemPromptSaysWhatKayaCannotDoYet(t *testing.T) {
	p := SystemPrompt(now, time.UTC, llm.Cloud)
	for _, want := range []string{
		"never offer them, promise them or suggest you will: reminders",
		"reading their other WhatsApp chats, searching the web",
		"There are no sessions and nothing resets",
		"cannot yet pin it as a lasting fact",
		"marked with how long ago",
		"cannot check anything live",
		"Never make up facts",
		"Read past typos and autocorrect",
		"answer in Hinglish in Latin letters, never in Devanagari",
		"Never claim to have done something you did not do",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestSystemPromptMatchesTheTypeScriptCore(t *testing.T) {
	raw, err := os.ReadFile("testdata/prompt.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Cases []struct {
			Now      time.Time    `json:"now"`
			Zone     string       `json:"zone"`
			Locality llm.Locality `json:"locality"`
			Want     string       `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Cases) < 10 {
		t.Fatalf("only %d cases", len(g.Cases))
	}
	for _, c := range g.Cases {
		if got := SystemPrompt(c.Now, zone(t, c.Zone), c.Locality); got != c.Want {
			t.Errorf("%v %s %s:\n got %q\nwant %q", c.Now, c.Zone, c.Locality, lastLine(got), lastLine(c.Want))
		}
	}
}

func lastLine(s string) string { return s[strings.LastIndex(s, "\n")+1:] }

func TestNewRefusesACloudModelWithoutMasking(t *testing.T) {
	cloud := cloudStub{llm.NewStub(llm.StubOptions{})}
	if _, err := New(Options{LLM: cloud, Logger: logger, Zone: time.UTC}); err == nil {
		t.Fatal("a cloud model was accepted without masking")
	}
	if _, err := New(Options{LLM: llm.NewStub(llm.StubOptions{}), Logger: logger, Zone: time.UTC}); err != nil {
		t.Fatalf("a local model needs no masking: %v", err)
	}
	if _, err := New(Options{LLM: llm.NewStub(llm.StubOptions{}), Logger: logger, Zone: time.UTC, History: history()}); err == nil {
		t.Fatal("history was accepted without DatabaseAway")
	}
}

func TestReplyAsksTheStrongerModelWithTheSystemPromptAndTheMessage(t *testing.T) {
	stub := llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "**Hi!** I'm Kaya." }})
	got := reply(t, newChat(t, Options{LLM: stub}), "who are you?")
	if got != (Answer{Text: "*Hi!* I'm Kaya.", Remember: true}) {
		t.Fatalf("got %+v", got)
	}
	r := stub.Requests()[0]
	if r.Tier != llm.Heavy || r.Effort != llm.Medium || r.MaxTokens != 3072 || len(r.Messages) != 2 ||
		r.Messages[0].Role != llm.System || r.Messages[1] != (llm.Message{Role: llm.User, Content: "who are you?"}) {
		t.Fatalf("request %+v", r)
	}
}

func TestReplyCarriesTheRecentConversationAskedForTheQuestionBeingAnswered(t *testing.T) {
	stub := llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "Namaste" }})
	var seen []string
	c := newChat(t, Options{LLM: stub, History: func(_ context.Context, q Question) ([]memory.Exchange, error) {
		seen = append(seen, q.ID)
		return []memory.Exchange{{Question: "how do I say hello?", Answer: "Hello.", At: now}}, nil
	}})
	reply(t, c, "and in Hindi?")
	want := []llm.Message{
		{Role: llm.User, Content: "(sent just now) how do I say hello?"},
		{Role: llm.Assistant, Content: "Hello."},
		{Role: llm.User, Content: "and in Hindi?"},
	}
	if got := stub.Requests()[0].Messages[1:]; !reflect.DeepEqual(got, want) || !reflect.DeepEqual(seen, []string{"3EB0Q"}) {
		t.Fatalf("got %+v, seen %v", got, seen)
	}
}

func TestReplyFitsTheConversationToTheModelsBudget(t *testing.T) {
	stub := llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "ok" }, ContextTokens: 1000})
	long := strings.Repeat("x", 2000)
	reply(t, newChat(t, Options{LLM: stub, History: history(
		memory.Exchange{Question: long, Answer: long, At: now},
		memory.Exchange{Question: "short", Answer: "reply", At: now},
	)}), "next?")
	var got []string
	for _, m := range stub.Requests()[0].Messages[1:] {
		got = append(got, m.Content)
	}
	if !reflect.DeepEqual(got, []string{"(sent just now) short", "reply", "next?"}) {
		t.Fatalf("got %q", got)
	}
}

// The next three use a stand-in for the masking engine; G.6 runs them again with the real one.

func TestReplyMasksWhatTheOwnerDidNotTypeForACloudModelAndRestoresTheAnswer(t *testing.T) {
	var calls []string
	cloud := cloudStub{llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "Call [PHONE_1] or mail [EMAIL_1]." }})}
	c := newChat(t, Options{LLM: cloud,
		NewMaskSession: masking(map[string]string{"9876543210": "[PHONE_1]", "rahul@x.com": "[EMAIL_1]"}, &calls),
		History: history(memory.Exchange{Question: "what's Rahul's number?", Answer: "Rahul is on 9876543210, mail rahul@x.com", At: now}),
	})
	got := reply(t, c, "my own number is 9123456780, and Rahul's again?")
	sent := cloud.Requests()[0].Messages
	if sent[2].Content != "Rahul is on [PHONE_1], mail [EMAIL_1]" ||
		sent[3].Content != "my own number is 9123456780, and Rahul's again?" {
		t.Fatalf("sent %+v", sent)
	}
	body, _ := json.Marshal(sent)
	if strings.Contains(string(body), "9876543210") || strings.Contains(string(body), "rahul@x.com") {
		t.Fatalf("leaked: %s", body)
	}
	if got.Text != "Call 9876543210 or mail rahul@x.com." {
		t.Fatalf("got %q", got.Text)
	}
}

func TestReplyHidesAnOTPInAPastAnswerWhenTheQuestionAskedForOne(t *testing.T) {
	var calls []string
	cloud := cloudStub{llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "ok" }})}
	c := newChat(t, Options{LLM: cloud, NewMaskSession: masking(map[string]string{"482913": "[OTP_1]"}, &calls),
		History: history(memory.Exchange{Question: "what was that OTP?", Answer: "Here it is: 482913", At: now})})
	reply(t, c, "thanks")
	if got := cloud.Requests()[0].Messages[2].Content; got != "Here it is: [OTP_1]" {
		t.Fatalf("got %q", got)
	}
}

func TestReplyPutsDetailsBackBeforeTheAnswerIsFormatted(t *testing.T) {
	var calls []string
	cloud := cloudStub{llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "[PHONE_1](https://wa.me/[PHONE_1])" }})}
	c := newChat(t, Options{LLM: cloud, NewMaskSession: masking(map[string]string{"9876543210": "[PHONE_1]"}, &calls),
		History: history(memory.Exchange{Question: "number?", Answer: "9876543210", At: now})})
	if got := reply(t, c, "link?"); strings.Contains(got.Text, "PHONE_1") {
		t.Fatalf("got %q", got.Text)
	}
}

func TestReplySendsEverythingAsItIsToALocalModel(t *testing.T) {
	local := llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "ok" }})
	reply(t, newChat(t, Options{LLM: local, History: history(memory.Exchange{Question: "number?", Answer: "9876543210", At: now})}), "again?")
	if got := local.Requests()[0].Messages[2].Content; got != "9876543210" {
		t.Fatalf("got %q", got)
	}
}

func TestReplyMasksBeforeCountingRestoresBeforeFormattingAndKeepsOneSessionAcrossRetries(t *testing.T) {
	var calls []string
	llmCalls := 0
	model := &scripted{cloud: true, responses: func(r llm.Request) (llm.Completion, error) {
		llmCalls++
		calls = append(calls, "complete")
		switch {
		case len(r.Messages) > 2:
			return llm.Completion{}, &llm.Error{Provider: "groq", Code: llm.TooLong}
		case r.Effort == llm.Medium:
			return llm.Completion{FinishReason: llm.FinishLength}, nil
		default:
			return llm.Completion{Text: "**[PHONE_1]**", FinishReason: llm.FinishStop}, nil
		}
	}}
	c := newChat(t, Options{LLM: model, NewMaskSession: masking(map[string]string{"9876543210": "[PHONE_1]"}, &calls),
		History: history(memory.Exchange{Question: "number?", Answer: "9876543210", At: now})})
	got := reply(t, c, "again?")
	if want := []string{"session", "mask", "complete", "complete", "complete", "restore"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v, want %v", calls, want)
	}
	if got.Text != "*9876543210*" || llmCalls != 3 {
		t.Fatalf("got %q after %d calls", got.Text, llmCalls)
	}
}

func TestReplySaysSoWhenTheModelGivesNothingBack(t *testing.T) {
	stub := llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "   " }})
	if got := reply(t, newChat(t, Options{LLM: stub}), "x"); got != (Answer{Text: "I had no answer to that. Try asking another way."}) {
		t.Fatalf("got %+v", got)
	}
}

func TestReplyClipsALongAnswerAndMarksOneTheModelCouldNotFinish(t *testing.T) {
	long := llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return strings.Repeat("y", 5000) }})
	if got := reply(t, newChat(t, Options{LLM: long, MaxReplyLength: 10}), "x"); got.Text != strings.Repeat("y", 10)+"…"+cutShort {
		t.Fatalf("got %q", got.Text)
	}
	half := &scripted{responses: func(llm.Request) (llm.Completion, error) {
		return llm.Completion{Text: "half an answer", FinishReason: llm.FinishLength}, nil
	}}
	if got := reply(t, newChat(t, Options{LLM: half}), "x"); got.Text != "half an answer"+cutShort || !got.Remember {
		t.Fatalf("got %+v", got)
	}
	empty := &scripted{responses: func(llm.Request) (llm.Completion, error) {
		return llm.Completion{FinishReason: llm.FinishLength}, nil
	}}
	if got := reply(t, newChat(t, Options{LLM: empty}), "x"); got != (Answer{Text: "I ran out of room before I could answer. Try a narrower question."}) {
		t.Fatalf("got %+v", got)
	}
}

func TestReplyAnswersWithoutTheConversationWhenItCannotBeReadButNotWhenTheDatabaseIsAway(t *testing.T) {
	stub := llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "fine" }})
	denied := errors.New("permission denied for table commands")
	away := errors.New("connect ECONNREFUSED")
	c := newChat(t, Options{LLM: stub,
		History:      func(context.Context, Question) ([]memory.Exchange, error) { return nil, denied },
		DatabaseAway: func(err error) bool { return errors.Is(err, away) },
	})
	if got := reply(t, c, "hi"); got.Text != "fine" || len(stub.Requests()[0].Messages) != 2 {
		t.Fatalf("got %+v", got)
	}
	c = newChat(t, Options{LLM: stub,
		History:      func(context.Context, Question) ([]memory.Exchange, error) { return nil, away },
		DatabaseAway: func(err error) bool { return errors.Is(err, away) },
	})
	if _, err := c.Reply(bg, q("hi")); !errors.Is(err, away) {
		t.Fatalf("err = %v", err)
	}
}

func TestReplyAsksAgainWithLessEffortWhenTheModelSpentAllItsRoomThinking(t *testing.T) {
	var efforts []llm.Effort
	model := &scripted{responses: func(r llm.Request) (llm.Completion, error) {
		efforts = append(efforts, r.Effort)
		if r.Effort == llm.Low {
			return llm.Completion{Text: "here you go", FinishReason: llm.FinishLength}, nil
		}
		return llm.Completion{FinishReason: llm.FinishLength}, nil
	}}
	got := reply(t, newChat(t, Options{LLM: model}), "hard one")
	if !reflect.DeepEqual(efforts, []llm.Effort{llm.Medium, llm.Low}) || !strings.HasPrefix(got.Text, "here you go") {
		t.Fatalf("efforts %v, got %q", efforts, got.Text)
	}
}

func TestReplyAsksAgainWithoutTheConversationWhenTheRequestIsTooLong(t *testing.T) {
	var sizes []int
	model := &scripted{responses: func(r llm.Request) (llm.Completion, error) {
		sizes = append(sizes, len(r.Messages))
		if len(r.Messages) > 2 {
			return llm.Completion{}, &llm.Error{Provider: "groq", Code: llm.TooLong, Status: 413}
		}
		return llm.Completion{Text: "short answer", FinishReason: llm.FinishStop}, nil
	}}
	got := reply(t, newChat(t, Options{LLM: model, History: history(memory.Exchange{Question: "old", Answer: "older", At: now})}), "hi")
	if got.Text != "short answer" || !reflect.DeepEqual(sizes, []int{4, 2}) {
		t.Fatalf("got %q, sizes %v", got.Text, sizes)
	}
}

func TestReplyKeepsTheConversationOutOfTheLowEffortRetryOnceItWasTooLong(t *testing.T) {
	type asked struct {
		size   int
		effort llm.Effort
	}
	var calls []asked
	model := &scripted{responses: func(r llm.Request) (llm.Completion, error) {
		calls = append(calls, asked{len(r.Messages), r.Effort})
		if len(r.Messages) > 2 {
			return llm.Completion{}, &llm.Error{Provider: "groq", Code: llm.TooLong, Status: 413}
		}
		if r.Effort == llm.Low {
			return llm.Completion{Text: "made it", FinishReason: llm.FinishLength}, nil
		}
		return llm.Completion{FinishReason: llm.FinishLength}, nil
	}}
	got := reply(t, newChat(t, Options{LLM: model, History: history(memory.Exchange{Question: "old", Answer: "older", At: now})}), "hi")
	if want := []asked{{4, llm.Medium}, {2, llm.Medium}, {2, llm.Low}}; !reflect.DeepEqual(calls, want) || !strings.HasPrefix(got.Text, "made it") {
		t.Fatalf("calls %v, got %q", calls, got.Text)
	}
}

func TestReplyStillAnswersWhenARememberedAnswerLooksLikeASpecialToken(t *testing.T) {
	groq, err := llm.NewGroq(config.Groq{HeavyModel: "openai/gpt-oss-120b", RoutineModel: "openai/gpt-oss-20b", ContextTokens: 2000}, llm.GroqOptions{})
	if err != nil {
		t.Fatal(err)
	}
	model := &scripted{responses: func(r llm.Request) (llm.Completion, error) {
		for _, m := range r.Messages {
			groq.CountTokens(m.Content)
		}
		return llm.Completion{Text: "sure", FinishReason: llm.FinishStop}, nil
	}}
	c := newChat(t, Options{LLM: model, History: history(memory.Exchange{Question: "what ends a text?", Answer: "It is the <|endoftext|> token.", At: now})})
	if got := reply(t, c, "and the next one?"); got.Text != "sure" {
		t.Fatalf("got %q", got.Text)
	}
}

func TestReplyReturnsTheCallersReasonWhenItStops(t *testing.T) {
	stopping := errors.New("core is stopping")
	ctx, cancel := context.WithCancelCause(bg)
	cancel(stopping)
	c := newChat(t, Options{LLM: llm.NewStub(llm.StubOptions{}),
		History: func(ctx context.Context, _ Question) ([]memory.Exchange, error) { return nil, ctx.Err() }})
	if _, err := c.Reply(ctx, q("hi")); !errors.Is(err, stopping) {
		t.Fatalf("err = %v", err)
	}
}

// identity is a mask session with nothing to hide, for the golden requests, whose history holds
// no private details.
type identity struct{}

func (identity) Mask(text, _ string) string { return text }
func (identity) Restore(text string) string { return text }
func (identity) Counts() map[string]int     { return nil }

func TestReplySendsGroqTheSameRequestAsTheTypeScriptCore(t *testing.T) {
	raw, err := os.ReadFile("../llm/testdata/request.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Now  time.Time `json:"now"`
		Zone string    `json:"zone"`
		Chat []struct {
			Question string            `json:"question"`
			History  []memory.Exchange `json:"history"`
			Reply    string            `json:"reply"`
			Body     json.RawMessage   `json:"body"`
			Want     struct {
				Text     string `json:"text"`
				Remember bool   `json:"remember"`
			} `json:"want"`
		} `json:"chat"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Chat) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range g.Chat {
		var body []byte
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ = io.ReadAll(r.Body)
			answer, _ := json.Marshal(c.Reply)
			_, _ = io.WriteString(w, `{"model":"openai/gpt-oss-120b","choices":[{"message":{"role":"assistant","content":`+
				string(answer)+`},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
		}))
		groq, err := llm.NewGroq(config.Groq{APIKey: "not-a-real-key", HeavyModel: "openai/gpt-oss-120b",
			RoutineModel: "openai/gpt-oss-20b", ContextTokens: 2000}, llm.GroqOptions{BaseURL: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		chat := newChat(t, Options{LLM: groq, Zone: zone(t, g.Zone), Now: func() time.Time { return g.Now },
			NewMaskSession: func() MaskSession { return identity{} }, History: history(c.History...)})
		got, err := chat.Reply(bg, Question{ID: "3EB0GOLDEN", Text: c.Question, SentAt: g.Now})
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		var a, b any
		_ = json.Unmarshal(body, &a)
		_ = json.Unmarshal(c.Body, &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: body = %s\nwant %s", c.Question, body, c.Body)
		}
		if got.Text != c.Want.Text || got.Remember != c.Want.Remember {
			t.Errorf("%s: got %+v, want %+v", c.Question, got, c.Want)
		}
	}
}

