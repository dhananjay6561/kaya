package alerts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

var bg = context.Background()

// phone records what was sent to DJ's chat, failing the next sends with errs.
type phone struct {
	mu   sync.Mutex
	sent []channel.OutgoingMessage
	errs []error
}

func (p *phone) Send(_ context.Context, m channel.OutgoingMessage, _ channel.SendOptions) (channel.SentMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.errs) > 0 {
		err := p.errs[0]
		p.errs = p.errs[1:]
		return channel.SentMessage{}, err
	}
	p.sent = append(p.sent, m)
	return channel.SentMessage{ID: fmt.Sprint(len(p.sent))}, nil
}

type rig struct {
	t                 *testing.T
	core, admin       *store.DB
	me, ritu          int64
	ritusChat, office int64
	seq               int
	phone             *phone
	pause             *pause.Memory
	shift             time.Duration
}

func newRig(t *testing.T) *rig {
	t.Helper()
	core, admin := testdb.MigratedPair(t, "kaya_core")
	r := &rig{t: t, core: core, admin: admin, phone: &phone{}, pause: &pause.Memory{}}
	r.me = r.one("SELECT contact_id_for('100000000000001@lid', $1)", "919999999999@s.whatsapp.net")
	r.ritu = r.one("SELECT contact_id_for('100000000000002@lid', $1)", "918888888888@s.whatsapp.net")
	r.exec("UPDATE contacts SET saved_name = 'Ritu' WHERE id = $1", r.ritu)
	r.ritusChat = r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.ritu)
	r.office = r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', $1, 'Office Team') RETURNING id", "120363000000000001@g.us")
	return r
}

func (r *rig) one(sql string, args ...any) int64 {
	r.t.Helper()
	var id int64
	if err := r.admin.QueryRow(bg, sql, args...).Scan(&id); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.admin.Exec(bg, sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

func (r *rig) now() time.Time { return time.Now().Add(r.shift) }

// say stores a message in chat, from DJ when mine, a minute ago.
func (r *rig) say(chat int64, mine bool, body string) int64 {
	r.t.Helper()
	r.seq++
	sender := r.ritu
	if mine {
		sender = r.me
	}
	return r.one(`INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source)
		VALUES ($1, $2, $3, $4, now() - interval '1 minute' + make_interval(secs => $5), 'text', $6, 'live') RETURNING id`,
		chat, fmt.Sprintf("3EB0MSG%d", r.seq), sender, mine, r.seq, body)
}

func (r *rig) alert(chat int64, urgent bool, note string) int64 {
	r.t.Helper()
	id := r.say(chat, false, note)
	r.exec("INSERT INTO alerts (message_id, chat_id, urgent, note) VALUES ($1, $2, $3, $4)", id, chat, urgent, note)
	return id
}

func (r *rig) alerter(o Options) *Alerter {
	r.t.Helper()
	o.DB, o.Channel, o.Pause, o.Logger, o.Zone = r.core, r.phone, r.pause, slog.New(slog.NewTextHandler(io.Discard, nil)), time.UTC
	o.Now = r.now
	if o.PerHour == 0 {
		o.PerHour = 10
	}
	a, err := New(o)
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func (r *rig) step(a *Alerter) {
	r.t.Helper()
	if err := a.Step(bg); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) texts() []string {
	r.phone.mu.Lock()
	defer r.phone.mu.Unlock()
	out := make([]string, len(r.phone.sent))
	for i, m := range r.phone.sent {
		out[i] = m.Text
	}
	return out
}

func (r *rig) skipped() map[string]int {
	r.t.Helper()
	rows, err := r.admin.SQL().QueryContext(bg, "SELECT coalesce(skipped, CASE WHEN sent_at IS NULL THEN 'waiting' ELSE 'sent' END) FROM alerts")
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			r.t.Fatal(err)
		}
		out[s]++
	}
	return out
}

func TestAnUrgentAlertGoesOutAtOnceAndOnlyOnce(t *testing.T) {
	r := newRig(t)
	r.alert(r.ritusChat, true, "Ritu is worried you're unwell and says you should see a doctor")
	a := r.alerter(Options{})
	r.step(a)
	r.step(a)
	got := r.texts()
	if len(got) != 1 || got[0] != "*Heads up*\nFrom Ritu: Ritu is worried you're unwell and says you should see a doctor" {
		t.Fatalf("sent %q", got)
	}
	if s := r.skipped(); s["sent"] != 1 {
		t.Fatalf("alerts %v", s)
	}
}

func TestAlertsThatCanWaitAreGatheredIntoOneMessage(t *testing.T) {
	r := newRig(t)
	r.alert(r.office, false, "Your name came up: Asad asks if you can take the demo")
	r.alert(r.ritusChat, false, "Ritu asks if you're free on Sunday")
	a := r.alerter(Options{})
	r.step(a)
	if got := r.texts(); len(got) != 0 {
		t.Fatalf("sent before gathering: %q", got)
	}
	r.alert(r.ritusChat, true, "Ritu needs the rent money today")
	r.step(a)
	got := r.texts()
	want := "*Heads up*\n• From Ritu: Ritu needs the rent money today\n• From Office Team: Your name came up: Asad asks if you can take the demo\n• From Ritu: Ritu asks if you're free on Sunday"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("sent %q", got)
	}

	r.alert(r.office, false, "Standup moved to 11")
	r.step(a)
	r.shift = 46 * time.Minute
	r.step(a)
	if got := r.texts(); len(got) != 2 || got[1] != "*Heads up*\nFrom Office Team: Standup moved to 11" {
		t.Fatalf("a gathered alert was not sent after waiting: %q", got)
	}
}

func TestAlertsThatNoLongerMatterArePassedOver(t *testing.T) {
	r := newRig(t)
	r.alert(r.ritusChat, false, "Ritu asks about dinner")
	r.say(r.ritusChat, true, "haan 8 baje")
	gone := r.alert(r.office, false, "Deleted ask")
	r.exec("UPDATE messages SET deleted_at = now() WHERE id = $1", gone)
	r.alert(r.office, false, "Old news")
	r.exec("UPDATE alerts SET created_at = now() - interval '7 hours' WHERE note = 'Old news'")
	r.step(r.alerter(Options{}))
	if s := r.skipped(); s["answered"] != 1 || s["deleted"] != 1 || s["stale"] != 1 || len(r.texts()) != 0 {
		t.Fatalf("alerts %v, sent %q", s, r.texts())
	}
}

func TestARemindersAlertGoesOutBeforeItsTime(t *testing.T) {
	r := newRig(t)
	soon := r.say(r.ritusChat, false, "call at 6")
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title, due_at, due_has_time) VALUES ($1, $2, 'reminder', 'Call with Shaurya', $3, true)`,
		soon, r.ritusChat, time.Now().Add(20*time.Minute))
	later := r.say(r.ritusChat, false, "dinner at 9")
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title, due_at, due_has_time) VALUES ($1, $2, 'reminder', 'Dinner with Ritu', $3, true)`,
		later, r.ritusChat, time.Now().Add(3*time.Hour))
	a := r.alerter(Options{})
	r.step(a)
	r.step(a)
	got := r.texts()
	if len(got) != 1 || !strings.HasPrefix(got[0], "*Heads up*\nReminder: In 20 min, at ") || !strings.HasSuffix(got[0], ": Call with Shaurya") {
		t.Fatalf("sent %q", got)
	}

	closed := r.say(r.ritusChat, false, "gym at 7")
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title, due_at, due_has_time) VALUES ($1, $2, 'reminder', 'Gym', $3, true)`,
		closed, r.ritusChat, time.Now().Add(10*time.Minute))
	_, _ = r.pause.Pause(bg, "test")
	r.step(a)
	_, _ = r.pause.Resume(bg, "test")
	r.exec("UPDATE tasks SET state = 'done', done_at = now() WHERE title = 'Gym'")
	r.exec("INSERT INTO alerts (task_id, due_at, chat_id, urgent, note) SELECT id, due_at, chat_id, true, 'Gym soon' FROM tasks WHERE title = 'Gym'")
	r.step(a)
	if s := r.skipped(); s["done"] != 1 || len(r.texts()) != 1 {
		t.Fatalf("alerts %v, sent %q", s, r.texts())
	}
}

func TestAlertsKeepToTheirHourlyCap(t *testing.T) {
	r := newRig(t)
	a := r.alerter(Options{PerHour: 1})
	r.alert(r.ritusChat, true, "first")
	r.step(a)
	r.alert(r.ritusChat, true, "second")
	r.step(a)
	if got := r.texts(); len(got) != 1 {
		t.Fatalf("sent %q", got)
	}
	r.shift = 61 * time.Minute
	r.step(a)
	if got := r.texts(); len(got) != 2 || !strings.HasSuffix(got[1], "second") {
		t.Fatalf("sent %q", got)
	}
}

func TestNothingGoesOutWhilePausedOrSwitchedOff(t *testing.T) {
	r := newRig(t)
	a := r.alerter(Options{})
	r.alert(r.ritusChat, true, "while paused")
	_, _ = r.pause.Pause(bg, "test")
	r.step(a)
	if len(r.texts()) != 0 {
		t.Fatal("an alert went out while paused")
	}
	_, _ = r.pause.Resume(bg, "test")
	sw := NewSwitch(r.core)
	if changed, err := sw.Set(bg, false); !changed || err != nil {
		t.Fatalf("off: %v, %v", changed, err)
	}
	r.alert(r.ritusChat, true, "while off")
	r.step(a)
	if changed, _ := sw.Set(bg, false); changed {
		t.Fatal("switching off twice changed something")
	}
	if changed, err := sw.Set(bg, true); !changed || err != nil {
		t.Fatalf("on: %v, %v", changed, err)
	}
	r.step(a)
	if got := r.texts(); len(got) != 1 || !strings.HasSuffix(got[0], "while off") {
		t.Fatalf("sent %q", got)
	}
	if s := r.skipped(); s["off"] != 1 {
		t.Fatalf("alerts %v", s)
	}
	if n, err := sw.Waiting(bg); n != 0 || err != nil {
		t.Fatalf("waiting %d, %v", n, err)
	}
}

func TestASendThatWasCutOffGoesOutUnderTheSameKey(t *testing.T) {
	r := newRig(t)
	a := r.alerter(Options{})
	r.alert(r.ritusChat, true, "first try")
	r.phone.errs = []error{errors.New("gateway away")}
	if err := a.Step(bg); err == nil {
		t.Fatal("a failed send was not reported")
	}
	r.alert(r.ritusChat, true, "arrived meanwhile")
	r.step(a)
	r.step(a)
	r.phone.mu.Lock()
	sent := append([]channel.OutgoingMessage(nil), r.phone.sent...)
	r.phone.mu.Unlock()
	if len(sent) != 2 || !strings.HasSuffix(sent[0].Text, "first try") || !strings.HasSuffix(sent[1].Text, "arrived meanwhile") || sent[0].IdempotencyKey == sent[1].IdempotencyKey {
		t.Fatalf("sent %+v", sent)
	}

	r.alert(r.ritusChat, true, "never sent")
	r.phone.errs = []error{&channel.SendFailedError{Code: "expired"}}
	r.step(a)
	if s := r.skipped(); s["send_expired"] != 1 {
		t.Fatalf("alerts %v", s)
	}
}

func TestARefusedSendNeverHoldsUpLaterAlerts(t *testing.T) {
	r := newRig(t)
	a := r.alerter(Options{})
	r.alert(r.ritusChat, true, "refused")
	r.phone.errs = []error{&channel.SendFailedError{Code: "rejected"}}
	r.step(a)
	r.alert(r.ritusChat, true, "after it")
	r.step(a)
	if got := r.texts(); len(got) != 1 || !strings.HasSuffix(got[0], "after it") {
		t.Fatalf("sent %q", got)
	}
	if s := r.skipped(); s["send_rejected"] != 1 {
		t.Fatalf("alerts %v", s)
	}
}

func TestAMovedReminderGetsANewAlertAtItsNewTime(t *testing.T) {
	r := newRig(t)
	id := r.say(r.ritusChat, false, "call at 6")
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title, due_at, due_has_time) VALUES ($1, $2, 'reminder', 'Call with Shaurya', $3, true)`,
		id, r.ritusChat, time.Now().Add(20*time.Minute))
	a := r.alerter(Options{})
	_, _ = r.pause.Pause(bg, "test")
	r.step(a)
	_, _ = r.pause.Resume(bg, "test")
	r.exec("INSERT INTO alerts (task_id, due_at, chat_id, urgent, note) SELECT id, due_at, chat_id, true, 'old time' FROM tasks")
	r.exec("UPDATE tasks SET due_at = due_at + interval '2 hours'")
	r.step(a)
	if s := r.skipped(); s["moved"] != 1 || len(r.texts()) != 0 {
		t.Fatalf("alerts %v, sent %q", s, r.texts())
	}
	r.shift = 2 * time.Hour
	r.step(a)
	if got := r.texts(); len(got) != 1 || !strings.HasSuffix(got[0], "Call with Shaurya") || strings.Contains(got[0], "old time") {
		t.Fatalf("sent %q", got)
	}
}

func TestRemindersGoOutPastTheHourlyCap(t *testing.T) {
	r := newRig(t)
	a := r.alerter(Options{PerHour: 1})
	r.alert(r.ritusChat, true, "uses the hour")
	r.step(a)
	r.alert(r.ritusChat, true, "waits")
	id := r.say(r.ritusChat, false, "call at 6")
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title, due_at, due_has_time) VALUES ($1, $2, 'reminder', 'Call with Shaurya', $3, true)`,
		id, r.ritusChat, time.Now().Add(10*time.Minute))
	r.step(a)
	if got := r.texts(); len(got) != 2 || !strings.HasSuffix(got[1], "Call with Shaurya") || strings.Contains(got[1], "waits") {
		t.Fatalf("sent %q", got)
	}
}

func TestOldNewsIsNeverSent(t *testing.T) {
	r := newRig(t)
	a := r.alerter(Options{})
	r.alert(r.ritusChat, true, "stuck in a batch")
	r.exec("UPDATE alerts SET batch = 'alerts-old', created_at = now() - interval '7 hours'")
	old := r.say(r.ritusChat, false, "from yesterday")
	r.exec("UPDATE messages SET sent_at = now() - interval '20 hours' WHERE id = $1", old)
	r.exec("INSERT INTO alerts (message_id, chat_id, urgent, note) VALUES ($1, $2, true, 'yesterday''s news')", old, r.ritusChat)
	r.step(a)
	if s := r.skipped(); s["stale"] != 2 || len(r.texts()) != 0 {
		t.Fatalf("alerts %v, sent %q", s, r.texts())
	}

	r.alert(r.ritusChat, true, "keyed when switched off")
	r.exec("UPDATE alerts SET batch = 'alerts-new' WHERE note = 'keyed when switched off'")
	sw := NewSwitch(r.core)
	_, _ = sw.Set(bg, false)
	_, _ = sw.Set(bg, true)
	r.step(a)
	if len(r.texts()) != 0 {
		t.Fatalf("a batch from before /alerts off went out: %q", r.texts())
	}
}

func TestInAGroupOnlyAReplyToItCountsAsAnswered(t *testing.T) {
	r := newRig(t)
	asked := r.alert(r.office, false, "Asad asks if you can take the demo")
	r.exec("UPDATE messages SET sent_at = now() - interval '1 hour' WHERE id = $1", asked)
	other := r.say(r.office, true, "lunch?")
	r.exec("UPDATE messages SET sent_at = now() - interval '30 minutes' WHERE id = $1", other)
	a := r.alerter(Options{})
	r.step(a)
	if s := r.skipped(); s["answered"] != 0 {
		t.Fatalf("an unrelated message in the group answered it: %v", s)
	}
	reply := r.say(r.office, true, "haan le lunga")
	r.exec(`UPDATE messages SET reply_to_wa_id = (SELECT wa_id FROM messages WHERE id = $2),
		reply_to_sender_id = (SELECT sender_id FROM messages WHERE id = $2) WHERE id = $1`, reply, asked)
	r.exec("UPDATE alerts SET batch = NULL WHERE sent_at IS NULL")
	r.step(a)
	if s := r.skipped(); s["answered"] != 1 {
		t.Fatalf("a reply did not answer it: %v, sent %q", s, r.texts())
	}
}
