package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/events"
)

// Applied says what storing a record did.
type Applied struct {
	// Changed is true when a message row was inserted or changed and an event was emitted.
	Changed bool
	// NewGroup is true when the record created a group chat row, whose name is not known yet.
	NewGroup bool
	// NoTarget is true when an edit, revoke or reaction pointed at a message that is not stored.
	NoTarget bool
}

// Store writes records inside the caller's transaction, together with the event for each change.
type Store struct {
	outbox *events.Outbox
}

// NewStore returns a store that emits its events through outbox.
func NewStore(outbox *events.Outbox) *Store {
	return &Store{outbox: outbox}
}

func (s *Store) contact(ctx context.Context, tx pgx.Tx, a Address) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, "SELECT contact_id_for(NULLIF($1, ''), NULLIF($2, ''), NULLIF($3, ''))", a.LID, a.PN, a.Bot).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("resolve contact: %w", err)
	}
	return id, nil
}

// findChat returns the chat's id, or 0 when it is not stored.
func (s *Store) findChat(ctx context.Context, tx pgx.Tx, ref ChatRef) (int64, error) {
	var id int64
	var err error
	if ref.Kind == ChatDirect {
		var contactID int64
		if contactID, err = s.contact(ctx, tx, ref.Contact); err != nil {
			return 0, err
		}
		err = tx.QueryRow(ctx, "SELECT id FROM chats WHERE contact_id = $1", contactID).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, "SELECT id FROM chats WHERE jid = $1", ref.JID).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("find chat: %w", err)
	}
	return id, nil
}

func (s *Store) ensureChat(ctx context.Context, tx pgx.Tx, ref ChatRef) (id int64, created bool, err error) {
	if id, err = s.findChat(ctx, tx, ref); err != nil || id != 0 {
		return id, false, err
	}
	if ref.Kind == ChatDirect {
		var contactID int64
		if contactID, err = s.contact(ctx, tx, ref.Contact); err != nil {
			return 0, false, err
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO chats (kind, contact_id) VALUES ('direct', $1)
			ON CONFLICT (contact_id) DO NOTHING RETURNING id`, contactID).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, `
			INSERT INTO chats (kind, jid) VALUES ($1, $2)
			ON CONFLICT (jid) DO NOTHING RETURNING id`, ref.Kind, ref.JID).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		id, err = s.findChat(ctx, tx, ref)
		return id, false, err
	}
	if err != nil {
		return 0, false, fmt.Errorf("create chat: %w", err)
	}
	return id, true, nil
}

// Apply stores rec. Storing the same record twice changes nothing and emits nothing.
func (s *Store) Apply(ctx context.Context, tx pgx.Tx, rec Record) (Applied, error) {
	if rec.Op == OpSkip {
		return Applied{}, nil
	}
	senderID, err := s.contact(ctx, tx, rec.Sender)
	if err != nil {
		return Applied{}, err
	}
	if err := s.setSeenNames(ctx, tx, senderID, rec.PushName, rec.BusinessName); err != nil {
		return Applied{}, err
	}
	switch rec.Op {
	case OpStore:
		return s.storeMessage(ctx, tx, rec, senderID)
	case OpEdit, OpRevoke:
		return s.changeMessage(ctx, tx, rec)
	case OpReact:
		return s.react(ctx, tx, rec, senderID)
	default:
		return Applied{}, fmt.Errorf("unknown record op %d", rec.Op)
	}
}

func (s *Store) setSeenNames(ctx context.Context, tx pgx.Tx, contactID int64, pushName, businessName string) error {
	if pushName != "" {
		if _, err := tx.Exec(ctx, "UPDATE contacts SET push_name = $2 WHERE id = $1 AND push_name IS DISTINCT FROM $2", contactID, pushName); err != nil {
			return fmt.Errorf("store push name: %w", err)
		}
	}
	if businessName != "" {
		if _, err := tx.Exec(ctx, "UPDATE contacts SET business_name = $2 WHERE id = $1 AND business_name IS DISTINCT FROM $2", contactID, businessName); err != nil {
			return fmt.Errorf("store business name: %w", err)
		}
	}
	return nil
}

func (s *Store) storeMessage(ctx context.Context, tx pgx.Tx, rec Record, senderID int64) (Applied, error) {
	chatID, created, err := s.ensureChat(ctx, tx, rec.Chat)
	if err != nil {
		return Applied{}, err
	}
	out := Applied{NewGroup: created && rec.Chat.Kind == ChatGroup}

	var replyID *string
	var replySender *int64
	if rec.ReplyTo != nil {
		id, err := s.contact(ctx, tx, rec.ReplyTo.Sender)
		if err != nil {
			return Applied{}, err
		}
		replyID, replySender = &rec.ReplyTo.WAID, &id
	}

	var messageID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, reply_to_wa_id,
			reply_to_sender_id, source, view_once, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (chat_id, wa_id, sender_id) DO NOTHING
		RETURNING id`,
		chatID, rec.WAID, senderID, rec.FromMe, rec.At, rec.Kind, rec.Body, replyID, replySender, rec.Source,
		rec.ViewOnce, rec.ExpiresAt).Scan(&messageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return Applied{}, fmt.Errorf("store message: %w", err)
	}
	_, err = tx.Exec(ctx, "UPDATE chats SET last_message_at = $2 WHERE id = $1 AND (last_message_at IS NULL OR last_message_at < $2)", chatID, rec.At)
	if err != nil {
		return Applied{}, fmt.Errorf("update chat: %w", err)
	}
	out.Changed = true
	return out, s.emit(ctx, tx, contracts.MessageStored{
		MessageID: messageID, ChatID: chatID, SenderID: senderID, WAID: rec.WAID, FromMe: rec.FromMe,
		Kind: rec.Kind, Source: rec.Source, Change: "new", SentAt: contracts.At(rec.At),
	})
}

func (s *Store) changeMessage(ctx context.Context, tx pgx.Tx, rec Record) (Applied, error) {
	chatID, err := s.findChat(ctx, tx, rec.Chat)
	if err != nil {
		return Applied{}, err
	}
	if chatID == 0 {
		return Applied{NoTarget: true}, nil
	}
	targetSender, err := s.contact(ctx, tx, rec.Target.Sender)
	if err != nil {
		return Applied{}, err
	}

	query, change := `
		UPDATE messages SET body = $4, edited_at = $5
		WHERE chat_id = $1 AND wa_id = $2 AND sender_id = $3 AND deleted_at IS NULL
			AND (edited_at IS NULL OR edited_at < $5)
		RETURNING id, from_me, kind, source, sent_at`, "edited"
	args := []any{chatID, rec.Target.WAID, targetSender, rec.Body, rec.At}
	if rec.Op == OpRevoke {
		query, change = `
			UPDATE messages SET body = NULL, deleted_at = $4
			WHERE chat_id = $1 AND wa_id = $2 AND sender_id = $3 AND deleted_at IS NULL
			RETURNING id, from_me, kind, source, sent_at`, "deleted"
		args = []any{chatID, rec.Target.WAID, targetSender, rec.At}
	}

	stored := contracts.MessageStored{ChatID: chatID, SenderID: targetSender, WAID: rec.Target.WAID, Change: change}
	var sentAt time.Time
	err = tx.QueryRow(ctx, query, args...).Scan(&stored.MessageID, &stored.FromMe, &stored.Kind, &stored.Source, &sentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		err = tx.QueryRow(ctx, "SELECT EXISTS (SELECT FROM messages WHERE chat_id = $1 AND wa_id = $2 AND sender_id = $3)",
			chatID, rec.Target.WAID, targetSender).Scan(&exists)
		return Applied{NoTarget: !exists}, err
	}
	if err != nil {
		return Applied{}, fmt.Errorf("change message: %w", err)
	}
	stored.SentAt = contracts.At(sentAt)
	return Applied{Changed: true}, s.emit(ctx, tx, stored)
}

func (s *Store) react(ctx context.Context, tx pgx.Tx, rec Record, senderID int64) (Applied, error) {
	chatID, err := s.findChat(ctx, tx, rec.Chat)
	if err != nil {
		return Applied{}, err
	}
	if chatID == 0 {
		return Applied{NoTarget: true}, nil
	}
	targetSender, err := s.contact(ctx, tx, rec.Target.Sender)
	if err != nil {
		return Applied{}, err
	}
	if rec.Emoji == "" {
		_, err = tx.Exec(ctx, `
			DELETE FROM reactions
			WHERE chat_id = $1 AND target_wa_id = $2 AND target_sender_id = $3 AND sender_id = $4 AND reacted_at <= $5`,
			chatID, rec.Target.WAID, targetSender, senderID, rec.At)
	} else {
		_, err = tx.Exec(ctx, `
			INSERT INTO reactions (chat_id, target_wa_id, target_sender_id, sender_id, emoji, reacted_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (chat_id, target_wa_id, target_sender_id, sender_id) DO UPDATE
			SET emoji = EXCLUDED.emoji, reacted_at = EXCLUDED.reacted_at
			WHERE reactions.reacted_at < EXCLUDED.reacted_at`,
			chatID, rec.Target.WAID, targetSender, senderID, rec.Emoji, rec.At)
	}
	if err != nil {
		return Applied{}, fmt.Errorf("store reaction: %w", err)
	}
	return Applied{}, nil
}

func (s *Store) emit(ctx context.Context, tx pgx.Tx, stored contracts.MessageStored) error {
	event, err := contracts.NewEvent(contracts.EventMessageStored, stored)
	if err != nil {
		return err
	}
	return s.outbox.Add(ctx, tx, event)
}

// SetChatName records the name of a group or broadcast list, creating the chat row if needed. A
// name with a set time replaces an older one; a name without one only fills an empty name.
func (s *Store) SetChatName(ctx context.Context, tx pgx.Tx, ref ChatRef, name string, setAt time.Time) error {
	if name == "" || ref.Kind == ChatDirect || ref.JID == "" {
		return nil
	}
	var at *time.Time
	if !setAt.IsZero() {
		at = &setAt
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO chats (kind, jid, name, name_set_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (jid) DO UPDATE SET name = EXCLUDED.name, name_set_at = EXCLUDED.name_set_at
		WHERE chats.name IS NULL
			OR (EXCLUDED.name_set_at IS NOT NULL AND chats.name IS DISTINCT FROM EXCLUDED.name
				AND (chats.name_set_at IS NULL OR chats.name_set_at < EXCLUDED.name_set_at))`,
		ref.Kind, ref.JID, name, at)
	if err != nil {
		return fmt.Errorf("store chat name: %w", err)
	}
	return nil
}

// ContactNames are the names known for one person. Saved and First come from the address book and
// are stored as given, so clearing a contact clears them; Push and Business are only ever filled.
type ContactNames struct {
	Address  Address
	Saved    string
	First    string
	Push     string
	Business string
	// AddressBook is true when Saved and First are authoritative, including when they are empty.
	AddressBook bool
}

// SetContactNames creates the contact if needed and writes the names that changed.
func (s *Store) SetContactNames(ctx context.Context, tx pgx.Tx, names ContactNames) error {
	if names.Address.IsZero() {
		return nil
	}
	id, err := s.contact(ctx, tx, names.Address)
	if err != nil {
		return err
	}
	if names.AddressBook {
		_, err = tx.Exec(ctx, `
			UPDATE contacts SET saved_name = NULLIF($2, ''), first_name = NULLIF($3, '')
			WHERE id = $1 AND (saved_name IS DISTINCT FROM NULLIF($2, '') OR first_name IS DISTINCT FROM NULLIF($3, ''))`,
			id, names.Saved, names.First)
		if err != nil {
			return fmt.Errorf("store saved name: %w", err)
		}
	}
	return s.setSeenNames(ctx, tx, id, names.Push, names.Business)
}

// DeadLetter is a message that could not be stored, kept so it can be replayed after a fix.
type DeadLetter struct {
	WAID      string
	ChatJID   string
	SenderJID string
	ErrorCode string
	Info      []byte
	Raw       []byte
}

// AddDeadLetter stores d.
func (s *Store) AddDeadLetter(ctx context.Context, tx pgx.Tx, d DeadLetter) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO ingest_dead (wa_id, chat_jid, sender_jid, error_code, info, raw)
		VALUES (NULLIF($1, ''), NULLIF($2, ''), NULLIF($3, ''), $4, $5, $6)`,
		d.WAID, d.ChatJID, d.SenderJID, d.ErrorCode, d.Info, d.Raw)
	if err != nil {
		return fmt.Errorf("store dead letter: %w", err)
	}
	return nil
}
