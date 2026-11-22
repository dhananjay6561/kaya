package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
)

type fakePublisher struct {
	mu       sync.Mutex
	err      error
	attempts []string
	stored   []contracts.Event
	entered  chan struct{}
	release  chan struct{}
}

// Publish fails on a finished context, as a database write would.
func (f *fakePublisher) Publish(ctx context.Context, e contracts.Event) error {
	if f.entered != nil {
		f.entered <- struct{}{}
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, e.ID)
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.err != nil {
		return f.err
	}
	f.stored = append(f.stored, e)
	return nil
}

func (f *fakePublisher) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakePublisher) tried() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.attempts)
}

func (f *fakePublisher) events() []contracts.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.stored)
}

func (f *fakePublisher) states(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, e := range f.events() {
		var data contracts.ConnectionState
		if err := json.Unmarshal(e.Data, &data); err != nil {
			t.Fatal(err)
		}
		s := data.State
		if data.Reason != "" {
			s += ": " + data.Reason
		}
		out = append(out, s)
	}
	return out
}

func linked() *fakeLoader {
	id := types.NewJID("919999999999", types.DefaultUserServer)
	id.Device = 7
	return &fakeLoader{device: &store.Device{ID: &id}}
}

func publishingConn(loader sessionLoader, made *clients) (*Conn, *fakePublisher) {
	c, _ := newTestConn(loader, made)
	pub := &fakePublisher{}
	c.SetPublisher(pub)
	c.publishRetry = time.Millisecond
	c.publishRetryMax = 4 * time.Millisecond
	c.publishTimeout = time.Second
	c.publishOnStop = time.Second
	return c, pub
}

func wantStates(t *testing.T, pub *fakePublisher, want ...string) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d stored state changes", len(want)), func() bool { return len(pub.events()) >= len(want) })
	if got := pub.states(t); !slices.Equal(got, want) {
		t.Fatalf("stored state changes:\n got %q\nwant %q", got, want)
	}
}

func TestEveryStateChangeIsPublishedOnceAndInOrder(t *testing.T) {
	made := &clients{}
	c, pub := publishingConn(linked(), made)
	started := time.Now().Add(-time.Second)
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	cli := made.get(0)

	cli.emit(&events.Connected{})
	cli.emit(&events.Connected{})
	cli.emit(&events.Disconnected{})
	cli.emit(&events.Disconnected{})
	cli.emit(&events.Connected{})
	stop(t, cancel, done)

	wantStates(t, pub,
		"connecting",
		"connected",
		"disconnected: "+ReasonReconnecting,
		"connected",
		"disconnected: "+ReasonShuttingDown,
	)
	validator, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	last := started
	for _, e := range pub.events() {
		if _, err := validator.EncodeEvent(e); err != nil {
			t.Fatalf("a published event does not fit the contract: %v", err)
		}
		var data contracts.ConnectionState
		_ = json.Unmarshal(e.Data, &data)
		if data.OwnJID != "919999999999@s.whatsapp.net" {
			t.Fatalf("own_jid must be the account without its device part, got %q", data.OwnJID)
		}
		if e.OccurredAt.Before(last) || e.OccurredAt.After(time.Now()) {
			t.Fatalf("occurred_at %s is out of order", e.OccurredAt)
		}
		last = e.OccurredAt.Time
	}
	if got := c.Status(); got.Since.IsZero() || got.Since.Before(started) {
		t.Fatalf("status must say when it last changed, got %+v", got)
	}
}

func TestAnUnpairedGatewayPublishesOnlyThatItIsNotPaired(t *testing.T) {
	c, pub := publishingConn(&fakeLoader{}, &clients{})
	cancel, done := runInBackground(t, c)
	wantStates(t, pub, "logged_out: "+ReasonNotPaired)
	stop(t, cancel, done)
	wantStates(t, pub, "logged_out: "+ReasonNotPaired)
	var data contracts.ConnectionState
	_ = json.Unmarshal(pub.events()[0].Data, &data)
	if data.OwnJID != "" {
		t.Fatalf("no account is linked, got own_jid %q", data.OwnJID)
	}
}

func TestALongOutageIsOneStateChange(t *testing.T) {
	down := errors.New("dial tcp: no route to host")
	made := &clients{connectErrs: []error{down, down, down, down, down}}
	c, pub := publishingConn(linked(), made)
	cancel, done := runInBackground(t, c)

	waitFor(t, "the attempt that works", func() bool { return made.count() == 6 })
	made.get(5).emit(&events.Connected{})
	wantStates(t, pub, "connecting", "disconnected: "+ReasonUnreachable, "connected")
	stop(t, cancel, done)
}

func TestLogoutIsPublished(t *testing.T) {
	made := &clients{}
	c, pub := publishingConn(linked(), made)
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	made.get(0).emit(&events.Connected{})
	made.get(0).emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	wantStates(t, pub, "connecting", "connected", "logged_out: "+ReasonLoggedOut)
	stop(t, cancel, done)
	wantStates(t, pub, "connecting", "connected", "logged_out: "+ReasonLoggedOut)
}

func TestStateChangesWaitForTheDatabaseAndKeepTheirOrder(t *testing.T) {
	made := &clients{}
	c, pub := publishingConn(linked(), made)
	pub.setErr(errors.New("connection refused"))
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	made.get(0).emit(&events.Connected{})
	made.get(0).emit(&events.Disconnected{})

	waitFor(t, "several attempts", func() bool { return len(pub.tried()) >= 3 })
	tried := pub.tried()
	if slices.ContainsFunc(tried, func(id string) bool { return id != tried[0] }) {
		t.Fatal("a later change must not be tried before the first one is stored")
	}
	if len(pub.events()) != 0 {
		t.Fatal("nothing can be stored while the database is away")
	}

	pub.setErr(nil)
	wantStates(t, pub, "connecting", "connected", "disconnected: "+ReasonReconnecting)
	if pub.events()[0].ID != tried[0] {
		t.Fatal("a retry must reuse the event id, so a write that did go through is not stored twice")
	}
	stop(t, cancel, done)
}

func TestTheShutdownStateIsStoredEvenThoughTheContextIsDone(t *testing.T) {
	made := &clients{}
	c, pub := publishingConn(linked(), made)
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	made.get(0).emit(&events.Connected{})
	wantStates(t, pub, "connecting", "connected")

	stop(t, cancel, done)
	if got := pub.states(t); len(got) != 3 || got[2] != "disconnected: "+ReasonShuttingDown {
		t.Fatalf("Run must not return before the last state is stored, got %q", got)
	}
}

func TestOnlyTheNewestStateChangesAreKeptWhileTheDatabaseIsAway(t *testing.T) {
	c, pub := publishingConn(linked(), &clients{})
	for i := range maxUnpublished + 10 {
		c.set(contracts.StateDisconnected, fmt.Sprintf("reason %d", i))
	}
	if !c.publishQueued(context.Background()) {
		t.Fatal("the queue must drain")
	}
	got := pub.states(t)
	if len(got) != maxUnpublished || got[0] != "disconnected: reason 10" || got[len(got)-1] != fmt.Sprintf("disconnected: reason %d", maxUnpublished+9) {
		t.Fatalf("want the newest %d changes, got %d from %q to %q", maxUnpublished, len(got), got[0], got[len(got)-1])
	}
}

func TestAStateChangeThatBreaksTheContractIsDroppedNotRetried(t *testing.T) {
	c, pub := publishingConn(linked(), &clients{})
	c.set(contracts.StateDisconnected, "first")
	c.set(contracts.StateConnected, "")
	pub.setErr(fmt.Errorf("encode: %w", contracts.ErrInvalid))
	if !c.publishQueued(context.Background()) {
		t.Fatal("an event that can never be stored must not block the ones behind it")
	}
	if got := len(pub.tried()); got != 2 {
		t.Fatalf("want each tried once, got %d attempts", got)
	}
}

func TestASilentConnectionIsReportedUntilItAnswersAgain(t *testing.T) {
	made := &clients{}
	c, pub := publishingConn(linked(), made)
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	cli := made.get(0)

	cli.emit(&events.KeepAliveTimeout{ErrorCount: 1})
	if got := c.Status().State; got != contracts.StateConnecting {
		t.Fatalf("a missed ping before the connection is up changes nothing, got %q", got)
	}
	cli.emit(&events.Connected{})
	cli.emit(&events.KeepAliveRestored{})
	cli.emit(&events.KeepAliveTimeout{ErrorCount: 1})
	cli.emit(&events.KeepAliveTimeout{ErrorCount: 2})
	if got := c.Status(); got.State != contracts.StateDisconnected || got.Reason != ReasonNotAnswering {
		t.Fatalf("got %+v", got)
	}
	cli.emit(&events.KeepAliveRestored{})
	wantStates(t, pub, "connecting", "connected", "disconnected: "+ReasonNotAnswering, "connected")

	cli.emit(&events.Disconnected{})
	cli.emit(&events.KeepAliveRestored{})
	if got := c.Status(); got.Reason != ReasonReconnecting {
		t.Fatalf("a late answer must not mark a dropped connection as connected, got %+v", got)
	}
	stop(t, cancel, done)
}

func TestAChangeDroppedFromTheQueueWhileBeingWrittenTakesNothingElseWithIt(t *testing.T) {
	c, pub := publishingConn(linked(), &clients{})
	pub.entered, pub.release = make(chan struct{}), make(chan struct{})
	c.set(contracts.StateDisconnected, "being written")

	drained := make(chan bool)
	go func() { drained <- c.publishQueued(context.Background()) }()
	<-pub.entered
	for i := range maxUnpublished {
		c.set(contracts.StateDisconnected, fmt.Sprintf("reason %d", i))
	}
	go func() {
		for range pub.entered {
			pub.release <- struct{}{}
		}
	}()
	pub.release <- struct{}{}
	if !<-drained {
		t.Fatal("the queue must drain")
	}
	close(pub.entered)

	got := pub.states(t)
	if len(got) != maxUnpublished+1 || got[1] != "disconnected: reason 0" {
		t.Fatalf("want the change being written and all %d behind it, got %d starting %q", maxUnpublished, len(got), got[:min(len(got), 2)])
	}
}

func TestStoringIsRetriedLessOftenTheLongerItFails(t *testing.T) {
	c, pub := publishingConn(linked(), &clients{})
	c.publishRetry, c.publishRetryMax = 20*time.Millisecond, time.Hour
	pub.setErr(errors.New("connection refused"))
	stop, done := make(chan struct{}), make(chan struct{})
	go c.publishLoop(context.Background(), stop, done)
	c.set(contracts.StateDisconnected, ReasonReconnecting)

	time.Sleep(150 * time.Millisecond)
	close(stop)
	<-done
	if got := len(pub.tried()); got < 2 || got > 6 {
		t.Fatalf("want a few attempts spaced further and further apart (about 4, plus the last one), got %d", got)
	}
}

func TestALateEventCannotLeaveTheStateWrongForGood(t *testing.T) {
	made := &clients{}
	c, pub := publishingConn(linked(), made)
	c.watchEvery, c.watchAfter = 2*time.Millisecond, 20*time.Millisecond
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	cli := made.get(0)
	cli.emit(&events.Connected{})

	cli.emit(&events.Disconnected{})
	time.Sleep(60 * time.Millisecond)
	if got := c.Status().State; got != contracts.StateDisconnected {
		t.Fatalf("a connection that really is down must stay reported as down, got %q", got)
	}

	cli.setSocketUp(true)
	waitFor(t, "the correction", func() bool { return c.Status().State == contracts.StateConnected })
	wantStates(t, pub, "connecting", "connected", "disconnected: "+ReasonReconnecting, "connected")
	stop(t, cancel, done)
}

func TestTheWatchLeavesOtherStatesAlone(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(paired(), made)
	c.minBackoff = time.Hour
	c.watchEvery, c.watchAfter = 2*time.Millisecond, 0
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	cli := made.get(0)
	cli.setSocketUp(true)
	cli.emit(&events.Connected{})
	cli.emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	time.Sleep(30 * time.Millisecond)
	if got := c.Status(); got.State != contracts.StateLoggedOut {
		t.Fatalf("only a dropped or silent connection may be corrected, got %+v", got)
	}
	stop(t, cancel, done)
}

func TestNothingFromTheOldClientChangesTheStateAfterShutdown(t *testing.T) {
	made := &clients{}
	c, pub := publishingConn(linked(), made)
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	cli := made.get(0)
	cli.emit(&events.Disconnected{})
	stop(t, cancel, done)

	cli.emit(&events.Connected{})
	if got := c.Status(); got.State != contracts.StateDisconnected || got.Reason != ReasonShuttingDown {
		t.Fatalf("a stopped gateway must not report itself connected, got %+v", got)
	}
	if got := pub.states(t); got[len(got)-1] != "disconnected: "+ReasonShuttingDown {
		t.Fatalf("got %q", got)
	}
}

func TestOnlyASilentConnectionIsRestoredByAnAnswer(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(paired(), made)
	c.minBackoff = time.Hour
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	cli := made.get(0)
	cli.emit(&events.Connected{})
	cli.emit(&events.ConnectFailure{})
	cli.emit(&events.KeepAliveRestored{})
	if got := c.Status(); got.State != contracts.StateDisconnected || got.Reason != ReasonConnectFailed {
		t.Fatalf("got %+v", got)
	}
	stop(t, cancel, done)
}

func TestASilentConnectionIsReplacedAfterAMinute(t *testing.T) {
	if whatsmeow.KeepAliveMaxFailTime != time.Minute {
		t.Fatalf("got %s", whatsmeow.KeepAliveMaxFailTime)
	}
}

func TestTheRetryDelayStartsOverOnlyAfterAConnectionHeld(t *testing.T) {
	const failures = 8
	for name, tc := range map[string]struct {
		hold  time.Duration
		drop  bool
		reset bool
	}{
		"held":                               {hold: 60 * time.Millisecond, reset: true},
		"dropped at once":                    {hold: 0, reset: false},
		"dropped at once, given up on later": {hold: 60 * time.Millisecond, drop: true, reset: false},
	} {
		t.Run(name, func(t *testing.T) {
			made := &clients{}
			c, _ := newTestConn(paired(), made)
			c.minBackoff, c.maxBackoff, c.stableAfter = time.Millisecond, time.Second, 30*time.Millisecond
			cancel, done := runInBackground(t, c)
			for i := range failures {
				waitFor(t, "the next client", func() bool { return made.count() == i+1 })
				made.get(i).emit(&events.ConnectFailure{})
			}
			waitFor(t, "the client that connects", func() bool { return made.count() == failures+1 })
			cli := made.get(failures)
			cli.emit(&events.Connected{})
			if tc.drop {
				cli.emit(&events.Disconnected{})
			}
			time.Sleep(tc.hold)
			cli.emit(&events.ConnectFailure{})
			dropped := time.Now()
			waitFor(t, "the client after it", func() bool { return made.count() == failures+2 })
			waited := time.Since(dropped)
			if tc.reset && waited > 100*time.Millisecond {
				t.Fatalf("waited %s; a connection that held must be retried from the shortest delay", waited)
			}
			if !tc.reset && waited < 200*time.Millisecond {
				t.Fatalf("waited only %s; a connection that never held must keep backing off", waited)
			}
			stop(t, cancel, done)
		})
	}
}
