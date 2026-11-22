// Package send delivers the messages core asks the gateway to send.
package send

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/ingest"
	"github.com/dhananjay6561/kaya/gateway/internal/logging"
)

// Request states, as stored in send_requests.state.
const (
	StatePending = "pending"
	StateSending = "sending"
	StateSent    = "sent"
	StateFailed  = "failed"
)

// Codes for a request that failed, as stored in send_requests.error and reported in send.result.
const (
	CodeInvalid       = "invalid_request"
	CodeExpired       = "expired"
	CodeNotAuthorized = "not_authorized"
	CodeNotOnWhatsApp = "recipient_not_on_whatsapp"
	CodeRejected      = "rejected"
	CodeUnknown       = "outcome_unknown"
)

var messages = map[string]string{
	CodeInvalid:       "the request does not fit the contract",
	CodeExpired:       "the request expired before it could be sent",
	CodeNotAuthorized: "the request is not allowed to go to that chat",
	CodeNotOnWhatsApp: "the recipient is not on WhatsApp",
	CodeRejected:      "WhatsApp refused the message",
	CodeUnknown:       "the gateway cannot tell whether the message was sent; check the chat before sending it again",
}

// sendTimeout is longer than whatsmeow's own wait for WhatsApp's answer, so its error comes first.
const sendTimeout = 90 * time.Second

// DB runs a function in one transaction.
type DB interface {
	InTx(ctx context.Context, fn func(pgx.Tx) error) error
}

// Client sends a message to WhatsApp. whatsmeow's client implements it.
type Client interface {
	GenerateMessageID() types.MessageID
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

// Outbox adds an event for core to a transaction.
type Outbox interface {
	Add(ctx context.Context, tx pgx.Tx, e contracts.Event) error
}

// Messages stores a message in the chat tables.
type Messages interface {
	Apply(ctx context.Context, tx pgx.Tx, rec ingest.Record) (ingest.Applied, error)
}

// Worker takes requests from send_requests one at a time, oldest first. A message is never sent
// twice: its WhatsApp id is stored, with the request marked as sending, before the network call,
// and a request found in that state later is reported as outcome unknown instead of sent again.
type Worker struct {
	db        DB
	validator *contracts.Validator
	outbox    Outbox
	messages  Messages
	logger    *slog.Logger

	recovered   bool
	recordRetry time.Duration
	recordLast  time.Duration
}

// NewWorker returns a worker that reports outcomes through outbox and stores what it sent in
// messages.
func NewWorker(db DB, validator *contracts.Validator, outbox Outbox, messages Messages, logger *slog.Logger) *Worker {
	return &Worker{
		db: db, validator: validator, outbox: outbox, messages: messages, logger: logger,
		recordRetry: time.Second, recordLast: 3 * time.Second,
	}
}

type request struct {
	id      string
	payload []byte
	expired bool
}

// Step handles one request and reports whether to look for another straight away. cli is nil while
// WhatsApp is not connected; then only requests that have expired are dealt with, and the rest wait.
func (w *Worker) Step(ctx context.Context, cli Client, own ingest.Own) bool {
	if !w.recovered {
		if err := w.recover(ctx); err != nil {
			w.logger.Warn("could not settle the requests an earlier run left unfinished", logging.Err(err))
			return false
		}
		w.recovered = true
	}
	req, err := w.next(ctx, cli != nil)
	if err != nil {
		w.logger.Warn("could not read the send queue", logging.Err(err))
		return false
	}
	if req == nil {
		return false
	}
	parsed, to, code := w.check(req, own)
	if code != "" {
		return w.refuse(ctx, req.id, code)
	}
	if cli == nil {
		return false
	}

	message, err := w.build(ctx, parsed)
	if err != nil {
		w.logger.Warn("could not prepare a message", "request_id", req.id, logging.Err(err))
		return false
	}
	waID := cli.GenerateMessageID()
	claimed, err := w.claim(ctx, req.id, waID)
	if err != nil {
		w.logger.Warn("could not mark a request as sending", "request_id", req.id, logging.Err(err))
		return false
	}
	if !claimed {
		return true
	}

	sending, cancel := context.WithTimeout(ctx, sendTimeout)
	resp, err := cli.SendMessage(sending, to, message, whatsmeow.SendRequestExtra{ID: waID})
	cancel()
	if err != nil {
		code := classify(err)
		w.logger.Warn("a message was not sent", "request_id", req.id, "error_code", code, logging.Err(err))
		return w.settle(ctx, func(tx pgx.Tx) error { return w.fail(ctx, tx, req.id, StateSending, code) })
	}
	w.logger.Info("message sent", "request_id", req.id)
	at := resp.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	return w.settle(ctx, func(tx pgx.Tx) error {
		done, err := w.finish(ctx, tx, req.id, StateSending, StateSent, "", contracts.SendResult{
			RequestID: req.id, Status: contracts.SendSent, WAID: waID,
		})
		if err != nil || !done {
			return err
		}
		w.keep(ctx, tx, to, waID, at, message, own)
		return nil
	})
}

// recover settles requests that were being sent when the gateway last stopped. Whether WhatsApp
// received them cannot be known, so they are never sent again.
func (w *Worker) recover(ctx context.Context) error {
	return w.db.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT id::text FROM send_requests WHERE state = $1 ORDER BY seq", StateSending)
		if err != nil {
			return fmt.Errorf("read unfinished requests: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("read unfinished requests: %w", err)
		}
		for _, id := range ids {
			if err := w.fail(ctx, tx, id, StateSending, CodeUnknown); err != nil {
				return err
			}
		}
		if len(ids) > 0 {
			w.logger.Warn("requests were left mid-send by an earlier run and are reported as outcome unknown", "requests", len(ids))
		}
		return nil
	})
}

func (w *Worker) next(ctx context.Context, connected bool) (*request, error) {
	var req request
	err := w.db.InTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id::text, payload, expires_at <= now()
			FROM send_requests
			WHERE state = $1 AND ($2 OR expires_at <= now())
			ORDER BY seq
			LIMIT 1`, StatePending, connected).Scan(&req.id, &req.payload, &req.expired)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// check decides whether a request may be sent at all, and returns the code to refuse it with if
// not. Until confirmed actions exist, the only chat Kaya may write to is DJ's own.
func (w *Worker) check(req *request, own ingest.Own) (contracts.SendRequest, types.JID, string) {
	parsed, err := w.validator.DecodeSendRequest(req.payload)
	if err != nil || parsed.ID != req.id {
		return parsed, types.EmptyJID, CodeInvalid
	}
	if req.expired {
		return parsed, types.EmptyJID, CodeExpired
	}
	to, err := types.ParseJID(parsed.To)
	if err != nil || to.Device != 0 {
		return parsed, types.EmptyJID, CodeInvalid
	}
	self := parsed.To != "" && (parsed.To == own.PN || parsed.To == own.LID)
	if parsed.Authorization.Kind != contracts.AuthSelfChat || !self {
		return parsed, types.EmptyJID, CodeNotAuthorized
	}
	return parsed, to, ""
}

func (w *Worker) build(ctx context.Context, req contracts.SendRequest) (*waE2E.Message, error) {
	if req.ReplyTo == nil {
		return &waE2E.Message{Conversation: proto.String(req.Content.Text)}, nil
	}
	var quoted string
	err := w.db.InTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT coalesce(body, '') FROM messages WHERE wa_id = $1 ORDER BY id DESC LIMIT 1", req.ReplyTo.WAID).Scan(&quoted)
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read the quoted message: %w", err)
	}
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String(req.Content.Text),
		ContextInfo: &waE2E.ContextInfo{
			StanzaID:      proto.String(req.ReplyTo.WAID),
			Participant:   proto.String(req.ReplyTo.SenderJID),
			QuotedMessage: &waE2E.Message{Conversation: proto.String(quoted)},
		},
	}}, nil
}

func (w *Worker) claim(ctx context.Context, id, waID string) (bool, error) {
	var claimed bool
	err := w.db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE send_requests SET state = $3, wa_id = $2 WHERE id = $1 AND state = $4", id, waID, StateSending, StatePending)
		claimed = tag.RowsAffected() == 1
		return err
	})
	return claimed, err
}

func (w *Worker) refuse(ctx context.Context, id, code string) bool {
	err := w.db.InTx(ctx, func(tx pgx.Tx) error { return w.fail(ctx, tx, id, StatePending, code) })
	if err != nil {
		w.logger.Warn("could not record a refused request", "request_id", id, logging.Err(err))
		return false
	}
	w.logger.Warn("a send request was refused", "request_id", id, "error_code", code)
	return true
}

func (w *Worker) fail(ctx context.Context, tx pgx.Tx, id, from, code string) error {
	_, err := w.finish(ctx, tx, id, from, StateFailed, code, contracts.SendResult{
		RequestID: id, Status: contracts.SendFailed, Error: &contracts.Error{Code: code, Message: messages[code]},
	})
	return err
}

// finish moves a request to its final state and tells core, in one transaction. It reports false
// when the request was no longer in from, and then tells core nothing.
func (w *Worker) finish(ctx context.Context, tx pgx.Tx, id, from, to, code string, result contracts.SendResult) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE send_requests SET state = $3, error = nullif($4, ''), finished_at = now()
		WHERE id = $1 AND state = $2`, id, from, to, code)
	if err != nil {
		return false, fmt.Errorf("finish send request: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	event, err := contracts.NewEvent(contracts.EventSendResult, result)
	if err != nil {
		return false, err
	}
	return true, w.outbox.Add(ctx, tx, event)
}

// settle records an outcome that is already decided, trying until the database takes it. If the
// gateway stops first it makes one last attempt; a request it could not record stays in sending
// and is reported as outcome unknown by the next run.
func (w *Worker) settle(ctx context.Context, record func(pgx.Tx) error) bool {
	wait := w.recordRetry
	for {
		err := w.db.InTx(ctx, record)
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.recordLast)
			err = w.db.InTx(last, record)
			cancel()
			if err != nil {
				w.logger.Error("could not record what happened to a message before stopping", logging.Err(err))
			}
			return false
		}
		w.logger.Warn("could not record what happened to a message, trying again", logging.Err(err))
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

// keep stores the message Kaya just sent, since WhatsApp does not send a device its own messages
// back. A failure here is logged and nothing more: the message is sent either way.
func (w *Worker) keep(ctx context.Context, tx pgx.Tx, to types.JID, waID string, at time.Time, message *waE2E.Message, own ingest.Own) {
	sender, _ := types.ParseJID(own.PN)
	rec := ingest.FromMessage(&events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: to, Sender: sender, IsFromMe: true, IsGroup: to.Server == types.GroupServer},
			ID:            waID,
			Timestamp:     at,
		},
		Message: message,
	}, own, ingest.SourceLive)
	if rec.Op == ingest.OpSkip {
		w.logger.Warn("a sent message could not be mapped for storing", "reason", rec.SkipReason)
		return
	}
	point, err := tx.Begin(ctx)
	if err != nil {
		w.logger.Warn("could not store a sent message", logging.Err(err))
		return
	}
	if _, err := w.messages.Apply(ctx, point, rec); err != nil {
		_ = point.Rollback(ctx)
		w.logger.Warn("could not store a sent message", logging.Err(err))
		return
	}
	if err := point.Commit(ctx); err != nil {
		w.logger.Warn("could not store a sent message", logging.Err(err))
	}
}

// classify turns a send error into a code. Only errors that are certain to mean WhatsApp did not
// take the message count as a plain failure; anything else might have gone out, so it is reported
// as unknown and left for a person to check.
func classify(err error) string {
	switch {
	case errors.Is(err, whatsmeow.ErrRecipientNotOnWhatsApp):
		return CodeNotOnWhatsApp
	case errors.Is(err, whatsmeow.ErrServerReturnedError):
		return CodeRejected
	case errors.Is(err, whatsmeow.ErrRecipientADJID), errors.Is(err, whatsmeow.ErrUnknownServer), errors.Is(err, whatsmeow.ErrNoPermissionInGroup):
		return CodeNotAuthorized
	}
	return CodeUnknown
}
