package channel

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/kit/store"
)

const ownLID = "100000000000001@lid"

type inboxRig struct {
	t                                      *testing.T
	core, admin                            *store.DB
	own, friend, selfChat, friendChat, grp int64
	inbox                                  *Inbox
	next                                   int
}

func newInboxRig(t *testing.T) *inboxRig {
	t.Helper()
	core, admin := databases(t)
	r := &inboxRig{t: t, core: core, admin: admin}
	one := func(sql string, args ...any) int64 {
		var id int64
		if err := admin.QueryRow(bg, sql, args...).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	r.own = one("SELECT contact_id_for($1, $2)", ownLID, ownPN)
	r.friend = one("SELECT contact_id_for(NULL, '918888888888@s.whatsapp.net')")
	r.selfChat = one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.own)
	r.friendChat = one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.friend)
	r.grp = one("INSERT INTO chats (kind, jid) VALUES ('group', '120363000000000000@g.us') RETURNING id")
	exec(t, admin, "INSERT INTO connection_state (state, own_jid, occurred_at) VALUES ('connected', $1, now())", ownPN)
	t.Cleanup(r.stop)
	return r
}

func (r *inboxRig) stop() {
	if r.inbox != nil {
		r.inbox.Stop()
		r.inbox = nil
	}
}

type fixture struct {
	chat      int64
	sender    int64
	fromMe    *bool
	kind      string
	source    string
	change    string
	body      *string
	deleted   bool
	forwarded bool
	sentAt    time.Time
}

func text(s string) *string { return &s }
func no() *bool             { b := false; return &b }

func (r *inboxRig) message(f fixture) contracts.MessageStored {
	r.t.Helper()
	fromMe := f.fromMe == nil || *f.fromMe
	r.next++
	waID := fmt.Sprintf("3EB0TEST%04d", r.next)
	if f.sentAt.IsZero() {
		f.sentAt = time.Now()
	}
	if f.chat == 0 {
		f.chat = r.selfChat
	}
	if f.sender == 0 {
		f.sender = r.friend
		if fromMe {
			f.sender = r.own
		}
	}
	if f.kind == "" {
		f.kind = "text"
	}
	if f.source == "" {
		f.source = "live"
	}
	if f.change == "" {
		f.change = "new"
	}
	if f.body == nil {
		f.body = text("do the thing")
	}
	var deletedAt *time.Time
	if f.deleted {
		now := time.Now()
		deletedAt = &now
	}
	var id int64
	err := r.admin.QueryRow(bg, `
		INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, deleted_at, forwarded)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		f.chat, waID, f.sender, fromMe, f.sentAt, f.kind, f.body, f.source, deletedAt, f.forwarded).Scan(&id)
	if err != nil {
		r.t.Fatal(err)
	}
	return contracts.MessageStored{
		MessageID: id, ChatID: f.chat, SenderID: f.sender, WAID: waID, FromMe: fromMe,
		Kind: f.kind, Source: f.source, Change: f.change, SentAt: contracts.At(f.sentAt),
	}
}

func slash(text string) bool { return strings.HasPrefix(text, "/") }

func (r *inboxRig) record(m contracts.MessageStored, isUrgent func(string) bool) bool {
	r.t.Helper()
	box := r.inbox
	if box == nil {
		box = NewInbox(InboxOptions{DB: r.core, Logger: quiet, IsUrgent: isUrgent})
	}
	var added bool
	err := r.core.InTx(bg, func(tx pgx.Tx) error {
		var err error
		added, err = box.Record(bg, tx, m)
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return added
}

func (r *inboxRig) commands() []string {
	return column(r.t, r.admin, "SELECT wa_id || ' ' || state || coalesce(' ' || error, '') FROM commands ORDER BY id")
}

func (r *inboxRig) attempts() []string {
	return column(r.t, r.admin, "SELECT attempts::text FROM commands ORDER BY id")
}

func (r *inboxRig) settled() bool {
	for _, c := range r.commands() {
		if strings.HasSuffix(c, "pending") {
			return false
		}
	}
	return true
}

func (r *inboxRig) start(deliver Deliver, tune func(*InboxOptions), ready func() bool) *Inbox {
	r.t.Helper()
	o := InboxOptions{DB: r.core, Logger: quiet, Poll: time.Minute, RetryMin: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond}
	if tune != nil {
		tune(&o)
	}
	if ready == nil {
		ready = func() bool { return true }
	}
	r.inbox = NewInbox(o)
	r.inbox.Start(deliver, ready)
	return r.inbox
}

func states(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		_, rest, _ := strings.Cut(line, " ")
		out[i] = rest
	}
	return out
}

func TestTakesATextDJTypedInHisOwnChatOnce(t *testing.T) {
	r := newInboxRig(t)
	typed := r.message(fixture{})
	if !r.record(typed, slash) || r.record(typed, slash) {
		t.Fatal("the message is a command once, and never a second time")
	}
	if !slices.Equal(r.commands(), []string{typed.WAID + " pending"}) {
		t.Fatalf("got %v", r.commands())
	}
}

func TestTakesNothingElse(t *testing.T) {
	r := newInboxRig(t)
	cases := []struct {
		what string
		m    contracts.MessageStored
	}{
		{"a friend's message", r.message(fixture{chat: r.friendChat, fromMe: no()})},
		{"DJ's message to a friend", r.message(fixture{chat: r.friendChat})},
		{"DJ's message in a group", r.message(fixture{chat: r.grp})},
		{"an old message from the history sync", r.message(fixture{source: "history"})},
		{"an edit", r.message(fixture{change: "edited"})},
		{"a deletion", r.message(fixture{change: "deleted"})},
		{"a photo", r.message(fixture{kind: "image", body: text("a caption")})},
		{"a message deleted before it was looked at", r.message(fixture{deleted: true})},
		{"someone else's words, forwarded by DJ", r.message(fixture{forwarded: true})},
		{"a message from two days ago, handled now", r.message(fixture{sentAt: time.Now().Add(-48 * time.Hour)})},
	}
	for _, c := range cases {
		if r.record(c.m, slash) {
			t.Errorf("%s became a command", c.what)
		}
	}

	fromKaya := r.message(fixture{})
	exec(t, r.admin, `INSERT INTO send_requests (id, payload, expires_at)
		VALUES ('b3bb189e-8bf9-4888-9912-ace4e6543001',
		        '{"id": "b3bb189e-8bf9-4888-9912-ace4e6543001", "expires_at": "2026-11-23T10:00:00Z"}',
		        '2026-11-23T10:00:00Z')`)
	exec(t, r.admin, "UPDATE send_requests SET state = 'sending', wa_id = $1", fromKaya.WAID)
	if r.record(fromKaya, slash) {
		t.Error("a message Kaya sent is not a command to Kaya")
	}

	typed := r.message(fixture{})
	exec(t, r.admin, "UPDATE connection_state SET own_jid = '917777777777@s.whatsapp.net'")
	if r.record(typed, slash) {
		t.Error("the sender must be the account the gateway says is linked")
	}
	exec(t, r.admin, "UPDATE connection_state SET own_jid = $1", ownLID)
	if !r.record(typed, slash) {
		t.Error("either spelling of DJ's own address counts")
	}
	if !slices.Equal(r.commands(), []string{typed.WAID + " pending"}) {
		t.Fatalf("got %v", r.commands())
	}
}

func TestHandsCommandsOverOldestFirstOneAtATime(t *testing.T) {
	r := newInboxRig(t)
	sentAt := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	first := r.message(fixture{body: text("  first  "), sentAt: sentAt})
	second := r.message(fixture{body: text("second")})
	r.record(first, slash)
	r.record(second, slash)

	var mu sync.Mutex
	var seen []IncomingMessage
	var running, overlapped atomic.Int32
	r.start(func(_ context.Context, m IncomingMessage) (Handled, error) {
		if running.Add(1) > 1 {
			overlapped.Store(1)
		}
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		seen = append(seen, m)
		mu.Unlock()
		running.Add(-1)
		return Handled{}, nil
	}, nil, nil)
	eventually(t, "both commands", func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) == 2 })
	mu.Lock()
	defer mu.Unlock()
	if seen[0].ID != first.WAID || seen[0].Text != "first" || seen[1].ID != second.WAID || seen[1].Text != "second" {
		t.Fatalf("got %+v", seen)
	}
	if !seen[0].SentAt.Equal(sentAt) || time.Since(seen[0].ReceivedAt).Abs() > time.Minute {
		t.Fatalf("times: %+v", seen[0])
	}
	if overlapped.Load() != 0 {
		t.Fatal("commands overlapped")
	}
	eventually(t, "both marked done", r.settled)
}

func TestWakesForACommandRecordedWhileIdle(t *testing.T) {
	r := newInboxRig(t)
	seen := make(chan string, 1)
	box := r.start(func(_ context.Context, m IncomingMessage) (Handled, error) { seen <- m.Text; return Handled{}, nil }, nil, nil)
	time.Sleep(30 * time.Millisecond)
	r.record(r.message(fixture{body: text("typed just now")}), slash)
	box.Wake()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the command, without waiting for the poll")
	}
}

func TestKeepsCommandsWaitingWhileNobodyCanHandleThem(t *testing.T) {
	r := newInboxRig(t)
	typed := r.message(fixture{})
	r.record(typed, slash)
	var ready atomic.Bool
	seen := make(chan string, 1)
	box := r.start(func(_ context.Context, m IncomingMessage) (Handled, error) { seen <- m.ID; return Handled{}, nil }, nil, ready.Load)
	time.Sleep(40 * time.Millisecond)
	if !slices.Equal(r.commands(), []string{typed.WAID + " pending"}) || !slices.Equal(r.attempts(), []string{"0"}) {
		t.Fatalf("got %v %v", r.commands(), r.attempts())
	}
	ready.Store(true)
	box.Wake()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the command once a handler exists")
	}
}

type coded string

func (c coded) Error() string     { return "the reply was not sent" }
func (c coded) ErrorCode() string { return string(c) }

func TestMarksAFailedCommandAndCarriesOn(t *testing.T) {
	r := newInboxRig(t)
	bad := r.message(fixture{body: text("this one breaks")})
	gone := r.message(fixture{body: text("deleted before its turn")})
	good := r.message(fixture{body: text("this one works")})
	weird := r.message(fixture{body: text("this one fails oddly")})
	for _, m := range []contracts.MessageStored{bad, gone, good, weird} {
		r.record(m, slash)
	}
	exec(t, r.admin, "UPDATE messages SET deleted_at = now(), body = NULL WHERE id = $1", gone.MessageID)
	r.start(func(_ context.Context, m IncomingMessage) (Handled, error) {
		switch {
		case strings.Contains(m.Text, "breaks"):
			return Handled{}, coded("no_answer")
		case strings.Contains(m.Text, "oddly"):
			return Handled{}, coded("Not A Code!")
		}
		return Handled{}, nil
	}, nil, nil)
	eventually(t, "every command", r.settled)
	want := []string{bad.WAID + " failed no_answer", gone.WAID + " failed message_gone", good.WAID + " done", weird.WAID + " failed handler_failed"}
	if !slices.Equal(r.commands(), want) {
		t.Fatalf("got %v", r.commands())
	}
}

func TestDropsCommandsThatComeFasterThanAPersonTypes(t *testing.T) {
	r := newInboxRig(t)
	for i := range 5 {
		r.record(r.message(fixture{body: text(fmt.Sprintf("command %d", i))}), slash)
	}
	var mu sync.Mutex
	var seen []string
	r.start(func(_ context.Context, m IncomingMessage) (Handled, error) {
		mu.Lock()
		seen = append(seen, m.Text)
		mu.Unlock()
		return Handled{}, nil
	}, func(o *InboxOptions) { o.MaxPerMinute = 3 }, nil)
	eventually(t, "all five to be settled", r.settled)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []string{"command 0", "command 1", "command 2"}) ||
		!slices.Equal(states(r.commands()), []string{"done", "done", "done", "failed over_cap", "failed over_cap"}) {
		t.Fatalf("seen %v, commands %v", seen, r.commands())
	}
}

func TestRunsUrgentCommandsFirstAndNeverDropsThem(t *testing.T) {
	r := newInboxRig(t)
	for i := range 4 {
		r.record(r.message(fixture{body: text(fmt.Sprintf("command %d", i))}), slash)
	}
	r.record(r.message(fixture{body: text("/pause")}), slash)
	var mu sync.Mutex
	var seen []string
	r.start(func(_ context.Context, m IncomingMessage) (Handled, error) {
		mu.Lock()
		seen = append(seen, m.Text)
		mu.Unlock()
		return Handled{}, nil
	}, func(o *InboxOptions) { o.MaxPerMinute = 3 }, nil)
	eventually(t, "all five to be settled", r.settled)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []string{"/pause", "command 0", "command 1"}) ||
		!slices.Equal(states(r.commands()), []string{"done", "done", "failed over_cap", "failed over_cap", "done"}) {
		t.Fatalf("seen %v, commands %v", seen, r.commands())
	}
}

func TestKeepsTheExchangeAHandlerReports(t *testing.T) {
	r := newInboxRig(t)
	answered := r.message(fixture{body: text("a question")})
	r.record(answered, slash)
	r.record(r.message(fixture{body: text("/status")}), slash)
	r.start(func(_ context.Context, m IncomingMessage) (Handled, error) {
		if m.Text == "a question" {
			return Handled{AnswerID: "3EB0THEANSWER"}, nil
		}
		return Handled{}, nil
	}, nil, nil)
	eventually(t, "both to be settled", r.settled)
	got := column(t, r.admin, "SELECT question_message_id::text || ' ' || answer_wa_id FROM exchanges")
	if !slices.Equal(got, []string{fmt.Sprintf("%d 3EB0THEANSWER", answered.MessageID)}) {
		t.Fatalf("got %v", got)
	}
}

func TestDecidesUrgencyWhenRecorded(t *testing.T) {
	r := newInboxRig(t)
	r.record(r.message(fixture{body: text("plain words")}), func(string) bool { return true })
	if got := column(t, r.admin, "SELECT urgent::text FROM commands"); !slices.Equal(got, []string{"true"}) {
		t.Fatalf("got %v", got)
	}
}

func TestRunsEachCommandInOneWorkerEvenWhenTwoAreStarted(t *testing.T) {
	r := newInboxRig(t)
	for i := range 6 {
		r.record(r.message(fixture{body: text(fmt.Sprintf("command %d", i))}), slash)
	}
	var calls atomic.Int32
	deliver := func(context.Context, IncomingMessage) (Handled, error) {
		calls.Add(1)
		time.Sleep(5 * time.Millisecond)
		return Handled{}, nil
	}
	o := InboxOptions{DB: r.core, Logger: quiet, Poll: 10 * time.Millisecond, RetryMin: 5 * time.Millisecond}
	one, two := NewInbox(o), NewInbox(o)
	one.Start(deliver, func() bool { return true })
	two.Start(deliver, func() bool { return true })
	defer one.Stop()
	defer two.Stop()
	eventually(t, "all six", r.settled)
	if calls.Load() != 6 || !slices.Equal(r.attempts(), []string{"1", "1", "1", "1", "1", "1"}) {
		t.Fatalf("calls %d, attempts %v", calls.Load(), r.attempts())
	}
}

func TestStopsAtOnceAndAnOrderlyStopNeverCountsAgainstTheCommand(t *testing.T) {
	r := newInboxRig(t)
	typed := r.message(fixture{body: text("waiting on a reply")})
	r.record(typed, slash)
	for run := 1; run <= 7; run++ {
		entered := make(chan struct{})
		var once sync.Once
		box := r.start(func(ctx context.Context, _ IncomingMessage) (Handled, error) {
			once.Do(func() { close(entered) })
			<-ctx.Done()
			return Handled{}, ctx.Err()
		}, nil, nil)
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d did not start on it", run)
		}
		began := time.Now()
		box.Stop()
		r.inbox = nil
		if time.Since(began) > time.Second {
			t.Fatal("stop must not wait for the handler's own patience")
		}
	}
	if !slices.Equal(r.commands(), []string{typed.WAID + " pending"}) || !slices.Equal(r.attempts(), []string{"0"}) {
		t.Fatalf("got %v %v", r.commands(), r.attempts())
	}
}

func TestGivesUpOnACommandStartedTooManyTimes(t *testing.T) {
	r := newInboxRig(t)
	r.record(r.message(fixture{body: text("crashes core every time")}), slash)
	exec(t, r.admin, "UPDATE commands SET attempts = 2")
	var ran atomic.Bool
	r.start(func(context.Context, IncomingMessage) (Handled, error) { ran.Store(true); return Handled{}, nil },
		func(o *InboxOptions) { o.MaxAttempts = 2 }, nil)
	eventually(t, "the command to be given up on", func() bool {
		c := r.commands()
		return len(c) == 1 && strings.HasSuffix(c[0], "failed interrupted")
	})
	if ran.Load() {
		t.Fatal("a command given up on must not run")
	}
}

func TestLeavesTheCommandPendingWhenTheDatabaseGoesAwayAndFinishesItLater(t *testing.T) {
	r := newInboxRig(t)
	r.record(r.message(fixture{body: text("hello")}), slash)
	var calls atomic.Int32
	r.start(func(context.Context, IncomingMessage) (Handled, error) {
		if calls.Add(1) == 1 {
			return Handled{}, &pgconn.PgError{Code: "08006", Message: "connection failure"}
		}
		return Handled{}, nil
	}, nil, nil)
	eventually(t, "the retry", func() bool { c := r.commands(); return len(c) == 1 && strings.HasSuffix(c[0], "done") })
	if calls.Load() != 2 || !slices.Equal(r.attempts(), []string{"1"}) {
		t.Fatalf("an outage is not the command's fault: calls %d, attempts %v", calls.Load(), r.attempts())
	}
}

func TestDeletesFinishedCommandsAfterAWhileAndKeepsTheUnfinished(t *testing.T) {
	r := newInboxRig(t)
	r.record(r.message(fixture{body: text("done long ago")}), slash)
	r.start(func(context.Context, IncomingMessage) (Handled, error) { return Handled{}, nil },
		func(o *InboxOptions) {
			o.Retention = time.Nanosecond
			o.PurgeAlways = true
			o.Poll = 10 * time.Millisecond
		}, nil)
	eventually(t, "the finished command to be deleted", func() bool { return len(r.commands()) == 0 })
	r.stop()

	kept := r.message(fixture{body: text("still to do")})
	r.record(kept, slash)
	r.start(func(context.Context, IncomingMessage) (Handled, error) { return Handled{}, nil },
		func(o *InboxOptions) {
			o.Retention = time.Nanosecond
			o.PurgeAlways = true
			o.Poll = 10 * time.Millisecond
		},
		func() bool { return false })
	time.Sleep(40 * time.Millisecond)
	if !slices.Equal(r.commands(), []string{kept.WAID + " pending"}) {
		t.Fatalf("got %v", r.commands())
	}
}

func TestTakesTheWorkerLockAgainAfterItsConnectionIsLost(t *testing.T) {
	r := newInboxRig(t)
	seen := make(chan string, 4)
	box := r.start(func(_ context.Context, m IncomingMessage) (Handled, error) { seen <- m.Text; return Handled{}, nil },
		func(o *InboxOptions) { o.Poll = 10 * time.Millisecond }, nil)
	r.record(r.message(fixture{body: text("before")}), slash)
	box.Wake()
	<-seen
	pids := column(t, r.admin, `SELECT pid::text FROM pg_locks
		WHERE locktype = 'advisory' AND objid = $1
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`, inboxLock)
	if len(pids) != 1 {
		t.Fatalf("want one lock holder, got %v", pids)
	}
	exec(t, r.admin, "SELECT pg_terminate_backend($1::int)", pids[0])
	r.record(r.message(fixture{body: text("after")}), slash)
	box.Wake()
	select {
	case got := <-seen:
		if got != "after" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not take the lock again")
	}
	held := column(t, r.admin, `SELECT pid::text FROM pg_locks
		WHERE locktype = 'advisory' AND objid = $1
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`, inboxLock)
	if len(held) != 1 || held[0] == pids[0] {
		t.Fatalf("the worker must hold the lock again on a new connection, holders %v (was %s)", held, pids[0])
	}
}

func TestNeverDropsAnUrgentCommandOnceTheCapIsReached(t *testing.T) {
	r := newInboxRig(t)
	seen := make(chan string, 8)
	box := r.start(func(_ context.Context, m IncomingMessage) (Handled, error) { seen <- m.Text; return Handled{}, nil },
		func(o *InboxOptions) { o.MaxPerMinute = 2; o.Poll = 10 * time.Millisecond; o.IsUrgent = slash }, nil)
	for i := range 2 {
		r.record(r.message(fixture{body: text(fmt.Sprintf("command %d", i))}), slash)
	}
	box.Wake()
	<-seen
	<-seen
	eventually(t, "the cap to be used up", r.settled)
	r.record(r.message(fixture{body: text("plain")}), slash)
	r.record(r.message(fixture{body: text("/pause")}), slash)
	box.Wake()
	select {
	case got := <-seen:
		if got != "/pause" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an urgent command was not run once the cap was reached")
	}
	eventually(t, "the plain one to be dropped", r.settled)
	if got := states(r.commands()); !slices.Equal(got, []string{"done", "done", "failed over_cap", "done"}) {
		t.Fatalf("got %v", got)
	}
}
