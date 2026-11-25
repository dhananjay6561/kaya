package llm

import (
	"context"
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
)

// Message is one turn of a conversation sent to the model.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
type Request struct {
	Tier        Tier
	Effort      Effort
	Messages    []Message
	MaxTokens   int
	Temperature *float64
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

// Completion is a model's answer.
type Completion struct {
	Text         string
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

var errNoMessages = errors.New("completion needs at least one message")

// New builds the provider config names.
func New(cfg config.LLM) (Provider, error) {
	switch cfg.Provider {
	case "groq":
		return NewGroq(cfg.Groq, GroqOptions{})
	case "stub":
		return NewStub(StubOptions{}), nil
	default:
		return nil, fmt.Errorf("unknown llm provider %q", cfg.Provider)
	}
}

func checkModels(models ...string) error {
	for _, m := range models {
		if !strings.HasPrefix(m, "openai/gpt-oss-") {
			return fmt.Errorf("groq model %q is not supported: Kaya counts tokens for gpt-oss models only, so set GROQ_MODEL_HEAVY and GROQ_MODEL_ROUTINE to openai/gpt-oss-* models", m)
		}
	}
	return nil
}
