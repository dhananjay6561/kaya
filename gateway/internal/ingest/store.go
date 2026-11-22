package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/events"
)

// Applied says what storing a record did.
type Applied struct {
	// Changed is true when a message row was inserted or changed.
	Changed bool
	// NewChat is true when the record created its chat row.
	NewChat bool
	// Parked is true when an edit or revoke had no target yet and was kept for later.
	Parked bool
	// RemovedFile is the SHA-256 of a downloaded file that no stored message refers to any more,
	// after a revoke. The caller deletes the file once the transaction has committed.
	RemovedFile string
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
	if rec.Source == SourceHistory {
		err = s.fillSeenNames(ctx, tx, senderID, rec.PushName, rec.BusinessName)
	} else {
		err = s.setSeenNames(ctx, tx, senderID, rec.PushName, rec.BusinessName)
	}
	if err != nil {
		return Applied{}, err
	}
	switch rec.Op {
	case OpStore:
		return s.storeMessage(ctx, tx, rec, senderID)
	case OpEdit, OpRevoke:
		out, err := s.changeMessage(ctx, tx, rec)
		if err == nil && out.NoTarget && rec.Source != SourceHistory {
			out.Parked = true
			err = s.park(ctx, tx, rec)
		}
		return out, err
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

// fillSeenNames stores names only where none is stored: history carries names as they were then.
func (s *Store) fillSeenNames(ctx context.Context, tx pgx.Tx, contactID int64, pushName, businessName string) error {
	if pushName == "" && businessName == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE contacts SET push_name = COALESCE(push_name, NULLIF($2, '')), business_name = COALESCE(business_name, NULLIF($3, ''))
		WHERE id = $1 AND ((push_name IS NULL AND $2 <> '') OR (business_name IS NULL AND $3 <> ''))`,
		contactID, pushName, businessName)
	if err != nil {
		return fmt.Errorf("fill names: %w", err)
	}
	return nil
}

func (s *Store) storeMessage(ctx context.Context, tx pgx.Tx, rec Record, senderID int64) (Applied, error) {
	chatID, created, err := s.ensureChat(ctx, tx, rec.Chat)
	if err != nil {
		return Applied{}, err
	}
	out := Applied{NewChat: created, NewGroup: created && rec.Chat.Kind == ChatGroup}

	var replyID *string
	var replySender *int64
	if rec.ReplyTo != nil {
		id, err := s.contact(ctx, tx, rec.ReplyTo.Sender)
		if err != nil {
			return Applied{}, err
		}
		replyID, replySender = &rec.ReplyTo.WAID, &id
	}

	// A live copy of a message that history stored first becomes live, so core still hears of it.
	var messageID int64
	var inserted bool
	err = tx.QueryRow(ctx, `
		INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, reply_to_wa_id,
			reply_to_sender_id, source, view_once, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (chat_id, wa_id, sender_id) DO UPDATE SET source = 'live'
			WHERE messages.source = 'history' AND EXCLUDED.source = 'live'
		RETURNING id, xmax = 0`,
		chatID, rec.WAID, senderID, rec.FromMe, rec.At, rec.Kind, rec.Body, replyID, replySender, rec.Source,
		rec.ViewOnce, rec.ExpiresAt).Scan(&messageID, &inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return Applied{}, fmt.Errorf("store message: %w", err)
	}
	out.Changed = true
	if inserted {
		_, err = tx.Exec(ctx, "UPDATE chats SET last_message_at = $2 WHERE id = $1 AND (last_message_at IS NULL OR last_message_at < $2)", chatID, rec.At)
		if err != nil {
			return Applied{}, fmt.Errorf("update chat: %w", err)
		}
		if err := s.storeMedia(ctx, tx, messageID, rec.Media); err != nil {
			return Applied{}, err
		}
	}
	if rec.Source == SourceHistory {
		return out, nil
	}
	if err := s.emit(ctx, tx, contracts.MessageStored{
		MessageID: messageID, ChatID: chatID, SenderID: senderID, WAID: rec.WAID, FromMe: rec.FromMe,
		Kind: rec.Kind, Source: SourceLive, Change: "new", SentAt: contracts.At(rec.At),
	}); err != nil {
		return Applied{}, err
	}
	// An edit or revoke may have arrived before this message; apply it now that its target exists.
	var waiting bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT FROM pending_changes)").Scan(&waiting); err != nil {
		return Applied{}, fmt.Errorf("check parked changes: %w", err)
	}
	if waiting {
		if _, err := s.ReplayParked(ctx, tx); err != nil {
			return Applied{}, err
		}
	}
	return out, nil
}

// storeMedia records a message's file. The keys go in their own table, which only the gateway can
// read, and only when everything needed to fetch the file is there.
func (s *Store) storeMedia(ctx context.Context, tx pgx.Tx, messageID int64, m *Media) error {
	if m == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO media (message_id, mime_type, size_bytes, file_name, duration_s, width, height, sha256)
		VALUES ($1, NULLIF($2, ''), $3, NULLIF($4, ''), NULLIF($5, 0), NULLIF($6, 0), NULLIF($7, 0), NULLIF($8, ''))`,
		messageID, m.MimeType, m.Size, m.FileName, m.DurationS, m.Width, m.Height, m.SHA256)
	if err != nil {
		return fmt.Errorf("store media: %w", err)
	}
	if !m.Fetchable() {
		return nil
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO media_keys (message_id, media_key, direct_path, file_enc_sha256, media_type)
		VALUES ($1, $2, $3, $4, $5)`, messageID, m.Key, m.DirectPath, m.EncSHA256, m.Type)
	if err != nil {
		return fmt.Errorf("store media keys: %w", err)
	}
	return nil
}

// removeMedia forgets a revoked message's file: its rows go, and if no other stored message has
// the same file, the file's hash is returned so the caller can delete it.
func (s *Store) removeMedia(ctx context.Context, tx pgx.Tx, messageID int64) (string, error) {
	var sha *string
	var state string
	err := tx.QueryRow(ctx, "DELETE FROM media WHERE message_id = $1 RETURNING sha256, state", messageID).Scan(&sha, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("remove media: %w", err)
	}
	if state != "stored" || sha == nil {
		return "", nil
	}
	var shared bool
	err = tx.QueryRow(ctx, "SELECT EXISTS (SELECT FROM media WHERE sha256 = $1 AND state = 'stored')", *sha).Scan(&shared)
	if err != nil || shared {
		return "", err
	}
	return *sha, nil
}

func (s *Store) park(ctx context.Context, tx pgx.Tx, rec Record) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode parked change: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO pending_changes (record) VALUES ($1)", raw); err != nil {
		return fmt.Errorf("park change: %w", err)
	}
	return nil
}

const parkedFor = 7 * 24 * time.Hour

// ReplayParked applies the edits and revokes that were waiting for their message, and drops the
// ones that have waited too long. It returns how many it applied.
func (s *Store) ReplayParked(ctx context.Context, tx pgx.Tx) (int, error) {
	if _, err := tx.Exec(ctx, "DELETE FROM pending_changes WHERE created_at < $1", time.Now().Add(-parkedFor)); err != nil {
		return 0, fmt.Errorf("purge parked changes: %w", err)
	}
	rows, err := tx.Query(ctx, "SELECT id, record FROM pending_changes ORDER BY id")
	if err != nil {
		return 0, fmt.Errorf("read parked changes: %w", err)
	}
	type parked struct {
		id  int64
		rec Record
	}
	var all []parked
	for rows.Next() {
		var p parked
		var raw []byte
		if err := rows.Scan(&p.id, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		if json.Unmarshal(raw, &p.rec) == nil {
			all = append(all, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	applied := 0
	for _, p := range all {
		out, err := s.changeMessage(ctx, tx, p.rec)
		if err != nil {
			return applied, err
		}
		if out.NoTarget {
			continue
		}
		if _, err := tx.Exec(ctx, "DELETE FROM pending_changes WHERE id = $1", p.id); err != nil {
			return applied, fmt.Errorf("clear parked change: %w", err)
		}
		applied++
	}
	return applied, nil
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
	out := Applied{Changed: true}
	if rec.Op == OpRevoke {
		if out.RemovedFile, err = s.removeMedia(ctx, tx, stored.MessageID); err != nil {
			return Applied{}, err
		}
	}
	return out, s.emit(ctx, tx, stored)
}

// react stores a reaction even when its message, or its whole chat, is not stored yet: reactions
// can arrive first, which is why the table has no foreign key to messages.
func (s *Store) react(ctx context.Context, tx pgx.Tx, rec Record, senderID int64) (Applied, error) {
	chatID, created, err := s.ensureChat(ctx, tx, rec.Chat)
	if err != nil {
		return Applied{}, err
	}
	out := Applied{NewChat: created, NewGroup: created && rec.Chat.Kind == ChatGroup}
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
	return out, nil
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
	// FillOnly keeps a name that is already stored: for old names, such as those in a history sync.
	FillOnly bool
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
	if names.FillOnly {
		return s.fillSeenNames(ctx, tx, id, names.Push, names.Business)
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
		SELECT NULLIF($1, ''), NULLIF($2, ''), NULLIF($3, ''), $4, $5, $6
		WHERE NOT EXISTS (
			SELECT FROM ingest_dead
			WHERE wa_id IS NOT DISTINCT FROM NULLIF($1, '') AND chat_jid IS NOT DISTINCT FROM NULLIF($2, '')
				AND sender_jid IS NOT DISTINCT FROM NULLIF($3, '') AND error_code = $4)`,
		d.WAID, d.ChatJID, d.SenderJID, d.ErrorCode, d.Info, d.Raw)
	if err != nil {
		return fmt.Errorf("store dead letter: %w", err)
	}
	return nil
}
