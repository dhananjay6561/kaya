package whatsapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/ingest"
	"github.com/dhananjay6561/kaya/gateway/internal/media"
	"github.com/dhananjay6561/kaya/gateway/internal/send"
	"github.com/dhananjay6561/kaya/kit/logging"
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
	socketUp     bool
	handler      whatsmeow.EventHandlerWithSuccessStatus
	groupQueries int
	downloadErrs []error
	downloads    int
	receipts     []string
}

func (f *fakeClient) DownloadHistorySync(context.Context, *waE2E.HistorySyncNotification, bool) (*waHistorySync.HistorySync, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads++
	if len(f.downloadErrs) > 0 {
		err := f.downloadErrs[0]
		f.downloadErrs = f.downloadErrs[1:]
		return nil, err
	}
	return &waHistorySync.HistorySync{}, nil
}

func (f *fakeClient) DownloadMediaWithPathToFile(context.Context, string, []byte, []byte, []byte, whatsmeow.MediaType, string, bool, whatsmeow.File) error {
	return nil
}

func (f *fakeClient) GenerateMessageID() types.MessageID { return "3EB0TEST" }

func (f *fakeClient) SendMessage(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	return whatsmeow.SendResponse{}, nil
}

func (f *fakeClient) ParseWebMessage(types.JID, *waWeb.WebMessageInfo) (*events.Message, error) {
	return nil, errors.New("not used")
}

func (f *fakeClient) SendProtocolMessageReceipt(_ context.Context, id types.MessageID, kind types.ReceiptType) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receipts = append(f.receipts, id+":"+string(kind))
	return nil
}

func (f *fakeClient) history() (downloads int, receipts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.downloads, append([]string(nil), f.receipts...)
}

func (f *fakeClient) AddEventHandlerWithSuccessStatus(h whatsmeow.EventHandlerWithSuccessStatus) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
	return 1
}

func (f *fakeClient) GetJoinedGroups(context.Context) ([]*types.GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groupQueries++
	return []*types.GroupInfo{{JID: types.NewJID("120363000000000000", types.GroupServer), GroupName: types.GroupName{Name: "Trip"}}}, nil
}

func (f *fakeClient) queries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groupQueries
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

// emit waits for the connection to register its handler: a client exists a moment before that.
func (f *fakeClient) emit(evt any) bool {
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(50 * time.Microsecond) {
		f.mu.Lock()
		h := f.handler
		f.mu.Unlock()
		if h != nil {
			return h(evt)
		}
		if time.Now().After(deadline) {
			panic("the connection never registered its event handler")
		}
	}
}

func (f *fakeClient) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.socketUp
}

func (f *fakeClient) IsLoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.socketUp
}

func (f *fakeClient) setSocketUp(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.socketUp = v
}

type fakeSink struct {
	mu       sync.Mutex
	refuse   bool
	handled  []any
	groups   int
	contacts int
	jobs     []*ingest.HistoryJob
	storeErr error
	log      []string
	fails    map[int64]int
}

func (f *fakeSink) NextHistoryJob(_ context.Context, skip []int64) (*ingest.HistoryJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, job := range f.jobs {
		if slices.Contains(skip, job.ID) {
			continue
		}
		if job.State == ingest.JobPending || job.State == ingest.JobStored {
			copied := *job
			return &copied, nil
		}
	}
	return nil, nil
}

func (f *fakeSink) job(id int64) *ingest.HistoryJob {
	for _, job := range f.jobs {
		if job.ID == id {
			return job
		}
	}
	return nil
}

func (f *fakeSink) StoreHistory(_ context.Context, job *ingest.HistoryJob, _ *waHistorySync.HistorySync, _ ingest.Parser, _ ingest.Own) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if job.WAID == "REFUSED" {
		return errors.New("history chunk refused by the database: 22021")
	}
	if f.storeErr != nil {
		err := f.storeErr
		f.storeErr = nil
		return err
	}
	f.job(job.ID).State = ingest.JobStored
	f.log = append(f.log, job.WAID+" stored")
	return nil
}

func (f *fakeSink) FailHistory(_ context.Context, job *ingest.HistoryJob, code string, final bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, job.WAID+" failed "+code)
	f.fails[job.ID]++
	if final || f.fails[job.ID] >= 5 {
		f.job(job.ID).State = ingest.JobDead
		return true, nil
	}
	return false, nil
}

func (f *fakeSink) FinishHistory(_ context.Context, job *ingest.HistoryJob) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j := f.job(job.ID); j.State == ingest.JobStored {
		j.State = ingest.JobDone
		f.log = append(f.log, job.WAID+" done")
	}
	return nil
}

func (f *fakeSink) history() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakeSink) Handle(_ context.Context, _ ingest.Own, evt any) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse {
		return false
	}
	f.handled = append(f.handled, evt)
	return true
}

func (f *fakeSink) SyncGroups(_ context.Context, groups []*types.GroupInfo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups += len(groups)
	return nil
}

func (f *fakeSink) SyncContacts(context.Context, []ingest.ContactNames) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contacts++
	return nil
}

func (f *fakeSink) setRefuse(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = v
}

func (f *fakeSink) seen() (handled, groups int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.handled), f.groups
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
	c := NewConn(nil, nil, logging.New(&buf, "info"))
	c.sessions = loader
	c.newClient = made.next
	c.minBackoff = time.Millisecond
	c.maxBackoff = 4 * time.Millisecond
	c.storeRetry = 5 * time.Millisecond
	c.watchEvery = time.Hour
	c.storeCheckEvery = 2 * time.Millisecond
	c.storeCheckTimeout = time.Second
	c.historyPoll = 10 * time.Millisecond
	c.historyRetry = 2 * time.Millisecond
	c.mediaPoll = 10 * time.Millisecond
	c.sendPoll = 10 * time.Millisecond
	c.groupSyncDebounce = 5 * time.Millisecond
	c.groupSyncMinGap = 30 * time.Millisecond
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

func TestEventsAreAcknowledgedOnlyWhenTheSinkStoredThem(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(paired(), made)
	sink := &fakeSink{}
	c.sink = sink
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	first := made.get(0)

	if !first.emit(&events.Message{}) {
		t.Fatal("a stored message is acknowledged")
	}
	sink.setRefuse(true)
	if first.emit(&events.Message{}) {
		t.Fatal("a message the sink could not store must not be acknowledged")
	}
	if got := c.Status(); got.State != contracts.StateDisconnected || got.Reason != ReasonDatabaseDown {
		t.Fatalf("got %+v", got)
	}
	sink.setRefuse(false)
	if first.emit(&events.Message{}) {
		t.Fatal("once a connection failed to store, nothing more on it is acknowledged")
	}

	waitFor(t, "a new connection", func() bool { return made.count() == 2 })
	if !first.isDisconnected() {
		t.Fatal("the failed connection must be dropped so WhatsApp delivers again")
	}
	if first.emit(&events.Message{}) {
		t.Fatal("an event from a replaced connection is never acknowledged")
	}
	if !made.get(1).emit(&events.Message{}) {
		t.Fatal("the new connection stores and acknowledges again")
	}
	if handled, _ := sink.seen(); handled != 2 {
		t.Fatalf("want 2 stored events, got %d", handled)
	}
	stop(t, cancel, done)
}

func TestGroupNamesAreFetchedOnConnectAndBatchedAfterThat(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(paired(), made)
	sink := &fakeSink{}
	c.sink = sink
	c.groupSyncMinGap = 200 * time.Millisecond
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	cli := made.get(0)

	cli.emit(&events.Connected{})
	waitFor(t, "the first sweep", func() bool { return cli.queries() == 1 })
	cli.emit(&events.Connected{})
	time.Sleep(15 * time.Millisecond)
	if cli.queries() != 1 {
		t.Fatalf("a reconnect within 12 h must not query again, got %d queries", cli.queries())
	}

	for range 5 {
		c.RequestGroupSync()
	}
	time.Sleep(40 * time.Millisecond)
	if cli.queries() != 1 {
		t.Fatal("a requested sweep waits for the minimum gap")
	}
	waitFor(t, "one batched sweep", func() bool { return cli.queries() == 2 })
	time.Sleep(60 * time.Millisecond)
	if cli.queries() != 2 {
		t.Fatalf("five requests make one sweep, got %d queries", cli.queries())
	}
	if _, groups := sink.seen(); groups != 2 {
		t.Fatalf("each sweep stores its groups, got %d", groups)
	}
	stop(t, cancel, done)
}

func TestHistoryIsConfirmedOnlyAfterItIsStored(t *testing.T) {
	notification, _ := proto.Marshal(&waE2E.HistorySyncNotification{DirectPath: proto.String("/v/t62/x")})
	made := &clients{}
	c, _ := newTestConn(paired(), made)
	sink := &fakeSink{
		storeErr: ingest.ErrUnavailable,
		fails:    map[int64]int{},
		jobs: []*ingest.HistoryJob{
			{ID: 1, WAID: "GONE", State: ingest.JobPending, Notification: notification},
			{ID: 2, WAID: "BROKEN", State: ingest.JobPending, Notification: []byte{0xff, 0xff}},
			{ID: 3, WAID: "REFUSED", State: ingest.JobPending, Notification: notification},
			{ID: 4, WAID: "CHUNK", State: ingest.JobPending, Notification: notification},
			{ID: 5, WAID: "RESUMED", State: ingest.JobStored},
		},
	}
	c.sink = sink
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	cli := made.get(0)
	cli.mu.Lock()
	cli.downloadErrs = []error{whatsmeow.ErrMediaDownloadFailedWith410}
	cli.mu.Unlock()

	time.Sleep(30 * time.Millisecond)
	if downloads, _ := cli.history(); downloads != 0 {
		t.Fatal("history must wait until WhatsApp is connected")
	}
	cli.emit(&events.Connected{})

	waitFor(t, "every job handled", func() bool {
		_, receipts := cli.history()
		return len(receipts) == 5
	})
	log := strings.Join(sink.history(), ", ")
	for _, step := range []string{"GONE failed http_410", "BROKEN failed unreadable", "CHUNK stored", "CHUNK done", "RESUMED done"} {
		if !strings.Contains(log, step) {
			t.Fatalf("missing %q in %s", step, log)
		}
	}
	if n := strings.Count(log, "REFUSED failed store_refused"); n != 5 {
		t.Fatalf("a chunk the database refuses is counted and given up on after 5 tries, got %d in %s", n, log)
	}
	if strings.Index(log, "RESUMED done") > strings.LastIndex(log, "REFUSED failed") {
		t.Fatalf("a failing chunk must not hold up the ones behind it: %s", log)
	}
	_, receipts := cli.history()
	slices.Sort(receipts)
	if strings.Join(receipts, ",") != "BROKEN:hist_sync,CHUNK:hist_sync,GONE:hist_sync,REFUSED:hist_sync,RESUMED:hist_sync" {
		t.Fatalf("each chunk is confirmed once, after it is stored or given up on: %v", receipts)
	}
	stop(t, cancel, done)
}

func TestClassifyHistoryDownload(t *testing.T) {
	for _, tc := range []struct {
		err              error
		code             string
		final, countable bool
	}{
		{whatsmeow.ErrMediaDownloadFailedWith404, "http_404", true, true},
		{fmt.Errorf("failed to download: %w", whatsmeow.ErrMediaDownloadFailedWith403), "http_403", true, true},
		{errors.New("failed to decompress: zlib: invalid header"), "bad_blob", false, true},
		{errors.New("failed to unmarshal: proto: cannot parse"), "bad_blob", false, true},
		{whatsmeow.ErrInvalidMediaHMAC, "bad_blob", false, true},
		{whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 500}}, "http_error", false, true},
		{errors.New("websocket not connected"), "download_failed", false, false},
	} {
		code, final, countable := classifyHistoryDownload(tc.err)
		if code != tc.code || final != tc.final || countable != tc.countable {
			t.Errorf("%v: got %s %v %v", tc.err, code, final, countable)
		}
	}
}

type fakeMedia struct {
	mu    sync.Mutex
	left  int
	steps int
}

func (f *fakeMedia) Step(_ context.Context, dl media.Downloader) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if dl == nil || f.left == 0 {
		return false
	}
	f.left--
	f.steps++
	return true
}

func (f *fakeMedia) done() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.steps
}

func TestRequestedMediaIsDownloadedOnlyWhileConnected(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(paired(), made)
	worker := &fakeMedia{left: 3}
	c.SetMedia(worker)
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })

	c.RequestMedia()
	time.Sleep(30 * time.Millisecond)
	if worker.done() != 0 {
		t.Fatal("media must wait until WhatsApp is connected")
	}
	made.get(0).emit(&events.Connected{})
	waitFor(t, "the queue drained", func() bool { return worker.done() == 3 })
	stop(t, cancel, done)
}

func TestMergeContactsKeepsTheSavedName(t *testing.T) {
	pn := types.NewJID("911111111111", types.DefaultUserServer)
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	stranger := types.NewJID("100000000000002", types.HiddenUserServer)
	all := map[types.JID]types.ContactInfo{
		pn:       {FullName: "Rahul Sharma", FirstName: "Rahul", PushName: "rahul.s"},
		lid:      {PushName: "rahul.s", BusinessName: "Sharma Travels"},
		stranger: {PushName: "Someone"},
	}
	for range 50 {
		got := mergeContacts(all, map[types.JID]types.JID{pn: lid})
		if len(got) != 2 {
			t.Fatalf("want one entry per person, got %+v", got)
		}
		for _, c := range got {
			switch c.Address.PN {
			case pn.String():
				want := ingest.ContactNames{
					Address: ingest.Address{LID: lid.String(), PN: pn.String()},
					Saved:   "Rahul Sharma", First: "Rahul", Push: "rahul.s", Business: "Sharma Travels", AddressBook: true,
				}
				if c != want {
					t.Fatalf("got %+v", c)
				}
			case "":
				if c.Address.LID != stranger.String() || c.AddressBook || c.Push != "Someone" {
					t.Fatalf("a person known by LID alone must not clear a saved name: %+v", c)
				}
			default:
				t.Fatalf("unexpected entry %+v", c)
			}
		}
	}
}

func TestAPanickingSinkIsNeverAcknowledged(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(paired(), made)
	c.sink = &panickySink{}
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	if made.get(0).emit(&events.Message{}) {
		t.Fatal("a panic must not count as stored")
	}
	stop(t, cancel, done)
}

type panickySink struct{ fakeSink }

func (*panickySink) Handle(context.Context, ingest.Own, any) bool { panic("bug") }

type fakeSender struct {
	mu      sync.Mutex
	steps   []bool
	owns    []ingest.Own
	entered chan struct{}
	release chan struct{}
}

func (f *fakeSender) Step(_ context.Context, cli send.Client, own ingest.Own) bool {
	if f.entered != nil {
		f.entered <- struct{}{}
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, cli != nil)
	f.owns = append(f.owns, own)
	return false
}

func (f *fakeSender) seen() (withClient, without int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, connected := range f.steps {
		if connected {
			withClient++
		} else {
			without++
		}
	}
	return withClient, without
}

func TestTheSendWorkerGetsAClientOnlyWhileConnected(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(linked(), made)
	sender := &fakeSender{}
	c.SetSender(sender)
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })

	c.RequestSend()
	waitFor(t, "a step while still connecting", func() bool { _, without := sender.seen(); return without > 0 })
	if with, _ := sender.seen(); with != 0 {
		t.Fatal("nothing may be sent before WhatsApp is connected")
	}

	made.get(0).emit(&events.Connected{})
	waitFor(t, "a step with the client", func() bool { with, _ := sender.seen(); return with > 0 })
	sender.mu.Lock()
	own := sender.owns[len(sender.owns)-1]
	sender.mu.Unlock()
	if own.PN != "919999999999@s.whatsapp.net" {
		t.Fatalf("the worker must be told whose account this is, got %+v", own)
	}

	made.get(0).emit(&events.KeepAliveTimeout{ErrorCount: 1})
	before, _ := sender.seen()
	c.RequestSend()
	time.Sleep(30 * time.Millisecond)
	if after, _ := sender.seen(); after != before {
		t.Fatal("nothing may be sent over a connection that stopped answering")
	}
	stop(t, cancel, done)
}

func TestRunWaitsForASendThatIsUnderWay(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(linked(), made)
	sender := &fakeSender{entered: make(chan struct{}, 8), release: make(chan struct{})}
	c.SetSender(sender)
	c.sendPoll = time.Hour
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })

	c.RequestSend()
	<-sender.entered
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Run returned while a send was still recording its outcome (%v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(sender.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop once the send finished")
	}
}

type fakeStore struct {
	mu     sync.Mutex
	down   bool
	checks int
}

func (f *fakeStore) check(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if f.down {
		return errors.New("failed to connect to `user=kaya_gateway database=kaya`")
	}
	return nil
}

func (f *fakeStore) set(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks
}

func TestTheConnectionIsDroppedWhileTheDatabaseIsAwayAndComesBack(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(paired(), made)
	db := &fakeStore{}
	c.SetStoreCheck(db.check)
	c.storeRetry = 20 * time.Millisecond
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	first := made.get(0)
	first.emit(&events.Connected{})
	waitFor(t, "a few checks", func() bool { return db.count() >= 3 })
	if first.isDisconnected() || c.Status().State != contracts.StateConnected {
		t.Fatal("a database that answers must leave the connection alone")
	}

	db.set(true)
	waitFor(t, "the connection to be dropped", first.isDisconnected)
	if first.emit(&events.Message{}) {
		t.Fatal("nothing from the dropped connection may be acknowledged")
	}
	if got := c.Status(); got.Reason != ReasonDatabaseDown {
		t.Fatalf("health must say why, in its fixed words: %+v", got)
	}

	waitFor(t, "a new connection", func() bool { return made.count() >= 2 })
	second := made.get(1)
	second.emit(&events.Connected{})
	waitFor(t, "the new connection to be dropped too, while the database is still away", second.isDisconnected)
	if got := c.Status(); got.State != contracts.StateDisconnected || got.Reason != ReasonDatabaseDown {
		t.Fatalf("got %+v", got)
	}

	db.set(false)
	waitFor(t, "a third connection", func() bool { return made.count() >= 3 })
	third := made.get(made.count() - 1)
	third.emit(&events.Connected{})
	time.Sleep(30 * time.Millisecond)
	if third.isDisconnected() || c.Status().State != contracts.StateConnected {
		t.Fatalf("once the database answers the connection must stay up, got %+v", c.Status())
	}
	stop(t, cancel, done)
}

func TestOneMissedDatabaseCheckDoesNotDropTheConnection(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(paired(), made)
	calls := 0
	var mu sync.Mutex
	c.SetStoreCheck(func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if calls++; calls%2 == 0 {
			return errors.New("timeout")
		}
		return nil
	})
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	made.get(0).emit(&events.Connected{})
	waitFor(t, "several checks", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 10
	})
	if made.get(0).isDisconnected() || made.count() != 1 {
		t.Fatal("a check that fails once and then passes is a blip, not an outage")
	}
	stop(t, cancel, done)
}

func TestTheDatabaseIsNotCheckedWhenNothingIsConnected(t *testing.T) {
	made := &clients{}
	c, _ := newTestConn(paired(), made)
	db := &fakeStore{down: true}
	c.SetStoreCheck(db.check)
	c.minBackoff = time.Hour
	cancel, done := runInBackground(t, c)
	waitFor(t, "first client", func() bool { return made.count() == 1 })
	made.get(0).emit(&events.Connected{})
	made.get(0).emit(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	time.Sleep(10 * time.Millisecond)
	before := db.count()
	time.Sleep(30 * time.Millisecond)
	if got := c.Status(); got.State != contracts.StateLoggedOut || db.count() != before {
		t.Fatalf("a logged-out gateway has no connection to drop: %+v, %d checks since", got, db.count()-before)
	}
	stop(t, cancel, done)
}
