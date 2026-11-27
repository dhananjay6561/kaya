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

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/masking"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/core/internal/search"
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
	defer func() { _ = rows.Close() }()
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
		{"line":2,"tags":["important","task","task"],"task":"Send Rahul the deck","mine":true,"reminder":"","due":""},
		{"line":3,"tags":["reminder"],"task":"","mine":true,"reminder":"Call with Priya","due":"2026-11-27 17:00"},
		{"line":4,"tags":["ignore"],"task":"","reminder":"","due":""},
		{"line":9,"tags":["important"],"task":"","reminder":"","due":""}]}`)
	tg := r.tagger(m, Options{})
	if found, err := r.step(tg); !found || err != nil {
		t.Fatalf("found %v, err %v", found, err)
	}
	same(t, r.tags(), []string{"hey: -", "can you send me the deck tonight?: important task", "and the call with Priya is tomorrow 5pm: reminder", "50% off on shoes!!: ignore"})
	open, err := NewTasks(r.core, func() time.Time { return today }).Open(bg, 10)
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
	direct := m.requests()[0].Messages[1].Content
	if !strings.Contains(direct, "Earlier messages:\n[") || !strings.Contains(direct, "Rahul: old 4\n") ||
		strings.Contains(direct, "old 3") || !strings.Contains(direct, "New messages:\n1. [") || strings.Contains(direct, "group news") {
		t.Fatalf("request = %q", direct)
	}
	if !strings.Contains(m.requests()[1].Messages[1].Content, "Chat: Office Team\n") {
		t.Fatalf("a direct chat did not go first: %q", m.requests()[1].Messages[1].Content)
	}
}

func TestMasksMessagesForACloudModelAndPutsDetailsBackInTitles(t *testing.T) {
	r := newRig(t)
	r.say(msg{body: "call me on 9876543210 tomorrow"})
	m := (&model{locality: llm.Cloud}).script(`{"tagged":[{"line":1,"tags":["task"],"task":"Call Rahul on [PHONE_1]","mine":true,"reminder":"","due":""}]}`)
	if _, err := r.step(r.tagger(m, Options{})); err != nil {
		t.Fatal(err)
	}
	asked := m.requests()[0].Messages[1].Content
	if strings.Contains(asked, "9876543210") || !strings.Contains(asked, "[PHONE_1]") {
		t.Fatalf("not masked: %q", asked)
	}
	open, _ := NewTasks(r.core, func() time.Time { return today }).Open(bg, 10)
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
		{"line":1,"tags":["reminder"],"task":"","mine":true,"reminder":"Book the dentist","due":"soon"},
		{"line":2,"tags":["reminder","task"],"task":"Revise for the exam","mine":true,"reminder":"Exam","due":"2026-12-03"}]}`)
	if _, err := r.step(r.tagger(m, Options{})); err != nil {
		t.Fatal(err)
	}
	open, _ := NewTasks(r.core, func() time.Time { return today }).Open(bg, 10)
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
	m := (&model{locality: llm.Local}).script("not json", &llm.Error{Code: llm.Rejected}, &llm.Error{Code: llm.TooLong},
		&llm.Error{Code: llm.Rejected})
	tg := r.tagger(m, Options{})
	for range 3 {
		if _, err := r.step(tg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.step(tg); !errors.Is(err, errRefused) {
		t.Fatalf("err = %v", err)
	}
	same(t, r.tags(), []string{"a: - (skipped bad_json)", "b: - (skipped rejected)", "c: - (skipped too_long)", "d: - (skipped rejected)"})
	if tg.refusals != 0 {
		t.Fatal("the count of refusals was not reset after the pause")
	}
}

func TestHalvesABatchTheModelRefusesUntilOneMessageFails(t *testing.T) {
	r := newRig(t)
	for i := range 4 {
		r.say(msg{body: fmt.Sprintf("m%d", i)})
	}
	m := (&model{locality: llm.Local}).script(&llm.Error{Code: llm.TooLong}, &llm.Error{Code: llm.Rejected},
		`{"tagged":[{"line":1,"tags":["important"],"task":"","reminder":"","due":""}]}`)
	tg := r.tagger(m, Options{})
	if _, err := r.step(tg); err != nil {
		t.Fatal(err)
	}
	same(t, r.tags(), []string{"m0: important"})
	var sizes []int
	for _, req := range m.requests() {
		sizes = append(sizes, strings.Count(req.Messages[1].Content, "] Rahul: m"))
	}
	if !slices.Equal(sizes, []int{4, 2, 1}) {
		t.Fatalf("batch sizes %v", sizes)
	}
	if _, err := r.step(tg); err != nil {
		t.Fatal(err)
	}
	if n := r.one("SELECT count(*) FROM message_tags"); n != 4 {
		t.Fatalf("%d rows", n)
	}
}

func TestFitsABatchToItsInputTokens(t *testing.T) {
	r := newRig(t)
	for i := range 10 {
		r.say(msg{body: fmt.Sprintf("message %d %s", i, strings.Repeat("words ", 40))})
	}
	m := &model{locality: llm.Local}
	tg := r.tagger(m, Options{})
	tg.o.InputTokens = 1000
	if _, err := r.step(tg); err != nil {
		t.Fatal(err)
	}
	asked := m.requests()[0]
	if n := strings.Count(asked.Messages[1].Content, "] Rahul: message"); n == 0 || n == 10 {
		t.Fatalf("%d messages in one request", n)
	}
	if n := r.one("SELECT count(*) FROM message_tags"); n == 0 || n == 10 {
		t.Fatalf("%d rows", n)
	}
	one := &model{locality: llm.Local}
	tiny := r.tagger(one, Options{})
	tiny.o.InputTokens = 1
	if _, err := r.step(tiny); err != nil || !strings.Contains(one.requests()[0].Messages[1].Content, "\n1. [") ||
		strings.Contains(one.requests()[0].Messages[1].Content, "\n2. [") {
		t.Fatalf("a request too small for any message did not carry one: %v", err)
	}
}

func TestPassesOverOldMessagesAndRetagsEditedOnes(t *testing.T) {
	r := newRig(t)
	old := r.say(msg{body: "from last week", sentAt: time.Now().Add(-7 * 24 * time.Hour)})
	edited := r.say(msg{body: "call at 4 tomorrow"})
	m := (&model{locality: llm.Local}).script(
		`{"tagged":[{"line":1,"tags":["reminder"],"task":"","mine":true,"reminder":"Call","due":"2026-11-27 16:00"}]}`,
		`{"tagged":[{"line":1,"tags":["reminder"],"task":"","mine":true,"reminder":"Call","due":"2026-11-27 17:00"}]}`)
	tg := r.tagger(m, Options{})
	if _, err := r.step(tg); err != nil {
		t.Fatal(err)
	}
	if got := r.one("SELECT count(*) FROM message_tags WHERE message_id = $1 AND skipped = 'stale'", old); got != 1 {
		t.Fatal("an old message was not passed over")
	}
	r.exec("UPDATE messages SET body = 'call at 5 tomorrow', edited_at = now() + interval '1 second' WHERE id = $1", edited)
	time.Sleep(1100 * time.Millisecond)
	if _, err := r.step(tg); err != nil {
		t.Fatal(err)
	}
	open, _ := NewTasks(r.core, func() time.Time { return today }).Open(bg, 10)
	if len(open) != 1 || !open[0].Due.Equal(time.Date(2026, 11, 27, 17, 0, 0, 0, kolkata)) {
		t.Fatalf("open = %+v", open)
	}
	if started := r.one("SELECT started_after FROM tagging"); started >= edited {
		t.Fatalf("started_after %d passed a message of the last day", started)
	}
}

func TestDropsADueDayBeforeTheMessageOrAfterAYear(t *testing.T) {
	r := newRig(t)
	r.say(msg{body: "a"})
	r.say(msg{body: "b"})
	m := (&model{locality: llm.Local}).script(`{"tagged":[
		{"line":1,"tags":["reminder"],"task":"","mine":true,"reminder":"Past","due":"2025-11-27 10:00"},
		{"line":2,"tags":["reminder"],"task":"","mine":true,"reminder":"Far","due":"2031-01-01"}]}`)
	if _, err := r.step(r.tagger(m, Options{})); err != nil {
		t.Fatal(err)
	}
	open, _ := NewTasks(r.core, func() time.Time { return today }).Open(bg, 10)
	if len(open) != 2 || open[0].Kind != KindTask || open[1].Kind != KindTask {
		t.Fatalf("open = %+v", open)
	}
}

func TestOpenLeavesOutTasksFromDeletedMessages(t *testing.T) {
	r := newRig(t)
	a := r.say(msg{body: "a"})
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title) VALUES ($1, $2, 'task', 'gone')`, a, r.direct)
	r.exec("UPDATE messages SET deleted_at = now(), body = NULL WHERE id = $1", a)
	if open, _ := NewTasks(r.core, func() time.Time { return today }).Open(bg, 10); len(open) != 0 {
		t.Fatalf("open = %+v", open)
	}
	if n, _ := NewTasks(r.core, func() time.Time { return today }).CountOpen(bg); n != 0 {
		t.Fatalf("counted %d", n)
	}
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
	s := NewTasks(r.core, func() time.Time { return today })
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

func TestMasksNamesCodesAcrossMessagesAndDetailsAtTheCut(t *testing.T) {
	r := newRig(t)
	stranger := r.one("SELECT contact_id_for('100000000000003@lid', $1)", "917777766666@s.whatsapp.net")
	chat := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", stranger)
	send := func(body string) {
		r.seq++
		r.exec(`INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, created_at)
			VALUES ($1, $2, $3, false, $4, 'text', $5, 'live', now() - interval '1 minute')`,
			chat, fmt.Sprintf("3EB0S%d", r.seq), stranger, today.Add(time.Duration(r.seq)*time.Second), body)
	}
	send("bhai code bhej jaldi")
	send("482913")
	send(strings.Repeat("a", maxLineLength-5) + " 9876543210 later")
	m := &model{locality: llm.Cloud}
	if _, err := r.step(r.tagger(m, Options{})); err != nil {
		t.Fatal(err)
	}
	asked := m.requests()[0].Messages[1].Content
	for _, leak := range []string{"7777766666", "482913", "98765"} {
		if strings.Contains(asked, leak) {
			t.Errorf("%s reached the model: %q", leak, asked)
		}
	}
}

func TestANewTaskThatRepeatsAnOpenOneIsNotAddedAndTheModelSeesTheList(t *testing.T) {
	r := newRig(t)
	first := r.say(msg{body: "hourly reminder: Asad (Drizz) today at 3 pm"})
	due := time.Date(2026, 11, 26, 15, 0, 0, 0, kolkata)
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title, due_at, due_has_time) VALUES ($1, $2, 'reminder', 'Asad (Drizz) today at 3 pm', $3, true)`, first, r.direct, due)
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title) VALUES ($1, $2, 'task', 'Send Rahul the deck')`, first, r.direct)
	r.exec(`INSERT INTO message_tags (message_id, tags) VALUES ($1, '{reminder}')`, first)
	r.say(msg{body: "hourly reminder: Asad (drizz) today 3pm"})
	r.say(msg{body: "send the deck to rahul and the invoice too"})
	m := (&model{locality: llm.Local}).script(`{"tagged":[
		{"line":1,"tags":["reminder"],"task":"","mine":true,"reminder":"Asad (drizz) today 3pm","due":"2026-11-26 15:00"},
		{"line":2,"tags":["task"],"task":"Send the deck to Rahul","mine":true,"reminder":"","due":""},
		{"line":2,"tags":["task"],"task":"","reminder":"","due":""}]}`)
	tg := r.tagger(m, Options{})
	if found, err := r.step(tg); !found || err != nil {
		t.Fatalf("found %v, err %v", found, err)
	}
	open, err := NewTasks(r.core, func() time.Time { return today }).Open(bg, 10)
	if err != nil || len(open) != 2 {
		t.Fatalf("open %+v, %v", open, err)
	}
	user := m.requests()[0].Messages[1].Content
	if !strings.HasPrefix(user, "Already on their list:\n- Asad (Drizz) today at 3 pm (was at Thu 26 Nov 15:00)\n- Send Rahul the deck\n\nChat: Rahul\n") {
		t.Fatalf("request = %q", user)
	}
	system := m.requests()[0].Messages[0].Content
	for _, rule := range []string{"Only the owner's own to-dos are tasks", "makes sense on its own weeks later", `"Already on their list" holds`} {
		if !strings.Contains(system, rule) {
			t.Errorf("the prompt lacks %q", rule)
		}
	}

	r.say(msg{body: "and send rahul the invoice"})
	m.script(`{"tagged":[{"line":1,"tags":["task"],"task":"Send Rahul the invoice","mine":true,"reminder":"","due":""}]}`)
	if found, err := r.step(tg); !found || err != nil {
		t.Fatalf("found %v, err %v", found, err)
	}
	if n, _ := NewTasks(r.core, func() time.Time { return today }).CountOpen(bg); n != 3 {
		t.Fatalf("a different task was taken for a repeat: %d open", n)
	}
}

func TestRemindersDropOffWhenTheyAreOverAndLateTasksSaySo(t *testing.T) {
	r := newRig(t)
	at := time.Date(2026, 11, 27, 22, 0, 0, 0, kolkata)
	add := func(kind, title string, due *time.Time, timed bool) {
		id := r.say(msg{body: title})
		r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title, due_at, due_has_time) VALUES ($1, $2, $3, $4, $5, $6)`, id, r.direct, kind, title, due, timed)
	}
	day := func(y, m, d, h, min int) *time.Time {
		t := time.Date(y, time.Month(m), d, h, min, 0, 0, kolkata)
		return &t
	}
	add(KindReminder, "call at three", day(2026, 11, 27, 15, 0), true)
	add(KindReminder, "call at eight", day(2026, 11, 27, 20, 30), true)
	add(KindReminder, "birthday today", day(2026, 11, 27, 0, 0), false)
	add(KindReminder, "birthday yesterday", day(2026, 11, 26, 0, 0), false)
	add(KindTask, "send the form", day(2026, 11, 26, 18, 0), true)
	add(KindTask, "pay rent", day(2026, 11, 28, 0, 0), false)
	s := NewTasks(r.core, func() time.Time { return at })
	open, err := s.Open(bg, 10)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, o := range open {
		lines = append(lines, Describe(o, kolkata, at)[strings.Index(Describe(o, kolkata, at), " ")+1:])
	}
	same(t, lines, []string{
		"send the form, overdue: was due Thu 26 Nov 18:00 (Rahul, 26 Nov)",
		"birthday today, due Fri 27 Nov (Rahul, 26 Nov)",
		"call at eight, was at Fri 27 Nov 20:30 (Rahul, 26 Nov)",
		"pay rent, due Sat 28 Nov (Rahul, 26 Nov)",
	})
	if n, err := s.CountOpen(bg); err != nil || n != 4 {
		t.Fatalf("count %d, %v", n, err)
	}
}

func TestAReminderThatRestatesItsTaskBecomesItsDeadline(t *testing.T) {
	due := time.Date(2026, 11, 27, 0, 0, 0, 0, kolkata)
	got := kinds(found{tags: []string{KindTask, KindReminder}, task: "Review Rahul's resume and send feedback", reminder: "Review Rahul's resume", reminderDue: &due})
	if len(got) != 1 || got[0].kind != KindTask || got[0].due != &due {
		t.Fatalf("got %+v", got)
	}
	got = kinds(found{tags: []string{KindTask}, task: "Send the deck", taskDue: &due})
	if len(got) != 1 || got[0].due != &due {
		t.Fatalf("a task's own deadline was dropped: %+v", got)
	}
	got = kinds(found{tags: []string{KindTask, KindReminder}, task: "Revise for the exam", reminder: "Exam", reminderDue: &due})
	if len(got) != 2 || got[0].kind != KindReminder || got[1].due != nil {
		t.Fatalf("an event and its task were merged: %+v", got)
	}
}

func TestATaskThatIsSomeoneElsesJobIsNotKept(t *testing.T) {
	r := newRig(t)
	r.say(msg{fromMe: true, body: "Approve the payment yaar"})
	r.say(msg{body: "can you send me the deck?"})
	m := (&model{locality: llm.Local}).script(`{"tagged":[
		{"line":1,"tags":["task"],"task":"Approve the payment","mine":false,"reminder":"","due":""},
		{"line":2,"tags":["task"],"task":"Send Rahul the deck","mine":true,"reminder":"","due":""}]}`)
	if found, err := r.step(r.tagger(m, Options{})); !found || err != nil {
		t.Fatalf("found %v, err %v", found, err)
	}
	open, err := NewTasks(r.core, func() time.Time { return today }).Open(bg, 10)
	if err != nil || len(open) != 1 || open[0].Title != "Send Rahul the deck" {
		t.Fatalf("open %+v, %v", open, err)
	}
	same(t, r.tags(), []string{"Approve the payment yaar: task", "can you send me the deck?: task"})
	if system := m.requests()[0].Messages[0].Content; !strings.Contains(system, "mine says whether its task or reminder is the owner's") {
		t.Fatal("the prompt does not ask whose job a task is")
	}
}

func TestKeepStoresNewTasksSkipsRepeatsAndMovesRescheduledOnes(t *testing.T) {
	r := newRig(t)
	at := func(day, hour int) *time.Time {
		d := time.Date(2026, 11, day, hour, 0, 0, 0, kolkata)
		return &d
	}
	keepOne := func(k newTask) bool {
		t.Helper()
		id := r.say(msg{body: k.title})
		var added bool
		err := r.core.InTx(bg, func(tx pgx.Tx) error {
			var err error
			added, err = keep(bg, tx, search.ChatLine{Line: search.Line{MessageID: id, ChatID: r.direct}}, k)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return added
	}
	open := func() []string {
		t.Helper()
		got, err := NewTasks(r.core, func() time.Time { return today }).Open(bg, 20)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, o := range got {
			line := o.Kind + " " + o.Title
			if o.Due != nil {
				line += " @" + o.Due.In(kolkata).Format("2 15:04")
			}
			out = append(out, line)
		}
		return out
	}
	if !keepOne(newTask{kind: KindReminder, title: "Investor call with Priya", due: at(27, 17), timed: true}) {
		t.Fatal("a new reminder was not kept")
	}
	if keepOne(newTask{kind: KindReminder, title: "Call with Priya", due: at(27, 17), timed: true}) {
		t.Fatal("a repeat inside a longer title was kept")
	}
	if keepOne(newTask{kind: KindReminder, title: "Call with Priya", due: at(27, 18), timed: true}) {
		t.Fatal("a new time was kept as a second reminder")
	}
	if keepOne(newTask{kind: KindTask, title: "investor call with priya"}) {
		t.Fatal("a repeat without a day was kept as a task")
	}
	keepOne(newTask{kind: KindTask, title: "Send Rahul the deck"})
	keepOne(newTask{kind: KindTask, title: "Send Rahul the deck by tomorrow", due: at(28, 0)})
	keepOne(newTask{kind: KindTask, title: "Send Rahul the invoice"})
	keepOne(newTask{kind: KindTask, title: "Prepare for the investor call with Priya"})
	same(t, open(), []string{
		"reminder Investor call with Priya @27 18:00",
		"task Send Rahul the deck @28 00:00",
		"task Prepare for the investor call with Priya",
		"task Send Rahul the invoice",
	})

	r.exec("UPDATE messages SET deleted_at = now() WHERE body = 'Send Rahul the invoice'")
	if !keepOne(newTask{kind: KindTask, title: "Send Rahul the invoice"}) {
		t.Fatal("a task was taken for a repeat of one whose message was deleted")
	}
}

func TestEachRowsDueGoesToWhatItNamesAndOnlyTheOwnersAreKept(t *testing.T) {
	r := newRig(t)
	r.say(msg{body: "send the deck tonight, and the call is friday 4pm"})
	r.say(msg{fromMe: true, body: "Rahul, call the client at 4pm"})
	m := (&model{locality: llm.Local}).script(`{"tagged":[
		{"line":1,"tags":["task"],"task":"Send Rahul the deck","mine":true,"reminder":"","due":"2026-11-26 21:00"},
		{"line":1,"tags":["reminder"],"task":"","mine":true,"reminder":"Call with Rahul","due":"2026-11-27 16:00"},
		{"line":2,"tags":["reminder"],"task":"","mine":false,"reminder":"Call with the client","due":"2026-11-26 16:00"}]}`)
	if found, err := r.step(r.tagger(m, Options{})); !found || err != nil {
		t.Fatalf("found %v, err %v", found, err)
	}
	open, err := NewTasks(r.core, func() time.Time { return today }).Open(bg, 10)
	if err != nil || len(open) != 2 {
		t.Fatalf("open %+v, %v", open, err)
	}
	if open[0].Title != "Send Rahul the deck" || !open[0].Due.Equal(time.Date(2026, 11, 26, 21, 0, 0, 0, kolkata)) ||
		open[1].Title != "Call with Rahul" || !open[1].Due.Equal(time.Date(2026, 11, 27, 16, 0, 0, 0, kolkata)) {
		t.Fatalf("open %+v", open)
	}
}

func TestTheModelSeesOnlyThisChatsOpenItems(t *testing.T) {
	r := newRig(t)
	other := r.say(msg{chat: r.group, body: "standup"})
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title) VALUES ($1, $2, 'task', 'Office secret task')`, other, r.group)
	mine := r.say(msg{body: "deck"})
	r.exec(`INSERT INTO tasks (message_id, chat_id, kind, title) VALUES ($1, $2, 'task', 'Send Rahul the deck')`, mine, r.direct)
	r.exec(`INSERT INTO message_tags (message_id, tags) VALUES ($1, '{task}'), ($2, '{task}')`, other, mine)
	r.say(msg{body: "hi"})
	m := (&model{locality: llm.Local}).script(`{"tagged":[]}`)
	if found, err := r.step(r.tagger(m, Options{})); !found || err != nil {
		t.Fatalf("found %v, err %v", found, err)
	}
	user := m.requests()[0].Messages[1].Content
	if !strings.Contains(user, "- Send Rahul the deck\n") || strings.Contains(user, "Office secret task") {
		t.Fatalf("request = %q", user)
	}
}

func TestTaggingQueuesAlertsForMessagesThatNeedTheOwner(t *testing.T) {
	r := newRig(t)
	sick := r.say(msg{body: "doctor pe chale jaao, call me on 9876543210"})
	r.say(msg{fromMe: true, body: "ok will go"})
	r.say(msg{body: "lol"})
	m := (&model{locality: llm.Cloud}).script(`{"tagged":[
		{"line":1,"tags":["important"],"task":"","mine":false,"reminder":"","due":"","alert":"later","note":"Rahul wants a call"},
		{"line":1,"tags":["important"],"task":"","mine":false,"reminder":"","due":"","alert":"now","note":"Rahul says see a doctor and call him on [PHONE_1]"},
		{"line":2,"tags":[],"task":"","mine":false,"reminder":"","due":"","alert":"now","note":"You said you will go"},
		{"line":3,"tags":[],"task":"","mine":false,"reminder":"","due":"","alert":"later","note":""}]}`)
	tg := r.tagger(m, Options{})
	if found, err := r.step(tg); !found || err != nil {
		t.Fatalf("found %v, err %v", found, err)
	}
	type alert struct {
		message int64
		urgent  bool
		note    string
	}
	read := func() []alert {
		t.Helper()
		rows, err := r.admin.SQL().QueryContext(bg, "SELECT message_id, urgent, note FROM alerts ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []alert
		for rows.Next() {
			var a alert
			if err := rows.Scan(&a.message, &a.urgent, &a.note); err != nil {
				t.Fatal(err)
			}
			out = append(out, a)
		}
		return out
	}
	got := read()
	if len(got) != 1 || got[0].message != sick || !got[0].urgent || got[0].note != "Rahul says see a doctor and call him on 9876543210" {
		t.Fatalf("alerts %+v", got)
	}
	if user := m.requests()[0].Messages[1].Content; strings.Contains(user, "9876543210") {
		t.Fatal("the number reached the cloud model")
	}
	if !strings.Contains(m.requests()[0].Messages[0].Content, `alert "now": it cannot wait`) {
		t.Fatal("the prompt does not explain alerts")
	}

	r.exec("UPDATE messages SET body = 'doctor pe chale jaao please', edited_at = now() WHERE id = $1", sick)
	r.exec("DELETE FROM message_tags WHERE message_id = $1", sick)
	m.script(`{"tagged":[{"line":1,"tags":["important"],"task":"","mine":false,"reminder":"","due":"","alert":"later","note":"Rahul says see a doctor"}]}`)
	if found, err := r.step(tg); !found || err != nil {
		t.Fatalf("found %v, err %v", found, err)
	}
	if got := read(); len(got) != 1 || got[0].urgent || got[0].note != "Rahul says see a doctor" {
		t.Fatalf("a retag did not update the waiting alert: %+v", got)
	}
	r.exec("UPDATE alerts SET batch = 'b', sent_at = now()")
	r.exec("DELETE FROM message_tags WHERE message_id = $1", sick)
	m.script(`{"tagged":[{"line":1,"tags":["important"],"task":"","mine":false,"reminder":"","due":"","alert":"now","note":"changed"}]}`)
	if _, err := r.step(tg); err != nil {
		t.Fatal(err)
	}
	if got := read(); got[0].note != "Rahul says see a doctor" {
		t.Fatalf("a sent alert was changed: %+v", got)
	}
}

func TestAlertNotesAreKayasOwnSafeWords(t *testing.T) {
	for _, c := range []struct {
		f       found
		message string
		want    string
	}{
		{found{}, "hi", ""},
		{found{alert: alertLater, note: "Ritu asks about Sunday"}, "free sunday?", "Ritu asks about Sunday"},
		{found{alert: alertLater, note: "Rahul shared the form at https://evil.example/x"}, "form: https://evil.example/x", "Rahul shared the form at (link)"},
		{found{alert: alertNow, note: "Bank says call [PHONE_9]"}, "call us", `Sent you this: "call us"`},
		{found{alert: alertLater, note: "Bank says call [PHONE_9]"}, "call us", ""},
		{found{alert: alertNow, note: "Someone wants your OTP"}, "send me the OTP now", "Careful, it mentions a code or password; never share one. Someone wants your OTP"},
		{found{alert: alertNow}, "urgent, call me at www.x.example", `Sent you this: "urgent, call me at (link)"`},
	} {
		if got := alertNote(c.f, c.message); got != c.want {
			t.Errorf("alertNote(%+v, %q) = %q, want %q", c.f, c.message, got, c.want)
		}
	}
}
