package whatsapp

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/logging"
)

type fakeLoader struct {
	mu     sync.Mutex
	errs   []error
	device *store.Device
}

func (f *fakeLoader) Load(context.Context) (*store.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, err
	}
	return f.device, nil
}

type fakeClient struct {
	mu           sync.Mutex
	connectErr   error
	disconnected bool
	handler      whatsmeow.EventHandler
}

func (f *fakeClient) AddEventHandler(h whatsmeow.EventHandler) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
	return 1
}

func (f *fakeClient) ConnectContext(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connectErr
}

func (f *fakeClient) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnected = true
}

func (f *fakeClient) emit(evt any) {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	h(evt)
}

func (f *fakeClient) isDisconnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disconnected
}

type clients struct {
	mu          sync.Mutex
	made        []*fakeClient
	connectErrs []error
}

func (c *clients) next(*store.Device) client {
	c.mu.Lock()
	defer c.mu.Unlock()
	cli := &fakeClient{}
	if len(c.connectErrs) > 0 {
		cli.connectErr = c.connectErrs[0]
		c.connectErrs = c.connectErrs[1:]
	}
	c.made = append(c.made, cli)
	return cli
}

func (c *clients) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.made)
}

func (c *clients) get(i int) *fakeClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.made[i]
}

func newTestConn(loader sessionLoader, made *clients) (*Conn, *bytes.Buffer) {
	var buf bytes.Buffer
	c := NewConn(nil, logging.New(&buf, "info"))
	c.sessions = loader
	c.newClient = made.next
	c.minBackoff = time.Millisecond
	c.maxBackoff = 4 * time.Millisecond
	return c, &buf
}

func paired() *fakeLoader { return &fakeLoader{device: &store.Device{}} }

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func runInBackground(t *testing.T, c *Conn) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, done
}

func stop(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after ctx was cancelled")
	}
}

func TestRunWithoutASessionWaitsAndReportsNotPaired(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(&fakeLoader{}, made)
	cancel, done := runInBackground(t, c)

	waitFor(t, "not paired", func() bool { return c.Status().Reason == ReasonNotPaired })
	if c.Status().State != contracts.StateLoggedOut {
		t.Fatalf("got %+v", c.Status())
	}
	stop(t, cancel, done)
	if made.count() != 0 {
		t.Fatal("must not connect without a session")
	}
}

func TestRunRetriesUntilTheFirstConnectionWorks(t *testing.T) {
	loader := &fakeLoader{device: &store.Device{}, errs: []error{errors.New("db down at 10.0.0.5")}}
	made := &clients{connectErrs: []error{errors.New("dial tcp: lookup web.whatsapp.com")}}
	c, logs := newTestConn(loader, made)
	cancel, done := runInBackground(t, c)

	waitFor(t, "second client", func() bool { return made.count() == 2 })
	made.get(1).emit(&events.Connected{})
	if c.Status().State != contracts.StateConnected {
		t.Fatalf("got %+v", c.Status())
	}

	stop(t, cancel, done)
	if !made.get(1).isDisconnected() {
		t.Fatal("must disconnect on shutdown")
	}
	if !strings.Contains(logs.String(), "db down") || !strings.Contains(logs.String(), "lookup web.whatsapp.com") {
		t.Fatalf("failures must be logged: %s", logs)
	}
}

func TestStatusNeverCarriesErrorDetails(t *testing.T) {
	for _, tc := range []struct {
		loader *fakeLoader
		made   *clients
		want   string
	}{
		{&fakeLoader{errs: []error{errors.New("failed to connect to `user=kaya database=kaya`")}}, &clients{}, ReasonStoreUnavailable},
		{&fakeLoader{errs: []error{ErrSeveralSessions}}, &clients{}, ReasonSeveralSessions},
		{paired(), &clients{connectErrs: []error{errors.New("dial 157.240.0.1:443")}}, ReasonUnreachable},
	} {
		c, _ := newTestConn(tc.loader, tc.made)
		c.minBackoff = time.Hour
		cancel, done := runInBackground(t, c)
		waitFor(t, tc.want, func() bool { return c.Status().Reason == tc.want })
		stop(t, cancel, done)
	}
}

func TestRunStopsWhileWaitingToRetry(t *testing.T) {
	c, _ := newTestConn(&fakeLoader{errs: []error{errors.New("db down")}}, &clients{})
	c.minBackoff = time.Hour
	cancel, done := runInBackground(t, c)
	waitFor(t, "failure", func() bool { return c.Status().State == contracts.StateDisconnected })
	stop(t, cancel, done)
}

func TestRunReconnectsWhenWhatsmeowGivesUp(t *testing.T) {
	for name, evt := range map[string]any{
		"connect failure": &events.ConnectFailure{Reason: events.ConnectFailureBadUserAgent},
		"token refresh":   &events.CATRefreshError{Error: errors.New("boom")},
		"temporary ban":   &events.TemporaryBan{Expire: 20 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			made := &clients{}
			c, _ := newTestConn(paired(), made)
			cancel, done := runInBackground(t, c)
			waitFor(t, "first client", func() bool { return made.count() == 1 })

			started := time.Now()
			made.get(0).emit(evt)
			waitFor(t, "a new connection", func() bool { return made.count() == 2 })
			if !made.get(0).isDisconnected() {
				t.Fatal("the old client must be disconnected")
			}
			if ban, ok := evt.(*events.TemporaryBan); ok && time.Since(started) < ban.Expire {
				t.Fatalf("reconnected after %s, before the ban ended", time.Since(started))
			}
			stop(t, cancel, done)
		})
	}
}

func TestRunStopsTryingWhenTheSessionIsGone(t *testing.T) {
	for name, tc := range map[string]struct {
		evt    any
		state  string
		reason string
	}{
		"logged out": {&events.LoggedOut{Reason: events.ConnectFailureLoggedOut}, contracts.StateLoggedOut, ReasonLoggedOut},
		"replaced":   {&events.StreamReplaced{}, contracts.StateDisconnected, ReasonReplaced},
		"outdated":   {&events.ClientOutdated{}, contracts.StateDisconnected, ReasonOutdated},
	} {
		t.Run(name, func(t *testing.T) {
			made := &clients{}
			c, _ := newTestConn(paired(), made)
			cancel, done := runInBackground(t, c)
			waitFor(t, "first client", func() bool { return made.count() == 1 })

			made.get(0).emit(tc.evt)
			waitFor(t, "disconnect", func() bool { return made.get(0).isDisconnected() })
			time.Sleep(20 * time.Millisecond)
			if made.count() != 1 {
				t.Fatal("must not reconnect")
			}
			if got := c.Status(); got.State != tc.state || got.Reason != tc.reason {
				t.Fatalf("got %+v", got)
			}
			stop(t, cancel, done)
		})
	}
}

func TestEventsFromAnOldClientAreIgnored(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(paired(), made)
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	old := made.get(0)
	old.emit(&events.ConnectFailure{})
	waitFor(t, "second client", func() bool { return made.count() == 2 })
	made.get(1).emit(&events.Connected{})

	old.emit(&events.StreamReplaced{})
	old.emit(&events.Disconnected{})
	time.Sleep(20 * time.Millisecond)
	if got := c.Status(); got.State != contracts.StateConnected || made.get(1).isDisconnected() {
		t.Fatalf("a late event from the old client changed the new one: %+v", got)
	}
	stop(t, cancel, done)
}

func TestReconnectDelayIsCapped(t *testing.T) {
	cli := whatsmeow.NewClient(&store.Device{}, nil)
	cli.AutoReconnectErrors = 500
	if !capReconnectErrors(cli) || cli.AutoReconnectErrors != maxReconnectErrors {
		t.Fatalf("got %d", cli.AutoReconnectErrors)
	}
	cli.AutoReconnectErrors = 3
	capReconnectErrors(cli)
	if cli.AutoReconnectErrors != 3 {
		t.Fatal("small counts must be left alone")
	}
}
