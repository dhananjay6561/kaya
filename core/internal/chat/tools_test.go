package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dhananjay6561/kaya/core/internal/agent"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/memory"
)

func lookupTool(got *[]string, result string, err error) *agent.Registry {
	r, e := agent.NewRegistry(agent.Tool{
		Name: "lookup", Description: "Looks a number up.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"number":{"type":"string"}},"required":["number"]}`),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			*got = append(*got, string(args))
			return result, err
		},
	})
	if e != nil {
		panic(e)
	}
	return r
}

func toolMessage(r llm.Request) string {
	for _, m := range r.Messages {
		if m.Role == llm.ToolRole {
			return m.Content
		}
	}
	return ""
}

func lookupThenAnswer(args string) func(r llm.Request) (llm.Completion, error) {
	return func(r llm.Request) (llm.Completion, error) {
		if toolMessage(r) == "" {
			return llm.Completion{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "lookup", Arguments: args}}, FinishReason: llm.FinishToolCalls}, nil
		}
		return llm.Completion{Text: "It is " + toolMessage(r), FinishReason: llm.FinishStop}, nil
	}
}

func TestReplyCallsAToolAndAnswersFromItsResult(t *testing.T) {
	var ran []string
	model := &scripted{responses: lookupThenAnswer(`{"number":"42"}`)}
	c := newChat(t, Options{LLM: model, Tools: lookupTool(&ran, "Rahul's", nil)})
	got := reply(t, c, "whose number is 42?")
	if got.Text != "It is Rahul's" || !got.Remember || len(ran) != 1 || ran[0] != `{"number":"42"}` || len(model.requests) != 2 {
		t.Fatalf("got %+v, ran %v, %d requests", got, ran, len(model.requests))
	}
	for _, r := range model.requests {
		if len(r.Tools) != 1 || r.Tools[0].Name != "lookup" || r.Tier != llm.Heavy {
			t.Fatalf("request %+v", r)
		}
	}
}

func TestReplyLeavesRoomForTheToolsInTheBudget(t *testing.T) {
	var past []memory.Exchange
	for range 40 {
		past = append(past, memory.Exchange{Question: strings.Repeat("q", 1500), Answer: strings.Repeat("a", 1500), At: now})
	}
	tools := lookupTool(new([]string), "", nil)
	model := roomy{&scripted{responses: func(llm.Request) (llm.Completion, error) {
		return llm.Completion{Text: "ok", FinishReason: llm.FinishStop}, nil
	}}}
	reply(t, newChat(t, Options{LLM: model, History: history(past...), Tools: tools}), "hi")
	reply(t, newChat(t, Options{LLM: model, History: history(past...)}), "hi")
	withTools, without := model.requests[0].Messages, model.requests[1].Messages
	if len(withTools) >= len(without) {
		t.Fatalf("%d messages with tools, %d without", len(withTools), len(without))
	}
	sent := tools.Tokens(model.CountTokens)
	for _, m := range withTools {
		sent += model.CountTokens(m.Content)
	}
	if room := model.ContextTokens() / 8; sent > model.ContextTokens()-room {
		t.Fatalf("sent %d tokens with a budget of %d, leaving less than %d for tool results", sent, model.ContextTokens(), room)
	}
}

func TestReplyGivesUpOnAModelThatKeepsCallingTools(t *testing.T) {
	model := &scripted{responses: func(llm.Request) (llm.Completion, error) {
		return llm.Completion{ToolCalls: []llm.ToolCall{{ID: "c", Name: "lookup", Arguments: `{"number":"1"}`}}, FinishReason: llm.FinishToolCalls}, nil
	}}
	got := reply(t, newChat(t, Options{LLM: model, Tools: lookupTool(new([]string), "x", nil)}), "loop")
	if got.Text != goingInCircles || got.Remember {
		t.Fatalf("got %+v", got)
	}
}

func TestReplyFailsWhenAToolFindsTheDatabaseAway(t *testing.T) {
	away := errors.New("database away")
	model := &scripted{responses: lookupThenAnswer(`{"number":"1"}`)}
	c := newChat(t, Options{LLM: model, Tools: lookupTool(new([]string), "", away), DatabaseAway: func(err error) bool { return errors.Is(err, away) }})
	if _, err := c.Reply(bg, q("whose?")); !errors.Is(err, away) {
		t.Fatalf("err = %v", err)
	}
}

func TestReplyGivesAToolTheRealDetailsAndTheCloudModelOnlyPlaceholders(t *testing.T) {
	var ran []string
	model := &scripted{cloud: true, responses: lookupThenAnswer(`{"number":"[PHONE_1]"}`)}
	c := newChat(t, Options{
		LLM: model, NewMaskSession: realMasking, Tools: lookupTool(&ran, "9876543210 is Rahul, mail rahul@x.com", nil),
		History: history(memory.Exchange{Question: "what's that number?", Answer: "It is 9876543210", At: now}),
	})
	got := reply(t, c, "whose is it?")
	if len(ran) != 1 || ran[0] != `{"number":"9876543210"}` {
		t.Fatalf("tool got %v", ran)
	}
	told := toolMessage(model.requests[1])
	if told != "[PHONE_1] is Rahul, mail [EMAIL_1]" {
		t.Fatalf("model was told %q", told)
	}
	if got.Text != "It is 9876543210 is Rahul, mail rahul@x.com" {
		t.Fatalf("got %q", got.Text)
	}
}
