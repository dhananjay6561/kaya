package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/kit/store"
)

type rig struct {
	t        *testing.T
	core     *store.DB
	admin    *store.DB
	consumer *Consumer
}

func newRig(t *testing.T) *rig {
	t.Helper()
	core, admin := databases(t)
	r := &rig{t: t, core: core, admin: admin}
	exec(t, admin, "CREATE TABLE seen (n serial, state text)")
	exec(t, admin, "GRANT ALL ON seen, seen_n_seq TO kaya_core")
	t.Cleanup(r.stop)
	return r
}

func (r *rig) run(handle Handler, tune func(*ConsumerOptions)) {
	r.t.Helper()
	o := ConsumerOptions{
		DB: r.core, Validator: validator(r.t), Logger: quiet, Handle: handle,
		PollInterval: time.Minute, RetryMin: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond,
	}
	if tune != nil {
		tune(&o)
	}
	r.consumer = NewConsumer(o)
	r.consumer.Start(context.Background())
}

func (r *rig) stop() {
	if r.consumer != nil {
		r.consumer.Stop()
	}
}

func (r *rig) strings(sql string) []string {
	r.t.Helper()
	var out []string
	err := r.admin.InTx(context.Background(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), sql)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *rig) seen() []string {
	return r.strings("SELECT state FROM seen ORDER BY n")
}

func (r *rig) states() []string {
	return r.strings("SELECT state || coalesce(' ' || error, '') FROM gateway_events ORDER BY seq")
}

func (r *rig) listeners() []string {
	return r.strings("SELECT pid::text FROM pg_stat_activity WHERE application_name = 'kaya-core-listen' AND datname = current_database()")
}

func record(ctx context.Context, tx pgx.Tx, e contracts.Event) (AfterCommit, error) {
	if e.Type == contracts.EventConnectionState {
		var data contracts.ConnectionState
		if err := e.DecodeData(&data); err != nil {
			return nil, err
		}
		_, err := tx.Exec(ctx, "INSERT INTO seen (state) VALUES ($1)", data.State)
		return nil, err
	}
	return nil, nil
}

func stateOf(e contracts.Event) string {
	var data contracts.ConnectionState
	_ = e.DecodeData(&data)
	return data.State
}

func TestHandlesEventsInOrderWokenByTheInsert(t *testing.T) {
	r := newRig(t)
	r.run(record, nil)
	eventually(t, "the listener", func() bool { return len(r.listeners()) > 0 })
	for _, s := range []string{"connecting", "connected", "disconnected"} {
		add(t, r.admin, connectionEvent(t, s))
	}
	eventually(t, "three handled events", func() bool { return len(r.seen()) == 3 })
	if !slices.Equal(r.seen(), []string{"connecting", "connected", "disconnected"}) || !slices.Equal(r.states(), []string{"handled", "handled", "handled"}) {
		t.Fatalf("seen %v, states %v", r.seen(), r.states())
	}
}

func TestDrainsWhatWasQueuedBeforeItStarted(t *testing.T) {
	r := newRig(t)
	add(t, r.admin, connectionEvent(t, "connected"))
	add(t, r.admin, connectionEvent(t, "disconnected"))
	r.run(record, nil)
	eventually(t, "both events", func() bool { return len(r.seen()) == 2 })
	if !slices.Equal(r.seen(), []string{"connected", "disconnected"}) {
		t.Fatalf("got %v", r.seen())
	}
}

func TestSetsAsideAnEventThatDoesNotFitTheContract(t *testing.T) {
	r := newRig(t)
	r.run(record, nil)
	bad := connectionEvent(t, "connected")
	exec(t, r.admin, "INSERT INTO gateway_events (id, type, payload) VALUES ($1, 'connection.state', $2)",
		bad.ID, `{"id":"`+bad.ID+`","type":"connection.state","data":{"state":"sleeping"}}`)
	add(t, r.admin, connectionEvent(t, "connected"))
	eventually(t, "the valid event", func() bool { return len(r.seen()) == 1 })
	if !slices.Equal(r.states(), []string{"dead invalid_event", "handled"}) {
		t.Fatalf("got %v", r.states())
	}
}

func TestSetsAsideAFailingEventAndUndoesWhatItsHandlerWrote(t *testing.T) {
	r := newRig(t)
	r.run(func(ctx context.Context, tx pgx.Tx, e contracts.Event) (AfterCommit, error) {
		if _, err := record(ctx, tx, e); err != nil {
			return nil, err
		}
		switch stateOf(e) {
		case "logged_out":
			return nil, errors.New("handler bug")
		case "pairing":
			panic("handler panic")
		}
		return nil, nil
	}, nil)
	for _, s := range []string{"connecting", "logged_out", "pairing", "connected"} {
		add(t, r.admin, connectionEvent(t, s))
	}
	eventually(t, "the event after the bad ones", func() bool { return slices.Contains(r.seen(), "connected") })
	if !slices.Equal(r.seen(), []string{"connecting", "connected"}) ||
		!slices.Equal(r.states(), []string{"handled", "dead handler_failed", "dead handler_failed", "handled"}) {
		t.Fatalf("seen %v, states %v", r.seen(), r.states())
	}
}

func TestRetriesAfterAConnectionFailureWithoutSkippingOrReordering(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	failures := 2
	r.run(func(ctx context.Context, tx pgx.Tx, e contracts.Event) (AfterCommit, error) {
		mu.Lock()
		fail := stateOf(e) == "connected" && failures > 0
		if fail {
			failures--
		}
		mu.Unlock()
		if fail {
			return nil, &pgconn.PgError{Code: "08006", Message: "connection failure"}
		}
		return record(ctx, tx, e)
	}, nil)
	add(t, r.admin, connectionEvent(t, "connected"))
	add(t, r.admin, connectionEvent(t, "disconnected"))
	eventually(t, "both events", func() bool { return len(r.seen()) == 2 })
	if !slices.Equal(r.seen(), []string{"connected", "disconnected"}) || !slices.Equal(r.states(), []string{"handled", "handled"}) {
		t.Fatalf("seen %v, states %v", r.seen(), r.states())
	}
}

func TestRunsAFollowUpOnlyOnceTheEventIsCommitted(t *testing.T) {
	r := newRig(t)
	observed := make(chan []string, 1)
	r.run(func(context.Context, pgx.Tx, contracts.Event) (AfterCommit, error) {
		return func() {
			var states []string
			_ = r.admin.InTx(context.Background(), func(tx pgx.Tx) error {
				rows, err := tx.Query(context.Background(), "SELECT state FROM gateway_events")
				if err != nil {
					return err
				}
				states, err = pgx.CollectRows(rows, pgx.RowTo[string])
				return err
			})
			observed <- states
		}, nil
	}, nil)
	add(t, r.admin, connectionEvent(t, "connected"))
	select {
	case got := <-observed:
		if !slices.Equal(got, []string{"handled"}) {
			t.Fatalf("the follow-up saw %v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no follow-up")
	}
}

func TestDeletesHandledEventsAfterAWeekAndKeepsTheRest(t *testing.T) {
	r := newRig(t)
	r.run(record, func(o *ConsumerOptions) { o.PollInterval = 10 * time.Millisecond; o.PurgeAlways = true })
	old, dead := connectionEvent(t, "connected"), connectionEvent(t, "connected")
	add(t, r.admin, old)
	add(t, r.admin, connectionEvent(t, "disconnected"))
	add(t, r.admin, dead)
	eventually(t, "handled events", func() bool { return slices.Equal(r.states(), []string{"handled", "handled", "handled"}) })
	exec(t, r.admin, "UPDATE gateway_events SET finished_at = now() - interval '8 days' WHERE id = $1", old.ID)
	exec(t, r.admin, "UPDATE gateway_events SET state = 'dead', error = 'handler_failed', finished_at = now() - interval '8 days' WHERE id = $1", dead.ID)
	eventually(t, "the purge", func() bool { return len(r.states()) == 2 })
	if !slices.Equal(r.states(), []string{"handled", "dead handler_failed"}) {
		t.Fatalf("got %v", r.states())
	}
}

func TestTakesEventsByTheirNumberNotWhereTheySit(t *testing.T) {
	r := newRig(t)
	first := connectionEvent(t, "connecting")
	add(t, r.admin, first)
	add(t, r.admin, connectionEvent(t, "connected"))
	add(t, r.admin, connectionEvent(t, "disconnected"))
	exec(t, r.admin, "UPDATE gateway_events SET created_at = now() WHERE id = $1", first.ID)
	if got := r.strings("SELECT payload -> 'data' ->> 'state' FROM gateway_events"); got[0] == "connecting" {
		t.Fatal("the test needs the first event stored last")
	}

	u, err := url.Parse(r.core.URL())
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for _, setting := range []string{"enable_indexscan", "enable_bitmapscan", "enable_indexonlyscan"} {
		q.Set(setting, "off")
	}
	u.RawQuery = q.Encode()
	tableOrder, err := store.Open(u.String(), store.WithAppName("kaya-core"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		r.stop()
		tableOrder.Close()
	}()
	r.core = tableOrder
	r.run(record, nil)
	eventually(t, "three handled events", func() bool { return len(r.seen()) == 3 })
	r.stop()
	if !slices.Equal(r.seen(), []string{"connecting", "connected", "disconnected"}) {
		t.Fatalf("got %v", r.seen())
	}
}

func TestSurvivesLosingItsConnectionInTheMiddleOfAnEvent(t *testing.T) {
	r := newRig(t)
	var once sync.Once
	r.run(func(ctx context.Context, tx pgx.Tx, e contracts.Event) (AfterCommit, error) {
		var killErr error
		once.Do(func() {
			var pid int
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				killErr = err
				return
			}
			exec(t, r.admin, "SELECT pg_terminate_backend($1)", pid)
			_, killErr = tx.Exec(ctx, "SELECT 1")
		})
		if killErr != nil {
			return nil, killErr
		}
		return record(ctx, tx, e)
	}, nil)
	add(t, r.admin, connectionEvent(t, "connected"))
	add(t, r.admin, connectionEvent(t, "disconnected"))
	eventually(t, "both events", func() bool { return len(r.seen()) == 2 })
	if !slices.Equal(r.seen(), []string{"connected", "disconnected"}) || !slices.Equal(r.states(), []string{"handled", "handled"}) {
		t.Fatalf("seen %v, states %v", r.seen(), r.states())
	}
}

func TestListensAgainAfterItsListeningConnectionIsLost(t *testing.T) {
	r := newRig(t)
	r.run(record, nil)
	eventually(t, "the listener", func() bool { return len(r.listeners()) > 0 })
	before := r.listeners()[0]
	exec(t, r.admin, "SELECT pg_terminate_backend($1::int)", before)
	eventually(t, "a new listener", func() bool {
		pids := r.listeners()
		return len(pids) > 0 && !slices.Contains(pids, before)
	})
	add(t, r.admin, connectionEvent(t, "connected"))
	eventually(t, "the event, without the poll", func() bool { return len(r.seen()) == 1 })
}

func TestDoesNotSleepThroughAnEventThatArrivesWhileFinishingARound(t *testing.T) {
	r := newRig(t)
	r.run(record, func(o *ConsumerOptions) { o.PurgeAlways = true })
	eventually(t, "the listener", func() bool { return len(r.listeners()) > 0 })
	arrived := connectionEvent(t, "connected")

	exec(t, r.admin, "ALTER TABLE gateway_events DISABLE TRIGGER gateway_events_notify")
	defer exec(t, r.admin, "ALTER TABLE gateway_events ENABLE TRIGGER gateway_events_notify")
	err := r.admin.InTx(context.Background(), func(blocker pgx.Tx) error {
		ctx := context.Background()
		if _, err := blocker.Exec(ctx, "LOCK TABLE gateway_events IN SHARE MODE"); err != nil {
			return err
		}
		exec(t, r.admin, "NOTIFY gateway_events")
		eventually(t, "the round to be held up at its clean-up", func() bool {
			return len(r.strings("SELECT pid::text FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE 'DELETE FROM gateway_events%'")) > 0
		})
		raw, err := json.Marshal(arrived)
		if err != nil {
			return err
		}
		if _, err := blocker.Exec(ctx, "INSERT INTO gateway_events (id, type, payload) VALUES ($1, 'connection.state', $2)", arrived.ID, raw); err != nil {
			return err
		}
		exec(t, r.admin, "NOTIFY gateway_events")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the event that arrived meanwhile", func() bool { return slices.Contains(r.seen(), "connected") })
}

func TestReportsThroughItsHealthCheckWhetherItIsRunning(t *testing.T) {
	r := newRig(t)
	r.run(record, nil)
	if err := r.consumer.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	r.consumer.Stop()
	if time.Since(started) > time.Second {
		t.Fatal("stop must not wait out the poll")
	}
	if err := r.consumer.Check(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("got %v", err)
	}
}

func TestStopsAtOnceWhateverItIsDoing(t *testing.T) {
	r := newRig(t)
	for _, wait := range []time.Duration{0, time.Millisecond, 3 * time.Millisecond, 10 * time.Millisecond, 30 * time.Millisecond} {
		r.run(record, nil)
		time.Sleep(wait)
		started := time.Now()
		r.consumer.Stop()
		if d := time.Since(started); d > time.Second {
			t.Fatalf("stop took %s, %s after start", d, wait)
		}
	}
}

func TestLeavesAnEventPendingWhenItStopsMidHandler(t *testing.T) {
	r := newRig(t)
	inside := make(chan struct{})
	r.run(func(ctx context.Context, tx pgx.Tx, e contracts.Event) (AfterCommit, error) {
		close(inside)
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil)
	add(t, r.admin, connectionEvent(t, "connected"))
	<-inside
	r.stop()
	if !slices.Equal(r.states(), []string{"pending"}) {
		t.Fatalf("a shutdown must never set an event aside, got %v", r.states())
	}
}

func TestSetsAsideAHandlerThatBreaksItsOwnConnection(t *testing.T) {
	r := newRig(t)
	r.run(func(ctx context.Context, tx pgx.Tx, e contracts.Event) (AfterCommit, error) {
		switch stateOf(e) {
		case "connecting":
			short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
			_, err := tx.Exec(short, "SELECT pg_sleep(1)")
			return nil, err
		case "pairing":
			rows, err := tx.Query(ctx, "SELECT generate_series(1, 1000)")
			if err != nil {
				return nil, err
			}
			_ = rows
			return nil, nil
		}
		return record(ctx, tx, e)
	}, nil)
	for _, s := range []string{"connecting", "pairing", "connected"} {
		add(t, r.admin, connectionEvent(t, s))
	}
	eventually(t, "the event after the broken ones", func() bool { return slices.Contains(r.seen(), "connected") })
	if !slices.Equal(r.states(), []string{"dead handler_failed", "dead handler_failed", "handled"}) {
		t.Fatalf("got %v", r.states())
	}
}

func TestReportsStoppedWhenItsParentIsCancelledAndCanStartAgain(t *testing.T) {
	r := newRig(t)
	parent, cancel := context.WithCancel(context.Background())
	r.consumer = NewConsumer(ConsumerOptions{
		DB: r.core, Validator: validator(t), Logger: quiet, Handle: record,
		PollInterval: time.Minute, RetryMin: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond,
	})
	r.consumer.Start(parent)
	cancel()
	eventually(t, "the health check to fail", func() bool { return errors.Is(r.consumer.Check(context.Background()), ErrNotRunning) })
	r.consumer.Start(context.Background())
	add(t, r.admin, connectionEvent(t, "connected"))
	eventually(t, "the event after restarting", func() bool { return len(r.seen()) == 1 })
}

func TestEveryStopWaitsForTheLoopsToEnd(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	r.run(func(context.Context, pgx.Tx, contracts.Event) (AfterCommit, error) {
		return func() { <-release }, nil
	}, nil)
	add(t, r.admin, connectionEvent(t, "connected"))
	eventually(t, "the follow-up to start", func() bool { return slices.Equal(r.states(), []string{"handled"}) })
	stopped := make(chan struct{}, 2)
	go func() { r.consumer.Stop(); stopped <- struct{}{} }()
	go func() { r.consumer.Stop(); stopped <- struct{}{} }()
	select {
	case <-stopped:
		t.Fatal("a stop returned while a follow-up was still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	<-stopped
	<-stopped
}

func TestClosesItsListeningConnectionWhenItStops(t *testing.T) {
	r := newRig(t)
	r.run(record, nil)
	eventually(t, "the listener", func() bool { return len(r.listeners()) > 0 })
	r.stop()
	eventually(t, "no listener", func() bool {
		return len(r.strings("SELECT pid::text FROM pg_stat_activity WHERE datname = current_database() AND query LIKE 'LISTEN%' AND state <> 'idle in transaction (aborted)' AND application_name LIKE 'kaya-core%'")) == 0
	})
}

func TestConsumerStartAndStopRacingNeverHang(t *testing.T) {
	r := newRig(t)
	c := NewConsumer(ConsumerOptions{
		DB: r.core, Validator: validator(t), Logger: quiet, Handle: record,
		PollInterval: 5 * time.Millisecond, RetryMin: 5 * time.Millisecond,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			var wg sync.WaitGroup
			wg.Go(func() { c.Start(context.Background()) })
			wg.Go(c.Stop)
			wg.Wait()
		}
		c.Stop()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("start and stop hung")
	}
}
