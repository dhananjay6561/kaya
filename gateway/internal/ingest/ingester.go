package ingest

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/logging"
)

// DB runs a function in one transaction.
type DB interface {
	InTx(ctx context.Context, fn func(pgx.Tx) error) error
}

// Ingester stores live WhatsApp events. Handle's result tells whatsmeow whether the event may be
// acknowledged: true once it is stored (or safely set aside), false when it must be delivered again.
type Ingester struct {
	db     DB
	store  *Store
	logger *slog.Logger

	// NewGroup is called, if set, when a message arrives from a group that had no row yet.
	NewGroup func()
	// HistoryQueued is called, if set, when a history chunk was announced and queued.
	HistoryQueued func()
	// RemoveFile is called, if set, with the SHA-256 of a downloaded file that a revoke left unused.
	RemoveFile func(sha256 string)

	attemptTimeout time.Duration
	retryFirst     time.Duration
	retryMax       time.Duration
	retryFor       time.Duration
}

// NewIngester returns an ingester writing through db.
func NewIngester(db DB, store *Store, logger *slog.Logger) *Ingester {
	return &Ingester{
		db: db, store: store, logger: logger,
		attemptTimeout: 10 * time.Second,
		retryFirst:     250 * time.Millisecond,
		retryMax:       4 * time.Second,
		retryFor:       30 * time.Second,
	}
}

// Handle stores one whatsmeow event. Events it does not store are acknowledged.
func (i *Ingester) Handle(ctx context.Context, own Own, evt any) (ok bool) {
	// whatsmeow treats a handler that panics as one that succeeded, and would acknowledge the event.
	// A message whose handling panics is set aside instead; anything else is simply not acknowledged.
	defer func() {
		if r := recover(); r != nil {
			i.logger.Error("the ingest handler panicked", "event", fmt.Sprintf("%T", evt))
			ok = false
			switch e := evt.(type) {
			case *events.Message:
				ok = i.settledMessage(ctx, e.Info, e.RawMessage)
			case *events.UndecryptableMessage:
				ok = i.settledMessage(ctx, e.Info, nil)
			}
		}
	}()
	switch e := evt.(type) {
	case *events.Connected:
		i.settled(ctx, "parked changes", func(tx pgx.Tx) error {
			_, err := i.store.ReplayParked(ctx, tx)
			return err
		})
		return true
	case *events.Message:
		if notification := e.Message.GetProtocolMessage().GetHistorySyncNotification(); notification != nil && e.Info.IsFromMe {
			return i.historyAnnounced(ctx, e.Info, notification)
		}
		return i.message(ctx, e.Info, e.RawMessage, func() Record { return FromMessage(e, own, SourceLive) })
	case *events.UndecryptableMessage:
		return i.message(ctx, e.Info, nil, func() Record { return FromUndecryptable(e, own) })
	case *events.GroupInfo:
		if e.Name == nil {
			return true
		}
		return i.chatName(ctx, e.JID, e.Name.Name, e.Name.NameSetAt)
	case *events.JoinedGroup:
		return i.chatName(ctx, e.JID, e.Name, e.NameSetAt)
	case *events.Contact:
		names := ContactNames{Address: address(e.JID, types.EmptyJID, Address{}), AddressBook: true}
		if e.Action != nil {
			names.Saved, names.First = e.Action.GetFullName(), e.Action.GetFirstName()
			names.Address = mergeAddress(names.Address, parseAddress(e.Action.GetLidJID()), parseAddress(e.Action.GetPnJID()))
		}
		return i.settled(ctx, "contact", func(tx pgx.Tx) error { return i.store.SetContactNames(ctx, tx, names) })
	default:
		return true
	}
}

const syncBatch = 200

// SyncGroups stores the names of every group in groups.
func (i *Ingester) SyncGroups(ctx context.Context, groups []*types.GroupInfo) error {
	for start := 0; start < len(groups); start += syncBatch {
		batch := groups[start:min(start+syncBatch, len(groups))]
		err := i.db.InTx(ctx, func(tx pgx.Tx) error {
			for _, g := range batch {
				if g == nil || g.JID.Server != types.GroupServer {
					continue
				}
				ref := ChatRef{Kind: ChatGroup, JID: g.JID.ToNonAD().String()}
				if err := i.store.SetChatName(ctx, tx, ref, g.Name, g.NameSetAt); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return scrub(err)
		}
	}
	return nil
}

// SyncContacts stores the names of every contact in contacts.
func (i *Ingester) SyncContacts(ctx context.Context, contacts []ContactNames) error {
	for start := 0; start < len(contacts); start += syncBatch {
		batch := contacts[start:min(start+syncBatch, len(contacts))]
		err := i.db.InTx(ctx, func(tx pgx.Tx) error {
			for _, names := range batch {
				if err := i.store.SetContactNames(ctx, tx, names); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return scrub(err)
		}
	}
	return nil
}

// historyAnnounced queues the chunk WhatsApp just announced. The announcement is acknowledged once
// the job row is stored; downloading and storing the chunk happens afterwards.
func (i *Ingester) historyAnnounced(ctx context.Context, info types.MessageInfo, notification *waE2E.HistorySyncNotification) bool {
	waID := info.ID
	raw, err := proto.Marshal(notification)
	if err != nil || waID == "" {
		i.logger.Error("a history announcement could not be queued", "error_code", "unreadable")
		return i.deadLetter(ctx, info, notification, "history_unreadable")
	}
	job := HistoryJob{
		WAID: waID, Notification: raw, SyncType: notification.GetSyncType().String(),
		ChunkOrder: int(notification.GetChunkOrder()),
	}
	if notification.Progress != nil {
		progress := int(min(notification.GetProgress(), 100))
		job.Progress = &progress
	}
	result, code := i.run(ctx, "history job", func(tx pgx.Tx) error { return i.store.AddHistoryJob(ctx, tx, job) })
	switch result {
	case unavailable:
		return false
	case rejected:
		return i.deadLetter(ctx, info, notification, "history_"+code)
	}
	if i.HistoryQueued != nil {
		i.HistoryQueued()
	}
	return true
}

func mergeAddress(base Address, others ...Address) Address {
	for _, o := range others {
		if base.LID == "" {
			base.LID = o.LID
		}
		if base.PN == "" {
			base.PN = o.PN
		}
		if base.Bot == "" {
			base.Bot = o.Bot
		}
	}
	return base
}

func (i *Ingester) chatName(ctx context.Context, jid types.JID, name string, setAt time.Time) bool {
	ref := ChatRef{Kind: ChatGroup, JID: jid.ToNonAD().String()}
	if jid.Server != types.GroupServer {
		return true
	}
	return i.settled(ctx, "chat name", func(tx pgx.Tx) error { return i.store.SetChatName(ctx, tx, ref, name, setAt) })
}

func (i *Ingester) message(ctx context.Context, info types.MessageInfo, raw proto.Message, mapper func() Record) bool {
	rec, code := safely(mapper)
	if code == "" {
		if rec.Op == OpSkip {
			return true
		}
		var applied Applied
		result, failure := i.run(ctx, "message", func(tx pgx.Tx) (err error) {
			applied, err = i.store.Apply(ctx, tx, rec)
			return err
		})
		switch result {
		case stored:
			i.afterStored(applied)
			return true
		case unavailable:
			return false
		}
		code = failure
	}
	return i.deadLetter(ctx, info, raw, code)
}

// safely runs the mapper and turns a panic into an error code, so one odd message cannot take the
// connection down or be acknowledged without a copy.
func safely(mapper func() Record) (rec Record, code string) {
	defer func() {
		if r := recover(); r != nil {
			code = "mapper_panic"
		}
	}()
	return mapper(), ""
}

// afterStored runs the follow-ups of a stored record. They happen after the commit, so a panic in
// one of them must not undo the acknowledgement.
func (i *Ingester) afterStored(applied Applied) {
	defer func() {
		if r := recover(); r != nil {
			i.logger.Error("a follow-up to a stored message panicked")
		}
	}()
	if applied.Parked {
		i.logger.Info("kept an edit or revoke for a message that is not stored yet")
	}
	if applied.NewGroup && i.NewGroup != nil {
		i.NewGroup()
	}
	if applied.RemovedFile != "" && i.RemoveFile != nil {
		i.RemoveFile(applied.RemovedFile)
	}
}

// settledMessage is the last stop for a message whose handling panicked: keep a copy and
// acknowledge it, unless the database is away.
func (i *Ingester) settledMessage(ctx context.Context, info types.MessageInfo, raw proto.Message) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	return i.deadLetter(ctx, info, raw, "handler_panic")
}

// deadLetter keeps a copy of a message that could not be stored and reports whether it may now be
// acknowledged. If the database refuses the copy as written (an id it cannot store, say), the ids
// are moved into the metadata in hex and it is tried once more. If even that is refused, the
// message is acknowledged anyway: refusing it for ever would block every message behind it.
func (i *Ingester) deadLetter(ctx context.Context, info types.MessageInfo, raw proto.Message, code string) bool {
	d := DeadLetter{
		WAID: info.ID, ChatJID: info.Chat.ToNonAD().String(), SenderJID: info.Sender.ToNonAD().String(), ErrorCode: code,
	}
	meta := map[string]any{
		"from_me": info.IsFromMe, "is_group": info.IsGroup, "type": info.Type, "media_type": info.MediaType,
		"edit": string(info.Edit), "category": info.Category,
	}
	if year := info.Timestamp.UTC().Year(); year > 0 && year < 10000 {
		meta["sent_at"] = info.Timestamp.UTC()
	}
	d.Info, _ = json.Marshal(meta)
	if raw != nil && !isNil(raw) {
		d.Raw, _ = proto.Marshal(raw)
	}
	return i.keepDeadLetter(ctx, d, meta)
}

func (i *Ingester) keepDeadLetter(ctx context.Context, d DeadLetter, meta map[string]any) bool {
	result, _ := i.run(ctx, "dead letter", func(tx pgx.Tx) error { return i.store.AddDeadLetter(ctx, tx, d) })
	if result == rejected {
		meta["wa_id_hex"] = hex.EncodeToString([]byte(d.WAID))
		meta["chat_jid_hex"] = hex.EncodeToString([]byte(d.ChatJID))
		meta["sender_jid_hex"] = hex.EncodeToString([]byte(d.SenderJID))
		plain := DeadLetter{ErrorCode: d.ErrorCode, Raw: d.Raw}
		plain.Info, _ = json.Marshal(meta)
		result, _ = i.run(ctx, "dead letter", func(tx pgx.Tx) error { return i.store.AddDeadLetter(ctx, tx, plain) })
	}
	switch result {
	case stored:
		i.logger.Error("a message could not be stored and was set aside in ingest_dead", "error_code", d.ErrorCode)
		return true
	case unavailable:
		i.logger.Error("could not keep a copy of a message that failed to store; it will be delivered again", "error_code", d.ErrorCode)
		return false
	default:
		i.logger.Error("a message could not be stored and no copy could be kept; acknowledging it so ingest can go on", "error_code", d.ErrorCode)
		return true
	}
}

func isNil(m proto.Message) bool {
	return m == nil || !m.ProtoReflect().IsValid()
}

type outcome int

const (
	stored outcome = iota
	// unavailable: the database could not be reached for the whole retry window.
	unavailable
	// rejected: the database or the contract refused the data; retrying cannot help.
	rejected
)

// run tries op in a transaction until it commits, the retry window ends, or it fails in a way that
// retrying cannot fix. It never sleeps while holding a connection.
func (i *Ingester) run(ctx context.Context, what string, op func(pgx.Tx) error) (result outcome, code string) {
	deadline := time.Now().Add(i.retryFor)
	delay := i.retryFirst
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, i.attemptTimeout)
		err := i.db.InTx(attemptCtx, op)
		cancel()
		if err == nil {
			return stored, ""
		}
		if code, permanent := classify(err); permanent {
			i.logger.Error("storing failed and will not be retried", "what", what, "error_code", code, logging.Err(scrub(err)))
			return rejected, code
		}
		if ctx.Err() != nil || time.Now().Add(delay).After(deadline) {
			i.logger.Error("database unavailable; the event will be delivered again", "what", what, logging.Err(scrub(err)))
			return unavailable, ""
		}
		select {
		case <-ctx.Done():
			return unavailable, ""
		case <-time.After(delay):
		}
		delay = min(delay*2, i.retryMax)
	}
}

// settled runs op for an event that is not worth a dead letter (a name change): it reports false
// only while the database is away, and true once it is stored or refused. WhatsApp sends a group
// change again after a false; a contact change is not sent again, and the contact sync on the next
// connect picks the name up instead.
func (i *Ingester) settled(ctx context.Context, what string, op func(pgx.Tx) error) bool {
	result, _ := i.run(ctx, what, op)
	return result != unavailable
}

// classify tells the database being away (worth waiting for) from everything else. Only failures
// of the connection itself count as away: a refused value, a broken constraint or an error raised
// before the query was even sent would fail the same way every time, so they are permanent and get
// a short code.
func classify(err error) (code string, permanent bool) {
	if errors.Is(err, contracts.ErrInvalid) {
		return "contract_invalid", true
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch {
		case strings.HasPrefix(pg.Code, "08"), strings.HasPrefix(pg.Code, "53"), strings.HasPrefix(pg.Code, "57"),
			pg.Code == "40001", pg.Code == "40P01":
			return "", false
		}
		return pg.Code, true
	}
	var connect *pgconn.ConnectError
	var network net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, pgx.ErrTxClosed), errors.Is(err, pgx.ErrTxCommitRollback),
		errors.As(err, &connect), errors.As(err, &network),
		pgconn.SafeToRetry(err), strings.Contains(err.Error(), "conn closed"), strings.Contains(err.Error(), "closed pool"):
		return "", false
	}
	return "client_error", true
}

// scrub keeps a database error's code and field names and drops its detail, which can quote the
// values of the row that failed.
func scrub(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return fmt.Errorf("postgres %s on %s.%s (%s)", pg.Code, pg.TableName, pg.ColumnName, pg.ConstraintName)
	}
	return err
}
