package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/core/internal/memory"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/kit/logging"
	"github.com/dhananjay6561/kaya/kit/store"
)

// Sender sends a message to DJ's own chat.
type Sender interface {
	Send(ctx context.Context, m channel.OutgoingMessage, o channel.SendOptions) (channel.SentMessage, error)
}

// Options configure a Scheduler. Zero durations take the defaults noted.
type Options struct {
	DB      *store.DB
	Channel Sender
	Pause   pause.Switch
	Logger  *slog.Logger
	Zone    *time.Location
	Now     func() time.Time
	// Ask answers an ask job's text as if DJ had just sent it; nil sends it as a reminder.
	Ask func(ctx context.Context, text string) (string, error)
	// Late is how far past its time a job says it was late (2 minutes); Stale is how far past its
	// time a repeating job is passed over instead of sent (12 hours).
	Late  time.Duration
	Stale time.Duration
	// MaxWait bounds a sleep with nothing due (1 hour); RetryMax bounds the wait after a failure
	// (5 minutes).
	MaxWait  time.Duration
	RetryMax time.Duration
}

const schedulerLock = 5011608

// Scheduler fires jobs at their time.
type Scheduler struct {
	o    Options
	lock *store.Lock
	wake chan struct{}

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
}

// NewScheduler checks the options and returns a stopped scheduler.
func NewScheduler(o Options) (*Scheduler, error) {
	if o.DB == nil || o.Channel == nil || o.Pause == nil || o.Logger == nil || o.Zone == nil {
		return nil, errors.New("jobs need a database, a channel, the pause switch, a logger and a time zone")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	for _, d := range []struct {
		d        *time.Duration
		fallback time.Duration
	}{{&o.Late, 2 * time.Minute}, {&o.Stale, 12 * time.Hour}, {&o.MaxWait, time.Hour}, {&o.RetryMax, 5 * time.Minute}} {
		if *d.d == 0 {
			*d.d = d.fallback
		}
	}
	return &Scheduler{o: o, lock: store.NewLock(o.DB, schedulerLock), wake: make(chan struct{}, 1)}, nil
}

// Start runs the scheduler until Stop.
func (s *Scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.cancel, s.done, s.running = cancel, done, true
	go s.o.DB.Listen(ctx, "jobs", s.Poke)
	go func() {
		defer close(done)
		s.run(ctx)
	}()
}

// Stop ends the scheduler and waits for it.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.running = false
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Poke wakes the scheduler to look again, as when a job is added or changed.
func (s *Scheduler) Poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	case <-s.wake:
	}
}

func (s *Scheduler) run(ctx context.Context) {
	defer s.lock.Release()
	delay := time.Second
	failing := false
	for ctx.Err() == nil {
		err := s.lock.Hold(ctx)
		var wait time.Duration
		if err == nil {
			wait, err = s.Step(ctx)
		}
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			if failing {
				s.o.Logger.Info("jobs work again")
			}
			failing, delay = false, time.Second
			s.sleep(ctx, wait)
		case errors.Is(err, store.ErrLockBusy):
			s.sleep(ctx, 30*time.Second)
		default:
			if !failing {
				s.o.Logger.Warn("jobs failed; trying again", logging.Err(err))
			}
			failing = true
			s.sleep(ctx, delay)
			delay = min(delay*2, s.o.RetryMax)
		}
	}
}

// Step fires every job that is due and says how long to sleep until the next one.
func (s *Scheduler) Step(ctx context.Context) (time.Duration, error) {
	st, err := s.o.Pause.State(ctx)
	if err != nil {
		return 0, err
	}
	if st.Paused {
		return 30 * time.Second, nil
	}
	store := NewStore(s.o.DB)
	due, err := store.query(ctx, `SELECT `+jobColumns+` FROM jobs WHERE state = 'active' AND next_at <= $1 ORDER BY next_at, id`, s.o.Now())
	if err != nil {
		return 0, err
	}
	for _, j := range due {
		if err := s.fire(ctx, j); err != nil {
			return 0, err
		}
	}
	var next *time.Time
	if err := s.o.DB.QueryRow(ctx, `SELECT min(next_at) FROM jobs WHERE state = 'active'`).Scan(&next); err != nil {
		return 0, err
	}
	if next == nil {
		return s.o.MaxWait, nil
	}
	return min(max(next.Sub(s.o.Now()), 0), s.o.MaxWait), nil
}

// fire sends one due job, once for its time, and moves it to its next time.
func (s *Scheduler) fire(ctx context.Context, j Job) error {
	now := s.o.Now()
	due := j.NextAt
	late := now.Sub(due)
	var sent *time.Time
	if _, err := s.o.DB.Exec(ctx, `INSERT INTO job_runs (job_id, due_at, late) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		j.ID, due, late > s.o.Late); err != nil {
		return err
	}
	if err := s.o.DB.QueryRow(ctx, `SELECT sent_at FROM job_runs WHERE job_id = $1 AND due_at = $2`, j.ID, due).Scan(&sent); err != nil {
		return err
	}
	switch {
	case sent != nil:
	case j.Repeat.Kind != Once && late > s.o.Stale:
		s.o.Logger.Info("a repeating job was too late to send; it moves on", "job", j.ID, "late_minutes", int(late.Minutes()))
	default:
		text := s.message(ctx, j, late > s.o.Late)
		_, err := s.o.Channel.Send(ctx, channel.OutgoingMessage{IdempotencyKey: fmt.Sprintf("job:%d:%d", j.ID, due.Unix()), Text: text},
			channel.SendOptions{TTL: time.Hour, TextMayChange: true})
		var failed *channel.SendFailedError
		switch {
		case errors.As(err, &failed):
			s.o.Logger.Warn("a job's message was not sent", "job", j.ID, "error_code", failed.Code)
		case err != nil:
			return err
		default:
			if _, err := s.o.DB.Exec(ctx, `UPDATE job_runs SET sent_at = now(), text = $3 WHERE job_id = $1 AND due_at = $2`,
				j.ID, due, text); err != nil {
				return err
			}
			s.o.Logger.Info("a job fired", "job", j.ID, "ask", j.Ask, "late_seconds", int(late.Seconds()))
		}
	}
	return s.advance(ctx, j, now)
}

// message is what a job sends: the reminder itself, or for an ask job the chat model's answer.
func (s *Scheduler) message(ctx context.Context, j Job, late bool) string {
	note := ""
	if late {
		note = "\n(It was due at " + j.NextAt.In(s.o.Zone).Format("15:04") + "; I was offline then.)"
	}
	if !j.Ask || s.o.Ask == nil {
		return "*Reminder:* " + j.Text + note
	}
	answer, err := s.o.Ask(ctx, j.Text)
	if err != nil || strings.TrimSpace(answer) == "" {
		s.o.Logger.Warn("could not answer a scheduled job; sending it as a reminder", "job", j.ID, logging.Err(err))
		return "*Scheduled:* " + j.Text + "\n(I couldn't prepare it just now. Ask me again.)" + note
	}
	return answer + note
}

// advance moves j to its next time after now, passing over any it missed, or ends it.
func (s *Scheduler) advance(ctx context.Context, j Job, now time.Time) error {
	next := j.NextAt
	var err error
	for range 10000 {
		next, err = Next(next, j.Clock, j.Repeat, s.o.Zone)
		if err != nil || next.After(now) {
			break
		}
	}
	if errors.Is(err, ErrNoMore) {
		_, err = s.o.DB.Exec(ctx, `UPDATE jobs SET state = 'done', next_at = NULL WHERE id = $1 AND next_at = $2`, j.ID, j.NextAt)
		return err
	}
	if err != nil {
		return err
	}
	_, err = s.o.DB.Exec(ctx, `UPDATE jobs SET next_at = $3 WHERE id = $1 AND next_at = $2`, j.ID, j.NextAt, next)
	return err
}

// FiredHeadsUp stands where DJ's question would be for a job's message in the conversation.
const FiredHeadsUp = "(Nothing from them here: a reminder or scheduled job they set fired, and you sent the message below.)"

// Recent is the job messages sent since, as exchanges for the conversation, oldest first, at most
// limit of the newest, so "10 min baad" after a reminder has it in context.
func Recent(ctx context.Context, db *store.DB, since time.Time, limit int) ([]memory.Exchange, error) {
	var out []memory.Exchange
	err := db.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT text, sent_at FROM (
				SELECT text, sent_at FROM job_runs WHERE sent_at > $1 AND text IS NOT NULL ORDER BY sent_at DESC LIMIT $2) r
			ORDER BY sent_at`, since, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (memory.Exchange, error) {
			e := memory.Exchange{Question: FiredHeadsUp, Unasked: true}
			err := row.Scan(&e.Answer, &e.At)
			return e, err
		})
		return err
	})
	return out, err
}
