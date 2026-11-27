package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/actions"
	"github.com/dhananjay6561/kaya/core/internal/agent"
	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

type finder map[string]int64

func (f finder) FindChat(_ context.Context, name string) (int64, string, bool, error) {
	id, ok := f[strings.ToLower(name)]
	return id, name, ok, nil
}

type outbox struct {
	sent []channel.OutgoingMessage
	err  error
}

func (o *outbox) Send(_ context.Context, m channel.OutgoingMessage, _ channel.SendOptions) (channel.SentMessage, error) {
	if o.err != nil {
		return channel.SentMessage{}, o.err
	}
	o.sent = append(o.sent, m)
	return channel.SentMessage{ID: "3EB0SENT"}, nil
}

func TestSendMessageDraftsForAContactOrAGroupAndNeverSends(t *testing.T) {
	core, admin := testdb.MigratedPair(t, "kaya_core")
	var rahul, rahulsChat, office int64
	if err := admin.QueryRow(bg, "SELECT contact_id_for('100000000000002@lid', '918888888888@s.whatsapp.net')").Scan(&rahul); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(bg, "INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", rahul).Scan(&rahulsChat); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(bg, "INSERT INTO chats (kind, jid, name) VALUES ('group', '120363000000000001@g.us', 'Office Team') RETURNING id").Scan(&office); err != nil {
		t.Fatal(err)
	}
	tool := SendMessage(finder{"rahul": rahulsChat, "office team": office}, core)
	if _, err := agent.NewRegistry(tool); err != nil {
		t.Fatal(err)
	}
	p, err := tool.Propose(bg, json.RawMessage(`{"to":"Rahul","text":"  running late, 10 min  "}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != SendKind || p.TargetJID != "918888888888@s.whatsapp.net" || p.Summary != "Send to Rahul (contact, +918888888888):\nrunning late, 10 min" ||
		string(p.Payload) != `{"chat_id":`+itoa(rahulsChat)+`,"title":"Rahul","text":"running late, 10 min"}` {
		t.Fatalf("proposal %+v %s", p, p.Payload)
	}
	if p, err := tool.Propose(bg, json.RawMessage(`{"to":"Office Team","text":"standup at 11"}`)); err != nil || p.TargetJID != "120363000000000001@g.us" ||
		!strings.HasPrefix(p.Summary, "Send to Office Team (group):") {
		t.Fatalf("group %+v, %v", p, err)
	}
	var problem *agent.Problem
	for args, want := range map[string]string{
		`{"to":"Nobody","text":"hi"}`: `no contact or group in their chats matches "Nobody"`,
		`{"to":"Rahul","text":"   "}`: "the message is empty",
	} {
		if _, err := tool.Propose(bg, json.RawMessage(args)); !errors.As(err, &problem) || !strings.HasPrefix(problem.Message, want) {
			t.Errorf("%s: err = %v", args, err)
		}
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestSendExecutorSendsTheConfirmedMessageWithinItsLimit(t *testing.T) {
	core, admin := testdb.MigratedPair(t, "kaya_core")
	store := actions.NewStore(core, nil, 10*time.Minute)
	payload := json.RawMessage(`{"chat_id":1,"title":"Rahul","text":"running late"}`)
	a, err := store.Propose(bg, actions.Proposal{Kind: SendKind, TargetJID: "918888888888@s.whatsapp.net", Summary: "Send to Rahul", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	box := &outbox{}
	run := SendExecutor(box, store, 2, time.Now)
	result, err := run(bg, a)
	if err != nil || result != "Sent to Rahul." || len(box.sent) != 1 {
		t.Fatalf("result %q, %v, sent %+v", result, err, box.sent)
	}
	m := box.sent[0]
	if m.To != "918888888888@s.whatsapp.net" || m.ActionID != a.ID || m.IdempotencyKey != "action:"+a.ID || m.Text != "running late" {
		t.Fatalf("sent %+v", m)
	}
	box.err = &channel.SendFailedError{Code: "recipient_not_on_whatsapp"}
	if result, err := run(bg, a); err == nil || result != "they are not on WhatsApp" {
		t.Fatalf("result %q, %v", result, err)
	}
	box.err = nil
	if _, err := admin.Exec(bg, "CREATE SEQUENCE pending_seq"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := admin.Exec(bg, `INSERT INTO pending_actions (kind, target_jid, summary, payload, state, expires_at, shown_at, shown_wa_id, decided_at, decided_by)
			VALUES ('send_message', '918888888888@s.whatsapp.net', 'x', '{}', 'done', now() + interval '5 minutes', now(), 'w', now(), nextval('pending_seq'))`); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := run(bg, a); err == nil || !strings.Contains(result, "the most I send") {
		t.Fatalf("over the limit: %q, %v", result, err)
	}
}
