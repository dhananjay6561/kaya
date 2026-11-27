package llm

import (
	"context"
	"slices"
	"sync"
	"unicode/utf16"
)

// StubOptions configure a stub model. A nil Reply echoes the last user message. Respond, when
// set, answers instead of Reply and may call tools.
type StubOptions struct {
	Reply         func(r Request) string
	Respond       func(r Request) Completion
	ContextTokens int
}

// Stub is a model that answers without calling out, for tests and for running without a key.
type Stub struct {
	reply         func(r Request) string
	respond       func(r Request) Completion
	contextTokens int

	mu       sync.Mutex
	requests []Request
}

// NewStub returns a stub model.
func NewStub(o StubOptions) *Stub {
	s := &Stub{reply: o.Reply, respond: o.Respond, contextTokens: o.ContextTokens}
	if s.reply == nil {
		s.reply = echo
	}
	if s.contextTokens == 0 {
		s.contextTokens = 4000
	}
	return s
}

func echo(r Request) string {
	for _, m := range slices.Backward(r.Messages) {
		if m.Role == User {
			return "stub reply: " + m.Content
		}
	}
	return "stub reply: "
}

// roughTokens is a quarter of the text's length in UTF-16 units, as the TypeScript stub counted.
func roughTokens(text string) int {
	n := 0
	for _, r := range text {
		n += utf16.RuneLen(r)
	}
	return (n + 3) / 4
}

// Name is "stub".
func (s *Stub) Name() string { return "stub" }

// Locality is local: nothing leaves the machine.
func (s *Stub) Locality() Locality { return Local }

// ContextTokens is the stub's budget.
func (s *Stub) ContextTokens() int { return s.contextTokens }

// CountTokens estimates tokens from the text's length.
func (s *Stub) CountTokens(text string) int { return roughTokens(text) + 4 }

// Requests are the requests the stub has answered, oldest first.
func (s *Stub) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Complete answers with the stub's reply.
func (s *Stub) Complete(ctx context.Context, r Request) (Completion, error) {
	if err := ctx.Err(); err != nil {
		return Completion{}, context.Cause(ctx)
	}
	if err := checkRequest(r); err != nil {
		return Completion{}, err
	}
	s.mu.Lock()
	s.requests = append(s.requests, r)
	s.mu.Unlock()
	var c Completion
	if s.respond != nil {
		c = s.respond(r)
	} else {
		c.Text = s.reply(r)
	}
	if c.Model == "" {
		c.Model = "stub-" + string(r.Tier)
	}
	if c.FinishReason == "" {
		c.FinishReason = FinishStop
		if len(c.ToolCalls) > 0 {
			c.FinishReason = FinishToolCalls
		}
	}
	if c.Usage == (Usage{}) {
		for _, m := range r.Messages {
			c.Usage.InputTokens += roughTokens(m.Content)
		}
		c.Usage.OutputTokens = roughTokens(c.Text)
	}
	return c, nil
}
