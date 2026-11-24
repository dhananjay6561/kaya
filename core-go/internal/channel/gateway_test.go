package channel

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/core/internal/gateway"
	"github.com/dhananjay6561/kaya/kit/store"
)

func TestRequestIDsMatchTheTypeScriptCore(t *testing.T) {
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(RequestIDFor("reply-to-42")) {
		t.Fatalf("not a version 5 style UUID: %s", RequestIDFor("reply-to-42"))
	}
	for key, want := range map[string]string{
		"reply-to-42":          "00d3fe7d-6014-5f70-a92a-032150ef2316",
		"command:3EB0ABC:chat": "1c646cd6-4298-5c4f-8dd5-0a343f8e6cbd",
	} {
		if got := RequestIDFor(key); got != want {
			t.Errorf("%q: got %s, want %s (what the TypeScript core made)", key, got, want)
		}
	}
	if RequestIDFor("reply-to-43") == RequestIDFor("reply-to-42") {
		t.Fatal("different keys must give different ids")
	}
}

type sendRig struct {
	t     *testing.T
	core  *store.DB
	admin *store.DB
	v     *contracts.Validator
}

func newSendRig(t *testing.T) *sendRig {
	t.Helper()
	core, admin := databases(t)
	return &sendRig{t: t, core: core, admin: admin, v: validator(t)}
}

func (r *sendRig) linked() {
	exec(r.t, r.admin, "INSERT INTO connection_state (state, own_jid, occurred_at) VALUES ('connected', $1, now())", ownPN)
}

func (r *sendRig) channel(tune func(*GatewayOptions)) *Gateway {
	r.t.Helper()
	o := GatewayOptions{DB: r.core, Validator: r.v, Poll: 5 * time.Millisecond}
	if tune != nil {
		tune(&o)
	}
	g := NewGateway(o)
	if err := g.Start(bg); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(g.Stop)
	return g
}

type queuedRow struct {
	id      string
	state   string
	payload contracts.SendRequest
}

func (r *sendRig) queued() []queuedRow {
	r.t.Helper()
	ids := column(r.t, r.admin, "SELECT id::text FROM send_requests ORDER BY seq")
	rows := make([]queuedRow, 0, len(ids))
	for _, id := range ids {
		var state string
		var raw []byte
		if err := r.admin.QueryRow(bg, "SELECT state, payload FROM send_requests WHERE id = $1", id).Scan(&state, &raw); err != nil {
			r.t.Fatal(err)
		}
		var payload contracts.SendRequest
		if err := json.Unmarshal(raw, &payload); err != nil {
			r.t.Fatal(err)
		}
		rows = append(rows, queuedRow{id: id, state: state, payload: payload})
	}
	return rows
}

func (r *sendRig) gatewaySends(id, waID string) {
	exec(r.t, r.admin, "UPDATE send_requests SET state = 'sending', wa_id = $2 WHERE id = $1", id, waID)
	exec(r.t, r.admin, "UPDATE send_requests SET state = 'sent', finished_at = now() WHERE id = $1", id)
}

func send(g *Gateway, ctx context.Context, m OutgoingMessage, o SendOptions) <-chan result[SentMessage] {
	return background(func() (SentMessage, error) { return g.Send(ctx, m, o) })
}

func TestQueuesASelfChatRequestAndAnswersOnceSent(t *testing.T) {
	r := newSendRig(t)
	r.linked()
	g := r.channel(nil)
	sending := send(g, bg, OutgoingMessage{IdempotencyKey: "k1", Text: "  hello DJ  ", ReplyTo: "3EB0QUESTION"}, SendOptions{})
	eventually(t, "the queued request", func() bool { return len(r.queued()) == 1 })
	row := r.queued()[0]
	raw, _ := json.Marshal(row.payload)
	if _, err := r.v.DecodeSendRequest(raw); err != nil {
		t.Fatalf("the stored request breaks the contract: %v", err)
	}
	p := row.payload
	if row.id != RequestIDFor("k1") || row.state != "pending" || p.To != ownPN || p.Authorization.Kind != "self_chat" ||
		p.Content.Text != "hello DJ" || p.ReplyTo == nil || *p.ReplyTo != (contracts.ReplyTo{WAID: "3EB0QUESTION", SenderJID: ownPN}) {
		t.Fatalf("got %+v", row)
	}
	r.gatewaySends(row.id, "3EB0ANSWER")
	got := await(t, sending)
	if got.err != nil || got.value.ID != "3EB0ANSWER" || got.value.SentAt.IsZero() {
		t.Fatalf("got %+v", got)
	}
}

func TestSendsARepeatedIdempotencyKeyOnlyOnce(t *testing.T) {
	r := newSendRig(t)
	r.linked()
	g := r.channel(nil)
	first := send(g, bg, OutgoingMessage{IdempotencyKey: "same", Text: "hi"}, SendOptions{})
	second := send(g, bg, OutgoingMessage{IdempotencyKey: "same", Text: "hi"}, SendOptions{})
	eventually(t, "the queued request", func() bool { return len(r.queued()) == 1 })
	r.gatewaySends(RequestIDFor("same"), "3EB0ONCE")
	a, b := await(t, first), await(t, second)
	if a.err != nil || b.err != nil || a.value.ID != "3EB0ONCE" || b.value.ID != "3EB0ONCE" {
		t.Fatalf("got %+v %+v", a, b)
	}
	again, err := g.Send(bg, OutgoingMessage{IdempotencyKey: "same", Text: "hi"}, SendOptions{})
	if err != nil || again.ID != "3EB0ONCE" || len(r.queued()) != 1 {
		t.Fatalf("got %+v %v, %d queued", again, err, len(r.queued()))
	}
}

type results struct {
	mu        sync.Mutex
	listeners map[string]gateway.SendResultListener
}

func (s *results) OnSendResult(id string, l gateway.SendResultListener) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners[id] = l
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.listeners, id)
	}
}

func (s *results) fire(id string) bool {
	s.mu.Lock()
	l, ok := s.listeners[id]
	s.mu.Unlock()
	if ok {
		l(contracts.SendResult{RequestID: id, Status: "sent"})
	}
	return ok
}

func TestWakesAtOnceWhenToldTheResultIsIn(t *testing.T) {
	r := newSendRig(t)
	r.linked()
	told := &results{listeners: map[string]gateway.SendResultListener{}}
	g := r.channel(func(o *GatewayOptions) { o.Poll = time.Minute; o.Results = told })
	sending := send(g, bg, OutgoingMessage{IdempotencyKey: "fast", Text: "hi"}, SendOptions{})
	id := RequestIDFor("fast")
	eventually(t, "the waiter", func() bool { told.mu.Lock(); defer told.mu.Unlock(); return told.listeners[id] != nil })
	r.gatewaySends(id, "3EB0FAST")
	told.fire(id)
	if got := await(t, sending); got.err != nil || got.value.ID != "3EB0FAST" {
		t.Fatalf("got %+v", got)
	}
	told.mu.Lock()
	defer told.mu.Unlock()
	if len(told.listeners) != 0 {
		t.Fatal("the waiter must stop listening once it has its answer")
	}
}

func TestReportsWhyTheGatewayDidNotSend(t *testing.T) {
	r := newSendRig(t)
	r.linked()
	g := r.channel(nil)
	sending := send(g, bg, OutgoingMessage{IdempotencyKey: "refused", Text: "hi"}, SendOptions{})
	id := RequestIDFor("refused")
	eventually(t, "the queued request", func() bool { return len(r.queued()) == 1 })
	exec(t, r.admin, "UPDATE send_requests SET state = 'failed', error = 'outcome_unknown', finished_at = now() WHERE id = $1", id)
	got := await(t, sending)
	var failed *SendFailedError
	if !errors.As(got.err, &failed) || failed.Code != "outcome_unknown" || failed.RequestID != id {
		t.Fatalf("got %+v", got)
	}
}

func TestKeepsWaitingWhileSendingAndSaysUnknownIfItNeverHearsBack(t *testing.T) {
	r := newSendRig(t)
	r.linked()
	day := 24 * time.Hour
	id := RequestIDFor("slow")
	first := send(r.channel(nil), bg, OutgoingMessage{IdempotencyKey: "slow", Text: "hi"}, SendOptions{})
	eventually(t, "the queued request", func() bool { return len(r.queued()) == 1 })
	exec(t, r.admin, "UPDATE send_requests SET state = 'sending', wa_id = '3EB0SLOW' WHERE id = $1", id)

	patient := send(r.channel(func(o *GatewayOptions) { o.Grace = -day; o.SendingGrace = 2 * day }), bg,
		OutgoingMessage{IdempotencyKey: "slow", Text: "hi"}, SendOptions{})
	select {
	case got := <-patient:
		t.Fatalf("a request being sent may still go out, even past its expiry; got %+v", got)
	case <-time.After(60 * time.Millisecond):
	}

	longAfter := r.channel(func(o *GatewayOptions) { o.Grace = -day; o.SendingGrace = time.Nanosecond })
	_, err := longAfter.Send(bg, OutgoingMessage{IdempotencyKey: "slow", Text: "hi"}, SendOptions{})
	var failed *SendFailedError
	if !errors.As(err, &failed) || failed.Code != "outcome_unknown" {
		t.Fatalf("got %v", err)
	}

	exec(t, r.admin, "UPDATE send_requests SET state = 'sent', finished_at = now() WHERE id = $1", id)
	if a, b := await(t, first), await(t, patient); a.value.ID != "3EB0SLOW" || b.value.ID != "3EB0SLOW" {
		t.Fatalf("got %+v %+v", a, b)
	}
}

func TestRefusesAKeyUsedForADifferentMessage(t *testing.T) {
	r := newSendRig(t)
	r.linked()
	g := r.channel(nil)
	first := send(g, bg, OutgoingMessage{IdempotencyKey: "reused", Text: "the first message"}, SendOptions{})
	eventually(t, "the queued request", func() bool { return len(r.queued()) == 1 })
	for _, m := range []OutgoingMessage{
		{IdempotencyKey: "reused", Text: "a different message"},
		{IdempotencyKey: "reused", Text: "the first message", ReplyTo: "3EB0OTHER"},
	} {
		if _, err := g.Send(bg, m, SendOptions{}); !errors.As(err, new(*KeyReusedError)) {
			t.Fatalf("%+v: got %v", m, err)
		}
	}
	r.gatewaySends(RequestIDFor("reused"), "3EB0FIRST")
	if got := await(t, first); got.value.ID != "3EB0FIRST" || len(r.queued()) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestKeepsTheFirstTextWhenItMayChangeButStillChecksTheReply(t *testing.T) {
	r := newSendRig(t)
	r.linked()
	g := r.channel(nil)
	first := send(g, bg, OutgoingMessage{IdempotencyKey: "status", Text: "up 1m"}, SendOptions{})
	eventually(t, "the queued request", func() bool { return len(r.queued()) == 1 })
	retried := send(g, bg, OutgoingMessage{IdempotencyKey: "status", Text: "up 2m"}, SendOptions{TextMayChange: true})
	if _, err := g.Send(bg, OutgoingMessage{IdempotencyKey: "status", Text: "up 2m", ReplyTo: "3EB0OTHER"}, SendOptions{TextMayChange: true}); !errors.As(err, new(*KeyReusedError)) {
		t.Fatalf("got %v", err)
	}
	r.gatewaySends(RequestIDFor("status"), "3EB0STATUS")
	if a, b := await(t, retried), await(t, first); a.value.ID != "3EB0STATUS" || b.value.ID != "3EB0STATUS" {
		t.Fatalf("got %+v %+v", a, b)
	}
	if rows := r.queued(); len(rows) != 1 || rows[0].payload.Content.Text != "up 1m" {
		t.Fatalf("got %+v", rows)
	}
}

func TestFollowsAMessageAlreadyQueued(t *testing.T) {
	r := newSendRig(t)
	r.linked()
	g := r.channel(nil)
	if got, err := g.Follow(bg, "never-sent"); got != nil || err != nil {
		t.Fatalf("got %v %v", got, err)
	}
	first := send(g, bg, OutgoingMessage{IdempotencyKey: "followed", Text: "whatever it said"}, SendOptions{})
	eventually(t, "the queued request", func() bool { return len(r.queued()) == 1 })
	following := background(func() (*SentMessage, error) { return g.Follow(bg, "followed") })
	r.gatewaySends(RequestIDFor("followed"), "3EB0FOLLOWED")
	if got := await(t, following); got.err != nil || got.value == nil || got.value.ID != "3EB0FOLLOWED" {
		t.Fatalf("got %+v", got)
	}
	if got := await(t, first); got.value.ID != "3EB0FOLLOWED" {
		t.Fatalf("got %+v", got)
	}
	if got, _ := g.Follow(bg, "followed"); got == nil || got.ID != "3EB0FOLLOWED" || len(r.queued()) != 1 {
		t.Fatal("following must never queue anything")
	}
}

func TestGivesUpAWhileAfterTheRequestExpired(t *testing.T) {
	r := newSendRig(t)
	r.linked()
	g := r.channel(func(o *GatewayOptions) { o.Grace = -24 * time.Hour })
	_, err := g.Send(bg, OutgoingMessage{IdempotencyKey: "nobody home", Text: "hi"}, SendOptions{})
	var failed *SendFailedError
	if !errors.As(err, &failed) || failed.Code != "no_answer" || r.queued()[0].state != "pending" {
		t.Fatalf("got %v", err)
	}
}

func TestStopsWaitingWhenTheCallerGivesUp(t *testing.T) {
	r := newSendRig(t)
	r.linked()
	g := r.channel(func(o *GatewayOptions) { o.Poll = time.Minute })
	ctx, giveUp := context.WithCancel(bg)
	sending := send(g, ctx, OutgoingMessage{IdempotencyKey: "impatient", Text: "hi"}, SendOptions{})
	eventually(t, "the queued request", func() bool { return len(r.queued()) == 1 })
	giveUp()
	if got := await(t, sending); !errors.Is(got.err, context.Canceled) || r.queued()[0].state != "pending" {
		t.Fatalf("got %+v", got)
	}
}

func TestEndsAWaitAtOnceWhenTheChannelStops(t *testing.T) {
	r := newSendRig(t)
	r.linked()
	g := r.channel(func(o *GatewayOptions) { o.Poll = time.Minute })
	sending := send(g, bg, OutgoingMessage{IdempotencyKey: "interrupted by a stop", Text: "hi"}, SendOptions{})
	eventually(t, "the queued request", func() bool { return len(r.queued()) == 1 })
	began := time.Now()
	g.Stop()
	got := await(t, sending)
	if !errors.Is(got.err, ErrNotStarted) || time.Since(began) > time.Second {
		t.Fatalf("got %+v after %s", got, time.Since(began))
	}
	if r.queued()[0].state != "pending" {
		t.Fatal("the request itself stays queued")
	}
}

func TestLetsOneMessageSetHowLongItStaysWorthSending(t *testing.T) {
	r := newSendRig(t)
	r.linked()
	g := r.channel(nil)
	_ = send(g, bg, OutgoingMessage{IdempotencyKey: "short-lived", Text: "hi"}, SendOptions{TTL: 45 * time.Second})
	eventually(t, "the queued request", func() bool { return len(r.queued()) == 1 })
	p := r.queued()[0].payload
	if got := p.ExpiresAt.Sub(p.CreatedAt.Time); got != 45*time.Second {
		t.Fatalf("got %s", got)
	}
}

func TestRefusesToSendUnlinkedStoppedOrEmpty(t *testing.T) {
	r := newSendRig(t)
	g := r.channel(nil)
	if _, err := g.Send(bg, OutgoingMessage{IdempotencyKey: "k", Text: "hi"}, SendOptions{}); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("got %v", err)
	}
	exec(t, r.admin, "INSERT INTO connection_state (state, reason, occurred_at) VALUES ('logged_out', 'not paired: run make pair', now())")
	if _, err := g.Send(bg, OutgoingMessage{IdempotencyKey: "k", Text: "hi"}, SendOptions{}); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("got %v", err)
	}
	for _, m := range []OutgoingMessage{{IdempotencyKey: "k", Text: "   "}, {IdempotencyKey: " ", Text: "hi"}} {
		if _, err := g.Send(bg, m, SendOptions{}); !errors.Is(err, ErrEmpty) {
			t.Fatalf("%+v: got %v", m, err)
		}
	}
	if len(r.queued()) != 0 {
		t.Fatal("nothing may be queued")
	}
	if err := g.Check(bg); err != nil {
		t.Fatal(err)
	}
	g.Stop()
	if _, err := g.Send(bg, OutgoingMessage{IdempotencyKey: "k", Text: "hi"}, SendOptions{}); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("got %v", err)
	}
	if err := g.Check(bg); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("got %v", err)
	}
}
