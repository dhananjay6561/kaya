package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dhananjay6561/kaya/core/internal/config"
)

// Message roles.
const (
	System    = "system"
	User      = "user"
	Assistant = "assistant"
	ToolRole  = "tool"
)

// Message is one turn of a conversation sent to the model. An assistant turn may carry the tools
// the model called, and a tool turn answers one of them by its ID.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is the model asking to run a tool. Arguments is the JSON the model wrote, unchecked.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type wireCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// MarshalJSON writes the call the way OpenAI-compatible APIs take it back.
func (c ToolCall) MarshalJSON() ([]byte, error) {
	w := wireCall{ID: c.ID, Type: "function"}
	w.Function.Name, w.Function.Arguments = c.Name, c.Arguments
	return json.Marshal(w)
}

// ToolSpec offers the model a tool: its name, what it does and a JSON Schema of its arguments.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// MarshalJSON writes the tool the way OpenAI-compatible APIs take it.
func (t ToolSpec) MarshalJSON() ([]byte, error) {
	w := wireTool{Type: "function"}
	w.Function.Name, w.Function.Description, w.Function.Parameters = t.Name, t.Description, t.Parameters
	return json.Marshal(w)
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// Tier picks a model: routine for cheap work, heavy for answers DJ reads.
type Tier string

// Model tiers.
const (
	Routine Tier = "routine"
	Heavy   Tier = "heavy"
)

// Effort is how hard a reasoning model thinks before it answers.
type Effort string

// Reasoning efforts.
const (
	Low    Effort = "low"
	Medium Effort = "medium"
	High   Effort = "high"
)

// Request asks a model for one completion. A zero Effort or MaxTokens takes the tier's default.
// With Schema the answer is JSON that the schema allows. With Tools the model may answer by
// calling them instead; a request cannot have both.
type Request struct {
	Tier        Tier
	Effort      Effort
	Messages    []Message
	MaxTokens   int
	Temperature *float64
	Schema      *Schema
	Tools       []ToolSpec
}

// Schema is a JSON Schema an answer must follow, with a name for it.
type Schema struct {
	Name   string
	Schema json.RawMessage
}

// Why a model stopped. Anything a provider reports beyond these counts as FinishStop.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
)

// Usage is what a completion cost, in tokens.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Completion is a model's answer: text, or tools it wants run, or both.
type Completion struct {
	Text         string
	ToolCalls    []ToolCall
	Model        string
	FinishReason string
	Usage        Usage
}

// Locality says where a provider runs: context sent to a cloud model leaves the machine.
type Locality string

// Localities.
const (
	Cloud Locality = "cloud"
	Local Locality = "local"
)

// Provider is a language model Kaya can ask.
type Provider interface {
	Name() string
	Locality() Locality
	// ContextTokens is how many tokens of context a request may carry.
	ContextTokens() int
	// CountTokens is how many tokens one message takes for this model, its chat framing included.
	CountTokens(text string) int
	Complete(ctx context.Context, r Request) (Completion, error)
}

var (
	errNoMessages      = errors.New("completion needs at least one message")
	errSchemaWithTools = errors.New("a completion cannot follow a schema and offer tools at once")
)

func checkRequest(r Request) error {
	switch {
	case len(r.Messages) == 0:
		return errNoMessages
	case r.Schema != nil && len(r.Tools) > 0:
		return errSchemaWithTools
	}
	return nil
}

// New builds the provider config names, and gives the routine tier its own provider when one
// is set.
func New(cfg config.LLM) (Provider, error) {
	var main Provider
	var err error
	switch cfg.Provider {
	case "groq":
		main, err = NewGroq(cfg.Groq, ClientOptions{})
	case "compatible":
		main, err = NewCompatible(cfg.Compatible, ClientOptions{})
	case "stub":
		main = NewStub(StubOptions{})
	default:
		return nil, fmt.Errorf("unknown llm provider %q", cfg.Provider)
	}
	if err != nil || cfg.Routine == nil {
		return main, err
	}
	routine, err := NewCompatible(*cfg.Routine, ClientOptions{})
	if err != nil {
		return nil, fmt.Errorf("routine provider: %w", err)
	}
	return tiered{heavy: main, routine: routine}, nil
}

func checkModels(models ...string) error {
	for _, m := range models {
		if !strings.HasPrefix(m, "openai/gpt-oss-") {
			return fmt.Errorf("groq model %q is not supported: Kaya counts tokens for gpt-oss models only, so set GROQ_MODEL_HEAVY and GROQ_MODEL_ROUTINE to openai/gpt-oss-* models", m)
		}
	}
	return nil
}
