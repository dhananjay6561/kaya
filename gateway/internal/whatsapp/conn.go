package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/ingest"
	"github.com/dhananjay6561/kaya/gateway/internal/logging"
	"github.com/dhananjay6561/kaya/gateway/internal/media"
)

// Reasons reported in Status. They are fixed strings, so /health never carries error details.
const (
	ReasonNotPaired        = "not paired: run make pair"
	ReasonStoreUnavailable = "session store unavailable"
	ReasonSeveralSessions  = "more than one session is stored"
	ReasonUnreachable      = "cannot reach WhatsApp"
	ReasonReconnecting     = "connection lost, reconnecting"
	ReasonConnectFailed    = "WhatsApp refused the connection, retrying"
	ReasonTemporaryBan     = "temporarily banned by WhatsApp, retrying when the ban ends"
	ReasonLoggedOut        = "logged out from the phone: run make pair"
	ReasonReplaced         = "the session is in use elsewhere: stop the other gateway, then restart this one"
	ReasonOutdated         = "WhatsApp rejected this client version: update whatsmeow"
	ReasonShuttingDown     = "shutting down"
	ReasonDatabaseDown     = "database unavailable, reconnecting when it is back"
)

const (
	groupSyncEvery    = 12 * time.Hour
	groupSyncDebounce = 30 * time.Second
	groupSyncMinGap   = 10 * time.Minute
)

// Sink stores what the connection receives. Handle reports whether the event is safely stored;
// false means WhatsApp must deliver it again, and the connection is dropped until the store works.
type Sink interface {
	Handle(ctx context.Context, own ingest.Own, evt any) bool
	SyncGroups(ctx context.Context, groups []*types.GroupInfo) error
	SyncContacts(ctx context.Context, contacts []ingest.ContactNames) error
	NextHistoryJob(ctx context.Context, skip []int64) (*ingest.HistoryJob, error)
	StoreHistory(ctx context.Context, job *ingest.HistoryJob, data *waHistorySync.HistorySync, parser ingest.Parser, own ingest.Own) error
	FailHistory(ctx context.Context, job *ingest.HistoryJob, code string, final bool) (dead bool, err error)
	FinishHistory(ctx context.Context, job *ingest.HistoryJob) error
}

// maxReconnectErrors caps whatsmeow's reconnect delay, which grows by 2 s per failed attempt.
const maxReconnectErrors = 30

// Status is the connection state, using the states of the connection.state contract.
type Status struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type sessionLoader interface {
	Load(ctx context.Context) (*store.Device, error)
}

type client interface {
	AddEventHandlerWithSuccessStatus(handler whatsmeow.EventHandlerWithSuccessStatus) uint32
	ConnectContext(ctx context.Context) error
	Disconnect()
	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
	DownloadHistorySync(ctx context.Context, notification *waE2E.HistorySyncNotification, synchronousStorage bool) (*waHistorySync.HistorySync, error)
	ParseWebMessage(chatJID types.JID, webMsg *waWeb.WebMessageInfo) (*events.Message, error)
	SendProtocolMessageReceipt(ctx context.Context, id types.MessageID, msgType types.ReceiptType) error
	media.Downloader
}

// MediaWorker downloads the files that were asked for. Step handles one request and reports
// whether to go straight on to the next.
type MediaWorker interface {
	Step(ctx context.Context, dl media.Downloader) bool
}

// ending is what a connection that whatsmeow will not bring back asks Run to do next.
type ending struct {
	generation uint64
	retryAfter time.Duration
	permanent  bool
}

// Conn keeps Kaya connected to WhatsApp with the stored session.
type Conn struct {
	sessions  sessionLoader
	sink      Sink
	logger    *slog.Logger
	newClient func(*store.Device) client

	minBackoff time.Duration
	maxBackoff time.Duration
	storeRetry time.Duration

	ended      chan ending
	generation atomic.Uint64
	failed     atomic.Uint64

	historyAsk        chan struct{}
	historyPoll       time.Duration
	historyRetry      time.Duration
	media             MediaWorker
	mediaAsk          chan struct{}
	mediaPoll         time.Duration
	groupSyncAsk      chan struct{}
	groupSyncDebounce time.Duration
	groupSyncMinGap   time.Duration
	syncing           sync.Mutex

	mu            sync.RWMutex
	status        Status
	current       client
	device        *store.Device
	historyWaits  map[int64]historyWait
	lastGroupSync time.Time
	contactsDone  bool
}

// NewConn returns a connection that uses the session in sessions and hands what it receives to
// sink. A nil sink stores nothing and acknowledges everything.
func NewConn(sessions *Sessions, sink Sink, logger *slog.Logger) *Conn {
	waLogger := NewLogger(logger)
	return &Conn{
		sessions: sessions,
		sink:     sink,
		logger:   logger,
		newClient: func(device *store.Device) client {
			cli := whatsmeow.NewClient(device, waLogger)
			cli.AutoReconnectHook = func(error) bool { return capReconnectErrors(cli) }
			cli.SynchronousAck = true
			cli.EnableDecryptedEventBuffer = true
			cli.ManualHistorySyncDownload = true
			cli.DisableManualHistorySyncReceipt = true
			return cli
		},
		minBackoff:        time.Second,
		maxBackoff:        time.Minute,
		storeRetry:        time.Minute,
		ended:             make(chan ending, 1),
		historyAsk:        make(chan struct{}, 1),
		historyPoll:       time.Minute,
		historyRetry:      time.Minute,
		mediaAsk:          make(chan struct{}, 1),
		mediaPoll:         time.Minute,
		groupSyncAsk:      make(chan struct{}, 1),
		groupSyncDebounce: groupSyncDebounce,
		groupSyncMinGap:   groupSyncMinGap,
		status:            Status{State: contracts.StateConnecting},
	}
}

func capReconnectErrors(cli *whatsmeow.Client) bool {
	cli.AutoReconnectErrors = min(cli.AutoReconnectErrors, maxReconnectErrors)
	return true
}

// Status reports the current connection state.
func (c *Conn) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

func (c *Conn) set(state, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = Status{State: state, Reason: reason}
}

// Run connects with the stored session and keeps it connected until ctx is done. Without a session
// it waits, since pairing happens in a separate `gateway pair` run that restarts the service.
// whatsmeow reconnects by itself after ordinary drops; Run starts a new connection, with backoff,
// when the first attempt fails or whatsmeow gives up (a refused connection or a temporary ban). It
// stops trying when the phone logged Kaya out, the session is in use elsewhere, or the client is
// too old.
func (c *Conn) Run(ctx context.Context) error {
	go c.groupSyncLoop(ctx)
	go c.historyLoop(ctx)
	go c.mediaLoop(ctx)
	backoff := c.minBackoff
	for {
		cli, err := c.connect(ctx)
		if ctx.Err() != nil {
			c.disconnect(cli)
			return nil
		}
		var wait time.Duration
		switch {
		case err != nil:
			c.logger.Warn("whatsapp connection failed, retrying", "retry_in", backoff.String(), logging.Err(err))
			wait = backoff
		case cli == nil:
			<-ctx.Done()
			return nil
		default:
			end, ok := c.waitForEnd(ctx)
			if !ok {
				c.disconnect(cli)
				return nil
			}
			cli.Disconnect()
			if end.permanent {
				<-ctx.Done()
				return nil
			}
			wait = max(end.retryAfter, backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(backoff*2, c.maxBackoff)
	}
}

func (c *Conn) waitForEnd(ctx context.Context) (ending, bool) {
	for {
		select {
		case <-ctx.Done():
			return ending{}, false
		case end := <-c.ended:
			if end.generation == c.generation.Load() {
				return end, true
			}
		}
	}
}

func (c *Conn) disconnect(cli client) {
	if cli != nil {
		cli.Disconnect()
	}
	c.set(contracts.StateDisconnected, ReasonShuttingDown)
}

func (c *Conn) connect(ctx context.Context) (client, error) {
	device, err := c.sessions.Load(ctx)
	if err != nil {
		reason := ReasonStoreUnavailable
		if errors.Is(err, ErrSeveralSessions) {
			reason = ReasonSeveralSessions
		}
		c.set(contracts.StateDisconnected, reason)
		return nil, err
	}
	if device == nil {
		c.set(contracts.StateLoggedOut, ReasonNotPaired)
		c.logger.Warn("whatsapp is not paired; run make pair")
		return nil, nil
	}
	generation := c.generation.Add(1)
	cli := c.newClient(device)
	c.mu.Lock()
	c.current, c.device = cli, device
	c.mu.Unlock()
	cli.AddEventHandlerWithSuccessStatus(func(evt any) bool {
		return c.receive(ctx, generation, evt)
	})
	c.set(contracts.StateConnecting, "")
	if err := cli.ConnectContext(ctx); err != nil {
		c.set(contracts.StateDisconnected, ReasonUnreachable)
		return nil, fmt.Errorf("connect to whatsapp: %w", err)
	}
	return cli, nil
}

// receive handles one event and reports whether whatsmeow may acknowledge it. An event from a
// connection that was replaced, or that already failed to store something, is never acknowledged:
// WhatsApp delivers it again on the next connection.
func (c *Conn) receive(ctx context.Context, generation uint64, evt any) (ok bool) {
	// whatsmeow counts a handler that panics as one that succeeded. Nothing here may be
	// acknowledged on the strength of a panic.
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("the event handler panicked", "event", fmt.Sprintf("%T", evt))
			ok = false
		}
	}()
	if generation != c.generation.Load() || generation == c.failed.Load() {
		return false
	}
	c.handle(generation, evt)
	if c.sink == nil {
		return true
	}
	if !c.sink.Handle(ctx, c.own(), evt) {
		if ctx.Err() == nil {
			c.failed.Store(generation)
			c.set(contracts.StateDisconnected, ReasonDatabaseDown)
			c.end(generation, ending{retryAfter: c.storeRetry})
		}
		return false
	}
	switch e := evt.(type) {
	case *events.Connected:
		c.afterConnected(ctx)
	case *events.AppStateSyncComplete:
		if e.Name == appstate.WAPatchCriticalUnblockLow {
			go c.syncContacts(ctx)
		}
	}
	return true
}

func (c *Conn) own() ingest.Own {
	c.mu.RLock()
	device := c.device
	c.mu.RUnlock()
	if device == nil {
		return ingest.Own{}
	}
	return ingest.AddressOf(device.GetJID(), device.GetLID())
}

func (c *Conn) afterConnected(ctx context.Context) {
	c.mu.Lock()
	contacts := !c.contactsDone
	c.contactsDone = true
	groups := c.lastGroupSync.IsZero() || time.Since(c.lastGroupSync) >= groupSyncEvery
	c.mu.Unlock()
	if contacts {
		go c.syncContacts(ctx)
	}
	if groups {
		go c.syncGroups(ctx)
	}
	c.RequestHistory()
	c.RequestMedia()
}

// SetMedia gives the connection the worker that downloads requested files. Call it before Run.
func (c *Conn) SetMedia(w MediaWorker) { c.media = w }

// RequestMedia wakes the media worker, for when a file was just asked for.
func (c *Conn) RequestMedia() {
	select {
	case c.mediaAsk <- struct{}{}:
	default:
	}
}

// mediaLoop downloads requested files one at a time while connected.
func (c *Conn) mediaLoop(ctx context.Context) {
	ticker := time.NewTicker(c.mediaPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.mediaAsk:
		case <-ticker.C:
		}
		for ctx.Err() == nil {
			c.mu.RLock()
			cli := c.current
			c.mu.RUnlock()
			if c.media == nil || cli == nil || c.Status().State != contracts.StateConnected || !c.media.Step(ctx, cli) {
				break
			}
		}
	}
}

// RequestHistory wakes the history worker, for when a chunk was just queued.
func (c *Conn) RequestHistory() {
	select {
	case c.historyAsk <- struct{}{}:
	default:
	}
}

// historyLoop works through queued history chunks one at a time while connected. Each chunk is
// downloaded, stored, and only then confirmed to WhatsApp, so a crash at any point repeats the
// chunk instead of losing it.
func (c *Conn) historyLoop(ctx context.Context) {
	ticker := time.NewTicker(c.historyPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.historyAsk:
		case <-ticker.C:
		}
		for ctx.Err() == nil && c.historyStep(ctx) {
		}
	}
}

// historyStep handles one job and reports whether to look for another straight away. A job that
// failed for now is skipped for a growing while, so the jobs behind it are not held up.
func (c *Conn) historyStep(ctx context.Context) bool {
	c.mu.RLock()
	cli := c.current
	c.mu.RUnlock()
	if c.sink == nil || cli == nil || c.Status().State != contracts.StateConnected {
		return false
	}
	job, err := c.sink.NextHistoryJob(ctx, c.historyWaiting())
	if err != nil {
		c.logger.Warn("could not read the history queue", logging.Err(err))
		return false
	}
	if job == nil {
		return false
	}
	if job.State == ingest.JobPending {
		var notification waE2E.HistorySyncNotification
		if err := proto.Unmarshal(job.Notification, &notification); err != nil {
			return c.failHistory(ctx, cli, job, "unreadable", true)
		}
		data, err := cli.DownloadHistorySync(ctx, &notification, true)
		if err != nil {
			code, final, countable := classifyHistoryDownload(err)
			if !countable {
				c.logger.Warn("could not download a history chunk, trying again later", "sync_type", job.SyncType, "error_code", code)
				return c.historyLater(job)
			}
			return c.failHistory(ctx, cli, job, code, final)
		}
		switch err := c.sink.StoreHistory(ctx, job, data, cli, c.own()); {
		case errors.Is(err, ingest.ErrUnavailable):
			c.logger.Warn("could not store a history chunk, trying again later", "sync_type", job.SyncType, logging.Err(err))
			return c.historyLater(job)
		case err != nil:
			c.logger.Warn("a history chunk was refused", "sync_type", job.SyncType, logging.Err(err))
			return c.failHistory(ctx, cli, job, "store_refused", false)
		}
	}
	if err := cli.SendProtocolMessageReceipt(ctx, job.WAID, types.ReceiptTypeHistorySync); err != nil {
		c.logger.Warn("could not confirm a history chunk, trying again later", logging.Err(err))
		return c.historyLater(job)
	}
	if err := c.sink.FinishHistory(ctx, job); err != nil {
		c.logger.Warn("could not finish a history job", logging.Err(err))
		return c.historyLater(job)
	}
	c.historyDone(job)
	go c.syncContacts(ctx)
	return true
}

type historyWait struct {
	fails int
	until time.Time
}

// historyLater puts a job aside for a while: one minute after the first failure, doubling up to
// half an hour. It returns true so the worker moves on to the next job.
func (c *Conn) historyLater(job *ingest.HistoryJob) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.historyWaits == nil {
		c.historyWaits = map[int64]historyWait{}
	}
	w := c.historyWaits[job.ID]
	w.fails++
	delay := c.historyRetry << min(w.fails-1, 5)
	w.until = time.Now().Add(min(delay, 30*c.historyRetry))
	c.historyWaits[job.ID] = w
	return true
}

func (c *Conn) historyDone(job *ingest.HistoryJob) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.historyWaits, job.ID)
}

// historyWaiting lists the jobs that are still waiting out a failure.
func (c *Conn) historyWaiting() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	ids := []int64{}
	for id, w := range c.historyWaits {
		if now.Before(w.until) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (c *Conn) failHistory(ctx context.Context, cli client, job *ingest.HistoryJob, code string, final bool) bool {
	dead, err := c.sink.FailHistory(ctx, job, code, final)
	if err != nil {
		c.logger.Warn("could not record a failed history job", logging.Err(err))
		return c.historyLater(job)
	}
	if !dead {
		return c.historyLater(job)
	}
	c.historyDone(job)
	if err := cli.SendProtocolMessageReceipt(ctx, job.WAID, types.ReceiptTypeHistorySync); err != nil {
		c.logger.Warn("could not confirm a history chunk that was given up on", logging.Err(err))
	}
	return true
}

// classifyHistoryDownload sorts download failures: gone from WhatsApp's servers (final), a payload
// that does not decode (counted, given up on after a few tries), or anything else, which is the
// network and worth retrying for as long as it takes.
func classifyHistoryDownload(err error) (code string, final, countable bool) {
	switch {
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403):
		return "http_403", true, true
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404):
		return "http_404", true, true
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410):
		return "http_410", true, true
	}
	var status whatsmeow.DownloadHTTPError
	msg := err.Error()
	switch {
	case errors.Is(err, whatsmeow.ErrInvalidMediaHMAC), errors.Is(err, whatsmeow.ErrInvalidMediaSHA256),
		errors.Is(err, whatsmeow.ErrInvalidMediaEncSHA256), errors.Is(err, whatsmeow.ErrTooShortFile),
		strings.Contains(msg, "decompress"), strings.Contains(msg, "unmarshal"):
		return "bad_blob", false, true
	case errors.As(err, &status):
		return "http_error", false, true
	}
	return "download_failed", false, false
}

// RequestGroupSync asks for the group names to be fetched again soon, for when a message arrives
// from a group that is not stored yet. Requests are batched and spaced out, since each one is a
// query to WhatsApp.
func (c *Conn) RequestGroupSync() {
	select {
	case c.groupSyncAsk <- struct{}{}:
	default:
	}
}

func (c *Conn) groupSyncLoop(ctx context.Context) {
	wait := func(d time.Duration) bool {
		if d <= 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.groupSyncAsk:
		}
		if !wait(c.groupSyncDebounce) {
			return
		}
		c.mu.RLock()
		last := c.lastGroupSync
		c.mu.RUnlock()
		if !last.IsZero() && !wait(c.groupSyncMinGap-time.Since(last)) {
			return
		}
		select {
		case <-c.groupSyncAsk:
		default:
		}
		c.syncGroups(ctx)
	}
}

// syncGroups fetches every joined group in one query and stores the names.
func (c *Conn) syncGroups(ctx context.Context) {
	c.syncing.Lock()
	defer c.syncing.Unlock()
	c.mu.Lock()
	cli := c.current
	c.lastGroupSync = time.Now()
	c.mu.Unlock()
	if cli == nil || c.sink == nil || ctx.Err() != nil {
		return
	}
	groups, err := cli.GetJoinedGroups(ctx)
	if err != nil {
		c.logger.Warn("could not fetch group names", logging.Err(err))
		return
	}
	if err := c.sink.SyncGroups(ctx, groups); err != nil {
		c.logger.Warn("could not store group names", logging.Err(err))
		return
	}
	c.logger.Info("group names synced", "groups", len(groups))
}

// syncContacts copies names from whatsmeow's own contact store, which it fills from the phone's
// address book. It reads the local database only; nothing is asked of WhatsApp.
func (c *Conn) syncContacts(ctx context.Context) {
	c.mu.RLock()
	device := c.device
	c.mu.RUnlock()
	if device == nil || device.Contacts == nil || c.sink == nil || ctx.Err() != nil {
		return
	}
	all, err := device.Contacts.GetAllContacts(ctx)
	if err != nil {
		c.logger.Warn("could not read contacts", logging.Err(err))
		return
	}
	var pns []types.JID
	for jid := range all {
		if jid.Server == types.DefaultUserServer {
			pns = append(pns, jid)
		}
	}
	lids := map[types.JID]types.JID{}
	if device.LIDs != nil && len(pns) > 0 {
		if lids, err = device.LIDs.GetManyLIDsForPNs(ctx, pns); err != nil {
			c.logger.Warn("could not read the lid map", logging.Err(err))
			lids = map[types.JID]types.JID{}
		}
	}
	names := mergeContacts(all, lids)
	if err := c.sink.SyncContacts(ctx, names); err != nil {
		c.logger.Warn("could not store contacts", logging.Err(err))
		return
	}
	c.logger.Info("contacts synced", "contacts", len(names))
}

// mergeContacts turns whatsmeow's contact rows into one entry per person. whatsmeow keeps a row
// for each spelling of a person: the one under the phone number has the address-book name, the one
// under the LID usually only the name they set themselves. Taken one by one, the LID row would wipe
// the saved name, so the rows are joined first and a name from either one is kept.
func mergeContacts(all map[types.JID]types.ContactInfo, lidForPN map[types.JID]types.JID) []ingest.ContactNames {
	pnForLID := make(map[types.JID]types.JID, len(lidForPN))
	for pn, lid := range lidForPN {
		if !lid.IsEmpty() {
			pnForLID[lid.ToNonAD()] = pn.ToNonAD()
		}
	}
	first := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	people := map[types.JID]*ingest.ContactNames{}
	var order []types.JID
	for jid, info := range all {
		key, alt := jid.ToNonAD(), types.EmptyJID
		switch key.Server {
		case types.DefaultUserServer:
			alt = lidForPN[key].ToNonAD()
		case types.HiddenUserServer:
			if pn, ok := pnForLID[key]; ok {
				key, alt = pn, key
			}
		}
		p := people[key]
		if p == nil {
			p = &ingest.ContactNames{}
			people[key] = p
			order = append(order, key)
		}
		addr := ingest.AddressOf(key, alt)
		p.Address = ingest.Address{LID: first(p.Address.LID, addr.LID), PN: first(p.Address.PN, addr.PN), Bot: first(p.Address.Bot, addr.Bot)}
		p.Saved, p.First = first(p.Saved, info.FullName), first(p.First, info.FirstName)
		p.Push, p.Business = first(p.Push, info.PushName), first(p.Business, info.BusinessName)
	}
	names := make([]ingest.ContactNames, 0, len(order))
	for _, key := range order {
		p := people[key]
		// Only the phone-number row is the address book. A person known by LID alone says nothing
		// about whether they are saved, so their saved name is left as it is.
		p.AddressBook = p.Address.PN != "" || p.Saved != ""
		names = append(names, *p)
	}
	return names
}

func (c *Conn) end(generation uint64, e ending) {
	e.generation = generation
	select {
	case c.ended <- e:
	default:
	}
}

func (c *Conn) handle(generation uint64, evt any) {
	switch e := evt.(type) {
	case *events.Connected:
		c.set(contracts.StateConnected, "")
		c.logger.Info("whatsapp connected")
	case *events.Disconnected:
		c.set(contracts.StateDisconnected, ReasonReconnecting)
		c.logger.Warn("whatsapp disconnected, reconnecting")
	case *events.LoggedOut:
		c.set(contracts.StateLoggedOut, ReasonLoggedOut)
		c.logger.Error("whatsapp logged out; pair again with make pair", "reason", e.Reason.String())
		c.end(generation, ending{permanent: true})
	case *events.StreamReplaced:
		c.set(contracts.StateDisconnected, ReasonReplaced)
		c.logger.Error("whatsapp session opened elsewhere; this connection stopped")
		c.end(generation, ending{permanent: true})
	case *events.ClientOutdated:
		c.set(contracts.StateDisconnected, ReasonOutdated)
		c.logger.Error("whatsapp rejected this client version; update whatsmeow")
		c.end(generation, ending{permanent: true})
	case *events.TemporaryBan:
		c.set(contracts.StateDisconnected, ReasonTemporaryBan)
		c.logger.Error("whatsapp temporarily banned this device", "code", int(e.Code), "expires_in", e.Expire.String())
		c.end(generation, ending{retryAfter: e.Expire})
	case *events.ConnectFailure:
		c.set(contracts.StateDisconnected, ReasonConnectFailed)
		c.logger.Warn("whatsapp refused the connection", "reason", e.Reason.String())
		c.end(generation, ending{})
	case *events.CATRefreshError:
		c.set(contracts.StateDisconnected, ReasonConnectFailed)
		c.logger.Warn("whatsapp token refresh failed", logging.Err(e.Error))
		c.end(generation, ending{})
	}
}
