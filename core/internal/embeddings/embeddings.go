// Package embeddings turns text into vectors for the search index, behind a provider interface.
package embeddings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/core/internal/llm"
)

// Dimensions is the size of every vector: the size of the search index's column.
const Dimensions = 1024

// Kind says what a text is for: a document is stored and searched, a query is searched with.
type Kind string

// Kinds of text.
const (
	Document Kind = "document"
	Query    Kind = "query"
)

// Result is the vectors for a list of texts, in the same order, and the tokens the call used.
type Result struct {
	Vectors [][]float32
	Tokens  int
}

// Provider turns text into vectors.
type Provider interface {
	Name() string
	// Model names the model, stored with each vector: vectors from different models cannot be compared.
	Model() string
	Locality() llm.Locality
	// MaxBatch is the most texts one call may carry.
	MaxBatch() int
	Embed(ctx context.Context, texts []string, kind Kind) (Result, error)
}

// Error codes a provider reports.
const (
	RateLimited = "rate_limited"
	Timeout     = "timeout"
	Unavailable = "unavailable"
	Auth        = "auth"
	// Misconfigured means the request itself is wrong (an unknown model, a wrong address), so
	// every text would fail the same way.
	Misconfigured = "misconfigured"
	Rejected      = "rejected"
	BadResponse   = "bad_response"
)

// Error says why texts could not be embedded. It never carries the texts or the provider's message.
type Error struct {
	Provider string
	Code     string
	Status   int
	// RetryAfter is how long the provider asked to wait, or nil when it did not say.
	RetryAfter *time.Duration
}

func (e *Error) Error() string {
	msg := e.Provider + " could not embed: " + e.Code
	if e.Status != 0 {
		msg += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	return msg
}

// ErrorCode is the short code that may be stored.
func (e *Error) ErrorCode() string { return e.Code }

var errTooMany = errors.New("too many texts for one call")

// New builds the provider config names.
func New(cfg config.Embeddings) (Provider, error) {
	switch cfg.Provider {
	case "voyage":
		return NewVoyage(cfg.Voyage, VoyageOptions{}), nil
	case "stub":
		return NewStub(), nil
	default:
		return nil, fmt.Errorf("unknown embeddings provider %q", cfg.Provider)
	}
}
