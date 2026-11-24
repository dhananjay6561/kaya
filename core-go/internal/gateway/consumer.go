package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/kit/logging"
	"github.com/dhananjay6561/kaya/kit/store"
)

// Handler applies one event inside the transaction that marks it handled.
type Handler func(ctx context.Context, tx pgx.Tx, event contracts.Event) (AfterCommit, error)

// ErrNotRunning is what the consumer's health check reports while it is stopped.
var ErrNotRunning = errors.New("the gateway event consumer is not running")

// ConsumerOptions configures a Consumer. Zero durations take their defaults.
type ConsumerOptions struct {
	DB        *store.DB
	Validator *contracts.Validator
	Logger    *slog.Logger
	Handle    Handler

	PollInterval time.Duration
	RetryMin     time.Duration
	RetryMax     time.Duration
	Retention    time.Duration
	PurgeEvery   time.Duration
	// PurgeAlways runs the clean-up after every round, for tests.
	PurgeAlways bool
}

// Consumer drains gateway_events in order, one transaction per event, woken by NOTIFY and by a poll.
type Consumer struct {
	o    ConsumerOptions
	wake chan struct{}

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
}

const channel = "gateway_events"

// NewConsumer returns a stopped consumer.
func NewConsumer(o ConsumerOptions) *Consumer {
	if o.PollInterval == 0 {
		o.PollInterval = 30 * time.Second
	}
	if o.RetryMin == 0 {
		o.RetryMin = time.Second
	}
	if o.RetryMax == 0 {
		o.RetryMax = 30 * time.Second
	}
	if o.Retention == 0 {
		o.Retention = 7 * 24 * time.Hour
	}
	if o.PurgeEvery == 0 {
		o.PurgeEvery = time.Hour
	}
	return &Consumer{o: o, wake: make(chan struct{}, 1)}
}

// Start begins draining and listening. It does nothing if the consumer is already running, and
// waits for a previous run to finish before starting a new one.
func (c *Consumer) Start(parent context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return
	}
	if c.done != nil {
		<-c.done
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	c.cancel, c.done, c.running = cancel, done, true
	var wg sync.WaitGroup
	wg.Go(func() { c.drain(ctx) })
	wg.Go(func() { c.o.DB.Listen(ctx, channel, c.nudge) })
	go func() {
		wg.Wait()
		c.mu.Lock()
		if c.done == done {
			c.running = false
		}
		c.mu.Unlock()
		close(done)
	}()
}

// Stop ends both loops and waits for them, cutting short whatever they are doing. Every caller
// waits, however many call it.
func (c *Consumer) Stop() {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.running = false
	c.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Check reports whether the consumer is running.
func (c *Consumer) Check(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return ErrNotRunning
	}
	return nil
}

func (c *Consumer) nudge() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Consumer) drain(ctx context.Context) {
	delay := c.o.RetryMin
	failing := false
	nextPurge := time.Time{}
	for ctx.Err() == nil {
		handled, err := c.step(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !failing {
				c.o.Logger.Warn("could not handle gateway events, trying again", logging.Err(err))
			}
			failing = true
			c.sleep(ctx, delay)
			delay = min(delay*2, c.o.RetryMax)
			continue
		}
		if failing {
			c.o.Logger.Info("gateway events are being handled again")
		}
		failing, delay = false, c.o.RetryMin
		if handled {
			continue
		}
		if c.o.PurgeAlways || !time.Now().Before(nextPurge) {
			nextPurge = time.Now().Add(c.o.PurgeEvery)
			c.purge(ctx)
		}
		c.sleep(ctx, c.o.PollInterval)
	}
}

func (c *Consumer) sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-c.wake:
	case <-timer.C:
	}
}

func (c *Consumer) purge(ctx context.Context) {
	n, err := c.o.DB.Exec(ctx,
		"DELETE FROM gateway_events WHERE state = 'handled' AND finished_at < now() - make_interval(secs => $1)",
		c.o.Retention.Seconds())
	if err != nil {
		if ctx.Err() == nil {
			c.o.Logger.Warn("could not delete old gateway events", logging.Err(err))
		}
		return
	}
	if n > 0 {
		c.o.Logger.Info("old handled gateway events deleted", "events", n)
	}
}

// setAside is what step's transaction returns when an event must be marked dead but the
// transaction can no longer do it, because the handler left its connection unusable.
type setAside struct {
	seq    int64
	reason string
}

func (e *setAside) Error() string { return "set aside: " + e.reason }

func (c *Consumer) step(ctx context.Context) (handled bool, err error) {
	var after AfterCommit
	var seq int64
	err = c.o.DB.InTx(ctx, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx,
			"SELECT seq, payload FROM gateway_events WHERE state = 'pending' ORDER BY seq LIMIT 1 FOR UPDATE").
			Scan(&seq, &raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		handled = true

		dead := ""
		event, err := c.o.Validator.DecodeEvent(raw)
		if err != nil {
			var invalid *contracts.ValidationError
			if !errors.As(err, &invalid) {
				return err
			}
			dead = "invalid_event"
			c.o.Logger.Error("a gateway event does not fit the contract", "seq", seq, "problems", invalid.Problems)
		} else {
			after, dead, err = c.handleSafely(ctx, tx, seq, event)
			if err != nil {
				return err
			}
		}

		if dead != "" {
			_, err = tx.Exec(ctx,
				"UPDATE gateway_events SET state = 'dead', error = $2, finished_at = now() WHERE seq = $1", seq, dead)
		} else {
			_, err = tx.Exec(ctx,
				"UPDATE gateway_events SET state = 'handled', finished_at = now() WHERE seq = $1", seq)
		}
		if err != nil && !store.IsConnectionError(err) && ctx.Err() == nil {
			return &setAside{seq: seq, reason: "handler_failed"}
		}
		return err
	})
	var aside *setAside
	if errors.As(err, &aside) {
		return true, c.markDead(ctx, aside)
	}
	if err != nil {
		return false, err
	}
	if after != nil {
		c.runAfter(seq, after)
	}
	return handled, nil
}

// markDead records an event as dead in its own transaction, after the one that tried it failed.
func (c *Consumer) markDead(ctx context.Context, aside *setAside) error {
	_, err := c.o.DB.Exec(ctx,
		"UPDATE gateway_events SET state = 'dead', error = $2, finished_at = now() WHERE seq = $1 AND state = 'pending'",
		aside.seq, aside.reason)
	return err
}

// handleSafely runs the handler in a savepoint. What happens next depends only on the handler's own
// error: the database being away, or core stopping, means try the event again later; anything else
// sets the event aside, undoing only what the handler wrote. If even that undo fails, the event is
// set aside from a fresh transaction.
func (c *Consumer) handleSafely(ctx context.Context, tx pgx.Tx, seq int64, event contracts.Event) (AfterCommit, string, error) {
	inner, err := tx.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	after, handlerErr := c.callHandler(ctx, inner, event)
	if handlerErr == nil {
		if err := inner.Commit(ctx); err != nil {
			if store.IsConnectionError(err) || ctx.Err() != nil {
				return nil, "", err
			}
			c.o.Logger.Error("a gateway event's handler left its transaction unusable and was set aside",
				"seq", seq, "event_id", event.ID, "type", event.Type, logging.Err(err))
			return nil, "", &setAside{seq: seq, reason: "handler_failed"}
		}
		return after, "", nil
	}
	if ctx.Err() != nil || store.IsConnectionError(handlerErr) {
		return nil, "", handlerErr
	}
	reason := "handler_failed"
	if errors.Is(handlerErr, contracts.ErrInvalid) {
		reason = "invalid_event"
	}
	c.o.Logger.Error("a gateway event could not be handled and was set aside",
		"seq", seq, "event_id", event.ID, "type", event.Type, logging.Err(handlerErr))
	if err := inner.Rollback(ctx); err != nil {
		return nil, "", &setAside{seq: seq, reason: reason}
	}
	return nil, reason, nil
}

func (c *Consumer) callHandler(ctx context.Context, tx pgx.Tx, event contracts.Event) (after AfterCommit, err error) {
	defer func() {
		if p := recover(); p != nil {
			after, err = nil, fmt.Errorf("handler panicked: %v\n%s", p, debug.Stack())
		}
	}()
	return c.o.Handle(ctx, tx, event)
}

func (c *Consumer) runAfter(seq int64, after AfterCommit) {
	defer func() {
		if p := recover(); p != nil {
			c.o.Logger.Error("a follow-up to a handled gateway event failed", "seq", seq, "panic", fmt.Sprint(p))
		}
	}()
	after()
}
