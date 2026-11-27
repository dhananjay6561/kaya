package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/llm"
)

var bg = context.Background()

type step func(r llm.Request) (llm.Completion, error)

// model answers each request with the next step and keeps the requests.
type model struct {
	locality llm.Locality
	mu       sync.Mutex
	steps    []step
	requests []llm.Request
}

func (m *model) Name() string                { return "fake" }
func (m *model) Locality() llm.Locality      { return m.locality }
func (m *model) ContextTokens() int          { return 4000 }
func (m *model) CountTokens(text string) int { return len(text)/4 + 1 }

func (m *model) Complete(_ context.Context, r llm.Request) (llm.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, r)
	if len(m.steps) == 0 {
		return llm.Completion{}, errors.New("no more steps")
	}
	next := m.steps[0]
	m.steps = m.steps[1:]
	return next(r)
}

func calls(cs ...llm.ToolCall) step {
	return func(llm.Request) (llm.Completion, error) {
		return llm.Completion{ToolCalls: cs, FinishReason: llm.FinishToolCalls, Usage: llm.Usage{InputTokens: 100, OutputTokens: 10}}, nil
	}
}

func answer(text string) step {
	return func(llm.Request) (llm.Completion, error) {
		return llm.Completion{Text: text, FinishReason: llm.FinishStop, Usage: llm.Usage{InputTokens: 120, OutputTokens: 5}}, nil
	}
}

func failing(code string) step {
	return func(llm.Request) (llm.Completion, error) {
		return llm.Completion{}, &llm.Error{Provider: "fake", Code: code}
	}
}

func call(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: args}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var zoneParams = json.RawMessage(`{"type":"object","properties":{"zone":{"type":"string"}},"required":["zone"],"additionalProperties":false}`)

func echoTool(ran *[]string) Tool {
	return Tool{Name: "echo", Description: "Says the zone back.", Parameters: zoneParams,
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			var a struct{ Zone string }
			_ = json.Unmarshal(args, &a)
			*ran = append(*ran, a.Zone)
			return "it is noon in " + a.Zone, nil
		}}
}

func registry(t *testing.T, tools ...Tool) *Registry {
	t.Helper()
	r, err := NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func toolMessages(r llm.Request) []llm.Message {
	var out []llm.Message
	for _, m := range r.Messages {
		if m.Role == llm.ToolRole {
			out = append(out, m)
		}
	}
	return out
}

var question = llm.Request{Tier: llm.Heavy, Messages: []llm.Message{{Role: llm.System, Content: "be Kaya"}, {Role: llm.User, Content: "time in tokyo?"}}}

func TestTheModelCallsAToolAndAnswersWithItsResult(t *testing.T) {
	var ran []string
	m := &model{locality: llm.Local, steps: []step{calls(call("c1", "echo", `{"zone":"Asia/Tokyo"}`)), answer("noon in Tokyo")}}
	res, err := Run(bg, Options{LLM: m, Tools: registry(t, echoTool(&ran)), Logger: quiet}, question)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completion.Text != "noon in Tokyo" || res.Rounds != 1 || len(res.Calls) != 1 || res.Calls[0].Outcome != Ran {
		t.Fatalf("result %+v", res)
	}
	if res.Usage != (llm.Usage{InputTokens: 220, OutputTokens: 15}) {
		t.Fatalf("usage %+v", res.Usage)
	}
	if len(ran) != 1 || ran[0] != "Asia/Tokyo" || len(m.requests) != 2 {
		t.Fatalf("ran %v in %d requests", ran, len(m.requests))
	}
	for _, r := range m.requests {
		if len(r.Tools) != 1 || r.Tools[0].Name != "echo" {
			t.Fatalf("tools offered %+v", r.Tools)
		}
	}
	second := m.requests[1].Messages
	if len(second) != 4 || second[2].Role != llm.Assistant || len(second[2].ToolCalls) != 1 || second[2].ToolCalls[0].ID != "c1" {
		t.Fatalf("second request %+v", second)
	}
	if second[3].Role != llm.ToolRole || second[3].ToolCallID != "c1" || second[3].Content != "it is noon in Asia/Tokyo" {
		t.Fatalf("tool message %+v", second[3])
	}
	if len(question.Messages) != 2 {
		t.Fatal("the caller's messages were changed")
	}
}

func TestAnAnswerWithoutCallsIsReturnedAsItIs(t *testing.T) {
	m := &model{locality: llm.Local, steps: []step{answer("hi")}}
	res, err := Run(bg, Options{LLM: m, Tools: registry(t, echoTool(new([]string))), Logger: quiet}, question)
	if err != nil || res.Completion.Text != "hi" || res.Rounds != 0 || len(res.Calls) != 0 || len(m.requests) != 1 {
		t.Fatalf("result %+v, %v", res, err)
	}
}

func TestTheModelIsToldWhatWentWrongWithACall(t *testing.T) {
	tools := []Tool{
		echoTool(new([]string)),
		{Name: "picky", Description: "Fails with a reason.", Parameters: json.RawMessage(`{"type":"object"}`),
			Run: func(context.Context, json.RawMessage) (string, error) {
				return "", Problemf("unknown zone %q", "Mars/Base")
			}},
		{Name: "broken", Description: "Fails inside.", Parameters: json.RawMessage(`{"type":"object"}`),
			Run: func(context.Context, json.RawMessage) (string, error) { return "", errors.New("pq: secret internals") }},
		{Name: "slow", Description: "Never ends.", Parameters: json.RawMessage(`{"type":"object"}`),
			Run: func(ctx context.Context, _ json.RawMessage) (string, error) { <-ctx.Done(); return "", ctx.Err() }},
		{Name: "panics", Description: "Panics.", Parameters: json.RawMessage(`{"type":"object"}`),
			Run: func(context.Context, json.RawMessage) (string, error) { panic("boom") }},
		{Name: "empty", Description: "Says nothing.", Parameters: json.RawMessage(`{"type":"object"}`),
			Run: func(context.Context, json.RawMessage) (string, error) { return " ", nil }},
	}
	cases := []struct {
		call    llm.ToolCall
		outcome string
		want    string
	}{
		{call("a", "send_money", `{}`), Unknown, `Error: there is no tool named "send_money". The tools are broken, echo, empty, panics, picky, slow.`},
		{call("b", "echo", `{"zone":`), BadArguments, "Error: the arguments are not valid JSON."},
		{call("c", "echo", `{"zone":5}`), BadArguments, "Error: the arguments do not fit the parameters: /zone: got number, want string."},
		{call("d", "echo", ``), BadArguments, "Error: the arguments do not fit the parameters: /: missing property 'zone'."},
		{call("e", "picky", `{}`), Failed, `Error: unknown zone "Mars/Base"`},
		{call("f", "broken", `{}`), Failed, "Error: the tool failed."},
		{call("g", "slow", `{}`), Failed, "Error: the tool took too long."},
		{call("h", "panics", ``), Failed, "Error: the tool failed."},
		{call("i", "empty", `{}`), Ran, "(nothing)"},
	}
	for _, c := range cases {
		m := &model{locality: llm.Local, steps: []step{calls(c.call), answer("ok")}}
		res, err := Run(bg, Options{LLM: m, Tools: registry(t, tools...), Logger: quiet, ToolTimeout: 20 * time.Millisecond}, question)
		if err != nil {
			t.Fatalf("%s: %v", c.call.Name, err)
		}
		got := toolMessages(m.requests[1])
		if len(got) != 1 || got[0].Content != c.want || res.Calls[0].Outcome != c.outcome {
			t.Errorf("%s %s: told %q (%s), want %q (%s)", c.call.Name, c.call.Arguments, got[0].Content, res.Calls[0].Outcome, c.want, c.outcome)
		}
	}
}

func TestAFatalToolErrorOrTheCallerGivingUpStopsTheRun(t *testing.T) {
	away := errors.New("database away")
	tool := Tool{Name: "reads", Description: "Reads the database.", Parameters: json.RawMessage(`{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) { return "", away }}
	m := &model{locality: llm.Local, steps: []step{calls(call("a", "reads", `{}`)), answer("unused")}}
	_, err := Run(bg, Options{LLM: m, Tools: registry(t, tool), Logger: quiet, Fatal: func(err error) bool { return errors.Is(err, away) }}, question)
	if !errors.Is(err, away) || len(m.requests) != 1 {
		t.Fatalf("err = %v after %d requests", err, len(m.requests))
	}

	stopped := errors.New("stopped")
	ctx, cancel := context.WithCancelCause(bg)
	tool.Run = func(context.Context, json.RawMessage) (string, error) {
		cancel(stopped)
		return "", errors.New("cancelled")
	}
	m = &model{locality: llm.Local, steps: []step{calls(call("a", "reads", `{}`)), answer("unused")}}
	if _, err := Run(ctx, Options{LLM: m, Tools: registry(t, tool), Logger: quiet}, question); !errors.Is(err, stopped) {
		t.Fatalf("err = %v", err)
	}
}

func TestToolsRunAFewRoundsThenTheModelMustAnswer(t *testing.T) {
	var ran []string
	again := calls(call("x", "echo", `{"zone":"UTC"}`))
	m := &model{locality: llm.Local, steps: []step{again, again, again, again, answer("done")}}
	res, err := Run(bg, Options{LLM: m, Tools: registry(t, echoTool(&ran)), Logger: quiet}, question)
	if err != nil || res.Completion.Text != "done" || len(ran) != 3 || res.Rounds != 4 {
		t.Fatalf("result %+v, ran %d, %v", res, len(ran), err)
	}
	if last := toolMessages(m.requests[4]); last[len(last)-1].Content != limitReached || res.Calls[3].Outcome != NotRun {
		t.Fatalf("last result %q", last[len(last)-1].Content)
	}

	m = &model{locality: llm.Local, steps: []step{again, again, again, again, again, answer("unused")}}
	if _, err := Run(bg, Options{LLM: m, Tools: registry(t, echoTool(new([]string))), Logger: quiet}, question); !errors.Is(err, ErrTooManyRounds) || len(m.requests) != 5 {
		t.Fatalf("err = %v after %d requests", err, len(m.requests))
	}
}

func TestOnlyAFewCallsOfOneReplyRun(t *testing.T) {
	var ran []string
	var many []llm.ToolCall
	for i := range 6 {
		many = append(many, call(string(rune('a'+i)), "echo", `{"zone":"UTC"}`))
	}
	m := &model{locality: llm.Local, steps: []step{calls(many...), answer("ok")}}
	if _, err := Run(bg, Options{LLM: m, Tools: registry(t, echoTool(&ran)), Logger: quiet}, question); err != nil {
		t.Fatal(err)
	}
	told := toolMessages(m.requests[1])
	if len(ran) != 4 || len(told) != 6 || told[5].ToolCallID != "f" || !strings.HasPrefix(told[5].Content, "Not run: at most 4") {
		t.Fatalf("ran %d, told %+v", len(ran), told)
	}
}

func TestACallTheProviderCouldNotReadIsAskedAgainOnce(t *testing.T) {
	m := &model{locality: llm.Local, steps: []step{failing(llm.BadToolCall), answer("fine")}}
	res, err := Run(bg, Options{LLM: m, Tools: registry(t, echoTool(new([]string))), Logger: quiet}, question)
	if err != nil || res.Completion.Text != "fine" {
		t.Fatalf("result %+v, %v", res, err)
	}
	m = &model{locality: llm.Local, steps: []step{failing(llm.BadToolCall), failing(llm.BadToolCall), answer("unused")}}
	_, err = Run(bg, Options{LLM: m, Tools: registry(t, echoTool(new([]string))), Logger: quiet}, question)
	var failed *llm.Error
	if !errors.As(err, &failed) || failed.Code != llm.BadToolCall || len(m.requests) != 2 {
		t.Fatalf("err = %v after %d requests", err, len(m.requests))
	}
	m = &model{locality: llm.Local, steps: []step{failing(llm.RateLimited), answer("unused")}}
	if _, err := Run(bg, Options{LLM: m, Tools: registry(t, echoTool(new([]string))), Logger: quiet}, question); !errors.As(err, &failed) || len(m.requests) != 1 {
		t.Fatalf("err = %v after %d requests", err, len(m.requests))
	}
}

// privacy hides one number behind one placeholder.
type privacy struct{}

func (privacy) Mask(text, _ string) string {
	return strings.ReplaceAll(text, "+91 98765 43210", "[PHONE_1]")
}
func (privacy) Restore(text string) string {
	return strings.ReplaceAll(text, "[PHONE_1]", "+91 98765 43210")
}

func TestACloudModelNeverSeesTheRealDetailsButTheToolDoes(t *testing.T) {
	var got string
	lookup := Tool{Name: "lookup", Description: "Looks a number up.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"numbers":{"type":"array","items":{"type":"string"}}}}`),
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			got = string(args)
			return "+91 98765 43210 is Rahul. " + strings.Repeat("filler ", 2000), nil
		}}
	m := &model{locality: llm.Cloud, steps: []step{calls(call("a", "lookup", `{"numbers":["[PHONE_1]"]}`)), answer("ok")}}
	if _, err := Run(bg, Options{LLM: m, Tools: registry(t, lookup), Logger: quiet}, question); err == nil {
		t.Fatal("a cloud model was given results without masking")
	}
	m.steps = m.steps[:2]
	if _, err := Run(bg, Options{LLM: m, Tools: registry(t, lookup), Logger: quiet, Privacy: privacy{}}, question); err != nil {
		t.Fatal(err)
	}
	if got != `{"numbers":["+91 98765 43210"]}` {
		t.Fatalf("tool got %s", got)
	}
	told := toolMessages(m.requests[1])[0].Content
	if strings.Contains(told, "98765") || !strings.HasPrefix(told, "[PHONE_1] is Rahul.") || m.CountTokens(told) > 500 {
		t.Fatalf("model was told %d tokens: %.80q", m.CountTokens(told), told)
	}
}

func TestTheRegistryRefusesToolsItCannotOffer(t *testing.T) {
	run := func(context.Context, json.RawMessage) (string, error) { return "", nil }
	obj := json.RawMessage(`{"type":"object"}`)
	for name, tools := range map[string][]Tool{
		"bad name":        {{Name: "Send Money", Description: "x", Parameters: obj, Run: run}},
		"no description":  {{Name: "a", Description: " ", Parameters: obj, Run: run}},
		"no run":          {{Name: "a", Description: "x", Parameters: obj}},
		"not an object":   {{Name: "a", Description: "x", Parameters: json.RawMessage(`{"type":"string"}`), Run: run}},
		"not a schema":    {{Name: "a", Description: "x", Parameters: json.RawMessage(`{"type":"object","properties":5}`), Run: run}},
		"not json":        {{Name: "a", Description: "x", Parameters: json.RawMessage(`{`), Run: run}},
		"same name twice": {{Name: "a", Description: "x", Parameters: obj, Run: run}, {Name: "a", Description: "y", Parameters: obj, Run: run}},
		"acts for DJ":     {{Name: "send", Description: "x", Parameters: obj, Run: run, Outward: true}},
	} {
		if _, err := NewRegistry(tools...); err == nil {
			t.Errorf("%s: registered", name)
		}
	}
	if _, err := NewRegistry(Tool{Name: "send", Description: "x", Parameters: obj, Run: run, Outward: true}); !errors.Is(err, ErrOutward) {
		t.Fatalf("err = %v", err)
	}
	r := registry(t, echoTool(new([]string)), Tool{Name: "a_clock", Description: "x", Parameters: obj, Run: run})
	if specs := r.Specs(); len(specs) != 2 || specs[0].Name != "echo" || specs[1].Name != "a_clock" {
		t.Fatalf("specs %+v", specs)
	}
	if names := r.Names(); strings.Join(names, ",") != "a_clock,echo" {
		t.Fatalf("names %v", names)
	}
	if n := r.Tokens(func(s string) int { return len(s) }); n < 100 {
		t.Fatalf("tokens %d", n)
	}
	if n := registry(t).Tokens(func(s string) int { return len(s) }); n != 0 {
		t.Fatalf("no tools cost %d", n)
	}
}
