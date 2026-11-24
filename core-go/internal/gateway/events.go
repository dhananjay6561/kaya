// Package gateway reads what the WhatsApp gateway reports: its events, in order, and the state of
// the connection.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/contracts"
)

// AfterCommit runs once the event that produced it is committed as handled.
type AfterCommit func()

// Querier is what reading the connection state needs: a pool or a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Connection is the latest WhatsApp connection state the gateway reported.
type Connection struct {
	State      string
	Reason     string
	OwnJID     string
	OccurredAt time.Time
}

// ReadConnection returns the latest connection state, or nil when the gateway has reported none.
func ReadConnection(ctx context.Context, q Querier) (*Connection, error) {
	var c Connection
	var reason, own *string
	err := q.QueryRow(ctx, "SELECT state, reason, own_jid, occurred_at FROM connection_state").
		Scan(&c.State, &reason, &own, &c.OccurredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if reason != nil {
		c.Reason = *reason
	}
	if own != nil {
		c.OwnJID = *own
	}
	return &c, nil
}

// MessageListener sees each stored message inside the transaction that handles its event.
type MessageListener func(ctx context.Context, tx pgx.Tx, m contracts.MessageStored) (AfterCommit, error)

// SendResultListener hears what became of one send request.
type SendResultListener func(contracts.SendResult)

// Events turns gateway events into state and tells listeners about them.
type Events struct {
	logger *slog.Logger

	mu       sync.Mutex
	nextID   int
	messages map[int]MessageListener
	sends    map[string]map[int]SendResultListener
}

// NewEvents returns handlers for every gateway event type.
func NewEvents(logger *slog.Logger) *Events {
	return &Events{
		logger:   logger,
		messages: map[int]MessageListener{},
		sends:    map[string]map[int]SendResultListener{},
	}
}

// OnMessage adds a listener for stored messages and returns a function that removes it.
func (e *Events) OnMessage(listener MessageListener) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextID++
	id := e.nextID
	e.messages[id] = listener
	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.messages, id)
	}
}

// OnSendResult adds a listener for one send request's result and returns a function that removes it.
func (e *Events) OnSendResult(requestID string, listener SendResultListener) func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextID++
	id := e.nextID
	if e.sends[requestID] == nil {
		e.sends[requestID] = map[int]SendResultListener{}
	}
	e.sends[requestID][id] = listener
	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.sends[requestID], id)
		if len(e.sends[requestID]) == 0 {
			delete(e.sends, requestID)
		}
	}
}

// Handle applies one event inside tx and returns what to run once it is committed.
func (e *Events) Handle(ctx context.Context, tx pgx.Tx, event contracts.Event) (AfterCommit, error) {
	switch event.Type {
	case contracts.EventConnectionState:
		var data contracts.ConnectionState
		if err := event.DecodeData(&data); err != nil {
			return nil, err
		}
		return e.connectionChanged(ctx, tx, data, event.OccurredAt.Time)
	case contracts.EventMessageStored:
		var data contracts.MessageStored
		if err := event.DecodeData(&data); err != nil {
			return nil, err
		}
		return e.messageStored(ctx, tx, data)
	case contracts.EventSendResult:
		var data contracts.SendResult
		if err := event.DecodeData(&data); err != nil {
			return nil, err
		}
		return func() { e.sendResult(data) }, nil
	case contracts.EventHistorySynced:
		var data contracts.HistorySynced
		if err := event.DecodeData(&data); err != nil {
			return nil, err
		}
		return func() {
			e.logger.Info("history batch stored",
				"chats", data.Chats, "messages", data.Messages, "progress", data.Progress, "complete", data.Complete)
		}, nil
	}
	return nil, fmt.Errorf("unknown event type %q", event.Type)
}

func (e *Events) connectionChanged(ctx context.Context, tx pgx.Tx, data contracts.ConnectionState, at time.Time) (AfterCommit, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO connection_state (state, reason, own_jid, occurred_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (only_row) DO UPDATE
		SET state = EXCLUDED.state, reason = EXCLUDED.reason,
		    own_jid = CASE WHEN EXCLUDED.state = 'logged_out' THEN EXCLUDED.own_jid
		                   ELSE coalesce(EXCLUDED.own_jid, connection_state.own_jid) END,
		    occurred_at = EXCLUDED.occurred_at, updated_at = now()
		WHERE connection_state.occurred_at <= EXCLUDED.occurred_at`,
		data.State, nullable(data.Reason), nullable(data.OwnJID), at)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	return func() {
		e.logger.Info("whatsapp connection changed", "state", data.State, "reason", data.Reason)
	}, nil
}

func (e *Events) messageStored(ctx context.Context, tx pgx.Tx, data contracts.MessageStored) (AfterCommit, error) {
	e.mu.Lock()
	listeners := make([]MessageListener, 0, len(e.messages))
	for id := 1; id <= e.nextID; id++ {
		if listener, ok := e.messages[id]; ok {
			listeners = append(listeners, listener)
		}
	}
	e.mu.Unlock()

	var followUps []AfterCommit
	for _, listener := range listeners {
		followUp, err := listener(ctx, tx, data)
		if err != nil {
			return nil, err
		}
		if followUp != nil {
			followUps = append(followUps, followUp)
		}
	}
	e.logger.Debug("message event handled", "message_id", data.MessageID, "chat_id", data.ChatID,
		"kind", data.Kind, "change", data.Change, "source", data.Source)
	return func() {
		for _, followUp := range followUps {
			followUp()
		}
	}, nil
}

func (e *Events) sendResult(result contracts.SendResult) {
	e.mu.Lock()
	listeners := make([]SendResultListener, 0, len(e.sends[result.RequestID]))
	for _, listener := range e.sends[result.RequestID] {
		listeners = append(listeners, listener)
	}
	e.mu.Unlock()
	for _, listener := range listeners {
		listener(result)
	}
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
