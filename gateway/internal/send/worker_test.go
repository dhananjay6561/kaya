package send

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/events"
	"github.com/dhananjay6561/kaya/gateway/internal/ingest"
	"github.com/dhananjay6561/kaya/gateway/internal/store"
	"github.com/dhananjay6561/kaya/gateway/internal/testdb"
)

const (
	ownPN  = "919999999999@s.whatsapp.net"
	ownLID = "100000000000001@lid"
)

var own = ingest.Own{PN: ownPN, LID: ownLID}

type sent struct {
	to   string
	id   string
	text string
	msg  *waE2E.Message
}

type fakeClient struct {
	mu     sync.Mutex
	errs   []error
	sends  []sent
	during func()
}

func (f *fakeClient) GenerateMessageID() types.MessageID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf("3EB0TEST%04d", len(f.sends)+1)
}

func (f *fakeClient) SendMessage(_ context.Context, to types.JID, m *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.mu.Lock()
	text := m.GetConversation()
	if text == "" {
		text = m.GetExtendedTextMessage().GetText()
	}
	f.sends = append(f.sends, sent{to: to.String(), id: extra[0].ID, text: text, msg: m})
	var err error
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	during := f.during
	f.mu.Unlock()
	if during != nil {
		during()
	}
	if err != nil {
		return whatsmeow.SendResponse{}, err
	}
	return whatsmeow.SendResponse{ID: extra[0].ID, Timestamp: time.Now()}, nil
}

func (f *fakeClient) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sends {
		out = append(out, s.text)
	}
	return out
}

type flakyDB struct {
	DB
	mu   sync.Mutex
	down bool
}

func (f *flakyDB) set(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *flakyDB) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	f.mu.Lock()
	down := f.down
	f.mu.Unlock()
	if down {
		return errors.New("connection refused")
	}
	return f.DB.InTx(ctx, fn)
}

type harness struct {
	t      *testing.T
	admin  *store.DB
	db     *flakyDB
	worker *Worker
	cli    *fakeClient
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	gateway, admin := testdb.MigratedPair(t, "kaya_gateway")
	h := &harness{t: t, admin: admin, db: &flakyDB{DB: gateway}, cli: &fakeClient{}}
	h.worker = h.newWorker()
	return h
}

func (h *harness) newWorker() *Worker {
	h.t.Helper()
	v, err := contracts.NewValidator()
	if err != nil {
		h.t.Fatal(err)
	}
	outbox := events.NewOutbox(v)
	w := NewWorker(h.db, v, outbox, ingest.NewStore(outbox), slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.recordRetry, w.recordLast = time.Millisecond, 50*time.Millisecond
	return w
}

type option func(map[string]any)

func to(jid string) option { return func(r map[string]any) { r["to"] = jid } }

func confirmed() option {
	return func(r map[string]any) {
		r["authorization"] = map[string]any{"kind": "confirmed", "action_id": contracts.NewID()}
	}
}

func expiredAnHourAgo() option {
	return func(r map[string]any) {
		r["created_at"] = contracts.At(time.Now().Add(-2 * time.Hour))
		r["expires_at"] = contracts.At(time.Now().Add(-time.Hour))
	}
}

func replyTo(waID, sender string) option {
	return func(r map[string]any) { r["reply_to"] = map[string]any{"wa_id": waID, "sender_jid": sender} }
}

func without(field string) option { return func(r map[string]any) { delete(r, field) } }

func (h *harness) queue(text string, opts ...option) string {
	h.t.Helper()
	id := contracts.NewID()
	req := map[string]any{
		"id": id, "version": 1,
		"created_at":    contracts.At(time.Now()),
		"expires_at":    contracts.At(time.Now().Add(time.Hour)),
		"to":            ownPN,
		"authorization": map[string]any{"kind": "self_chat"},
		"content":       map[string]any{"type": "text", "text": text},
	}
	for _, opt := range opts {
		opt(req)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		h.t.Fatal(err)
	}
	expires := req["expires_at"].(contracts.Time).Truncate(time.Microsecond)
	err = h.admin.InTx(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "INSERT INTO send_requests (id, payload, expires_at) VALUES ($1, $2, $3)", id, raw, expires)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) row(id string) (state, waID, code string) {
	h.t.Helper()
	err := h.admin.Query(context.Background(),
		"SELECT state, coalesce(wa_id, ''), coalesce(error, '') FROM send_requests WHERE id = $1", []any{id}, &state, &waID, &code)
	if err != nil {
		h.t.Fatal(err)
	}
	return state, waID, code
}

func (h *harness) results() []string {
	h.t.Helper()
	var out []string
	err := h.admin.InTx(context.Background(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `
			SELECT type || ' ' || coalesce(payload -> 'data' ->> 'status', payload -> 'data' ->> 'change')
				|| coalesce(' ' || (payload -> 'data' -> 'error' ->> 'code'), '')
			FROM gateway_events ORDER BY seq`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *harness) drain(cli Client) int {
	h.t.Helper()
	steps := 0
	for h.worker.Step(context.Background(), cli, own) {
		if steps++; steps > 50 {
			h.t.Fatal("the worker never ran out of work")
		}
	}
	return steps
}

func TestARequestIsSentOnceStoredAndReported(t *testing.T) {
	h := newHarness(t)
	first, second := h.queue("first"), h.queue("second")
	if steps := h.drain(h.cli); steps != 2 {
		t.Fatalf("want one step per request, got %d", steps)
	}

	if got := h.cli.texts(); !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("requests must go out in the order they were made, got %q", got)
	}
	for i, id := range []string{first, second} {
		state, waID, code := h.row(id)
		if state != StateSent || waID != h.cli.sends[i].id || code != "" {
			t.Fatalf("request %d: %s %q %q", i, state, waID, code)
		}
		if h.cli.sends[i].to != ownPN {
			t.Fatalf("sent to %q", h.cli.sends[i].to)
		}
	}
	want := []string{"send.result sent", "message.stored new", "send.result sent", "message.stored new"}
	if got := h.results(); !slices.Equal(got, want) {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}

	var stored int
	var body string
	var fromMe bool
	err := h.admin.Query(context.Background(), `
		SELECT count(*) OVER (), m.body, m.from_me FROM messages m JOIN chats c ON c.id = m.chat_id
		WHERE c.kind = 'direct' AND m.sender_id = c.contact_id AND m.wa_id = $1`, []any{h.cli.sends[0].id}, &stored, &body, &fromMe)
	if err != nil || stored != 1 || body != "first" || !fromMe {
		t.Fatalf("the sent message must be stored in DJ's own chat: %d %q %v %v", stored, body, fromMe, err)
	}

	h.drain(h.cli)
	if len(h.cli.texts()) != 2 {
		t.Fatal("a finished request is never sent again")
	}
}

func TestOnlyDJsOwnChatMayBeWrittenTo(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		id   string
		code string
	}{
		{h.queue("to a friend", to("918888888888@s.whatsapp.net")), CodeNotAuthorized},
		{h.queue("to a group", to("120363000000000000@g.us")), CodeNotAuthorized},
		{h.queue("confirmed, to a friend", to("918888888888@s.whatsapp.net"), confirmed()), CodeNotAuthorized},
		{h.queue("confirmed, to self", confirmed()), CodeNotAuthorized},
		{h.queue("no content", without("content")), CodeInvalid},
		{h.queue("too late", expiredAnHourAgo()), CodeExpired},
	}
	ok := h.queue("to the other spelling of DJ", to(ownLID))
	h.drain(h.cli)

	for _, tc := range cases {
		if state, waID, code := h.row(tc.id); state != StateFailed || code != tc.code || waID != "" {
			t.Fatalf("want failed %s, got %s %q %q", tc.code, state, code, waID)
		}
	}
	if state, _, _ := h.row(ok); state != StateSent {
		t.Fatalf("DJ's own LID is DJ's own chat, got %s", state)
	}
	if got := h.cli.texts(); !slices.Equal(got, []string{"to the other spelling of DJ"}) {
		t.Fatalf("a refused request must never reach WhatsApp, got %q", got)
	}
	want := []string{
		"send.result failed not_authorized", "send.result failed not_authorized", "send.result failed not_authorized",
		"send.result failed not_authorized", "send.result failed invalid_request", "send.result failed expired",
		"send.result sent", "message.stored new",
	}
	if got := h.results(); !slices.Equal(got, want) {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}
}

func TestNothingIsSentWithoutAKnownAccount(t *testing.T) {
	h := newHarness(t)
	id := h.queue("hello")
	for h.worker.Step(context.Background(), h.cli, ingest.Own{}) {
	}
	if state, _, code := h.row(id); state != StateFailed || code != CodeNotAuthorized || len(h.cli.texts()) != 0 {
		t.Fatalf("got %s %s", state, code)
	}
}

func TestWhileDisconnectedRequestsWaitAndExpiredOnesAreAnswered(t *testing.T) {
	h := newHarness(t)
	waiting := h.queue("still good")
	late := h.queue("too late", expiredAnHourAgo())

	if steps := h.drain(nil); steps != 1 {
		t.Fatalf("want only the expired request handled, got %d steps", steps)
	}
	if state, _, _ := h.row(waiting); state != StatePending {
		t.Fatalf("a request that can still be sent must wait for the connection, got %s", state)
	}
	if state, _, code := h.row(late); state != StateFailed || code != CodeExpired {
		t.Fatalf("got %s %s", state, code)
	}

	h.drain(h.cli)
	if state, _, _ := h.row(waiting); state != StateSent {
		t.Fatalf("got %s", state)
	}
}

func TestAFailedSendIsReportedAndNeverRepeated(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code string
	}{
		"refused by whatsapp": {fmt.Errorf("%w 479", whatsmeow.ErrServerReturnedError), CodeRejected},
		"no answer":           {whatsmeow.ErrMessageTimedOut, CodeUnknown},
		"dropped mid-send":    {whatsmeow.ErrNotConnected, CodeUnknown},
		"anything else":       {errors.New("failed to encrypt"), CodeUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.cli.errs = []error{tc.err}
			id := h.queue("hello")
			h.drain(h.cli)
			h.drain(h.cli)

			state, waID, code := h.row(id)
			if state != StateFailed || code != tc.code || waID == "" {
				t.Fatalf("got %s %q %q", state, code, waID)
			}
			if len(h.cli.texts()) != 1 {
				t.Fatalf("a send that may have gone out must never be tried again, got %d tries", len(h.cli.texts()))
			}
			if got := h.results(); !slices.Equal(got, []string{"send.result failed " + tc.code}) {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestARequestLeftMidSendIsReportedUnknownAndNotSentAgain(t *testing.T) {
	h := newHarness(t)
	id := h.queue("hello")
	ctx, cancel := context.WithCancel(context.Background())
	h.cli.during = func() {
		h.db.set(true)
		cancel()
	}
	if h.worker.Step(ctx, h.cli, own) {
		t.Fatal("a gateway that is stopping must not go on to the next request")
	}

	if state, waID, _ := h.row(id); state != StateSending || waID == "" {
		t.Fatalf("the WhatsApp id must be stored before the network call, got %s %q", state, waID)
	}

	h.db.set(false)
	h.cli.during = nil
	h.worker = h.newWorker()
	h.drain(h.cli)
	if state, _, code := h.row(id); state != StateFailed || code != CodeUnknown {
		t.Fatalf("got %s %s", state, code)
	}
	if len(h.cli.texts()) != 1 {
		t.Fatalf("sent %d times", len(h.cli.texts()))
	}
	if got := h.results(); !slices.Equal(got, []string{"send.result failed outcome_unknown"}) {
		t.Fatalf("got %q", got)
	}
}

func TestAnOutcomeIsRecordedOnceTheDatabaseIsBack(t *testing.T) {
	h := newHarness(t)
	id := h.queue("hello")
	h.cli.during = func() {
		h.db.set(true)
		time.AfterFunc(20*time.Millisecond, func() { h.db.set(false) })
	}
	h.drain(h.cli)

	if state, _, _ := h.row(id); state != StateSent {
		t.Fatalf("got %s", state)
	}
	if len(h.cli.texts()) != 1 {
		t.Fatalf("sent %d times", len(h.cli.texts()))
	}
	if got := h.results(); !slices.Equal(got, []string{"send.result sent", "message.stored new"}) {
		t.Fatalf("got %q", got)
	}
}

func TestAReplyQuotesTheStoredMessage(t *testing.T) {
	h := newHarness(t)
	h.queue("the question")
	h.drain(h.cli)
	asked := h.cli.sends[0].id

	h.queue("the answer", replyTo(asked, ownPN))
	h.queue("to a message nobody stored", replyTo("3EB0MISSING", ownPN))
	h.drain(h.cli)

	reply := h.cli.sends[1].msg.GetExtendedTextMessage()
	info := reply.GetContextInfo()
	if reply.GetText() != "the answer" || info.GetStanzaID() != asked || info.GetParticipant() != ownPN || info.GetQuotedMessage().GetConversation() != "the question" {
		t.Fatalf("got %v", reply)
	}
	if got := h.cli.sends[2].msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID(); got != "3EB0MISSING" {
		t.Fatalf("a reply to an unknown message still goes out as a reply, got %q", got)
	}
	var replyTo string
	if err := h.admin.Query(context.Background(), "SELECT reply_to_wa_id FROM messages WHERE wa_id = $1", []any{h.cli.sends[1].id}, &replyTo); err != nil || replyTo != asked {
		t.Fatalf("the stored copy must keep the reply link, got %q %v", replyTo, err)
	}
}

type refusingMessages struct{}

func (refusingMessages) Apply(context.Context, pgx.Tx, ingest.Record) (ingest.Applied, error) {
	return ingest.Applied{}, errors.New("violates check constraint")
}

func TestAMessageThatCannotBeStoredIsStillReportedSent(t *testing.T) {
	h := newHarness(t)
	h.worker.messages = refusingMessages{}
	id := h.queue("hello")
	h.drain(h.cli)
	if state, _, _ := h.row(id); state != StateSent {
		t.Fatalf("got %s", state)
	}
	if got := h.results(); !slices.Equal(got, []string{"send.result sent"}) {
		t.Fatalf("got %q", got)
	}
}

func TestEveryFailureCodeHasAMessageThatFitsTheContract(t *testing.T) {
	v, _ := contracts.NewValidator()
	for _, code := range []string{CodeInvalid, CodeExpired, CodeNotAuthorized, CodeNotOnWhatsApp, CodeRejected, CodeUnknown} {
		event, err := contracts.NewEvent(contracts.EventSendResult, contracts.SendResult{
			RequestID: contracts.NewID(), Status: contracts.SendFailed, Error: &contracts.Error{Code: code, Message: messages[code]},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.EncodeEvent(event); err != nil || messages[code] == "" {
			t.Fatalf("%s: %v", code, err)
		}
	}
}
