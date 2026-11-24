// Package llm talks to the language model behind a provider interface.
package llm

import (
	"fmt"
	"time"
)

// Error codes a provider reports. Each has its own notice to DJ.
const (
	RateLimited   = "rate_limited"
	Timeout       = "timeout"
	Unavailable   = "unavailable"
	Auth          = "auth"
	TooLong       = "too_long"
	Misconfigured = "misconfigured"
	Rejected      = "rejected"
	BadResponse   = "bad_response"
)

// Error says why a model could not answer. It never carries the prompt, the answer or the
// provider's own message, only a code and safe details.
type Error struct {
	Provider   string
	Code       string
	Status     int
	Detail     string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	msg := e.Provider + " could not answer: " + e.Code
	if e.Status != 0 {
		msg += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	if e.Detail != "" {
		msg += ", " + e.Detail
	}
	return msg
}

// ErrorCode is the code stored for a command the model could not answer.
func (e *Error) ErrorCode() string { return e.Code }
