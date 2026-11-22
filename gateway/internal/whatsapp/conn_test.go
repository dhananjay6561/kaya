package whatsapp

import (
	"bytes"
	"context"
	"errors"
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
	connectErrs  []error
	connects     int
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
	f.connects++
	if len(f.connectErrs) > 0 {
		err := f.connectErrs[0]
		f.connectErrs = f.connectErrs[1:]
		return err
	}
	return nil
}

func (f *fakeClient) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnected = true
}

func newTestConn(loader sessionLoader, cli *fakeClient) (*Conn, *bytes.Buffer) {
	var buf bytes.Buffer
	c := NewConn(nil, logging.New(&buf, "info"))
	c.sessions = loader
	c.newClient = func(*store.Device) client { return cli }
	c.minBackoff = time.Millisecond
	c.maxBackoff = 4 * time.Millisecond
	return c, &buf
}

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

func TestRunWithoutASessionWaitsAndReportsNotPaired(t *testing.T) {
	cli := &fakeClient{}
	c, _ := newTestConn(&fakeLoader{}, cli)
	cancel, done := runInBackground(t, c)

	waitFor(t, "not paired", func() bool { return c.Status().State == contracts.StateLoggedOut })
	if c.Status().Reason != ReasonNotPaired {
		t.Fatalf("got %+v", c.Status())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if cli.connects != 0 {
		t.Fatal("must not connect without a session")
	}
}

func TestRunRetriesUntilTheFirstConnectionWorks(t *testing.T) {
	loader := &fakeLoader{device: &store.Device{}, errs: []error{errors.New("db down")}}
	cli := &fakeClient{connectErrs: []error{errors.New("dial failed")}}
	c, logs := newTestConn(loader, cli)
	cancel, done := runInBackground(t, c)

	waitFor(t, "second connect", func() bool {
		cli.mu.Lock()
		defer cli.mu.Unlock()
		return cli.connects == 2
	})
	cli.mu.Lock()
	handler := cli.handler
	cli.mu.Unlock()
	handler(&events.Connected{})
	if c.Status().State != contracts.StateConnected {
		t.Fatalf("got %+v", c.Status())
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !cli.disconnected {
		t.Fatal("must disconnect on shutdown")
	}
	if !bytes.Contains(logs.Bytes(), []byte("db down")) || !bytes.Contains(logs.Bytes(), []byte("dial failed")) {
		t.Fatalf("failures must be logged: %s", logs)
	}
}

func TestRunStopsWhileWaitingToRetry(t *testing.T) {
	loader := &fakeLoader{errs: []error{errors.New("db down")}}
	c, _ := newTestConn(loader, &fakeClient{})
	c.minBackoff = time.Hour
	cancel, done := runInBackground(t, c)
	waitFor(t, "failure", func() bool { return c.Status().State == contracts.StateDisconnected })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run kept waiting after ctx was cancelled")
	}
}

func TestEventsUpdateTheStatus(t *testing.T) {
	c, _ := newTestConn(&fakeLoader{}, &fakeClient{})
	for _, tc := range []struct {
		evt   any
		state string
	}{
		{&events.Connected{}, contracts.StateConnected},
		{&events.Disconnected{}, contracts.StateDisconnected},
		{&events.LoggedOut{Reason: events.ConnectFailureLoggedOut}, contracts.StateLoggedOut},
		{&events.StreamReplaced{}, contracts.StateDisconnected},
		{&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople}, contracts.StateDisconnected},
		{&events.ClientOutdated{}, contracts.StateDisconnected},
	} {
		c.handle(tc.evt)
		if got := c.Status(); got.State != tc.state || (tc.state != contracts.StateConnected && got.Reason == "") {
			t.Fatalf("%T: got %+v, want state %s with a reason", tc.evt, got, tc.state)
		}
	}
}
