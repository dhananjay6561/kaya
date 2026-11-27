// Package voice turns voice notes into text: a speech model on Groq, and a worker that asks the
// gateway for each new voice note's file and keeps what was said.
package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
)

// Transcript is what a voice note said.
type Transcript struct {
	Text     string
	Language string
	Duration float64
	Model    string
}

// Transcriber turns an audio file into text.
type Transcriber interface {
	Transcribe(ctx context.Context, path string) (Transcript, error)
}

// Error is a transcription that failed. Retry says trying again later may work, as when the
// service is busy; otherwise the file itself was refused.
type Error struct {
	Code  string
	Retry bool
}

func (e *Error) Error() string { return "transcription failed: " + e.Code }

// hint steers the model to write Hindi as Hinglish in Latin letters, as DJ's chats are written,
// rather than in Devanagari.
const hint = "Haan bhai, kal milte hain. Main office mein hoon, meeting ke baad call karta hoon. OK, sounds good."

// Groq transcribes with a Whisper model on Groq.
type Groq struct {
	apiKey, model, baseURL string
	client                 *http.Client
}

// NewGroq returns a Groq transcriber; an empty baseURL is Groq's own.
func NewGroq(apiKey, model, baseURL string) *Groq {
	if baseURL == "" {
		baseURL = "https://api.groq.com/openai/v1"
	}
	return &Groq{apiKey: apiKey, model: model, baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{Timeout: 60 * time.Second}}
}

// Transcribe sends the file at path and returns what it said.
func (g *Groq) Transcribe(ctx context.Context, path string) (Transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return Transcript{}, err
	}
	defer func() { _ = f.Close() }()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "voice.ogg")
	if err != nil {
		return Transcript{}, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return Transcript{}, err
	}
	for k, v := range map[string]string{"model": g.model, "response_format": "verbose_json", "temperature": "0", "prompt": hint} {
		if err := w.WriteField(k, v); err != nil {
			return Transcript{}, err
		}
	}
	if err := w.Close(); err != nil {
		return Transcript{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/audio/transcriptions", &body)
	if err != nil {
		return Transcript{}, err
	}
	req.Header.Set("Authorization", "Bearer "+g.apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := g.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Transcript{}, context.Cause(ctx)
		}
		return Transcript{}, &Error{Code: "unavailable", Retry: true}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Transcript{}, &Error{Code: "unavailable", Retry: true}
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return Transcript{}, &Error{Code: fmt.Sprintf("http_%d", resp.StatusCode), Retry: true}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Transcript{}, &Error{Code: "auth", Retry: true}
	case resp.StatusCode != http.StatusOK:
		return Transcript{}, &Error{Code: fmt.Sprintf("http_%d", resp.StatusCode)}
	}
	var out struct {
		Text     string  `json:"text"`
		Language string  `json:"language"`
		Duration float64 `json:"duration"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Transcript{}, errors.Join(&Error{Code: "bad_response", Retry: true}, err)
	}
	return Transcript{Text: strings.TrimSpace(out.Text), Language: out.Language, Duration: out.Duration, Model: g.model}, nil
}
