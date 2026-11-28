package channel

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/text"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
	typedByDJ := m.FromMe && (m.Kind == "text" || m.Kind == "voice") && m.Source == "live" && m.Change == "new"
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
		  AND m.from_me AND m.kind IN ('text', 'voice') AND m.source = 'live' AND m.deleted_at IS NULL
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

// Start begins handing commands to deliver whenever ready says someone can take them. If a
// previous run is still ending, it waits for that first, without holding up a concurrent Stop.
func (in *Inbox) Start(deliver Deliver, ready func() bool) {
	for {
		in.mu.Lock()
		if in.running {
			in.mu.Unlock()
			return
		}
		if previous := in.done; previous != nil {
			select {
			case <-previous:
			default:
				in.mu.Unlock()
				<-previous
				continue
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		in.cancel, in.done, in.running = cancel, done, true
		in.mu.Unlock()
		go func() {
			in.run(ctx, deliver, ready)
			in.mu.Lock()
			if in.done == done {
				in.running = false
			}
			in.mu.Unlock()
			close(done)
		}()
		return
	}
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
		if errors.Is(err, errLockBusy) {
			// Another worker holds the lock, or the last one's connection is still closing: try
			// again soon rather than after a whole poll.
			in.sleep(ctx, delay)
			delay = min(delay*2, in.o.RetryMax)
			continue
		}
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
		if lock.alive(ctx) {
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

func (lock *lockHolder) alive(ctx context.Context) bool {
	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return lock.conn.Ping(ping) == nil
}

// release unlocks before closing, so the next worker can take the lock at once instead of waiting
// for Postgres to notice the connection is gone.
func (lock *lockHolder) release() {
	if lock.conn != nil {
		unlock, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = lock.conn.Exec(unlock, "SELECT pg_advisory_unlock($1)", inboxLock)
		cancel()
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

// errLockBusy means another worker holds the lock.
var errLockBusy = errors.New("another worker holds the command lock")

// errLockLost ends a delivery whose worker lock went away mid-way, since another worker may now
// take the same command.
var errLockLost = errors.New("the command worker lock was lost")

// dbWork is how long the inbox's own bookkeeping may take. It runs past a stop, so a command that
// was claimed or finished is always recorded as such.
const dbWork = 10 * time.Second

func (in *Inbox) step(ctx context.Context, lock *lockHolder, deliver Deliver, ready func() bool) (bool, error) {
	if !ready() {
		return false, nil
	}
	held, err := lock.holds(ctx, in.o.DB)
	if err != nil {
		return false, err
	}
	if !held {
		return false, errLockBusy
	}
	db, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbWork)
	defer cancel()

	var row commandRow
	err = in.o.DB.QueryRow(db, `
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
		return true, in.finish(db, row, "interrupted", "")
	}

	var body, kind, heard, spoken *string
	err = in.o.DB.QueryRow(db, `SELECT m.body, m.kind, t.state, t.text FROM messages m
		LEFT JOIN transcripts t ON t.message_id = m.id WHERE m.id = $1 AND m.deleted_at IS NULL`, row.messageID).Scan(&body, &kind, &heard, &spoken)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		in.giveBackAttempt(row.id)
		return false, err
	}
	words := ""
	if body != nil {
		words = text.Trim(*body)
	}
	if kind != nil && *kind == "voice" {
		switch {
		case heard != nil && *heard == "done" && spoken != nil && text.Trim(*spoken) != "":
			words = text.Trim(*spoken)
		case heard == nil && time.Since(row.createdAt) < voiceWait:
			in.giveBackAttempt(row.id)
			return false, nil
		default:
			words = cannotHear
		}
	}
	if words == "" {
		return true, in.finish(db, row, "message_gone", "")
	}

	if !row.urgent {
		var recent int
		err := in.o.DB.QueryRow(db, `SELECT count(*) FROM commands
			WHERE finished_at > now() - interval '1 minute' AND error IS DISTINCT FROM 'over_cap'`).Scan(&recent)
		if err != nil {
			in.giveBackAttempt(row.id)
			return false, err
		}
		if recent >= in.o.MaxPerMinute {
			in.o.Logger.Error("too many commands in a minute; this one was dropped", "command_id", row.id)
			return true, in.finish(db, row, "over_cap", "")
		}
	}

	handled, err := in.deliverWatched(ctx, lock, deliver,
		IncomingMessage{ID: row.waID, Text: words, SentAt: row.sentAt, ReceivedAt: row.createdAt})
	if err != nil {
		switch {
		case ctx.Err() != nil:
			in.giveBackAttempt(row.id)
			return false, nil
		case errors.Is(err, errLockLost):
			in.giveBackAttempt(row.id)
			lock.release()
			return false, err
		case DatabaseAway(in.o.DB, err):
			in.giveBackAttempt(row.id)
			return false, err
		}
		code := failureCode(err)
		in.o.Logger.Error("a command failed", "command_id", row.id, "error_code", code, logging.Err(err))
		done, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbWork)
		defer cancel()
		return true, in.finish(done, row, code, "")
	}
	// Answering can take a minute (searches, pages, waits on the model's rate limit), longer than
	// db's window, so the command is finished in a window of its own.
	done, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbWork)
	defer cancel()
	return true, in.finish(done, row, "", handled.AnswerID)
}

// DatabaseAway reports whether a handler failed because the database is down, not because of its
// own work: a coded failure is the handler's, and a network error counts only if Postgres itself
// does not answer, since a handler also talks to other services. Only then is the command run again.
func DatabaseAway(db *store.DB, err error) bool {
	if len(codesIn(err)) > 0 || !store.IsConnectionError(err) {
		return false
	}
	var pg *pgconn.PgError
	var connect *pgconn.ConnectError
	if errors.As(err, &pg) || errors.As(err, &connect) {
		return true
	}
	probe, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return db.Check(probe) != nil
}

// deliverWatched hands one command over while watching the worker lock: if its connection drops,
// Postgres frees the lock and another worker may take the same command, so the delivery is cut short.
func (in *Inbox) deliverWatched(ctx context.Context, lock *lockHolder, deliver Deliver, m IncomingMessage) (Handled, error) {
	watched, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stopWatching := make(chan struct{})
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stopWatching:
				return
			case <-watched.Done():
				return
			case <-tick.C:
				if !lock.alive(watched) && watched.Err() == nil {
					cancel(errLockLost)
					return
				}
			}
		}
	}()
	handled, err := deliver(watched, m)
	close(stopWatching)
	<-watching
	if errors.Is(context.Cause(watched), errLockLost) && ctx.Err() == nil {
		return Handled{}, errLockLost
	}
	return handled, err
}

// failureCode is the code to store for a failed command: the one usable code its error carries, or
// handler_failed when it carries none or several that disagree.
func failureCode(err error) string {
	codes := codesIn(err)
	if len(codes) == 1 {
		return codes[0]
	}
	return "handler_failed"
}

// codesIn lists the distinct usable codes anywhere in err's tree.
func codesIn(err error) []string {
	var codes []string
	var walk func(error)
	walk = func(err error) {
		if err == nil {
			return
		}
		if coded, ok := err.(Coded); ok && errorCode.MatchString(coded.ErrorCode()) && !slices.Contains(codes, coded.ErrorCode()) {
			codes = append(codes, coded.ErrorCode())
		}
		switch wrapped := err.(type) { //nolint:errorlint // this walks the error tree itself, one node at a time
		case interface{ Unwrap() []error }:
			for _, e := range wrapped.Unwrap() {
				walk(e)
			}
		case interface{ Unwrap() error }:
			walk(wrapped.Unwrap())
		}
	}
	walk(err)
	return codes
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

// voiceWait is how long a voice note DJ sent waits for its transcript before Kaya says she could
// not hear it; cannotHear is what she is given then, so she can ask him to type it.
const (
	voiceWait  = 2 * time.Minute
	cannotHear = "(They sent you a voice note, but it could not be turned into text. Tell them you couldn't hear it and ask them to type it or send it again.)"
)
