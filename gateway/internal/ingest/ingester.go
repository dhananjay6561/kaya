package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
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
func (i *Ingester) Handle(ctx context.Context, own Own, evt any) bool {
	switch e := evt.(type) {
	case *events.Message:
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
			if applied.NoTarget {
				i.logger.Info("dropped a change to a message that is not stored", "op", int(rec.Op))
			}
			if applied.NewGroup && i.NewGroup != nil {
				i.NewGroup()
			}
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

func (i *Ingester) deadLetter(ctx context.Context, info types.MessageInfo, raw proto.Message, code string) bool {
	d := DeadLetter{
		WAID: info.ID, ChatJID: info.Chat.ToNonAD().String(), SenderJID: info.Sender.ToNonAD().String(), ErrorCode: code,
	}
	d.Info, _ = json.Marshal(map[string]any{
		"sent_at": info.Timestamp.UTC(), "from_me": info.IsFromMe, "is_group": info.IsGroup,
		"type": info.Type, "media_type": info.MediaType, "edit": string(info.Edit), "category": info.Category,
	})
	if raw != nil && !isNil(raw) {
		d.Raw, _ = proto.Marshal(raw)
	}
	result, _ := i.run(ctx, "dead letter", func(tx pgx.Tx) error { return i.store.AddDeadLetter(ctx, tx, d) })
	if result != stored {
		i.logger.Error("could not keep a copy of a message that failed to store; it will be delivered again", "error_code", code)
		return false
	}
	i.logger.Error("a message could not be stored and was set aside in ingest_dead", "error_code", code)
	return true
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
// only while the database is away, so the event comes back, and true once it is stored or refused.
func (i *Ingester) settled(ctx context.Context, what string, op func(pgx.Tx) error) bool {
	result, _ := i.run(ctx, what, op)
	return result != unavailable
}

// classify tells failures that retrying can fix (the database is away, a deadlock) from ones it
// cannot (a constraint, a payload that breaks the contract), and names the latter with a short code.
func classify(err error) (code string, permanent bool) {
	if errors.Is(err, contracts.ErrInvalid) {
		return "contract_invalid", true
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return "", false
	}
	switch {
	case strings.HasPrefix(pg.Code, "08"), strings.HasPrefix(pg.Code, "53"), strings.HasPrefix(pg.Code, "57"),
		pg.Code == "40001", pg.Code == "40P01":
		return "", false
	}
	return pg.Code, true
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
