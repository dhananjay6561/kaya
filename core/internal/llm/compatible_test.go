package llm

import (
	"errors"
	"testing"

	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/core/internal/llm/o200k"
)

func compatible(t *testing.T, f *fakeGroq, c config.Compatible) *Client {
	t.Helper()
	c.BaseURL = f.server.URL + "/v1/"
	g, err := NewCompatible(c, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestACompatibleProviderGetsOnlyTheStandardFields(t *testing.T) {
	f := newFake(t, ok("hi"))
	g := compatible(t, f, config.Compatible{
		Kind: "compatible", APIKey: "key-not-real", HeavyModel: "gemini-heavy", RoutineModel: "gemini-light", ContextTokens: 6000, TokenFactor: 1.25,
	})
	if _, err := g.Complete(bg, Request{Tier: Routine, Messages: []Message{{Role: User, Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	c := f.call(0)
	if c.path != "/v1/chat/completions" || c.auth != "Bearer key-not-real" || c.body["model"] != "gemini-light" {
		t.Fatalf("call %+v", c)
	}
	for _, field := range []string{"reasoning_effort", "include_reasoning"} {
		if _, ok := c.body[field]; ok {
			t.Errorf("%s was sent to a provider that does not take it", field)
		}
	}
	if g.Name() != "127.0.0.1" || g.Locality() != Cloud || g.ContextTokens() != 6000 {
		t.Fatalf("name %q, locality %q, context %d", g.Name(), g.Locality(), g.ContextTokens())
	}
}

func TestACompatibleProviderCanTakeAnEffortAndRunLocally(t *testing.T) {
	f := newFake(t, ok("hi"))
	g := compatible(t, f, config.Compatible{Kind: "compatible", HeavyModel: "qwen3:4b", RoutineModel: "qwen3:4b", Effort: true, Local: true, TokenFactor: 1})
	if _, err := g.Complete(bg, Request{Tier: Heavy, Effort: High, Messages: []Message{{Role: User, Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if c := f.call(0); c.body["reasoning_effort"] != "high" || c.auth != "" {
		t.Fatalf("call %+v", c)
	}
	if g.Locality() != Local {
		t.Fatal("a local provider was called cloud")
	}
}

func TestTokensAreCountedWithRoomForAnotherTokenizer(t *testing.T) {
	f := newFake(t)
	exact := compatible(t, f, config.Compatible{Kind: "compatible", APIKey: "k", HeavyModel: "m", RoutineModel: "m", TokenFactor: 1})
	padded := compatible(t, f, config.Compatible{Kind: "compatible", APIKey: "k", HeavyModel: "m", RoutineModel: "m", TokenFactor: 1.5})
	text := "kal 6 baje yaad dilana, aur Rahul ko bol dena ki meeting 7 baje hai"
	if want := o200k.Count(text) + framingTokens; exact.CountTokens(text) != want || padded.CountTokens(text) < want*3/2 {
		t.Fatalf("exact %d, padded %d, o200k %d", exact.CountTokens(text), padded.CountTokens(text), want)
	}
}

func TestGroqAsACompatibleKindKeepsItsChecksAndFields(t *testing.T) {
	if _, err := NewCompatible(config.Compatible{Kind: "groq", BaseURL: "https://api.groq.com/openai/v1", HeavyModel: "llama-3.3-70b", RoutineModel: "llama-3.3-70b"}, ClientOptions{}); err == nil {
		t.Fatal("a groq model Kaya cannot count was accepted")
	}
	f := newFake(t, ok("hi"))
	g := compatible(t, f, config.Compatible{Kind: "groq", APIKey: "k", HeavyModel: "openai/gpt-oss-20b", RoutineModel: "openai/gpt-oss-20b", Effort: true, TokenFactor: 1})
	if _, err := g.Complete(bg, Request{Tier: Routine, Messages: []Message{{Role: User, Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if c := f.call(0); c.body["reasoning_effort"] != "low" || c.body["include_reasoning"] != false || g.Name() != "groq" {
		t.Fatalf("call %+v, name %q", c, g.Name())
	}
}

func TestErrorsNameTheProvider(t *testing.T) {
	f := newFake(t, failure(401, "invalid_api_key"))
	g := compatible(t, f, config.Compatible{Kind: "compatible", APIKey: "k", HeavyModel: "m", RoutineModel: "m", TokenFactor: 1})
	_, err := g.Complete(bg, Request{Tier: Heavy, Messages: []Message{{Role: User, Content: "hi"}}})
	var e *Error
	if !errors.As(err, &e) || e.Provider != "127.0.0.1" || e.Code != Auth {
		t.Fatalf("err %v", err)
	}
}

func TestTheRoutineTierCanGoToItsOwnProvider(t *testing.T) {
	heavy, routine := newFake(t, ok("heavy")), newFake(t, ok("routine"))
	p := tiered{
		heavy:   compatible(t, heavy, config.Compatible{Kind: "compatible", APIKey: "a", HeavyModel: "big", RoutineModel: "big", ContextTokens: 6000, TokenFactor: 1}),
		routine: compatible(t, routine, config.Compatible{Kind: "compatible", HeavyModel: "small", RoutineModel: "small", ContextTokens: 4000, Local: true, TokenFactor: 1.5}),
	}
	got, err := p.Complete(bg, Request{Tier: Routine, Messages: []Message{{Role: User, Content: "tag this"}}})
	if err != nil || got.Text != "routine" || routine.call(0).body["model"] != "small" || heavy.count() != 0 {
		t.Fatalf("routine went astray: %q %v", got.Text, err)
	}
	if got, _ := p.Complete(bg, Request{Tier: Heavy, Messages: []Message{{Role: User, Content: "answer"}}}); got.Text != "heavy" {
		t.Fatalf("heavy got %q", got.Text)
	}
	if p.Locality() != Cloud || p.ContextTokens() != 4000 || p.CountTokens("hello there") != p.routine.CountTokens("hello there") {
		t.Fatalf("locality %q, context %d", p.Locality(), p.ContextTokens())
	}
}

func TestNewBuildsTheConfiguredProviders(t *testing.T) {
	p, err := New(config.LLM{
		Provider: "groq", Groq: testGroq,
		Routine: &config.Compatible{Kind: "compatible", BaseURL: "http://ollama:11434/v1", HeavyModel: "qwen3:1.7b", RoutineModel: "qwen3:1.7b", Local: true, TokenFactor: 1.25},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "groq+ollama" || p.Locality() != Cloud {
		t.Fatalf("name %q, locality %q", p.Name(), p.Locality())
	}
	c, err := New(config.LLM{Provider: "compatible", Compatible: config.Compatible{Kind: "compatible", BaseURL: "https://api.openai.com/v1", APIKey: "k", HeavyModel: "m", RoutineModel: "m", TokenFactor: 1}})
	if err != nil || c.Name() != "api.openai.com" {
		t.Fatalf("compatible: %v %v", c, err)
	}
}
