package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/core/internal/llm"
)

// VoyageOptions tune the Voyage client. Zero values take the defaults.
type VoyageOptions struct {
	BaseURL string
	Timeout time.Duration
	// MaxRetries is how many times a call that failed because Voyage was down or slow is tried
	// again: 0 takes the default of 2, and a negative number means never. A rate limit is not
	// retried here: it goes back to the caller with Voyage's Retry-After, so the caller can wait and
	// send less.
	MaxRetries   int
	RetryBase    time.Duration
	MaxRetryWait time.Duration
	Client       *http.Client
	Now          func() time.Time
}

const (
	voyageMaxBatch   = 128
	maxVoyageAnswer  = 64 << 20
	maxVoyageProblem = 64 << 10
)

// Voyage embeds text with Voyage AI's API.
type Voyage struct {
	apiKey string
	model  string
	o      VoyageOptions
}

// NewVoyage returns a Voyage provider.
func NewVoyage(cfg config.Voyage, o VoyageOptions) *Voyage {
	if o.BaseURL == "" {
		o.BaseURL = "https://api.voyageai.com/v1"
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.Timeout == 0 {
		o.Timeout = time.Minute
	}
	switch {
	case o.MaxRetries == 0:
		o.MaxRetries = 2
	case o.MaxRetries < 0:
		o.MaxRetries = 0
	}
	if o.RetryBase == 0 {
		o.RetryBase = 2 * time.Second
	}
	if o.MaxRetryWait == 0 {
		o.MaxRetryWait = time.Minute
	}
	if o.Client == nil {
		o.Client = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Voyage{apiKey: cfg.APIKey, model: cfg.Model, o: o}
}

// Name is "voyage".
func (*Voyage) Name() string { return "voyage" }

// Model is VOYAGE_MODEL.
func (v *Voyage) Model() string { return v.model }

// Locality is cloud: what is sent leaves the machine.
func (*Voyage) Locality() llm.Locality { return llm.Cloud }

// MaxBatch is 128 texts.
func (*Voyage) MaxBatch() int { return voyageMaxBatch }

type voyageRequest struct {
	Input           []string `json:"input"`
	Model           string   `json:"model"`
	InputType       Kind     `json:"input_type"`
	OutputDimension int      `json:"output_dimension"`
}

// Embed asks Voyage for one vector per text, trying again while Voyage is down or slow.
func (v *Voyage) Embed(ctx context.Context, texts []string, kind Kind) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, context.Cause(ctx)
	}
	if len(texts) == 0 {
		return Result{}, nil
	}
	if len(texts) > voyageMaxBatch {
		return Result{}, errTooMany
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(voyageRequest{Input: texts, Model: v.model, InputType: kind, OutputDimension: Dimensions}); err != nil {
		return Result{}, err
	}
	for attempt := 0; ; attempt++ {
		result, err := v.once(ctx, body.Bytes(), len(texts))
		var failed *Error
		if err == nil || !errors.As(err, &failed) || attempt >= v.o.MaxRetries ||
			failed.Code != Unavailable && failed.Code != Timeout {
			return result, err
		}
		delay := v.o.RetryBase << attempt
		if delay > v.o.MaxRetryWait {
			return result, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return Result{}, context.Cause(ctx)
		}
	}
}

func (v *Voyage) once(ctx context.Context, body []byte, n int) (Result, error) {
	attempt, cancel := context.WithTimeout(ctx, v.o.Timeout)
	defer cancel()
	stopped := func() error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if attempt.Err() != nil {
			return &Error{Provider: "voyage", Code: Timeout}
		}
		return nil
	}
	req, err := http.NewRequestWithContext(attempt, http.MethodPost, v.o.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+v.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.o.Client.Do(req)
	if err != nil {
		if reason := stopped(); reason != nil {
			return Result{}, reason
		}
		return Result{}, &Error{Provider: "voyage", Code: Unavailable}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxVoyageProblem))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{Provider: "voyage", Status: resp.StatusCode}
		switch status := resp.StatusCode; {
		case status == http.StatusTooManyRequests:
			e.Code = RateLimited
			e.RetryAfter = v.retryAfter(resp.Header.Get("Retry-After"))
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			e.Code = Auth
		case status >= 500:
			e.Code = Unavailable
		default:
			e.Code = Rejected
		}
		return Result{}, e
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxVoyageAnswer))
	if err != nil {
		if reason := stopped(); reason != nil {
			return Result{}, reason
		}
	}
	result, ok := decodeVoyage(raw, n)
	if !ok {
		return Result{}, &Error{Provider: "voyage", Code: BadResponse, Status: resp.StatusCode}
	}
	return result, nil
}

// decodeVoyage reads exactly one vector of the right size for each text, in any order.
func decodeVoyage(raw []byte, n int) (Result, bool) {
	var answer struct {
		Data []*struct {
			Index     *int      `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil || len(answer.Data) != n {
		return Result{}, false
	}
	vectors := make([][]float32, n)
	for _, item := range answer.Data {
		if item == nil || item.Index == nil || *item.Index < 0 || *item.Index >= n || vectors[*item.Index] != nil ||
			len(item.Embedding) != Dimensions {
			return Result{}, false
		}
		vectors[*item.Index] = item.Embedding
	}
	return Result{Vectors: vectors, Tokens: answer.Usage.TotalTokens}, true
}

func (v *Voyage) retryAfter(header string) *time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil
	}
	var d time.Duration
	if seconds, err := strconv.ParseFloat(header, 64); err == nil && !math.IsInf(seconds, 0) && !math.IsNaN(seconds) {
		if seconds*float64(time.Second) >= math.MaxInt64 {
			d = math.MaxInt64
		} else {
			d = time.Duration(seconds * float64(time.Second))
		}
	} else if at, err := http.ParseTime(header); err == nil {
		d = at.Sub(v.o.Now())
	} else {
		return nil
	}
	d = max(d, 0)
	return &d
}
