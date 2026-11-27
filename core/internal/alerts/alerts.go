// Package alerts sends DJ heads-ups in his own chat without being asked: messages tagging flagged as
// needing him, and reminders about to start.
package alerts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/core/internal/search"
	"github.com/dhananjay6561/kaya/core/internal/text"
	"github.com/dhananjay6561/kaya/kit/logging"
	"github.com/dhananjay6561/kaya/kit/store"
)

// Sender sends a message to DJ's own chat.
type Sender interface {
	Send(ctx context.Context, m channel.OutgoingMessage, o channel.SendOptions) (channel.SentMessage, error)
}

// Options configure an Alerter. Zero durations take the defaults noted.
type Options struct {
	DB      *store.DB
	Channel Sender
	Pause   pause.Switch
	Logger  *slog.Logger
	Zone    *time.Location
	Now     func() time.Time
	// PerHour is the most alert messages sent in any hour; past it, alerts wait and go out together.
	PerHour int
	// Gather is how long an alert that is not urgent waits for others to go out with it (45 min).
	Gather time.Duration
	// Ahead is how long before a reminder's time it is sent (30 min).
	Ahead time.Duration
	// MaxAge is how old an alert may be and still be sent (6 hours).
	MaxAge time.Duration
	// Poll is how often alerts are checked (30 s); RetryMax bounds the wait after a failure (5 min).
	Poll     time.Duration
	RetryMax time.Duration
}

const (
	alertLock = 5011607
	perSend   = 10
	sendTTL   = time.Hour
)

// Alerter sends alerts as they become due.
type Alerter struct {
	o    Options
	lock *store.Lock

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
}

// New checks the options and returns a stopped alerter.
func New(o Options) (*Alerter, error) {
	switch {
	case o.DB == nil || o.Channel == nil || o.Pause == nil || o.Logger == nil || o.Zone == nil:
		return nil, errors.New("alerts need a database, a channel, the pause switch, a logger and a time zone")
	case o.PerHour < 1:
		return nil, errors.New("alerts need PerHour of at least 1")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	defaults := []struct {
		d        *time.Duration
		fallback time.Duration
	}{{&o.Gather, 45 * time.Minute}, {&o.Ahead, 30 * time.Minute}, {&o.MaxAge, 6 * time.Hour}, {&o.Poll, 30 * time.Second}, {&o.RetryMax, 5 * time.Minute}}
	for _, d := range defaults {
		if *d.d == 0 {
			*d.d = d.fallback
		}
	}
	return &Alerter{o: o, lock: store.NewLock(o.DB, alertLock)}, nil
}

// Start runs the alerter until Stop.
func (a *Alerter) Start() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.cancel, a.done, a.running = cancel, done, true
	go func() {
		defer close(done)
		a.run(ctx)
	}()
}

// Stop ends the alerter and waits for it. An alert being sent goes out under the same key next time.
func (a *Alerter) Stop() {
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.running = false
	a.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (a *Alerter) run(ctx context.Context) {
	defer a.lock.Release()
	delay := a.o.Poll
	failing := false
	for ctx.Err() == nil {
		err := a.lock.Hold(ctx)
		if err == nil {
			err = a.Step(ctx)
		}
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			if failing {
				a.o.Logger.Info("alerts work again")
			}
			failing, delay = false, a.o.Poll
			sleep(ctx, a.o.Poll)
		case errors.Is(err, store.ErrLockBusy):
			sleep(ctx, a.o.Poll)
		default:
			if !failing {
				a.o.Logger.Warn("alerts failed; trying again", logging.Err(err))
			}
			failing = true
			sleep(ctx, delay)
			delay = min(delay*2, a.o.RetryMax)
		}
	}
}

// Step does one round: queues reminders about to start, passes over alerts that no longer matter,
// and sends what is due.
func (a *Alerter) Step(ctx context.Context) error {
	st, err := a.o.Pause.State(ctx)
	if err != nil || st.Paused {
		return err
	}
	on, err := NewSwitch(a.o.DB).Enabled(ctx)
	if err != nil || !on {
		return err
	}
	now := a.o.Now()
	if err := a.queueReminders(ctx, now); err != nil {
		return err
	}
	if err := a.tidy(ctx, now); err != nil {
		return err
	}
	return a.send(ctx, now)
}

func (a *Alerter) queueReminders(ctx context.Context, now time.Time) error {
	type soon struct {
		id, chat int64
		title    string
		due      time.Time
	}
	var found []soon
	err := a.o.DB.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT t.id, t.chat_id, t.title, t.due_at FROM tasks t JOIN messages m ON m.id = t.message_id
		WHERE t.kind = 'reminder' AND t.state = 'open' AND t.due_has_time AND m.deleted_at IS NULL
		  AND t.due_at > $1 AND t.due_at <= $2
		  AND NOT EXISTS (SELECT 1 FROM alerts a WHERE a.task_id IS NOT NULL AND a.chat_id = t.chat_id AND a.due_at = t.due_at)`,
			now, now.Add(a.o.Ahead))
		if err != nil {
			return err
		}
		found, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (soon, error) {
			var s soon
			err := row.Scan(&s.id, &s.chat, &s.title, &s.due)
			return s, err
		})
		return err
	})
	if err != nil {
		return err
	}
	for _, s := range found {
		minutes := max(1, int(s.due.Sub(now).Round(time.Minute).Minutes()))
		note := fmt.Sprintf("In %d min, at %s: %s", minutes, s.due.In(a.o.Zone).Format("15:04"), text.Clip(s.title, 250))
		if _, err := a.o.DB.Exec(ctx, `INSERT INTO alerts (task_id, due_at, chat_id, urgent, note) VALUES ($1, $2, $3, true, $4)
			ON CONFLICT DO NOTHING`, s.id, s.due, s.chat, note); err != nil {
			return err
		}
	}
	return nil
}

// answered is true when DJ has written back since message m: in a direct chat any message of his,
// in a group a reply to that message or its sender, or anything within ten minutes of it.
const answered = `EXISTS (SELECT 1 FROM messages r JOIN chats c ON c.id = r.chat_id
	WHERE r.chat_id = m.chat_id AND r.from_me AND r.deleted_at IS NULL AND r.sent_at > m.sent_at
	  AND (c.kind <> 'group' OR r.reply_to_wa_id = m.wa_id OR r.reply_to_sender_id = m.sender_id
	       OR r.sent_at <= m.sent_at + interval '10 minutes'))`

// tidy passes over alerts that no longer matter: DJ has written back since, the message was
// deleted or is too old, the reminder was closed, moved, deleted or has come, or the alert waited
// too long. A batch given a key but never sent is passed over too once it is too old.
func (a *Alerter) tidy(ctx context.Context, now time.Time) error {
	old := now.Add(-a.o.MaxAge)
	_, err := a.o.DB.Exec(ctx, `UPDATE alerts a SET skipped = CASE
			WHEN m.deleted_at IS NOT NULL THEN 'deleted'
			WHEN `+answered+` THEN 'answered'
			ELSE 'stale' END
		FROM messages m
		WHERE a.message_id = m.id AND a.sent_at IS NULL AND a.skipped IS NULL
		  AND (a.batch IS NULL OR a.created_at < $1)
		  AND (m.deleted_at IS NOT NULL OR a.created_at < $1 OR m.sent_at < $1 OR (a.batch IS NULL AND `+answered+`))`, old)
	if err != nil {
		return err
	}
	_, err = a.o.DB.Exec(ctx, `UPDATE alerts a SET skipped = CASE
			WHEN t.id IS NULL THEN 'gone'
			WHEN t.state <> 'open' THEN 'done'
			WHEN t.due_at IS DISTINCT FROM a.due_at THEN 'moved'
			ELSE 'late' END
		FROM alerts x LEFT JOIN tasks t ON t.id = x.task_id
		WHERE x.id = a.id AND a.task_id IS NOT NULL AND a.sent_at IS NULL AND a.skipped IS NULL
		  AND (t.id IS NULL OR t.state <> 'open' OR t.due_at IS DISTINCT FROM a.due_at OR a.due_at <= $1)`, now)
	return err
}

type waiting struct {
	id      int64
	urgent  bool
	note    string
	chat    string
	task    bool
	due     *time.Time
	created time.Time
}

// send sends a batch cut off earlier under its own key, or else the waiting alerts once one is
// urgent or has waited Gather. Past PerHour messages in the last hour only reminders go out, since
// they cannot wait.
func (a *Alerter) send(ctx context.Context, now time.Time) error {
	key, batch, err := a.unfinished(ctx)
	if err != nil {
		return err
	}
	if key == "" {
		var sent int
		if err := a.o.DB.QueryRow(ctx, `SELECT count(DISTINCT batch) FROM alerts WHERE sent_at > $1`, now.Add(-time.Hour)).Scan(&sent); err != nil {
			return err
		}
		condition := "a.batch IS NULL AND a.sent_at IS NULL AND a.skipped IS NULL"
		if sent >= a.o.PerHour {
			condition += " AND a.task_id IS NOT NULL"
		}
		all, err := a.read(ctx, condition)
		if err != nil || len(all) == 0 {
			return err
		}
		due := false
		for _, w := range all {
			due = due || w.urgent || !w.created.After(now.Add(-a.o.Gather))
		}
		if !due {
			return nil
		}
		picked := all[:min(len(all), perSend)]
		ids := make([]string, len(picked))
		for i, w := range picked {
			ids[i] = strconv.FormatInt(w.id, 10)
		}
		sum := sha256.Sum256([]byte(strings.Join(ids, ",")))
		key = "alerts-" + hex.EncodeToString(sum[:12])
		if _, err := a.o.DB.Exec(ctx, `UPDATE alerts SET batch = $1 WHERE id = ANY($2) AND batch IS NULL AND skipped IS NULL AND sent_at IS NULL`,
			key, idsOf(picked)); err != nil {
			return err
		}
		if batch, err = a.read(ctx, "a.batch = $1 AND a.sent_at IS NULL AND a.skipped IS NULL", key); err != nil || len(batch) == 0 {
			return err
		}
	}
	ttl := sendTTL
	for _, w := range batch {
		if w.due != nil {
			ttl = min(ttl, max(w.due.Sub(now), time.Minute))
		}
	}
	_, err = a.o.Channel.Send(ctx, channel.OutgoingMessage{IdempotencyKey: key, Text: format(batch)},
		channel.SendOptions{TTL: ttl, TextMayChange: true})
	var failed *channel.SendFailedError
	switch {
	case errors.As(err, &failed):
		a.o.Logger.Warn("an alert message was not sent; its alerts are passed over", "alerts", len(batch), "error_code", failed.Code)
		_, err = a.o.DB.Exec(ctx, `UPDATE alerts SET skipped = $2 WHERE batch = $1 AND sent_at IS NULL AND skipped IS NULL`,
			key, "send_"+failed.Code)
		return err
	case err != nil:
		return err
	}
	if _, err := a.o.DB.Exec(ctx, `UPDATE alerts SET sent_at = now() WHERE batch = $1 AND sent_at IS NULL AND skipped IS NULL`, key); err != nil {
		return err
	}
	urgent := 0
	for _, w := range batch {
		if w.urgent {
			urgent++
		}
	}
	a.o.Logger.Info("alerts sent", "alerts", len(batch), "urgent", urgent)
	return nil
}

// unfinished is a batch that was given a key but not marked sent, so it goes out under that key.
func (a *Alerter) unfinished(ctx context.Context) (string, []waiting, error) {
	var key string
	err := a.o.DB.QueryRow(ctx, `SELECT batch FROM alerts WHERE batch IS NOT NULL AND sent_at IS NULL AND skipped IS NULL
		ORDER BY id LIMIT 1`).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	batch, err := a.read(ctx, "a.batch = $1 AND a.sent_at IS NULL AND a.skipped IS NULL", key)
	return key, batch, err
}

func (a *Alerter) read(ctx context.Context, condition string, args ...any) ([]waiting, error) {
	var out []waiting
	err := a.o.DB.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT a.id, a.urgent, a.note, `+search.ChatTitleSQL+`, a.task_id IS NOT NULL, a.due_at, a.created_at
		FROM alerts a LEFT JOIN chats ch ON ch.id = a.chat_id LEFT JOIN contacts cc ON cc.id = ch.contact_id
		WHERE `+condition+` ORDER BY a.urgent DESC, a.created_at, a.id`, args...)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (waiting, error) {
			var w waiting
			err := row.Scan(&w.id, &w.urgent, &w.note, &w.chat, &w.task, &w.due, &w.created)
			return w, err
		})
		return err
	})
	return out, err
}

func idsOf(batch []waiting) []int64 {
	ids := make([]int64, len(batch))
	for i, w := range batch {
		ids[i] = w.id
	}
	return ids
}

// format writes alerts as one WhatsApp message, urgent ones first: each says which chat it is about,
// so a note always reads as Kaya's summary of someone else's message.
func format(batch []waiting) string {
	label := func(w waiting) string {
		if w.task {
			return "Reminder"
		}
		return "From " + text.Clip(w.chat, 60)
	}
	if len(batch) == 1 {
		return "*Heads up*\n" + label(batch[0]) + ": " + batch[0].note
	}
	var b strings.Builder
	b.WriteString("*Heads up*")
	for _, w := range batch {
		b.WriteString("\n• " + label(w) + ": " + w.note)
	}
	return b.String()
}
