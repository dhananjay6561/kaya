package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/dhananjay6561/kaya/core/internal/config"
)

func TestStubAnswersFromTheLastUserMessageAndReportsTheTierAndUsage(t *testing.T) {
	s := NewStub(StubOptions{})
	got, err := s.Complete(bg, Request{Tier: Routine, Messages: []Message{
		{Role: System, Content: "be brief"},
		{Role: User, Content: "first"},
		{Role: Assistant, Content: "ok"},
		{Role: User, Content: "what did Rahul say?"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "stub reply: what did Rahul say?" || got.Model != "stub-routine" || got.FinishReason != FinishStop {
		t.Fatalf("got %+v", got)
	}
	if got.Usage.InputTokens <= 0 || got.Usage.OutputTokens <= 0 || len(s.Requests()) != 1 {
		t.Fatalf("usage %+v, %d requests", got.Usage, len(s.Requests()))
	}
}

func TestStubUsesACustomReply(t *testing.T) {
	s := NewStub(StubOptions{Reply: func(r Request) string { return "tier " + string(r.Tier) }})
	got, err := s.Complete(bg, Request{Tier: Heavy, Messages: []Message{{Role: User, Content: "x"}}})
	if err != nil || got.Text != "tier heavy" || got.Model != "stub-heavy" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestStubRejectsAnEmptyConversationAndAStoppedCall(t *testing.T) {
	if _, err := NewStub(StubOptions{}).Complete(bg, Request{Tier: Routine}); err == nil {
		t.Fatal("an empty conversation was answered")
	}
	stopped := errors.New("stopped")
	ctx, cancel := context.WithCancelCause(bg)
	cancel(stopped)
	if _, err := NewStub(StubOptions{}).Complete(ctx, Request{Tier: Routine, Messages: []Message{{Role: User, Content: "x"}}}); !errors.Is(err, stopped) {
		t.Fatalf("err = %v", err)
	}
}

func TestStubCountsLikeTheTypeScriptStub(t *testing.T) {
	s := NewStub(StubOptions{})
	for text, want := range map[string]int{"": 4, "abcd": 5, "abcde": 6, "😀": 5, "😀😀😀": 6, "क्ष": 5} {
		if got := s.CountTokens(text); got != want {
			t.Errorf("CountTokens(%q) = %d, want %d", text, got, want)
		}
	}
}

func TestNewBuildsTheProviderConfigNames(t *testing.T) {
	stub, err := New(config.LLM{Provider: "stub"})
	if err != nil || stub.Name() != "stub" || stub.Locality() != Local || stub.ContextTokens() != 4000 {
		t.Fatalf("stub: %v", err)
	}
	groq, err := New(config.LLM{Provider: "groq", Groq: testGroq})
	if err != nil || groq.Name() != "groq" || groq.Locality() != Cloud || groq.ContextTokens() != 4000 {
		t.Fatalf("groq: %v", err)
	}
	if _, err := New(config.LLM{Provider: "other"}); err == nil {
		t.Fatal("an unknown provider was built")
	}
}
