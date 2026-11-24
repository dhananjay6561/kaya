package channel

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/kit/logging"
	"github.com/dhananjay6561/kaya/kit/store"
)

// Deliver hands one command to whoever handles it. ctx ends when the inbox stops.
type Deliver func(ctx context.Context, m IncomingMessage) (Handled, error)

// InboxOptions configure an Inbox. Zero values take their defaults.
type InboxOptions struct {
	DB     *store.DB
	Logger *slog.Logger
	// IsUrgent marks commands that control Kaya itself: they go first and are never dropped by the cap.
	IsUrgent func(text string) bool

	Poll         time.Duration
	RetryMin     time.Duration
	RetryMax     time.Duration
	MaxAttempts  int
	MaxPerMinute int
	Retention    time.Duration
	PurgeEvery   time.Duration
	// PurgeAlways runs the clean-up after every idle round, for tests.
	PurgeAlways bool
}

// Inbox is core's queue of things DJ told Kaya to do (decision 64): it records them in the same
// transaction as the event that brought them, and one worker across every core process hands them
// over one at a time.
type Inbox struct {
	o    InboxOptions
	wake chan struct{}

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
}

const inboxLock = 5011603

var errorCode = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// NewInbox returns a stopped inbox.
func NewInbox(o InboxOptions) *Inbox {
	if o.Poll == 0 {
		o.Poll = 30 * time.Second
	}
	if o.RetryMin == 0 {
		o.RetryMin = time.Second
	}
	if o.RetryMax == 0 {
		o.RetryMax = 30 * time.Second
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = 5
	}
	if o.MaxPerMinute == 0 {
		o.MaxPerMinute = 20
	}
	if o.Retention == 0 {
		o.Retention = 30 * 24 * time.Hour
	}
	if o.PurgeEvery == 0 {
		o.PurgeEvery = time.Hour
	}
	if o.IsUrgent == nil {
		o.IsUrgent = func(string) bool { return false }
	}
	return &Inbox{o: o, wake: make(chan struct{}, 1)}
}

// Record makes a stored message a command if, and only if, DJ typed it as live text in his own
// chat, from the linked account, and it is not something the gateway sent. It reports whether a
// command was added.
func (in *Inbox) Record(ctx context.Context, tx pgx.Tx, m contracts.MessageStored) (bool, error) {
	typedByDJ := m.FromMe && m.Kind == "text" && m.Source == "live" && m.Change == "new"
	if !typedByDJ {
		return false, nil
	}
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO commands (message_id, wa_id, sent_at)
		SELECT m.id, m.wa_id, m.sent_at
		FROM messages m
		JOIN chats c ON c.id = m.chat_id
		JOIN contacts own ON own.id = m.sender_id
		JOIN connection_state linked ON linked.own_jid IN (own.pn, own.lid)
		WHERE m.id = $1
		  AND m.from_me AND m.kind = 'text' AND m.source = 'live' AND m.deleted_at IS NULL
		  AND NOT m.forwarded
		  AND m.sent_at > now() - interval '1 day'
		  AND c.kind = 'direct' AND c.contact_id = m.sender_id
		  AND NOT EXISTS (SELECT 1 FROM send_requests sent WHERE sent.wa_id = m.wa_id)
		ON CONFLICT (message_id) DO NOTHING
		RETURNING id`, m.MessageID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var body *string
	if err := tx.QueryRow(ctx, "SELECT body FROM messages WHERE id = $1", m.MessageID).Scan(&body); err != nil {
		return false, err
	}
	if body != nil && in.o.IsUrgent(*body) {
		if _, err := tx.Exec(ctx, "UPDATE commands SET urgent = true WHERE id = $1", id); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Wake makes the worker look for commands now instead of at its next poll. It never blocks.
func (in *Inbox) Wake() {
	select {
	case in.wake <- struct{}{}:
	default:
	}
}

// Start begins handing commands to deliver whenever ready says someone can take them.
func (in *Inbox) Start(deliver Deliver, ready func() bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.running {
		return
	}
	if in.done != nil {
		<-in.done
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	in.cancel, in.done, in.running = cancel, done, true
	go func() {
		in.run(ctx, deliver, ready)
		in.mu.Lock()
		if in.done == done {
			in.running = false
		}
		in.mu.Unlock()
		close(done)
	}()
}

// Stop ends the worker at once, cancelling the command it is handing over; that command stays
// pending and its attempt is not counted. Every caller waits for the worker to end.
func (in *Inbox) Stop() {
	in.mu.Lock()
	cancel, done := in.cancel, in.done
	in.running = false
	in.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

type lockHolder struct {
	conn *pgx.Conn
}

func (in *Inbox) run(ctx context.Context, deliver Deliver, ready func() bool) {
	lock := &lockHolder{}
	defer lock.release()
	delay := in.o.RetryMin
	failing := false
	nextPurge := time.Time{}
	for ctx.Err() == nil {
		worked, err := in.step(ctx, lock, deliver, ready)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !failing {
				in.o.Logger.Warn("could not work through commands, trying again", logging.Err(err))
			}
			failing = true
			in.sleep(ctx, delay)
			delay = min(delay*2, in.o.RetryMax)
			continue
		}
		if failing {
			in.o.Logger.Info("commands are being handled again")
		}
		failing, delay = false, in.o.RetryMin
		if worked {
			continue
		}
		if in.o.PurgeAlways || !time.Now().Before(nextPurge) {
			nextPurge = time.Now().Add(in.o.PurgeEvery)
			if _, err := in.o.DB.Exec(ctx,
				"DELETE FROM commands WHERE finished_at < now() - make_interval(secs => $1)",
				in.o.Retention.Seconds()); err != nil && ctx.Err() == nil {
				in.o.Logger.Warn("could not delete old commands", logging.Err(err))
			}
		}
		in.sleep(ctx, in.o.Poll)
	}
}

func (in *Inbox) sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-in.wake:
	case <-timer.C:
	}
}

// holds takes the worker lock if this inbox does not hold it yet. Only one worker may hand commands
// over, or two core processes would each run every command; the lock lives on one connection and is
// gone with it, so a crashed worker never blocks the next.
func (lock *lockHolder) holds(ctx context.Context, db *store.DB) (bool, error) {
	if lock.conn != nil && !lock.conn.IsClosed() {
		if err := lock.conn.Ping(ctx); err == nil {
			return true, nil
		}
	}
	lock.release()
	conn, err := db.Hijack(ctx)
	if err != nil {
		return false, err
	}
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", inboxLock).Scan(&locked); err != nil {
		_ = conn.Close(context.Background())
		return false, err
	}
	if !locked {
		_ = conn.Close(context.Background())
		return false, nil
	}
	lock.conn = conn
	return true, nil
}

func (lock *lockHolder) release() {
	if lock.conn != nil {
		_ = lock.conn.Close(context.Background())
		lock.conn = nil
	}
}

type commandRow struct {
	id        int64
	messageID int64
	waID      string
	sentAt    time.Time
	createdAt time.Time
	attempts  int
	urgent    bool
}

func (in *Inbox) step(ctx context.Context, lock *lockHolder, deliver Deliver, ready func() bool) (bool, error) {
	if !ready() {
		return false, nil
	}
	held, err := lock.holds(ctx, in.o.DB)
	if err != nil || !held {
		return false, err
	}
	var row commandRow
	err = in.o.DB.QueryRow(ctx, `
		UPDATE commands SET attempts = attempts + 1
		WHERE id = (SELECT id FROM commands WHERE state = 'pending' ORDER BY urgent DESC, id LIMIT 1)
		RETURNING id, message_id, wa_id, sent_at, created_at, attempts, urgent`).
		Scan(&row.id, &row.messageID, &row.waID, &row.sentAt, &row.createdAt, &row.attempts, &row.urgent)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if row.attempts > in.o.MaxAttempts {
		in.o.Logger.Error("a command kept being interrupted and was given up on", "command_id", row.id)
		return true, in.finish(ctx, row, "interrupted", "")
	}

	var body *string
	err = in.o.DB.QueryRow(ctx, "SELECT body FROM messages WHERE id = $1 AND deleted_at IS NULL", row.messageID).Scan(&body)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	text := ""
	if body != nil {
		text = strings.TrimSpace(*body)
	}
	if text == "" {
		return true, in.finish(ctx, row, "message_gone", "")
	}

	if !row.urgent {
		var recent int
		err := in.o.DB.QueryRow(ctx, `SELECT count(*) FROM commands
			WHERE finished_at > now() - interval '1 minute' AND error IS DISTINCT FROM 'over_cap'`).Scan(&recent)
		if err != nil {
			return false, err
		}
		if recent >= in.o.MaxPerMinute {
			in.o.Logger.Error("too many commands in a minute; this one was dropped", "command_id", row.id)
			return true, in.finish(ctx, row, "over_cap", "")
		}
	}

	handled, err := deliver(ctx, IncomingMessage{ID: row.waID, Text: text, SentAt: row.sentAt, ReceivedAt: row.createdAt})
	if err != nil {
		if ctx.Err() != nil || store.IsConnectionError(err) {
			in.giveBackAttempt(row.id)
			if ctx.Err() != nil {
				return false, nil
			}
			return false, err
		}
		code := failureCode(err)
		in.o.Logger.Error("a command failed", "command_id", row.id, "error_code", code, logging.Err(err))
		return true, in.finish(ctx, row, code, "")
	}
	return true, in.finish(ctx, row, "", handled.AnswerID)
}

func failureCode(err error) string {
	var coded Coded
	if errors.As(err, &coded) && errorCode.MatchString(coded.ErrorCode()) {
		return coded.ErrorCode()
	}
	return "handler_failed"
}

// finish marks a command done or failed, and keeps the exchange when the command was a question
// that got an answer, in one statement.
func (in *Inbox) finish(ctx context.Context, row commandRow, failure, answerWAID string) error {
	state := "done"
	var errCode, answer *string
	if failure != "" {
		state, errCode = "failed", &failure
	}
	if answerWAID != "" {
		answer = &answerWAID
	}
	_, err := in.o.DB.Exec(ctx, `
		WITH finished AS (
		  UPDATE commands SET state = $2, error = $3, finished_at = now()
		  WHERE id = $1 AND state = 'pending'
		  RETURNING message_id
		)
		INSERT INTO exchanges (question_message_id, answer_wa_id)
		SELECT message_id, $4 FROM finished WHERE $4::text IS NOT NULL
		ON CONFLICT (question_message_id) DO NOTHING`, row.id, state, errCode, answer)
	return err
}

// giveBackAttempt undoes the attempt count for a command that a stop or an outage interrupted.
func (in *Inbox) giveBackAttempt(id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = in.o.DB.Exec(ctx,
		"UPDATE commands SET attempts = attempts - 1 WHERE id = $1 AND state = 'pending' AND attempts > 0", id)
}
