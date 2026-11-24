package channel

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/core/internal/gateway"
	"github.com/dhananjay6561/kaya/kit/store"
)

// SendResults tells a waiting sender when the gateway has reported on its request.
type SendResults interface {
	OnSendResult(requestID string, listener gateway.SendResultListener) func()
}

// IncomingEvents hands stored messages over inside the transaction that handles their event.
type IncomingEvents interface {
	OnMessage(listener gateway.MessageListener) func()
}

// GatewayOptions configure the gateway channel. Zero durations take their defaults.
type GatewayOptions struct {
	DB        *store.DB
	Validator *contracts.Validator
	Results   SendResults
	Incoming  IncomingEvents
	Logger    *slog.Logger
	Inbox     InboxOptions

	TTL time.Duration
	// Poll is how often a waiting send looks at its request when no result event wakes it.
	Poll time.Duration
	// Grace is how long after expiry a pending request is given before it counts as unanswered.
	Grace time.Duration
	// SendingGrace is how long after that a request still being sent is waited for.
	SendingGrace time.Duration
}

// Gateway is the channel that talks through the WhatsApp gateway: DJ's own chat in, send requests out.
type Gateway struct {
	o     GatewayOptions
	inbox *Inbox

	mu          sync.Mutex
	started     bool
	stopping    context.Context
	stop        context.CancelFunc
	unsubscribe func()
	handlers    []handlerEntry
	nextID      int
}

// NewGateway returns a stopped gateway channel.
func NewGateway(o GatewayOptions) *Gateway {
	if o.Poll == 0 {
		o.Poll = 2 * time.Second
	}
	if o.Grace == 0 {
		o.Grace = 30 * time.Second
	}
	if o.SendingGrace == 0 {
		o.SendingGrace = 5 * time.Minute
	}
	g := &Gateway{o: o}
	if o.Incoming != nil && o.Logger != nil {
		inboxOptions := o.Inbox
		inboxOptions.DB, inboxOptions.Logger = o.DB, o.Logger
		g.inbox = NewInbox(inboxOptions)
	}
	return g
}

// RequestIDFor turns an idempotency key into the send request id, a version 5 style UUID, the same
// way the TypeScript core did, so a request queued by either is recognised by both.
func RequestIDFor(idempotencyKey string) string {
	h := sha256.Sum256([]byte("kaya:channel:" + idempotencyKey))
	h[6] = h[6]&0x0f | 0x50
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// Name is "gateway".
func (g *Gateway) Name() string { return "gateway" }

// Start subscribes to stored messages and starts the command inbox.
func (g *Gateway) Start(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started {
		return nil
	}
	g.started = true
	g.stopping, g.stop = context.WithCancel(context.Background())
	if g.inbox != nil {
		inbox := g.inbox
		g.unsubscribe = g.o.Incoming.OnMessage(func(ctx context.Context, tx pgx.Tx, m contracts.MessageStored) (gateway.AfterCommit, error) {
			added, err := inbox.Record(ctx, tx, m)
			if err != nil || !added {
				return nil, err
			}
			return inbox.Wake, nil
		})
		inbox.Start(g.deliver, g.hasHandlers)
	}
	return nil
}

// Stop ends every wait at once and stops the inbox; queued requests stay queued.
func (g *Gateway) Stop() {
	g.mu.Lock()
	if !g.started {
		g.mu.Unlock()
		return
	}
	g.started = false
	unsubscribe, stop := g.unsubscribe, g.stop
	g.unsubscribe = nil
	g.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
	stop()
	if g.inbox != nil {
		g.inbox.Stop()
	}
}

// Check reports whether the channel is started.
func (g *Gateway) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.started {
		return ErrNotStarted
	}
	return nil
}

// OnMessage adds a handler for DJ's commands and returns a function that removes it.
func (g *Gateway) OnMessage(h Handler) func() {
	g.mu.Lock()
	g.nextID++
	id := g.nextID
	g.handlers = append(g.handlers, handlerEntry{id: id, handler: h})
	g.mu.Unlock()
	if g.inbox != nil {
		g.inbox.Wake()
	}
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.handlers = slices.DeleteFunc(g.handlers, func(e handlerEntry) bool { return e.id == id })
	}
}

func (g *Gateway) hasHandlers() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.handlers) > 0
}

func (g *Gateway) deliver(ctx context.Context, m IncomingMessage) (Handled, error) {
	g.mu.Lock()
	handlers := slices.Clone(g.handlers)
	g.mu.Unlock()
	return deliverTo(ctx, handlers, m)
}

// waitContext ends when the caller gives up or the channel stops; a stop reads as ErrNotStarted.
func (g *Gateway) waitContext(ctx context.Context) (context.Context, func(), error) {
	g.mu.Lock()
	started, stopping := g.started, g.stopping
	g.mu.Unlock()
	if !started {
		return nil, nil, ErrNotStarted
	}
	merged, cancel := context.WithCancelCause(ctx)
	unhook := context.AfterFunc(stopping, func() { cancel(ErrNotStarted) })
	return merged, func() {
		unhook()
		cancel(nil)
	}, nil
}

func stopped(ctx context.Context) error {
	if cause := context.Cause(ctx); errors.Is(cause, ErrNotStarted) {
		return ErrNotStarted
	}
	return ctx.Err()
}

type requestRow struct {
	state      string
	waID       *string
	errCode    *string
	finishedAt *time.Time
	text       *string
	replyTo    *string
	overdue    bool
	abandoned  bool
}

// outcome reads where request id stands. found is false when there is no such request. When m is
// set, a request under the same key that says something else is refused.
func (g *Gateway) outcome(ctx context.Context, id string, m *OutgoingMessage, textMayChange bool) (sent *SentMessage, found bool, err error) {
	var r requestRow
	err = g.o.DB.QueryRow(ctx, `
		SELECT state, wa_id, error, finished_at,
		       payload -> 'content' ->> 'text', payload -> 'reply_to' ->> 'wa_id',
		       now() > expires_at + make_interval(secs => $2),
		       now() > expires_at + make_interval(secs => $3)
		FROM send_requests WHERE id = $1`,
		id, g.o.Grace.Seconds(), (g.o.Grace+g.o.SendingGrace).Seconds()).
		Scan(&r.state, &r.waID, &r.errCode, &r.finishedAt, &r.text, &r.replyTo, &r.overdue, &r.abandoned)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if m != nil {
		textDiffers := deref(r.text) != trimmed(m.Text) && !textMayChange
		if textDiffers || deref(r.replyTo) != m.ReplyTo {
			return nil, true, &KeyReusedError{RequestID: id}
		}
	}
	switch {
	case r.state == "sent" && r.waID != nil && r.finishedAt != nil:
		return &SentMessage{ID: *r.waID, SentAt: *r.finishedAt}, true, nil
	case r.state == "failed":
		code := "failed"
		if r.errCode != nil {
			code = *r.errCode
		}
		return nil, true, &SendFailedError{RequestID: id, Code: code}
	case r.state == "pending" && r.overdue:
		return nil, true, &SendFailedError{RequestID: id, Code: "no_answer"}
	case r.state == "sending" && r.abandoned:
		return nil, true, &SendFailedError{RequestID: id, Code: "outcome_unknown"}
	}
	return nil, true, nil
}

func (g *Gateway) waitFor(ctx context.Context, id string, m *OutgoingMessage, textMayChange bool) (SentMessage, error) {
	for {
		if ctx.Err() != nil {
			return SentMessage{}, stopped(ctx)
		}
		sent, found, err := g.outcome(ctx, id, m, textMayChange)
		if err != nil {
			if ctx.Err() != nil {
				return SentMessage{}, stopped(ctx)
			}
			return SentMessage{}, err
		}
		if !found {
			return SentMessage{}, errors.New("a queued send request disappeared")
		}
		if sent != nil {
			return *sent, nil
		}
		g.nudge(ctx, id)
	}
}

// nudge waits for the poll interval, or less when the gateway reports on request id.
func (g *Gateway) nudge(ctx context.Context, id string) {
	woken := make(chan struct{}, 1)
	if g.o.Results != nil {
		forget := g.o.Results.OnSendResult(id, func(contracts.SendResult) {
			select {
			case woken <- struct{}{}:
			default:
			}
		})
		defer forget()
	}
	timer := time.NewTimer(g.o.Poll)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-woken:
	case <-timer.C:
	}
}

// Send queues m as a request to DJ's own chat and waits until the gateway has sent it or given up.
func (g *Gateway) Send(ctx context.Context, m OutgoingMessage, o SendOptions) (SentMessage, error) {
	if err := ctx.Err(); err != nil {
		return SentMessage{}, err
	}
	wait, done, err := g.waitContext(ctx)
	if err != nil {
		return SentMessage{}, err
	}
	defer done()
	if err := checkOutgoing(m); err != nil {
		return SentMessage{}, err
	}
	id := RequestIDFor(m.IdempotencyKey)
	sent, found, err := g.outcome(wait, id, &m, o.TextMayChange)
	if err != nil {
		if wait.Err() != nil {
			return SentMessage{}, stopped(wait)
		}
		return SentMessage{}, err
	}
	if sent != nil {
		return *sent, nil
	}
	if !found {
		if err := g.enqueue(wait, id, m, o); err != nil {
			if wait.Err() != nil {
				return SentMessage{}, stopped(wait)
			}
			return SentMessage{}, err
		}
	}
	return g.waitFor(wait, id, &m, o.TextMayChange)
}

func (g *Gateway) enqueue(ctx context.Context, id string, m OutgoingMessage, o SendOptions) error {
	connection, err := gateway.ReadConnection(ctx, g.o.DB)
	if err != nil {
		return err
	}
	if connection == nil || connection.OwnJID == "" {
		return ErrNotLinked
	}
	ttl := o.TTL
	if ttl == 0 {
		ttl = g.o.TTL
	}
	var replyTo *contracts.ReplyTo
	if m.ReplyTo != "" {
		replyTo = &contracts.ReplyTo{WAID: m.ReplyTo, SenderJID: connection.OwnJID}
	}
	request, err := contracts.NewSendRequest(connection.OwnJID, m.Text, contracts.Authorization{Kind: "self_chat"}, replyTo, ttl)
	if err != nil {
		return err
	}
	request.ID = id
	raw, err := g.o.Validator.EncodeSendRequest(request)
	if err != nil {
		return err
	}
	_, err = g.o.DB.Exec(ctx,
		"INSERT INTO send_requests (id, payload, expires_at) VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING",
		id, raw, request.ExpiresAt.Time)
	return err
}

// Follow waits for whatever was queued under key, whatever it said; nil if nothing was.
func (g *Gateway) Follow(ctx context.Context, key string) (*SentMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wait, done, err := g.waitContext(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	id := RequestIDFor(key)
	sent, found, err := g.outcome(wait, id, nil, true)
	if err != nil {
		if wait.Err() != nil {
			return nil, stopped(wait)
		}
		return nil, err
	}
	if !found {
		return nil, nil
	}
	if sent != nil {
		return sent, nil
	}
	result, err := g.waitFor(wait, id, nil, true)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
