package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
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

// ClientOptions tune the client. Zero values take the defaults.
type ClientOptions struct {
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

// Client asks a model through an OpenAI-compatible chat completions API: Groq, OpenAI, Gemini,
// OpenRouter, Ollama and others differ only in their settings.
type Client struct {
	name          string
	apiKey        string
	models        map[Tier]string
	contextTokens int
	locality      Locality
	// effort sends reasoning_effort; groqReasoning also sends Groq's include_reasoning.
	effort, groqReasoning bool
	tokenFactor           float64
	o                     ClientOptions
}

// NewGroq returns a client for Groq. Both models must be gpt-oss models, since tokens are counted
// with their tokenizer.
func NewGroq(cfg config.Groq, o ClientOptions) (*Client, error) {
	if err := checkModels(cfg.HeavyModel, cfg.RoutineModel); err != nil {
		return nil, err
	}
	if o.BaseURL == "" {
		o.BaseURL = "https://api.groq.com/openai/v1"
	}
	g := newClient("groq", cfg.APIKey, cfg.HeavyModel, cfg.RoutineModel, cfg.ContextTokens, o)
	g.effort, g.groqReasoning = true, true
	return g, nil
}

// NewCompatible returns a client for any OpenAI-compatible provider set in .env. Groq's kind
// keeps its own defaults and the gpt-oss check; any other provider is named by its host.
func NewCompatible(c config.Compatible, o ClientOptions) (*Client, error) {
	if o.BaseURL == "" {
		o.BaseURL = c.BaseURL
	}
	name := "groq"
	if c.Kind == "groq" {
		if err := checkModels(c.HeavyModel, c.RoutineModel); err != nil {
			return nil, err
		}
	} else if u, err := url.Parse(c.BaseURL); err == nil && u.Hostname() != "" {
		name = u.Hostname()
	} else {
		return nil, fmt.Errorf("llm base url %q has no host", c.BaseURL)
	}
	g := newClient(name, c.APIKey, c.HeavyModel, c.RoutineModel, c.ContextTokens, o)
	g.effort, g.groqReasoning, g.tokenFactor = c.Effort, c.Kind == "groq", max(c.TokenFactor, 1)
	if c.Local {
		g.locality = Local
	}
	return g, nil
}

func newClient(name, apiKey, heavy, routine string, contextTokens int, o ClientOptions) *Client {
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	o.Timeout = orDuration(o.Timeout, 30*time.Second)
	o.Deadline = orDuration(o.Deadline, 45*time.Second)
	o.RetryBase = orDuration(o.RetryBase, time.Second)
	o.MaxRetryWait = orDuration(o.MaxRetryWait, o.Deadline)
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
	return &Client{
		name:          name,
		apiKey:        apiKey,
		models:        map[Tier]string{Routine: routine, Heavy: heavy},
		contextTokens: contextTokens,
		locality:      Cloud,
		tokenFactor:   1,
		o:             o,
	}
}

func orDuration(d, fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return d
}

// Name is "groq", or the provider's host.
func (g *Client) Name() string { return g.name }

// Locality is cloud unless the provider was set as local: what is sent to a cloud one leaves the
// machine.
func (g *Client) Locality() Locality { return g.locality }

// ContextTokens is GROQ_CONTEXT_TOKENS or LLM_CONTEXT_TOKENS.
func (g *Client) ContextTokens() int { return g.contextTokens }

// CountTokens counts text with the o200k tokenizer (exact for gpt-oss), plus the tokens its chat
// framing adds, scaled by the token factor for models whose tokenizer differs.
func (g *Client) CountTokens(text string) int {
	n := o200k.Count(text) + framingTokens
	if g.tokenFactor > 1 {
		n = int(math.Ceil(float64(n) * g.tokenFactor))
	}
	return n
}

type chatRequest struct {
	Model            string     `json:"model"`
	Messages         []Message  `json:"messages"`
	MaxTokens        int        `json:"max_completion_tokens"`
	Temperature      *float64   `json:"temperature,omitempty"`
	ReasoningEffort  *Effort    `json:"reasoning_effort,omitempty"`
	IncludeReasoning *bool      `json:"include_reasoning,omitempty"`
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

func (g *Client) body(r Request) ([]byte, error) {
	req := chatRequest{
		Model:       g.models[r.Tier],
		Messages:    r.Messages,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
		Tools:       r.Tools,
	}
	if g.effort {
		effort := r.Effort
		if effort == "" {
			effort = defaultEffort[r.Tier]
		}
		req.ReasoningEffort = &effort
	}
	if g.groqReasoning {
		hidden := false
		req.IncludeReasoning = &hidden
	}
	if r.Schema != nil {
		req.ResponseFormat = &format{Type: "json_schema", JSONSchema: jsonSchema{Name: r.Schema.Name, Strict: true, Schema: r.Schema.Schema}}
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = defaultMaxTokens[r.Tier]
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
func (g *Client) Complete(ctx context.Context, r Request) (Completion, error) {
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

func (g *Client) once(ctx context.Context, body []byte, budget time.Duration) (Completion, error) {
	attempt, cancel := context.WithTimeout(ctx, min(g.o.Timeout, budget))
	defer cancel()
	stopped := func() error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if attempt.Err() != nil {
			return &Error{Provider: g.name, Code: Timeout}
		}
		return nil
	}

	req, err := http.NewRequestWithContext(attempt, http.MethodPost, g.o.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Completion{}, err
	}
	if g.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.o.Client.Do(req)
	if err != nil {
		if reason := stopped(); reason != nil {
			return Completion{}, reason
		}
		return Completion{}, &Error{Provider: g.name, Code: Unavailable, Detail: networkDetail(err)}
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
		return Completion{}, &Error{Provider: g.name, Code: BadResponse, Status: resp.StatusCode}
	}
	return completion, nil
}

// problem names what went wrong from the status and Groq's error code, never its message.
func (g *Client) problem(resp *http.Response, raw []byte) *Error {
	var answer struct {
		Error *struct {
			Code any `json:"code"`
		} `json:"error"`
	}
	e := &Error{Provider: g.name, Status: resp.StatusCode}
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
func (g *Client) retryAfter(header string) *time.Duration {
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
