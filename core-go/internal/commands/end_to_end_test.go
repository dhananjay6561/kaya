package commands

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/core/internal/gateway"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

const ownPN = "919999999999@s.whatsapp.net"

type e2e struct {
	t           *testing.T
	core, admin *store.DB
	v           *contracts.Validator
	own, self   int64
}

func (e *e2e) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.admin.Exec(bg, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *e2e) one(sql string, args ...any) int64 {
	e.t.Helper()
	var id int64
	if err := e.admin.QueryRow(bg, sql, args...).Scan(&id); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (e *e2e) emit(typ contracts.EventType, data any) {
	e.t.Helper()
	event, err := contracts.NewEvent(typ, data)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec("INSERT INTO gateway_events (id, type, payload) VALUES ($1, $2, $3)", event.ID, string(typ), string(raw))
}

func (e *e2e) stored(waID, body string) contracts.MessageStored {
	e.t.Helper()
	id := e.one(`INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source)
	             VALUES ($1, $2, $3, true, now(), 'text', $4, 'live') RETURNING id`, e.self, waID, e.own, body)
	data := contracts.MessageStored{
		MessageID: id, ChatID: e.self, SenderID: e.own, WAID: waID, FromMe: true,
		Kind: "text", Source: "live", Change: "new", SentAt: contracts.At(time.Now()),
	}
	e.emit(contracts.EventMessageStored, data)
	return data
}

type request struct {
	id, state string
	payload   contracts.SendRequest
}

func (e *e2e) requests() []request {
	e.t.Helper()
	var out []request
	err := e.admin.InTx(bg, func(tx pgx.Tx) error {
		rows, err := tx.Query(bg, "SELECT id::text, state, payload::text FROM send_requests ORDER BY seq")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r request
			var raw string
			if err := rows.Scan(&r.id, &r.state, &raw); err != nil {
				return err
			}
			if r.payload, err = e.v.DecodeSendRequest([]byte(raw)); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *e2e) count(sql string) int64 {
	e.t.Helper()
	return e.one("SELECT count(*) FROM (" + sql + ") AS rows")
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestACommandEndToEndIsAnsweredOnceAndKayaNeverAnswersItself(t *testing.T) {
	core, admin := testdb.MigratedPair(t, "kaya_core", store.WithAppName("kaya-core"))
	v, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	e := &e2e{t: t, core: core, admin: admin, v: v}
	e.own = e.one("SELECT contact_id_for('100000000000001@lid', $1)", ownPN)
	e.self = e.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", e.own)

	events := gateway.NewEvents(quiet)
	consumer := gateway.NewConsumer(gateway.ConsumerOptions{
		DB: core, Validator: v, Logger: quiet, Handle: events.Handle, RetryMin: 5 * time.Millisecond, PollInterval: 50 * time.Millisecond,
	})
	ch := channel.NewGateway(channel.GatewayOptions{
		DB: core, Validator: v, Results: events, Incoming: events, Logger: quiet, Poll: 20 * time.Millisecond,
		Inbox: channel.InboxOptions{IsUrgent: IsBuiltin, Poll: 20 * time.Millisecond},
	})
	Start(Options{
		Channel: ch, Logger: quiet, Pause: pause.NewStored(core),
		Status: func(context.Context) (string, error) { return "ok", nil },
		Chat: func(_ context.Context, m channel.IncomingMessage) (ChatAnswer, error) {
			return ChatAnswer{Text: "You said: " + m.Text, Remember: true}, nil
		},
	})
	ctx, cancel := context.WithCancel(bg)
	if err := ch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	consumer.Start(ctx)
	t.Cleanup(func() {
		cancel()
		consumer.Stop()
		ch.Stop()
	})

	e.emit(contracts.EventConnectionState, contracts.ConnectionState{State: "connected", OwnJID: ownPN})
	typed := e.stored("3EB0TYPEDBYDJ", "hello kaya")

	var reqs []request
	eventually(t, "the reply to be queued", func() bool { reqs = e.requests(); return len(reqs) > 0 })
	p := reqs[0].payload
	if p.To != ownPN || p.Authorization.Kind != contracts.AuthSelfChat {
		t.Fatalf("got %+v", p)
	}
	if p.Content != (contracts.TextContent{Type: "text", Text: "You said: hello kaya"}) {
		t.Fatalf("got %+v", p.Content)
	}
	if p.ReplyTo == nil || *p.ReplyTo != (contracts.ReplyTo{WAID: "3EB0TYPEDBYDJ", SenderJID: ownPN}) {
		t.Fatalf("got %+v", p.ReplyTo)
	}
	if life := p.ExpiresAt.Sub(p.CreatedAt.Time); life > 10*time.Minute {
		t.Fatalf("the reply lives %v", life)
	}

	e.exec("UPDATE send_requests SET state = 'sending', wa_id = '3EB0KAYAREPLY' WHERE id = $1", reqs[0].id)
	e.exec("UPDATE send_requests SET state = 'sent', finished_at = now() WHERE id = $1", reqs[0].id)
	e.stored("3EB0KAYAREPLY", "You said: hello kaya")
	e.emit(contracts.EventSendResult, contracts.SendResult{RequestID: reqs[0].id, Status: "sent", WAID: "3EB0KAYAREPLY"})
	e.emit(contracts.EventMessageStored, typed)
	eventually(t, "every event and command to be handled", func() bool {
		return e.count("SELECT 1 FROM gateway_events WHERE state = 'pending' UNION ALL SELECT 1 FROM commands WHERE state = 'pending'") == 0
	})

	if n := len(e.requests()); n != 1 {
		t.Fatalf("the reply, a repeat of the event and Kaya's own message add nothing; %d requests", n)
	}
	if n := e.count("SELECT 1 FROM commands WHERE wa_id = '3EB0TYPEDBYDJ' AND state = 'done'"); n != 1 || e.count("SELECT 1 FROM commands") != 1 {
		t.Fatal("want exactly DJ's command, done")
	}
	if n := e.count("SELECT 1 FROM gateway_events WHERE state = 'dead'"); n != 0 {
		t.Fatalf("%d dead events", n)
	}
}
