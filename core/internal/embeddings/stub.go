package embeddings

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"

	"github.com/dhananjay6561/kaya/core/internal/llm"
)

// Stub makes vectors locally from the words in a text, so texts that share words point the same
// way. It is for tests and for running without a key.
type Stub struct{}

// NewStub returns the stub provider.
func NewStub() *Stub { return &Stub{} }

// Name is "stub".
func (*Stub) Name() string { return "stub" }

// Model is "stub-words".
func (*Stub) Model() string { return "stub-words" }

// Locality is local.
func (*Stub) Locality() llm.Locality { return llm.Local }

// MaxBatch is 128.
func (*Stub) MaxBatch() int { return 128 }

// Embed hashes each word of each text into the vector and scales it to length 1.
func (*Stub) Embed(ctx context.Context, texts []string, _ Kind) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, context.Cause(ctx)
	}
	if len(texts) > 128 {
		return Result{}, errTooMany
	}
	out := Result{Vectors: make([][]float32, len(texts))}
	for i, text := range texts {
		v := make([]float32, Dimensions)
		words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		for _, w := range words {
			h := fnv.New32a()
			_, _ = h.Write([]byte(w))
			v[h.Sum32()%Dimensions]++
		}
		if len(words) == 0 {
			v[0] = 1
		}
		var norm float64
		for _, x := range v {
			norm += float64(x * x)
		}
		scale := float32(1 / math.Sqrt(norm))
		for j := range v {
			v[j] *= scale
		}
		out.Vectors[i] = v
		out.Tokens += len(words)
	}
	return out, nil
}
