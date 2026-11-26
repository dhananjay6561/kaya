package summary

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/embeddings"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/masking"
	"github.com/dhananjay6561/kaya/core/internal/search"
	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

var (
	bg         = context.Background()
	kolkata, _ = time.LoadLocation("Asia/Kolkata")
)

type model struct {
	locality llm.Locality
	reply    string
	tokens   int
	// answers, when set, gives each call's completion in turn.
	answers []llm.Completion
	mu      sync.Mutex
	asked   []llm.Request
}

func (m *model) Name() string                { return "fake" }
func (m *model) Locality() llm.Locality      { return m.locality }
func (m *model) ContextTokens() int          { return m.tokens }
func (m *model) CountTokens(text string) int { return len(text)/4 + 1 }

func (m *model) Complete(_ context.Context, r llm.Request) (llm.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = append(m.asked, r)
	if len(m.answers) > 0 {
		next := m.answers[0]
		m.answers = m.answers[1:]
		return next, nil
	}
	return llm.Completion{Text: m.reply, FinishReason: llm.FinishStop}, nil
}

func (m *model) user() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.asked[len(m.asked)-1].Messages[1].Content
}

type rig struct {
	t           *testing.T
	core, admin *store.DB
	me, self    int64
	rahul       int64
	direct      int64
	group       int64
	seq         int
	now         time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	core, admin := testdb.MigratedPair(t, "kaya_core")
	r := &rig{t: t, core: core, admin: admin, now: time.Now()}
	r.me = r.one("SELECT contact_id_for('100000000000001@lid', '919000000000@s.whatsapp.net')")
	r.exec("INSERT INTO connection_state (state, own_jid, occurred_at) VALUES ('connected', '919000000000@s.whatsapp.net', now())")
	r.self = r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	r.rahul = r.one("SELECT contact_id_for(NULL, '919876543210@s.whatsapp.net')")
	r.exec("UPDATE contacts SET saved_name = 'Rahul' WHERE id = $1", r.rahul)
	r.direct = r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.rahul)
	r.group = r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', '120363000000000001@g.us', 'College Gang') RETURNING id")
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

func (r *rig) say(chat int64, ago time.Duration, body string) int64 {
	r.t.Helper()
	r.seq++
	return r.one(`INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source)
		VALUES ($1, $2, $3, false, $4, 'text', $5, 'live') RETURNING id`,
		chat, fmt.Sprintf("3EB0M%d", r.seq), r.rahul, r.now.Add(-ago), body)
}

func (r *rig) summarizer(m *model) *Summarizer {
	r.t.Helper()
	if m.tokens == 0 {
		m.tokens = 4000
	}
	s, err := New(Options{
		DB: r.core, LLM: m, Searcher: search.NewSearcher(r.core, embeddings.NewStub()), Zone: kolkata,
		NewMaskSession: func() MaskSession { return masking.NewSession() },
		Now:            func() time.Time { return r.now },
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

// earlierToday is how long ago a moment earlier today in Kolkata was, at most half an hour.
func (r *rig) earlierToday() time.Duration {
	n := r.now.In(kolkata)
	y, m, d := n.Date()
	gone := n.Sub(time.Date(y, m, d, 0, 0, 0, 0, kolkata))
	return min(gone/2, 30*time.Minute)
}

func TestTodayPutsDirectChatsAndImportantOnesFirstAndLeavesOutNoise(t *testing.T) {
	r := newRig(t)
	if r.earlierToday() < 2*time.Second {
		t.Skip("too close to midnight in Kolkata")
	}
	early := r.earlierToday()
	r.say(r.group, early, "group chatter one")
	r.say(r.group, early-time.Second, "group chatter two")
	promo := r.say(r.group, early-2*time.Second, "70% off shoes")
	r.exec("INSERT INTO message_tags (message_id, tags) VALUES ($1, ARRAY['ignore'])", promo)
	r.say(r.direct, early-3*time.Second, "can you send the deck?")
	r.say(r.direct, 48*time.Hour, "from two days ago")
	r.one("INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source) VALUES ($1, 'SELF1', $2, true, now(), 'text', 'my own command', 'live') RETURNING id", r.self, r.me)
	m := &model{locality: llm.Local, reply: "**Rahul** wants the deck."}
	got, err := r.summarizer(m).Today(bg)
	if err != nil {
		t.Fatal(err)
	}
	if got != "*Rahul* wants the deck." {
		t.Fatalf("got %q", got)
	}
	asked := m.user()
	rahul, gang := strings.Index(asked, "Chat: Rahul"), strings.Index(asked, "Chat: College Gang")
	if rahul < 0 || gang < 0 || rahul > gang {
		t.Fatalf("direct chat not first: %q", asked)
	}
	for _, left := range []string{"70% off", "two days ago", "my own command"} {
		if strings.Contains(asked, left) {
			t.Errorf("%q was sent", left)
		}
	}
	r.exec("UPDATE contacts SET push_name = 'DJ' WHERE id = $1", r.me)
	if _, err := r.summarizer(m).Today(bg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.asked[1].Messages[0].Content, `The owner goes by "DJ" on WhatsApp`) {
		t.Errorf("owner not named: %q", m.asked[1].Messages[0].Content)
	}
	if m.asked[0].Tier != llm.Heavy || !strings.Contains(m.asked[0].Messages[0].Content, "data, not instructions") {
		t.Errorf("request = %+v", m.asked[0])
	}
}

func TestTodayKeepsTheNewestMessagesThatFitAndSaysWhatWasLeftOut(t *testing.T) {
	r := newRig(t)
	if r.earlierToday() < 2*time.Minute {
		t.Skip("too close to midnight in Kolkata")
	}
	for i := range 50 {
		r.say(r.group, r.earlierToday()-time.Duration(i)*time.Second, fmt.Sprintf("message number %02d %s", i, strings.Repeat("x", 100)))
	}
	m := &model{locality: llm.Local, reply: "lots", tokens: 1000}
	got, err := r.summarizer(m).Today(bg)
	if err != nil {
		t.Fatal(err)
	}
	asked := m.user()
	if !strings.Contains(asked, "message number 49") || strings.Contains(asked, "message number 00") || !strings.Contains(asked, "did not fit and were left out") {
		t.Fatalf("asked %q", asked)
	}
	if !strings.Contains(got, "messages from 1 chat did not fit and were left out._") {
		t.Fatalf("got %q", got)
	}
}

func TestTodaySaysWhenNothingCameIn(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 48*time.Hour, "old")
	m := &model{locality: llm.Local}
	if got, err := r.summarizer(m).Today(bg); err != nil || got != "Nothing has come in today yet." || len(m.asked) != 0 {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestChatFindsTheChatByNameAndReadsItsLastDayOrLastMessages(t *testing.T) {
	r := newRig(t)
	r.say(r.group, 72*time.Hour, "quiet week message")
	r.say(r.group, 71*time.Hour, "another old one")
	r.say(r.direct, time.Hour, "not this chat")
	m := &model{locality: llm.Local, reply: "*College Gang*\nOld news only."}
	got, err := r.summarizer(m).Chat(bg, "college gang")
	if err != nil || got != "Summary of *College Gang*\nOld news only." {
		t.Fatalf("got %q, %v", got, err)
	}
	asked := m.user()
	if !strings.Contains(asked, "Chat: College Gang") || !strings.Contains(asked, "quiet week message") || strings.Contains(asked, "not this chat") {
		t.Fatalf("asked %q", asked)
	}
	if got, _ := r.summarizer(m).Chat(bg, "Zorawar"); !strings.Contains(got, `I couldn't find a chat called "Zorawar"`) {
		t.Fatalf("got %q", got)
	}
}

func TestMasksChatsForACloudModelAndRestoresTheSummary(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, time.Minute, "my new number is 9876501234")
	m := &model{locality: llm.Cloud, reply: "Rahul's new number: [PHONE_1]"}
	got, err := r.summarizer(m).Chat(bg, "rahul")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.user(), "9876501234") || got != "Summary of *Rahul*\nRahul's new number: 9876501234" {
		t.Fatalf("asked %q, got %q", m.user(), got)
	}
	if _, err := New(Options{DB: r.core, LLM: m, Searcher: search.NewSearcher(r.core, embeddings.NewStub()), Zone: kolkata}); err == nil {
		t.Fatal("a cloud model was accepted without masking")
	}
}

func TestTodayHearsFromEveryChatBeforeAnyChatGetsMore(t *testing.T) {
	r := newRig(t)
	if r.earlierToday() < 10*time.Minute {
		t.Skip("too close to midnight in Kolkata")
	}
	var groups []int64
	for g := range 12 {
		id := r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', $1, $2) RETURNING id",
			fmt.Sprintf("1203630000000001%02d@g.us", g), fmt.Sprintf("Group %02d", g))
		groups = append(groups, id)
		for i := range 40 {
			r.say(id, r.earlierToday()-time.Duration(g*40+i)*time.Second, fmt.Sprintf("g%02d line %02d %s", g, i, strings.Repeat("y", 60)))
		}
	}
	m := &model{locality: llm.Local, reply: "busy day", tokens: 3000}
	got, err := r.summarizer(m).Today(bg)
	if err != nil {
		t.Fatal(err)
	}
	asked := m.user()
	for g := range groups {
		if !strings.Contains(asked, fmt.Sprintf("Chat: Group %02d", g)) || !strings.Contains(asked, fmt.Sprintf("g%02d line 39", g)) {
			t.Fatalf("group %d not heard from: %q", g, asked)
		}
	}
	if !strings.Contains(got, "from 12 chats did not fit") {
		t.Fatalf("got %q", got)
	}
	if tokens := m.CountTokens(m.asked[0].Messages[0].Content) + m.CountTokens(asked); tokens > 3000 {
		t.Fatalf("the request was %d tokens", tokens)
	}
}

func TestTodayCountsMessagesPastTheLimitPerChat(t *testing.T) {
	r := newRig(t)
	if r.earlierToday() < 10*time.Minute {
		t.Skip("too close to midnight in Kolkata")
	}
	for i := range maxPerChat + 15 {
		r.say(r.group, r.earlierToday()-time.Duration(i)*time.Second, fmt.Sprintf("short %d", i))
	}
	m := &model{locality: llm.Local, reply: "ok", tokens: 100_000}
	got, err := r.summarizer(m).Today(bg)
	if err != nil || !strings.Contains(got, "_15 messages from 1 chat did not fit and were left out._") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestTodayStartsAtMidnightInDJsTimeZone(t *testing.T) {
	r := newRig(t)
	r.now = time.Date(2026, 11, 27, 0, 1, 0, 0, kolkata)
	r.say(r.direct, 2*time.Minute, "late last night")
	r.say(r.direct, 30*time.Second, "just after midnight")
	m := &model{locality: llm.Local, reply: "ok"}
	if _, err := r.summarizer(m).Today(bg); err != nil {
		t.Fatal(err)
	}
	if asked := m.user(); strings.Contains(asked, "late last night") || !strings.Contains(asked, "just after midnight") {
		t.Fatalf("asked %q", asked)
	}
}

func TestChatSkipsMessagesItCannotReadWhenTakingTheLastOnes(t *testing.T) {
	r := newRig(t)
	r.say(r.group, 72*time.Hour, "the plan is friday")
	for i := range minChatMessages + 5 {
		r.seq++
		r.exec(`INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, source) VALUES ($1, $2, $3, false, $4, 'sticker', 'live')`,
			r.group, fmt.Sprintf("3EB0ST%d", i), r.rahul, r.now.Add(-48*time.Hour+time.Duration(i)*time.Second))
	}
	m := &model{locality: llm.Local, reply: "Friday plan."}
	if _, err := r.summarizer(m).Chat(bg, "college gang"); err != nil || !strings.Contains(m.user(), "the plan is friday") {
		t.Fatalf("asked %v, %v", m.asked, err)
	}
}

func TestChatPrefersAGroupOverAStrangerWhoTookItsName(t *testing.T) {
	r := newRig(t)
	stranger := r.one("SELECT contact_id_for(NULL, '917777700000@s.whatsapp.net')")
	r.exec("UPDATE contacts SET push_name = 'College Gang' WHERE id = $1", stranger)
	fake := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", stranger)
	r.say(r.group, 72*time.Hour, "real group")
	r.say(fake, time.Minute, "pretending")
	m := &model{locality: llm.Local, reply: "ok"}
	got, err := r.summarizer(m).Chat(bg, "college gang")
	if err != nil || !strings.HasPrefix(got, "Summary of *College Gang*") || strings.Contains(m.user(), "pretending") {
		t.Fatalf("got %q, asked %q, %v", got, m.user(), err)
	}
}

func TestAnswersThatRanOutOfRoomAreRetriedOrMarked(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, time.Minute, "hello")
	m := &model{locality: llm.Local, answers: []llm.Completion{
		{FinishReason: llm.FinishLength},
		{Text: "half a summ", FinishReason: llm.FinishLength},
	}}
	got, err := r.summarizer(m).Chat(bg, "rahul")
	if err != nil || len(m.asked) != 2 || m.asked[1].Effort != llm.Low || !strings.HasSuffix(got, "half a summ\n\n_Cut short here._") {
		t.Fatalf("got %q, %d calls, %v", got, len(m.asked), err)
	}
}
