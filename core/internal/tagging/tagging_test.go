package tagging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/masking"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

var (
	bg         = context.Background()
	quiet      = slog.New(slog.DiscardHandler)
	kolkata, _ = time.LoadLocation("Asia/Kolkata")
	today      = time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
)

// model answers with the next scripted reply or error, and records what it was asked.
type model struct {
	locality llm.Locality
	mu       sync.Mutex
	replies  []any
	asked    []llm.Request
}

func (m *model) Name() string                 { return "fake" }
func (m *model) Locality() llm.Locality       { return m.locality }
func (m *model) ContextTokens() int           { return 4000 }
func (m *model) CountTokens(text string) int  { return len(text) / 4 }
func (m *model) script(replies ...any) *model { m.replies = append(m.replies, replies...); return m }

func (m *model) requests() []llm.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.asked)
}

func (m *model) Complete(_ context.Context, r llm.Request) (llm.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = append(m.asked, r)
	if len(m.replies) == 0 {
		return llm.Completion{Text: `{"tagged":[]}`, Usage: llm.Usage{InputTokens: 900, OutputTokens: 100}}, nil
	}
	next := m.replies[0]
	m.replies = m.replies[1:]
	if err, ok := next.(error); ok {
		return llm.Completion{}, err
	}
	return llm.Completion{Text: next.(string), Usage: llm.Usage{InputTokens: 900, OutputTokens: 100}}, nil
}

type rig struct {
	t           *testing.T
	core, admin *store.DB
	me, rahul   int64
	direct      int64
	group       int64
	seq         int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	core, admin := testdb.MigratedPair(t, "kaya_core")
	r := &rig{t: t, core: core, admin: admin}
	r.me = r.one("SELECT contact_id_for('100000000000001@lid', $1)", "919999999999@s.whatsapp.net")
	r.rahul = r.one("SELECT contact_id_for('100000000000002@lid', $1)", "918888888888@s.whatsapp.net")
	r.exec("UPDATE contacts SET saved_name = 'Rahul' WHERE id = $1", r.rahul)
	r.direct = r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.rahul)
	r.group = r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', $1, 'Office Team') RETURNING id", "120363000000000001@g.us")
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

type msg struct {
	chat    int64
	fromMe  bool
	body    string
	source  string
	sentAt  time.Time
	storedA time.Duration
}

// say stores a message and returns its id. By default it is a live text from Rahul, sent and stored
// a minute before today's reference time.
func (r *rig) say(m msg) int64 {
	r.t.Helper()
	r.seq++
	if m.chat == 0 {
		m.chat = r.direct
	}
	if m.source == "" {
		m.source = "live"
	}
	if m.sentAt.IsZero() {
		m.sentAt = today.Add(time.Duration(r.seq) * time.Second)
	}
	if m.storedA == 0 {
		m.storedA = time.Minute
	}
	sender := r.rahul
	if m.fromMe {
		sender = r.me
	}
	return r.one(`INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, created_at)
		VALUES ($1, $2, $3, $4, $5, 'text', $6, $7, now() - make_interval(secs => $8)) RETURNING id`,
		m.chat, fmt.Sprintf("3EB0MSG%d", r.seq), sender, m.fromMe, m.sentAt, m.body, m.source, m.storedA.Seconds())
}

func (r *rig) tagger(m *model, o Options) *Tagger {
	r.t.Helper()
	o.DB, o.LLM, o.Logger, o.Zone = r.core, m, quiet, kolkata
	if o.Pause == nil {
		o.Pause = &pause.Memory{}
	}
	if o.Now == nil {
		o.Now = func() time.Time { return today }
	}
	o.NewMaskSession = func() MaskSession { return masking.NewSession() }
	if o.Settle == 0 {
		o.Settle = 10 * time.Second
	}
	o.Poll, o.RetryMin, o.RetryMax, o.TokensPerMinute = 10*time.Millisecond, 5*time.Millisecond, 10*time.Millisecond, 1<<30
	tg, err := New(o)
	if err != nil {
		r.t.Fatal(err)
	}
	return tg
}

func (r *rig) step(tg *Tagger) (bool, error) {
	r.t.Helper()
	found, _, err := tg.next(bg)
	return found, err
}

// tags is each message's tags as "body: tag tag", in message order, with "-" for none.
func (r *rig) tags() []string {
	r.t.Helper()
	rows, err := r.admin.SQL().QueryContext(bg, `SELECT m.body, array_to_string(t.tags, ' '), coalesce(t.skipped, '')
		FROM message_tags t JOIN messages m ON m.id = t.message_id ORDER BY m.id`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var body, tags, skipped string
		if err := rows.Scan(&body, &tags, &skipped); err != nil {
			r.t.Fatal(err)
		}
		if tags == "" {
			tags = "-"
		}
		if skipped != "" {
			tags += " (skipped " + skipped + ")"
		}
		out = append(out, body+": "+tags)
	}
	return out
}

func same(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestTagsABatchAndKeepsItsTasksAndReminders(t *testing.T) {
	r := newRig(t)
	r.say(msg{body: "hey"})
	r.say(msg{body: "can you send me the deck tonight?"})
	r.say(msg{body: "and the call with Priya is tomorrow 5pm"})
	r.say(msg{body: "50% off on shoes!!"})
	m := (&model{locality: llm.Local}).script(`{"tagged":[
		{"line":2,"tags":["important","task","task"],"task":"Send Rahul the deck","reminder":"","due":""},
		{"line":3,"tags":["reminder"],"task":"","reminder":"Call with Priya","due":"2026-11-27 17:00"},
		{"line":4,"tags":["ignore"],"task":"","reminder":"","due":""},
		{"line":9,"tags":["important"],"task":"","reminder":"","due":""}]}`)
	tg := r.tagger(m, Options{})
	if found, err := r.step(tg); !found || err != nil {
		t.Fatalf("found %v, err %v", found, err)
	}
	same(t, r.tags(), []string{"hey: -", "can you send me the deck tonight?: important task", "and the call with Priya is tomorrow 5pm: reminder", "50% off on shoes!!: ignore"})
	open, err := NewTasks(r.core).Open(bg, 10)
	if err != nil || len(open) != 2 {
		t.Fatalf("open %+v, %v", open, err)
	}
	call, deck := open[0], open[1]
	if call.Kind != KindReminder || call.Title != "Call with Priya" || call.Due == nil || !call.DueHasTime ||
		!call.Due.Equal(time.Date(2026, 11, 27, 17, 0, 0, 0, kolkata)) || call.Chat != "Rahul" {
		t.Errorf("reminder = %+v", call)
	}
	if deck.Kind != KindTask || deck.Title != "Send Rahul the deck" || deck.Due != nil {
		t.Errorf("task = %+v", deck)
	}
	req := m.requests()[0]
	if req.Tier != llm.Routine || req.Schema == nil || !strings.Contains(req.Messages[1].Content, "Chat: Rahul\n") ||
		!strings.Contains(req.Messages[1].Content, "2. [Thu 26 Nov 2026 15:30] Rahul: can you send me the deck tonight?") {
		t.Errorf("request = %q", req.Messages[1].Content)
	}
	if found, err := r.step(tg); found || err != nil {
		t.Fatalf("a second pass found %v, %v", found, err)
	}
}

func TestLeavesOutDJsOwnChatHistoryUnsettledAndOldMessages(t *testing.T) {
	r := newRig(t)
	old := r.say(msg{body: "before tagging began"})
	r.exec("UPDATE tagging SET started_after = $1", old)
	r.say(msg{body: "from history", source: "history"})
	r.say(msg{body: "just now", storedA: time.Second})
	self := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	r.exec("INSERT INTO connection_state (state, own_jid, occurred_at) VALUES ('connected', '919999999999@s.whatsapp.net', now())")
	r.say(msg{chat: self, fromMe: true, body: "what did Rahul say?"})
	r.say(msg{fromMe: true, body: "I'll send it by Friday"})
	r.exec("UPDATE contacts SET push_name = 'DJ <b>' WHERE id = $1", r.me)
	m := &model{locality: llm.Local}
	tg := r.tagger(m, Options{})
	if _, err := r.step(tg); err != nil {
		t.Fatal(err)
	}
	same(t, r.tags(), []string{"I'll send it by Friday: -"})
	if !strings.Contains(m.requests()[0].Messages[0].Content, "The owner goes by \"DJ b\" on WhatsApp.") {
		t.Fatalf("system = %q", m.requests()[0].Messages[0].Content)
	}
	if !strings.Contains(m.requests()[0].Messages[1].Content, "] Me: I'll send it by Friday") {
		t.Fatalf("request = %q", m.requests()[0].Messages[1].Content)
	}
}

func TestShowsEarlierMessagesAsContextAndTagsOneChatAtATime(t *testing.T) {
	r := newRig(t)
	r.exec("UPDATE tagging SET started_after = 0")
	for i := range 7 {
		r.say(msg{body: fmt.Sprintf("old %d", i), source: "history"})
	}
	r.say(msg{chat: r.group, body: "group news"})
	r.say(msg{body: "new one"})
	m := &model{locality: llm.Local}
	tg := r.tagger(m, Options{Context: 3})
	for range 2 {
		if _, err := r.step(tg); err != nil {
			t.Fatal(err)
		}
	}
	direct := m.requests()[1].Messages[1].Content
	if !strings.Contains(direct, "Earlier messages:\n[") || !strings.Contains(direct, "Rahul: old 4\n") ||
		strings.Contains(direct, "old 3") || !strings.Contains(direct, "New messages:\n1. [") || strings.Contains(direct, "group news") {
		t.Fatalf("request = %q", direct)
	}
	if !strings.Contains(m.requests()[0].Messages[1].Content, "Chat: Office Team\n") {
		t.Fatalf("first request = %q", m.requests()[0].Messages[1].Content)
	}
}

func TestMasksMessagesForACloudModelAndPutsDetailsBackInTitles(t *testing.T) {
	r := newRig(t)
	r.say(msg{body: "call me on 9876543210 tomorrow"})
	m := (&model{locality: llm.Cloud}).script(`{"tagged":[{"line":1,"tags":["task"],"task":"Call Rahul on [PHONE_1]","reminder":"","due":""}]}`)
	if _, err := r.step(r.tagger(m, Options{})); err != nil {
		t.Fatal(err)
	}
	asked := m.requests()[0].Messages[1].Content
	if strings.Contains(asked, "9876543210") || !strings.Contains(asked, "[PHONE_1]") {
		t.Fatalf("not masked: %q", asked)
	}
	open, _ := NewTasks(r.core).Open(bg, 10)
	if len(open) != 1 || open[0].Title != "Call Rahul on 9876543210" {
		t.Fatalf("open = %+v", open)
	}
	if _, err := New(Options{DB: r.core, LLM: m, Pause: &pause.Memory{}, Logger: quiet}); err == nil {
		t.Fatal("a cloud model was accepted without masking")
	}
}

func TestAMessageCanGiveATaskAndAReminderAndAReminderWithoutADayIsATask(t *testing.T) {
	r := newRig(t)
	r.say(msg{body: "dentist sometime"})
	r.say(msg{body: "exam on 3 dec"})
	m := (&model{locality: llm.Local}).script(`{"tagged":[
		{"line":1,"tags":["reminder"],"task":"","reminder":"Book the dentist","due":"soon"},
		{"line":2,"tags":["reminder","task"],"task":"Revise for the exam","reminder":"Exam","due":"2026-12-03"}]}`)
	if _, err := r.step(r.tagger(m, Options{})); err != nil {
		t.Fatal(err)
	}
	open, _ := NewTasks(r.core).Open(bg, 10)
	if len(open) != 3 || open[0].Kind != KindReminder || open[0].Title != "Exam" || open[0].DueHasTime ||
		!open[0].Due.Equal(time.Date(2026, 12, 3, 0, 0, 0, 0, kolkata)) {
		t.Fatalf("open = %+v", open)
	}
	var rest []string
	for _, task := range open[1:] {
		rest = append(rest, task.Kind+": "+task.Title)
	}
	slices.Sort(rest)
	same(t, rest, []string{"task: Book the dentist", "task: Revise for the exam"})
}

func TestStopsForTheDayWhenItsTokensAreSpent(t *testing.T) {
	r := newRig(t)
	r.say(msg{body: "one"})
	m := &model{locality: llm.Local}
	day := today
	tg := r.tagger(m, Options{TokensPerDay: 1500, Now: func() time.Time { return day }})
	r.exec("UPDATE tagging SET usage_day = '2026-11-26', usage_tokens = 1500")
	if _, err := r.step(tg); !errors.Is(err, errDailyLimit) {
		t.Fatalf("err = %v", err)
	}
	if len(m.requests()) != 0 {
		t.Fatal("asked the model over the limit")
	}
	day = today.Add(24 * time.Hour)
	if _, err := r.step(tg); err != nil {
		t.Fatal(err)
	}
	if got := r.one("SELECT usage_tokens FROM tagging"); got != 1000 {
		t.Fatalf("usage = %d", got)
	}
	if d := tg.untilTomorrow(); d <= 0 || d > 24*time.Hour {
		t.Fatalf("until tomorrow = %v", d)
	}
}

func TestSkipsBatchesItCannotReadAndStopsWhenRefusalsRepeat(t *testing.T) {
	r := newRig(t)
	r.say(msg{body: "a"})
	r.say(msg{chat: r.group, body: "b"})
	g2 := r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', '120363000000000002@g.us', 'Two') RETURNING id")
	g3 := r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', '120363000000000003@g.us', 'Three') RETURNING id")
	r.say(msg{chat: g2, body: "c"})
	r.say(msg{chat: g3, body: "d"})
	limited := &llm.Error{Code: llm.RateLimited}
	m := (&model{locality: llm.Local}).script("not json", &llm.Error{Code: llm.Rejected}, &llm.Error{Code: llm.TooLong},
		&llm.Error{Code: llm.Rejected}, limited)
	tg := r.tagger(m, Options{})
	for range 3 {
		if _, err := r.step(tg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.step(tg); !errors.Is(err, errRefused) {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.step(tg); !errors.Is(err, limited) {
		t.Fatalf("err = %v", err)
	}
	same(t, r.tags(), []string{"a: - (skipped bad_json)", "b: - (skipped rejected)", "c: - (skipped too_long)"})
	if _, err := r.step(tg); err != nil {
		t.Fatal(err)
	}
	same(t, r.tags(), []string{"a: - (skipped bad_json)", "b: - (skipped rejected)", "c: - (skipped too_long)", "d: -"})
}

func TestWaitsWhilePaused(t *testing.T) {
	r := newRig(t)
	r.say(msg{body: "a"})
	sw := &pause.Memory{}
	if _, err := sw.Pause(bg, "test"); err != nil {
		t.Fatal(err)
	}
	m := &model{locality: llm.Local}
	if _, err := r.step(r.tagger(m, Options{Pause: sw})); !errors.Is(err, errPaused) || len(m.requests()) != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestCountsWhatIsWaiting(t *testing.T) {
	r := newRig(t)
	r.say(msg{body: "a"})
	r.say(msg{body: "b", storedA: time.Second})
	if n, err := Waiting(bg, r.core); err != nil || n != 2 {
		t.Fatalf("waiting %d, %v", n, err)
	}
}

func TestDoneClosesOnlyOpenTasksAndOpenListsDueFirst(t *testing.T) {
	r := newRig(t)
	a := r.say(msg{body: "a"})
	b := r.say(msg{body: "b"})
	c := r.say(msg{body: "c"})
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title, created_at) VALUES ($1, $2, 'task', 'older task', now() - interval '1 day')`, a, r.direct)
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title) VALUES ($1, $2, 'task', 'newer task')`, b, r.direct)
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title, due_at) VALUES ($1, $2, 'reminder', 'due', now() + interval '1 day')`, c, r.group)
	s := NewTasks(r.core)
	open, err := s.Open(bg, 10)
	if err != nil || len(open) != 3 || open[0].Title != "due" || open[0].Chat != "Office Team" || open[1].Title != "newer task" {
		t.Fatalf("open %+v, %v", open, err)
	}
	closed, err := s.Done(bg, []int64{open[1].ID, 999})
	if err != nil || len(closed) != 1 || !closed[0].Done || closed[0].Title != "newer task" {
		t.Fatalf("closed %+v, %v", closed, err)
	}
	if again, _ := s.Done(bg, []int64{open[1].ID}); len(again) != 0 {
		t.Fatal("closed twice")
	}
	if n, err := s.CountOpen(bg); err != nil || n != 2 {
		t.Fatalf("open %d, %v", n, err)
	}
}

func TestRunsInTheBackgroundUntilStopped(t *testing.T) {
	r := newRig(t)
	m := (&model{locality: llm.Local}).script(&llm.Error{Code: llm.Unavailable}, `{"tagged":[{"line":1,"tags":["important"],"task":"","reminder":"","due":""}]}`)
	tg := r.tagger(m, Options{})
	tg.Start()
	defer tg.Stop()
	r.say(msg{body: "urgent: call me"})
	deadline := time.Now().Add(5 * time.Second)
	for r.one("SELECT count(*) FROM message_tags") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("nothing was tagged")
		}
		time.Sleep(20 * time.Millisecond)
	}
	tg.Stop()
	tg.Stop()
	same(t, r.tags(), []string{"urgent: call me: important"})
}
