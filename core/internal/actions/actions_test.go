package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

var bg = context.Background()

type rig struct {
	t           *testing.T
	core, admin *store.DB
	me          int64
	chat        int64
	seq         int
	now         time.Time
	ran         []string
	fail        bool
	judged      []string
	judgeErr    error
}

func newRig(t *testing.T) *rig {
	t.Helper()
	core, admin := testdb.MigratedPair(t, "kaya_core")
	r := &rig{t: t, core: core, admin: admin, now: time.Now()}
	if err := admin.QueryRow(bg, "SELECT contact_id_for('100000000000001@lid', '919999999999@s.whatsapp.net')").Scan(&r.me); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(bg, "INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me).Scan(&r.chat); err != nil {
		t.Fatal(err)
	}
	return r
}

// say stores DJ's message in his own chat a second after the last one and returns its WhatsApp id.
func (r *rig) say(body string) string {
	r.t.Helper()
	r.seq++
	waID := fmt.Sprintf("3EB0DJ%d", r.seq)
	if _, err := r.admin.Exec(bg, `INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source)
		VALUES ($1, $2, $3, true, $5, 'text', $4, 'live')`, r.chat, waID, r.me, body, r.clock()); err != nil {
		r.t.Fatal(err)
	}
	return waID
}

func (r *rig) clock() time.Time { return time.Date(2026, 11, 27, 10, 0, r.seq, 0, time.UTC) }

// kaya stores a message Kaya sent to DJ's own chat, as the gateway records a sent request.
func (r *rig) kaya(body string) string {
	r.t.Helper()
	waID := r.say(body)
	if _, err := r.admin.Exec(bg, `WITH r AS (SELECT gen_random_uuid() AS id, date_trunc('second', now() + interval '1 hour') AS at)
		INSERT INTO send_requests (id, payload, expires_at, state, wa_id, finished_at)
		SELECT id, jsonb_build_object('id', id::text, 'expires_at', to_char(at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')),
		       at, 'sent', $1, now() FROM r`, waID); err != nil {
		r.t.Fatal(err)
	}
	return waID
}

// ask proposes p and shows it to DJ in a message from Kaya.
func (r *rig) ask(run *Runner) Action {
	r.t.Helper()
	a, err := run.Store.Propose(bg, proposal)
	if err != nil {
		r.t.Fatal(err)
	}
	waID := r.kaya("Send Rahul: running late? yes or no")
	if ok, err := run.Store.Shown(bg, a.ID, waID, r.clock()); err != nil || !ok {
		r.t.Fatalf("shown %v, %v", ok, err)
	}
	return a
}

func (r *rig) runner() *Runner {
	s := NewStore(r.core, func() time.Time { return r.now }, 10*time.Minute)
	return &Runner{
		Store:  s,
		Logger: slog.New(slog.DiscardHandler),
		Executors: map[string]Executor{"send_message": func(_ context.Context, a Action) (string, error) {
			r.ran = append(r.ran, a.ID)
			if r.fail {
				return "Rahul is not on WhatsApp", errors.New("not on whatsapp")
			}
			return "Sent to Rahul.", nil
		}},
		Judge: func(_ context.Context, asked, reply string) (string, error) {
			r.judged = append(r.judged, reply)
			if r.judgeErr != nil {
				return "", r.judgeErr
			}
			lower := strings.ToLower(reply)
			switch {
			case strings.Contains(lower, " but "):
				return Change, nil
			case strings.HasPrefix(lower, "yes"), strings.HasPrefix(lower, "haan"), strings.HasPrefix(lower, "ok"):
				return Yes, nil
			case strings.HasPrefix(lower, "no"), strings.HasPrefix(lower, "nahi"):
				return No, nil
			}
			return Other, nil
		},
		MessageID: func(ctx context.Context, waID string) (int64, error) {
			var id int64
			err := r.core.QueryRow(ctx, "SELECT id FROM messages WHERE wa_id = $1", waID).Scan(&id)
			return id, err
		},
	}
}

var proposal = Proposal{Kind: "send_message", TargetJID: "918888888888@s.whatsapp.net", Summary: "Send Rahul: running late", Payload: json.RawMessage(`{"text":"running late"}`)}

func (r *rig) handle(run *Runner, body string) (string, bool) {
	r.t.Helper()
	answer, handled, err := run.Handle(bg, r.say(body), body)
	if err != nil {
		r.t.Fatal(err)
	}
	return answer, handled
}

func TestAYesRunsTheActionOnceAndANoDropsIt(t *testing.T) {
	r := newRig(t)
	run := r.runner()
	if _, handled := r.handle(run, "yes"); handled || len(r.judged) != 0 {
		t.Fatal("a yes with nothing waiting was read or taken as a decision")
	}
	first := r.ask(run)
	if _, handled := r.handle(run, "yes but say 8 pm"); handled {
		t.Fatal("a longer reply decided the action")
	}
	second := r.ask(run)
	yes := r.say("haan")
	answer, handled, err := run.Handle(bg, yes, "haan")
	if err != nil || !handled || answer != "Sent to Rahul." || len(r.ran) != 1 || r.ran[0] != second.ID {
		t.Fatalf("answer %q, %v, ran %v, %v", answer, handled, r.ran, err)
	}
	if again, _, _ := run.Handle(bg, yes, "haan"); again != "Sent to Rahul." || len(r.ran) != 1 {
		t.Fatalf("the same yes ran it again: %q, ran %v", again, r.ran)
	}
	var states []string
	rows, _ := r.admin.SQL().QueryContext(bg, "SELECT state FROM pending_actions ORDER BY created_at")
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		states = append(states, s)
	}
	_ = rows.Close()
	if strings.Join(states, ",") != "superseded,done" || first.ID == second.ID {
		t.Fatalf("states %v", states)
	}

	r.ask(run)
	if answer, handled := r.handle(run, "nahi"); !handled || answer != "Okay, I won't." || len(r.ran) != 1 {
		t.Fatalf("no: %q %v, ran %v", answer, handled, r.ran)
	}
}

func TestALateYesDoesNothing(t *testing.T) {
	r := newRig(t)
	run := r.runner()
	r.ask(run)
	r.now = r.now.Add(11 * time.Minute)
	answer, handled := r.handle(run, "yes")
	if !handled || !strings.Contains(answer, "waited too long") || len(r.ran) != 0 {
		t.Fatalf("answer %q %v, ran %v", answer, handled, r.ran)
	}
	if _, handled := r.handle(run, "no"); handled {
		t.Fatal("a late no needs no answer of its own")
	}
	r.now = r.now.Add(2 * time.Hour)
	if _, handled := r.handle(run, "yes"); handled {
		t.Fatal("a yes long after anything lapsed was taken as a decision")
	}
}

func TestAFailedActionSaysWhy(t *testing.T) {
	r := newRig(t)
	r.fail = true
	run := r.runner()
	a := r.ask(run)
	answer, handled := r.handle(run, "yes")
	if !handled || answer != "I couldn't do it: Rahul is not on WhatsApp" {
		t.Fatalf("answer %q", answer)
	}
	var state, result string
	if err := r.admin.QueryRow(bg, "SELECT state, result FROM pending_actions WHERE id = $1::uuid", a.ID).Scan(&state, &result); err != nil || state != Failed || result != answer {
		t.Fatalf("state %s %q, %v", state, result, err)
	}
	if n, err := run.Store.CountDone(bg, "send_message", time.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("count %d, %v", n, err)
	}
	if !strings.Contains(ConfirmLine(a, 10*time.Minute), "Reply *yes* to do it or *no* to cancel (within 10 min)") {
		t.Fatal(ConfirmLine(a, 10*time.Minute))
	}
}

func TestOnlyAYesToAQuestionDJSawDecidesIt(t *testing.T) {
	r := newRig(t)
	run := r.runner()
	if _, err := run.Store.Propose(bg, proposal); err != nil {
		t.Fatal(err)
	}
	if _, handled := r.handle(run, "yes"); handled || len(r.ran) != 0 {
		t.Fatal("a draft DJ never saw was confirmed")
	}

	a, _ := run.Store.Propose(bg, proposal)
	early := r.say("yes")
	asked := r.kaya("Send Rahul: running late? yes or no")
	if ok, _ := run.Store.Shown(bg, a.ID, asked, r.clock()); !ok {
		t.Fatal("not shown")
	}
	if _, handled, _ := run.Handle(bg, early, "yes"); handled {
		t.Fatal("a yes from before the question confirmed it")
	}

	r.kaya("Heads up: Priya asks about dinner")
	if _, handled := r.handle(run, "ok"); handled || len(r.ran) != 0 {
		t.Fatal("an ok after another message from Kaya confirmed the action")
	}

	b := r.ask(run)
	if answer, handled := r.handle(run, "ok"); !handled || answer != "Sent to Rahul." || len(r.ran) != 1 || r.ran[0] != b.ID {
		t.Fatalf("answer %q, ran %v", answer, r.ran)
	}
	if ok, _ := run.Store.Shown(bg, b.ID, "3EB0OTHER", r.clock()); ok {
		t.Fatal("a decided action was shown again")
	}
}

func TestAReplyIsReadOnlyWhenItCouldAnswerAndAFailedReadDoesNothing(t *testing.T) {
	r := newRig(t)
	run := r.runner()
	if _, handled := r.handle(run, "what's the weather"); handled || len(r.judged) != 0 {
		t.Fatal("a message with nothing waiting was sent to the judge")
	}
	a := r.ask(run)
	if _, handled := r.handle(run, "btw what time is it"); handled || len(r.judged) != 1 {
		t.Fatalf("an unrelated reply was taken: judged %v", r.judged)
	}
	r.judgeErr = errors.New("groq away")
	answer, handled := r.handle(run, "haan bhej de")
	if !handled || !strings.Contains(answer, "did nothing") || len(r.ran) != 0 {
		t.Fatalf("answer %q, ran %v", answer, r.ran)
	}
	r.judgeErr = nil
	if answer, handled := r.handle(run, "Yess"); !handled || answer != "Sent to Rahul." || len(r.ran) != 1 || r.ran[0] != a.ID {
		t.Fatalf("answer %q %v, ran %v", answer, handled, r.ran)
	}
}
