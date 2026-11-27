package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/config"
	"github.com/dhananjay6561/kaya/core/internal/llm/o200k"
	"github.com/dhananjay6561/kaya/core/internal/text"
)

// GroqOptions tune the Groq client. Zero values take the defaults.
type GroqOptions struct {
	BaseURL string
	// Timeout bounds one attempt; Deadline bounds a whole Complete call, retries included.
	Timeout  time.Duration
	Deadline time.Duration
	// MaxRetries is how many times a busy, failing or slow call is tried again: 0 takes the
	// default of 2, and a negative number means never.
	MaxRetries   int
	RetryBase    time.Duration
	MaxRetryWait time.Duration
	Client       *http.Client
	Now          func() time.Time
}

var utf8BOM = []byte("\xef\xbb\xbf")

const (
	framingTokens   = 4
	maxAnswerBytes  = 1 << 20
	maxProblemBytes = 64 << 10
)

var (
	defaultEffort    = map[Tier]Effort{Routine: Low, Heavy: Medium}
	defaultMaxTokens = map[Tier]int{Routine: 1024, Heavy: 8192}
	finishReasons    = []string{FinishStop, FinishLength, FinishToolCalls, FinishContentFilter}
	safeDetail       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,60}$`)
	tooLongDetail    = regexp.MustCompile(`context_length|too_large|too_long|max_tokens`)
)

// Groq asks gpt-oss models on Groq through its OpenAI-compatible API.
type Groq struct {
	apiKey        string
	models        map[Tier]string
	contextTokens int
	o             GroqOptions
}

// NewGroq returns a Groq provider. Both models must be gpt-oss models, since tokens are counted
// with their tokenizer.
func NewGroq(cfg config.Groq, o GroqOptions) (*Groq, error) {
	if err := checkModels(cfg.HeavyModel, cfg.RoutineModel); err != nil {
		return nil, err
	}
	if o.BaseURL == "" {
		o.BaseURL = "https://api.groq.com/openai/v1"
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	o.Timeout = orDuration(o.Timeout, 30*time.Second)
	o.Deadline = orDuration(o.Deadline, 45*time.Second)
	o.RetryBase = orDuration(o.RetryBase, time.Second)
	o.MaxRetryWait = orDuration(o.MaxRetryWait, 10*time.Second)
	switch {
	case o.MaxRetries == 0:
		o.MaxRetries = 2
	case o.MaxRetries < 0:
		o.MaxRetries = 0
	}
	if o.Client == nil {
		o.Client = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	o200k.Load()
	return &Groq{
		apiKey:        cfg.APIKey,
		models:        map[Tier]string{Routine: cfg.RoutineModel, Heavy: cfg.HeavyModel},
		contextTokens: cfg.ContextTokens,
		o:             o,
	}, nil
}

func orDuration(d, fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return d
}

// Name is "groq".
func (g *Groq) Name() string { return "groq" }

// Locality is cloud: what is sent leaves the machine.
func (g *Groq) Locality() Locality { return Cloud }

// ContextTokens is GROQ_CONTEXT_TOKENS.
func (g *Groq) ContextTokens() int { return g.contextTokens }

// CountTokens counts text with the gpt-oss tokenizer, plus the tokens its chat framing adds.
func (g *Groq) CountTokens(text string) int { return o200k.Count(text) + framingTokens }

type groqRequest struct {
	Model            string     `json:"model"`
	Messages         []Message  `json:"messages"`
	MaxTokens        int        `json:"max_completion_tokens"`
	Temperature      *float64   `json:"temperature,omitempty"`
	ReasoningEffort  Effort     `json:"reasoning_effort"`
	IncludeReasoning bool       `json:"include_reasoning"`
	ResponseFormat   *format    `json:"response_format,omitempty"`
	Tools            []ToolSpec `json:"tools,omitempty"`
}

type format struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

func (g *Groq) body(r Request) ([]byte, error) {
	req := groqRequest{
		Model:           g.models[r.Tier],
		Messages:        r.Messages,
		MaxTokens:       r.MaxTokens,
		Temperature:     r.Temperature,
		ReasoningEffort: r.Effort,
		Tools:           r.Tools,
	}
	if r.Schema != nil {
		req.ResponseFormat = &format{Type: "json_schema", JSONSchema: jsonSchema{Name: r.Schema.Name, Strict: true, Schema: r.Schema.Schema}}
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = defaultMaxTokens[r.Tier]
	}
	if req.ReasoningEffort == "" {
		req.ReasoningEffort = defaultEffort[r.Tier]
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// Complete asks the model, trying again while Groq is busy, down or slow, within the deadline.
// When ctx ends first, its cause is returned.
func (g *Groq) Complete(ctx context.Context, r Request) (Completion, error) {
	if err := ctx.Err(); err != nil {
		return Completion{}, context.Cause(ctx)
	}
	if err := checkRequest(r); err != nil {
		return Completion{}, err
	}
	body, err := g.body(r)
	if err != nil {
		return Completion{}, err
	}
	deadline := g.o.Now().Add(g.o.Deadline)
	for attempt := 0; ; attempt++ {
		completion, err := g.once(ctx, body, deadline.Sub(g.o.Now()))
		var failed *Error
		if err == nil || !errors.As(err, &failed) || attempt >= g.o.MaxRetries {
			return completion, err
		}
		if failed.Code != RateLimited && failed.Code != Unavailable && failed.Code != Timeout {
			return completion, err
		}
		delay := g.o.RetryBase << attempt
		if failed.RetryAfter != nil {
			delay = *failed.RetryAfter
		}
		if delay > g.o.MaxRetryWait || !g.o.Now().Add(delay).Before(deadline) {
			return completion, err
		}
		if err := wait(ctx, delay); err != nil {
			return Completion{}, err
		}
	}
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (g *Groq) once(ctx context.Context, body []byte, budget time.Duration) (Completion, error) {
	attempt, cancel := context.WithTimeout(ctx, min(g.o.Timeout, budget))
	defer cancel()
	stopped := func() error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if attempt.Err() != nil {
			return &Error{Provider: "groq", Code: Timeout}
		}
		return nil
	}

	req, err := http.NewRequestWithContext(attempt, http.MethodPost, g.o.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Completion{}, err
	}
	req.Header.Set("Authorization", "Bearer "+g.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.o.Client.Do(req)
	if err != nil {
		if reason := stopped(); reason != nil {
			return Completion{}, reason
		}
		return Completion{}, &Error{Provider: "groq", Code: Unavailable, Detail: networkDetail(err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxProblemBytes))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxProblemBytes))
		if readErr != nil {
			if reason := stopped(); reason != nil {
				return Completion{}, reason
			}
		}
		return Completion{}, g.problem(resp, raw)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		if reason := stopped(); reason != nil {
			return Completion{}, reason
		}
	}
	completion, ok := decodeAnswer(raw)
	if !ok {
		return Completion{}, &Error{Provider: "groq", Code: BadResponse, Status: resp.StatusCode}
	}
	return completion, nil
}

// problem names what went wrong from the status and Groq's error code, never its message.
func (g *Groq) problem(resp *http.Response, raw []byte) *Error {
	var answer struct {
		Error *struct {
			Code any `json:"code"`
		} `json:"error"`
	}
	e := &Error{Provider: "groq", Status: resp.StatusCode}
	if json.Unmarshal(bytes.TrimPrefix(raw, utf8BOM), &answer) == nil && answer.Error != nil {
		if code, ok := answer.Error.Code.(string); ok && safeDetail.MatchString(code) {
			e.Detail = code
		}
	}
	switch status := resp.StatusCode; {
	case status == http.StatusTooManyRequests:
		e.Code = RateLimited
		e.RetryAfter = g.retryAfter(resp.Header.Get("Retry-After"))
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Code = Auth
	case status >= 500:
		e.Code = Unavailable
	case status == http.StatusRequestEntityTooLarge || (e.Detail != "" && tooLongDetail.MatchString(e.Detail)):
		e.Code = TooLong
	case status == http.StatusNotFound || e.Detail == "model_not_found" || e.Detail == "model_decommissioned":
		e.Code = Misconfigured
	case e.Detail == "tool_use_failed":
		e.Code = BadToolCall
	default:
		e.Code = Rejected
	}
	return e
}

// retryAfter reads Retry-After as seconds or as a date, as JavaScript's Number and Date.parse did.
func (g *Groq) retryAfter(header string) *time.Duration {
	if header == "" {
		return nil
	}
	var d time.Duration
	if seconds, ok := jsNumber(header); ok {
		if seconds*float64(time.Second) >= math.MaxInt64 {
			d = math.MaxInt64
		} else {
			d = time.Duration(seconds * float64(time.Second))
		}
	} else if at, ok := parseDate(header); ok {
		d = at.Sub(g.o.Now())
	} else {
		return nil
	}
	d = max(d, 0)
	return &d
}

// jsNumber reads s the way JavaScript's Number does: blank is 0, and 0x, 0o and 0b prefixes are
// read in their base. Infinity and NaN are not finite, so they are not numbers here.
func jsNumber(s string) (float64, bool) {
	s = strings.TrimFunc(s, text.IsSpace)
	if s == "" {
		return 0, true
	}
	if len(s) > 2 && s[0] == '0' {
		if base := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[s[1]]; base != 0 {
			n, err := strconv.ParseUint(s[2:], base, 64)
			return float64(n), err == nil
		}
	}
	if strings.ContainsAny(s, "_xXpP") {
		return 0, false
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, false
	}
	return n, true
}

func parseDate(s string) (time.Time, bool) {
	if at, err := http.ParseTime(s); err == nil {
		return at, true
	}
	if at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
		return at, true
	}
	return time.Time{}, false
}

// decodeAnswer reads a completion as strictly as the TypeScript core's schema did: a missing
// model, choice, message or token count makes the answer bad.
func decodeAnswer(raw []byte) (Completion, bool) {
	var answer struct {
		Model   *string `json:"model"`
		Choices []*struct {
			Message *struct {
				Content   *string     `json:"content"`
				ToolCalls []*wireCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     *float64 `json:"prompt_tokens"`
			CompletionTokens *float64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(raw, utf8BOM), &answer); err != nil {
		return Completion{}, false
	}
	if answer.Model == nil || len(answer.Choices) == 0 || answer.Usage == nil ||
		answer.Usage.PromptTokens == nil || answer.Usage.CompletionTokens == nil {
		return Completion{}, false
	}
	for _, c := range answer.Choices {
		if c == nil || c.Message == nil {
			return Completion{}, false
		}
	}
	first := answer.Choices[0]
	c := Completion{
		Model:        *answer.Model,
		FinishReason: FinishStop,
		Usage: Usage{
			InputTokens:  int(*answer.Usage.PromptTokens),
			OutputTokens: int(*answer.Usage.CompletionTokens),
		},
	}
	if first.Message.Content != nil {
		c.Text = *first.Message.Content
	}
	for _, call := range first.Message.ToolCalls {
		if call == nil || call.ID == "" || call.Function.Name == "" {
			return Completion{}, false
		}
		c.ToolCalls = append(c.ToolCalls, ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	if first.FinishReason != nil && slices.Contains(finishReasons, *first.FinishReason) {
		c.FinishReason = *first.FinishReason
	}
	switch {
	case len(c.ToolCalls) > 0:
		c.FinishReason = FinishToolCalls
	case c.FinishReason == FinishToolCalls:
		return Completion{}, false
	}
	return c, true
}

// networkDetail names a network failure the way Node did, from a fixed set.
func networkDetail(err error) string {
	var dns *net.DNSError
	switch {
	case errors.As(err, &dns) && dns.IsTemporary:
		return "eai_again"
	case errors.As(err, &dns):
		return "enotfound"
	case errors.Is(err, syscall.ETIMEDOUT):
		return "etimedout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "econnrefused"
	case errors.Is(err, syscall.ECONNRESET):
		return "econnreset"
	case strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509:"):
		return "tls"
	default:
		return ""
	}
}
