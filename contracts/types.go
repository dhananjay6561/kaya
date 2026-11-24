// Package contracts holds the Go side of the gateway ↔ core contract: typed payloads that match
// the JSON Schemas in /contracts, and a validator that checks every payload at the boundary.
package contracts

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Version is the contract version every payload carries.
const Version = 1

// EventType names what an event reports.
type EventType string

// Event types.
const (
	EventConnectionState EventType = "connection.state"
	EventMessageStored   EventType = "message.stored"
	EventHistorySynced   EventType = "history.synced"
	EventSendResult      EventType = "send.result"
)

// Time is a timestamp that always encodes as UTC with at most microsecond precision, which the
// contract requires. whatsmeow hands out local times, so every time field uses this type.
type Time struct {
	time.Time
}

// At wraps t.
func At(t time.Time) Time { return Time{t} }

// MarshalJSON writes t in UTC, e.g. "2026-11-22T09:30:00.123Z".
func (t Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano))
}

// UnmarshalJSON reads an RFC 3339 time.
func (t *Time) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = parsed.UTC()
	return nil
}

// Event is the envelope for everything the gateway tells core.
type Event struct {
	ID         string          `json:"id"`
	Version    int             `json:"version"`
	Type       EventType       `json:"type"`
	OccurredAt Time            `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

// Connection states.
const (
	StateConnecting   = "connecting"
	StatePairing      = "pairing"
	StateConnected    = "connected"
	StateDisconnected = "disconnected"
	StateLoggedOut    = "logged_out"
)

// ConnectionState is the data of a connection.state event.
type ConnectionState struct {
	State  string `json:"state"`
	OwnJID string `json:"own_jid,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// MessageStored is the data of a message.stored event. It points at database rows; the body stays
// in the database. Emit it only when the upsert inserted or changed the row.
type MessageStored struct {
	MessageID int64  `json:"message_id"`
	ChatID    int64  `json:"chat_id"`
	SenderID  int64  `json:"sender_id"`
	WAID      string `json:"wa_id"`
	FromMe    bool   `json:"from_me"`
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	Change    string `json:"change"`
	SentAt    Time   `json:"sent_at"`
}

// HistorySynced is the data of a history.synced event.
type HistorySynced struct {
	Chats    int  `json:"chats"`
	Messages int  `json:"messages"`
	Progress *int `json:"progress,omitempty"`
	Complete bool `json:"complete"`
}

// Send result statuses.
const (
	SendSent   = "sent"
	SendFailed = "failed"
)

// SendResult is the data of a send.result event.
type SendResult struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
	WAID      string `json:"wa_id,omitempty"`
	Error     *Error `json:"error,omitempty"`
}

// Error describes a failure without exposing internals.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SendRequest is a message core asks the gateway to send.
type SendRequest struct {
	ID            string        `json:"id"`
	Version       int           `json:"version"`
	CreatedAt     Time          `json:"created_at"`
	ExpiresAt     Time          `json:"expires_at"`
	To            string        `json:"to"`
	Authorization Authorization `json:"authorization"`
	Content       TextContent   `json:"content"`
	ReplyTo       *ReplyTo      `json:"reply_to,omitempty"`
}

// Authorization kinds.
const (
	AuthSelfChat  = "self_chat"
	AuthConfirmed = "confirmed"
)

// Authorization says why a send is allowed: Kaya's own chat with DJ, or a pending action DJ confirmed.
type Authorization struct {
	Kind     string `json:"kind"`
	ActionID string `json:"action_id,omitempty"`
}

// TextContent is a plain text message body.
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ReplyTo names the message a send request quotes.
type ReplyTo struct {
	WAID      string `json:"wa_id"`
	SenderJID string `json:"sender_jid"`
}

// NewEvent wraps data in an envelope with a fresh id and the current UTC time.
func NewEvent(t EventType, data any) (Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("encode %s data: %w", t, err)
	}
	return Event{ID: NewID(), Version: Version, Type: t, OccurredAt: At(time.Now()), Data: raw}, nil
}

// DefaultSendTTL is how long a send request stays worth sending unless its sender says otherwise.
const DefaultSendTTL = 15 * time.Minute

// NewSendRequest builds a request to send text to the JID to, worth sending for ttl
// (DefaultSendTTL when zero). Times are cut to the microsecond, the precision Postgres keeps, so a
// stored expires_at equals the one in the payload.
func NewSendRequest(to, text string, auth Authorization, replyTo *ReplyTo, ttl time.Duration) (SendRequest, error) {
	if ttl == 0 {
		ttl = DefaultSendTTL
	}
	if ttl < time.Millisecond || ttl > MaxSendTTL {
		return SendRequest{}, &ValidationError{Problems: []string{expiryProblem}}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	return SendRequest{
		ID:            NewID(),
		Version:       Version,
		CreatedAt:     At(now),
		ExpiresAt:     At(now.Add(ttl)),
		To:            to,
		Authorization: auth,
		Content:       TextContent{Type: "text", Text: strings.TrimSpace(text)},
		ReplyTo:       replyTo,
	}, nil
}

// NewID returns a random version 4 UUID.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
