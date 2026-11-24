package channel

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dhananjay6561/kaya/contracts"
)

// Stub is an in-memory channel for development and tests: it records what it sends, and Receive
// delivers a message to its handlers as if DJ had typed it.
type Stub struct {
	mu       sync.Mutex
	started  bool
	sent     []OutgoingMessage
	byKey    map[string]stubSent
	handlers []handlerEntry
	nextID   int
}

type stubSent struct {
	result  SentMessage
	message OutgoingMessage
}

type handlerEntry struct {
	id      int
	handler Handler
}

// NewStub returns a stopped stub channel.
func NewStub() *Stub {
	return &Stub{byKey: map[string]stubSent{}}
}

// Name is "stub".
func (s *Stub) Name() string { return "stub" }

// Start lets the stub send and receive.
func (s *Stub) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	return nil
}

// Stop makes the stub refuse to send or receive.
func (s *Stub) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = false
}

// Check reports whether the stub is started.
func (s *Stub) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return ErrNotStarted
	}
	return nil
}

// Sent lists every message sent, in order.
func (s *Stub) Sent() []OutgoingMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent)
}

// Send records m, once per idempotency key.
func (s *Stub) Send(ctx context.Context, m OutgoingMessage, o SendOptions) (SentMessage, error) {
	if err := ctx.Err(); err != nil {
		return SentMessage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return SentMessage{}, ErrNotStarted
	}
	if trimmed(m.IdempotencyKey) == "" {
		return SentMessage{}, ErrEmpty
	}
	if earlier, ok := s.byKey[m.IdempotencyKey]; ok {
		textDiffers := trimmed(earlier.message.Text) != trimmed(m.Text) && !o.TextMayChange
		if textDiffers || earlier.message.ReplyTo != m.ReplyTo {
			return SentMessage{}, &KeyReusedError{RequestID: m.IdempotencyKey}
		}
		return earlier.result, nil
	}
	if err := checkOutgoing(m); err != nil {
		return SentMessage{}, err
	}
	s.sent = append(s.sent, m)
	result := SentMessage{ID: contracts.NewID(), SentAt: time.Now()}
	s.byKey[m.IdempotencyKey] = stubSent{result: result, message: m}
	return result, nil
}

// Follow returns what was sent under key, if anything.
func (s *Stub) Follow(ctx context.Context, key string) (*SentMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return nil, ErrNotStarted
	}
	if earlier, ok := s.byKey[key]; ok {
		result := earlier.result
		return &result, nil
	}
	return nil, nil
}

// OnMessage adds a handler and returns a function that removes it.
func (s *Stub) OnMessage(h Handler) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := s.nextID
	s.handlers = append(s.handlers, handlerEntry{id: id, handler: h})
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.handlers = slices.DeleteFunc(s.handlers, func(e handlerEntry) bool { return e.id == id })
	}
}

// ReceiveOptions set what a received message looks like.
type ReceiveOptions struct {
	ID     string
	SentAt time.Time
}

// Receive delivers text to every handler as a message DJ typed, and reports what they returned. A
// failing handler does not stop the others; every failure is reported.
func (s *Stub) Receive(ctx context.Context, text string, o ReceiveOptions) (IncomingMessage, Handled, error) {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return IncomingMessage{}, Handled{}, ErrNotStarted
	}
	handlers := slices.Clone(s.handlers)
	s.mu.Unlock()

	now := time.Now()
	m := IncomingMessage{ID: o.ID, Text: text, SentAt: o.SentAt, ReceivedAt: now}
	if m.ID == "" {
		m.ID = contracts.NewID()
	}
	if m.SentAt.IsZero() {
		m.SentAt = now
	}
	handled, err := deliverTo(ctx, handlers, m)
	return m, handled, err
}

// deliverTo runs every handler, one after another, and reports what they returned. A handler that
// panics counts as one that failed.
func deliverTo(ctx context.Context, handlers []handlerEntry, m IncomingMessage) (Handled, error) {
	var errs []error
	var first Handled
	for _, entry := range handlers {
		handled, err := callHandler(ctx, entry.handler, m)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if first.AnswerID == "" {
			first = handled
		}
	}
	switch len(errs) {
	case 0:
		return first, nil
	case 1:
		return Handled{}, errs[0]
	}
	return Handled{}, errors.Join(errs...)
}

func callHandler(ctx context.Context, h Handler, m IncomingMessage) (handled Handled, err error) {
	defer func() {
		if p := recover(); p != nil {
			handled, err = Handled{}, fmt.Errorf("handler panicked: %v\n%s", p, debug.Stack())
		}
	}()
	return h(ctx, m)
}

// trimmed cuts the same characters JavaScript's trim() does, so text and keys compare the same way
// they did in the TypeScript core.
func trimmed(s string) string { return strings.TrimFunc(s, isJSSpace) }

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}
