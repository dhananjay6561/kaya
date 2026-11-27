package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/memory"
	"github.com/dhananjay6561/kaya/kit/logging"
)

// Privacy hides private details in what goes to a cloud model and puts them back in what comes
// from it. It is the question's own mask session, so placeholders match the rest of the request.
type Privacy interface {
	Mask(text, context string) string
	Restore(text string) string
}

// Options configure a Run.
type Options struct {
	LLM    llm.Provider
	Tools  *Registry
	Logger *slog.Logger
	// Privacy is required for a cloud model: tool arguments have placeholders put back before a
	// tool runs, and results are masked before the model reads them.
	Privacy Privacy
	// Fatal says whether a tool's error must fail the whole question, such as the database being
	// away; any other error is reported to the model.
	Fatal func(err error) bool
	// MaxRounds is how many replies with tool calls are run; the model then has one more to
	// answer. Zero means 3.
	MaxRounds int
	// MaxCalls is how many calls of one reply are run. Zero means 4.
	MaxCalls int
	// ToolTimeout bounds one call. Zero means 10 seconds.
	ToolTimeout time.Duration
	// ResultTokens is the most one result may take; a longer one is shortened. Zero means 500.
	ResultTokens int
}

// Outcomes of a call.
const (
	Ran          = "ran"
	Failed       = "failed"
	Unknown      = "unknown"
	BadArguments = "bad_arguments"
	NotRun       = "not_run"
)

// Call is one tool call the model made and how it went.
type Call struct {
	Tool    string
	Outcome string
	Elapsed time.Duration
}

// Result is the model's answer once it stopped calling tools, with what the calls and every
// round cost.
type Result struct {
	Completion llm.Completion
	Calls      []Call
	Rounds     int
	Usage      llm.Usage
}

// ErrTooManyRounds is a model that kept calling tools after it was told to answer.
var ErrTooManyRounds = errors.New("the model kept calling tools after it was told to answer")

const (
	limitReached = "Not run: no more tools can be used for this message. Answer with what you have."
	nothing      = "(nothing)"
)

func (o *Options) defaults() error {
	switch {
	case o.LLM == nil || o.Tools == nil || o.Logger == nil:
		return errors.New("agent needs a model, tools and a logger")
	case o.LLM.Locality() == llm.Cloud && o.Privacy == nil:
		return errors.New("agent needs masking to give tool results to a cloud model")
	}
	if o.MaxRounds == 0 {
		o.MaxRounds = 3
	}
	if o.MaxCalls == 0 {
		o.MaxCalls = 4
	}
	if o.ToolTimeout == 0 {
		o.ToolTimeout = 10 * time.Second
	}
	if o.ResultTokens == 0 {
		o.ResultTokens = 500
	}
	if o.Fatal == nil {
		o.Fatal = func(error) bool { return false }
	}
	return nil
}

// Run asks the model r with the tools offered, runs the calls it makes and asks again with the
// results, until it answers without calling one. A call the provider could not read is asked
// again once. When ctx ends, or a tool fails with a fatal error, Run stops with that error.
func Run(ctx context.Context, o Options, r llm.Request) (Result, error) {
	if err := o.defaults(); err != nil {
		return Result{}, err
	}
	r.Tools = o.Tools.Specs()
	r.Messages = append([]llm.Message(nil), r.Messages...)
	var res Result
	retried := false
	for round := 0; ; round++ {
		c, err := o.LLM.Complete(ctx, r)
		var failed *llm.Error
		if err != nil && errors.As(err, &failed) && failed.Code == llm.BadToolCall && !retried {
			o.Logger.Warn("the model made a tool call the provider could not read; asking again")
			retried = true
			round--
			continue
		}
		if err != nil {
			return res, err
		}
		res.Usage.InputTokens += c.Usage.InputTokens
		res.Usage.OutputTokens += c.Usage.OutputTokens
		res.Completion = c
		if len(c.ToolCalls) == 0 {
			return res, nil
		}
		if round > o.MaxRounds {
			return res, ErrTooManyRounds
		}
		res.Rounds = round + 1
		r.Messages = append(r.Messages, llm.Message{Role: llm.Assistant, Content: c.Text, ToolCalls: c.ToolCalls})
		for i, call := range c.ToolCalls {
			var content string
			var done Call
			switch {
			case round == o.MaxRounds:
				content, done = limitReached, Call{Tool: call.Name, Outcome: NotRun}
			case i >= o.MaxCalls:
				content = fmt.Sprintf("Not run: at most %d tools can be called at once.", o.MaxCalls)
				done = Call{Tool: call.Name, Outcome: NotRun}
			default:
				if content, done, err = o.run(ctx, call); err != nil {
					return res, err
				}
			}
			o.Logger.Info("the model called a tool", "tool", done.Tool, "outcome", done.Outcome, "ms", done.Elapsed.Milliseconds())
			res.Calls = append(res.Calls, done)
			r.Messages = append(r.Messages, llm.Message{Role: llm.ToolRole, ToolCallID: call.ID, Content: content})
		}
	}
}

// run runs one call and returns what the model is told. It fails only when ctx ends or the tool's
// error is fatal.
func (o *Options) run(ctx context.Context, call llm.ToolCall) (string, Call, error) {
	done := Call{Tool: call.Name}
	tool, ok := o.Tools.tools[call.Name]
	if !ok {
		done.Outcome = Unknown
		return o.out("Error: there is no tool named "+quoted(call.Name)+". The tools are "+strings.Join(o.Tools.Names(), ", ")+".", call), done, nil
	}
	args, problem := o.Tools.check(call.Name, call.Arguments)
	if problem != "" {
		done.Outcome = BadArguments
		return o.out("Error: "+problem+".", call), done, nil
	}
	raw, err := json.Marshal(o.restore(args))
	if err != nil {
		return "", done, err
	}
	started := time.Now()
	text, err := o.call(ctx, tool, raw)
	done.Elapsed = time.Since(started)
	var problemErr *Problem
	switch {
	case err == nil:
		done.Outcome = Ran
	case ctx.Err() != nil:
		return "", done, context.Cause(ctx)
	case o.Fatal(err):
		return "", done, err
	case errors.As(err, &problemErr):
		done.Outcome, text = Failed, "Error: "+problemErr.Message
	case errors.Is(err, context.DeadlineExceeded):
		done.Outcome, text = Failed, "Error: the tool took too long."
	default:
		o.Logger.Warn("a tool failed", "tool", call.Name, logging.Err(err))
		done.Outcome, text = Failed, "Error: the tool failed."
	}
	return o.out(text, call), done, nil
}

func (o *Options) call(ctx context.Context, tool Tool, args json.RawMessage) (text string, err error) {
	ctx, cancel := context.WithTimeout(ctx, o.ToolTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			text, err = "", fmt.Errorf("tool %s panicked: %v", tool.Name, p)
		}
	}()
	return tool.Run(ctx, args)
}

// out shortens a result to its share of the context and masks it for a cloud model.
func (o *Options) out(text string, call llm.ToolCall) string {
	if strings.TrimSpace(text) == "" {
		return nothing
	}
	text = memory.Shorten(text, o.ResultTokens, o.LLM.CountTokens)
	if o.Privacy != nil {
		text = o.Privacy.Mask(text, call.Arguments)
	}
	return text
}

// restore puts private details back into every string of the arguments the model wrote.
func (o *Options) restore(v any) any {
	if o.Privacy == nil {
		return v
	}
	switch v := v.(type) {
	case string:
		return o.Privacy.Restore(v)
	case []any:
		for i := range v {
			v[i] = o.restore(v[i])
		}
	case map[string]any:
		for k := range v {
			v[k] = o.restore(v[k])
		}
	}
	return v
}

func quoted(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
