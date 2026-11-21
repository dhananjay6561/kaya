// Package contracts holds the Go side of the gateway ↔ core contract: typed payloads that match
// the JSON Schemas in /contracts, and a validator that checks every payload at the boundary.
package contracts

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
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

// Event is the envelope for everything the gateway tells core.
type Event struct {
	ID         string          `json:"id"`
	Version    int             `json:"version"`
	Type       EventType       `json:"type"`
	OccurredAt time.Time       `json:"occurred_at"`
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

// MessageStored is the data of a message.stored event. The body stays in the database.
type MessageStored struct {
	MessageID int64     `json:"message_id"`
	ChatJID   string    `json:"chat_jid"`
	SenderJID string    `json:"sender_jid"`
	WAID      string    `json:"wa_id"`
	FromMe    bool      `json:"from_me"`
	Kind      string    `json:"kind"`
	Source    string    `json:"source"`
	Change    string    `json:"change"`
	SentAt    time.Time `json:"sent_at"`
}

// HistorySynced is the data of a history.synced event.
type HistorySynced struct {
	Chats    int  `json:"chats"`
	Messages int  `json:"messages"`
	Progress *int `json:"progress,omitempty"`
	Complete bool `json:"complete"`
}

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
	ID        string      `json:"id"`
	Version   int         `json:"version"`
	CreatedAt time.Time   `json:"created_at"`
	To        string      `json:"to"`
	Content   TextContent `json:"content"`
	ReplyTo   *ReplyTo    `json:"reply_to,omitempty"`
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
	return Event{ID: NewID(), Version: Version, Type: t, OccurredAt: time.Now().UTC(), Data: raw}, nil
}

// NewID returns a random version 4 UUID.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
