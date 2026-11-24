// Package channel is the one conversation between DJ and Kaya: messages DJ types to Kaya come in as
// commands, and Kaya's answers go out, through the gateway or, in tests, a stub.
package channel

import (
	"context"
	"errors"
	"time"
)

// IncomingMessage is something DJ typed to Kaya.
type IncomingMessage struct {
	ID   string
	Text string
	// SentAt is when DJ sent it.
	SentAt time.Time
	// ReceivedAt is when Kaya first took it in; the gap to SentAt is how long Kaya was away.
	ReceivedAt time.Time
}

// OutgoingMessage is an answer from Kaya.
type OutgoingMessage struct {
	IdempotencyKey string
	Text           string
	ReplyTo        string
}

// SendOptions tune one send.
type SendOptions struct {
	// TTL is how long the message stays worth sending; after that it is dropped, not sent late.
	// Zero means the channel's default.
	TTL time.Duration
	// TextMayChange lets a repeat under the same key carry other text; the text queued first is kept.
	TextMayChange bool
}

// SentMessage is a message WhatsApp accepted.
type SentMessage struct {
	ID     string
	SentAt time.Time
}

// Handled is what a handler reports back: the answer that makes the message part of the conversation.
type Handled struct {
	AnswerID string
}

// Handler deals with one incoming message. ctx ends when the channel stops.
type Handler func(ctx context.Context, m IncomingMessage) (Handled, error)

// Channel carries the conversation between DJ and Kaya.
type Channel interface {
	Name() string
	Start(ctx context.Context) error
	Stop()
	Check(ctx context.Context) error
	Send(ctx context.Context, m OutgoingMessage, o SendOptions) (SentMessage, error)
	// Follow waits for a message already queued under key; nil if none was queued.
	Follow(ctx context.Context, key string) (*SentMessage, error)
	// OnMessage adds a handler and returns a function that removes it.
	OnMessage(h Handler) func()
}

// ErrNotStarted is returned by a channel that is not started, or that stopped while waiting.
var ErrNotStarted = errors.New("the chat channel is not started")

// ErrNotLinked means the gateway has not reported which WhatsApp account it is linked to.
var ErrNotLinked = errors.New("the gateway has not reported a linked WhatsApp account yet")

// ErrEmpty refuses a message or key with nothing in it.
var ErrEmpty = errors.New("message text and idempotency key must not be empty")

// Coded errors carry a short code that may be stored, such as a send failure's reason.
type Coded interface {
	ErrorCode() string
}

// SendFailedError says the gateway did not send a message, and why.
type SendFailedError struct {
	RequestID string
	Code      string
}

func (e *SendFailedError) Error() string { return "the message was not sent: " + e.Code }

// ErrorCode is the reason the message was not sent.
func (e *SendFailedError) ErrorCode() string { return e.Code }

// KeyReusedError means an idempotency key was already used for a different message.
type KeyReusedError struct {
	RequestID string
}

func (e *KeyReusedError) Error() string {
	return "this idempotency key was already used for a different message"
}

func checkOutgoing(m OutgoingMessage) error {
	if trimmed(m.IdempotencyKey) == "" || trimmed(m.Text) == "" {
		return ErrEmpty
	}
	return nil
}
