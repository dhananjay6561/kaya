package search

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

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/dhananjay6561/kaya/core/internal/embeddings"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

var (
	bg    = context.Background()
	quiet = slog.New(slog.DiscardHandler)
)

// fakeEmbeddings records what it is asked and answers with the stub's vectors, unless told to
// fail.
type fakeEmbeddings struct {
	locality llm.Locality
	model    string
	max      int

	mu    sync.Mutex
	calls [][]string
	fail  func(texts []string) error
	// during runs inside Embed, after the texts are recorded, as a change landing mid-call.
	during func()
}

func (f *fakeEmbeddings) Name() string           { return "fake" }
func (f *fakeEmbeddings) Model() string          { return f.model }
func (f *fakeEmbeddings) Locality() llm.Locality { return f.locality }
func (f *fakeEmbeddings) MaxBatch() int          { return f.max }
func (f *fakeEmbeddings) texts() (out []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		out = append(out, c...)
	}
	return out
}

func (f *fakeEmbeddings) Embed(ctx context.Context, texts []string, kind embeddings.Kind) (embeddings.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(texts))
	fail, during := f.fail, f.during
	f.during = nil
	f.mu.Unlock()
	if during != nil {
		during()
	}
	if fail != nil {
		if err := fail(texts); err != nil {
			return embeddings.Result{}, err
		}
	}
	return embeddings.NewStub().Embed(ctx, texts, kind)
}

type rig struct {
	t           *testing.T
	core, admin *store.DB
	fake        *fakeEmbeddings
	switches    *pause.Memory
	ix          *Indexer
	rahul, me   int64
	direct      int64
	group       int64
	seq         int
}

func newRig(t *testing.T, tune ...func(*IndexerOptions)) *rig {
	t.Helper()
	core, admin := testdb.MigratedPair(t, "kaya_core")
	r := &rig{t: t, core: core, admin: admin, switches: &pause.Memory{}}
	r.fake = &fakeEmbeddings{locality: llm.Local, model: "stub-words", max: 128}
	r.me = r.one("SELECT contact_id_for('100000000000001@lid', '919000000000@s.whatsapp.net')")
	r.exec("INSERT INTO connection_state (state, own_jid, occurred_at) VALUES ('connected', '919000000000@s.whatsapp.net', now())")
	r.rahul = r.one("SELECT contact_id_for(NULL, '919876543210@s.whatsapp.net')")
	r.exec("UPDATE contacts SET saved_name = 'Rahul' WHERE id = $1", r.rahul)
	r.direct = r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.rahul)
	r.group = r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', '120363000000000001@g.us', 'College Gang') RETURNING id")
	o := IndexerOptions{
		DB: core, Embeddings: r.fake, Pause: r.switches, Logger: quiet, Zone: time.UTC,
		Settle: time.Minute, Poll: 20 * time.Millisecond, RetryMin: time.Millisecond, RetryMax: 5 * time.Millisecond, Refused: 10 * time.Millisecond,
	}
	for _, f := range tune {
		f(&o)
	}
	r.ix = NewIndexer(o)
	t.Cleanup(r.ix.Stop)
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
	chat   int64
	sender int64
	fromMe bool
	ago    time.Duration
	kind   string
	body   *string
	fresh  bool
}

func str(s string) *string { return &s }

// add stores a message sent ago before now; it is settled unless fresh.
func (r *rig) add(m msg) int64 {
	r.t.Helper()
	r.seq++
	if m.kind == "" {
		m.kind = "text"
	}
	if m.sender == 0 {
		m.sender = r.rahul
	}
	if m.fromMe {
		m.sender = r.me
	}
	created := "now() - interval '1 hour'"
	if m.fresh {
		created = "now()"
	}
	return r.one(`INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, created_at)
		VALUES ($1, $2, $3, $4, now() - make_interval(secs => $5), $6, $7, 'live', `+created+`) RETURNING id`,
		m.chat, fmt.Sprintf("3EB0%04d", r.seq), m.sender, m.fromMe, m.ago.Seconds(), m.kind, m.body)
}

func (r *rig) say(chat int64, minutesAgo int, body string) int64 {
	return r.add(msg{chat: chat, ago: time.Duration(minutesAgo) * time.Minute, body: str(body)})
}

func (r *rig) pass() int {
	r.t.Helper()
	if err := r.ix.lock.Hold(bg); err != nil {
		r.t.Fatal(err)
	}
	n, err := r.ix.pass(bg)
	if err != nil {
		r.t.Fatalf("pass: %v", err)
	}
	return n
}

func (r *rig) count(sql string, args ...any) int64 {
	r.t.Helper()
	return r.one("SELECT count(*) FROM ("+sql+") AS rows", args...)
}

func (r *rig) indexed(id int64) bool {
	return r.count("SELECT 1 FROM message_chunks WHERE message_id = $1", id) == 1
}

func TestIndexesEveryMessageOnceAndFindsNothingTheSecondTime(t *testing.T) {
	r := newRig(t)
	a := r.say(r.direct, 90, "kal milte hain?")
	b := r.say(r.direct, 88, "haan pakka")
	c := r.say(r.group, 50, "trip plan for Goa")
	if n := r.pass(); n != 3 {
		t.Fatalf("indexed %d", n)
	}
	if !r.indexed(a) || !r.indexed(b) || !r.indexed(c) || r.count("SELECT 1 FROM chunks") != 2 {
		t.Fatalf("chunks %d", r.count("SELECT 1 FROM chunks"))
	}
	if n := r.pass(); n != 0 || len(r.fake.calls) != 1 {
		t.Fatalf("second pass indexed %d with %d calls", n, len(r.fake.calls))
	}
	texts := r.fake.texts()
	if !slices.ContainsFunc(texts, func(s string) bool {
		return strings.HasPrefix(s, "Chat: Rahul\n") && strings.Contains(s, "Rahul: kal milte hain?")
	}) ||
		!slices.ContainsFunc(texts, func(s string) bool { return strings.HasPrefix(s, "Chat: College Gang\n") }) {
		t.Fatalf("texts %q", texts)
	}
}

func TestLeavesOutWhatIsNotTextOrNotSettledAndLabelsCaptions(t *testing.T) {
	r := newRig(t)
	deleted := r.say(r.direct, 90, "oops")
	r.exec("UPDATE messages SET deleted_at = now() WHERE id = $1", deleted)
	sticker := r.add(msg{chat: r.direct, ago: 89 * time.Minute, kind: "sticker"})
	blank := r.add(msg{chat: r.direct, ago: 88 * time.Minute, body: str("   ")})
	recent := r.add(msg{chat: r.direct, ago: 0, body: str("just now")})
	fresh := r.add(msg{chat: r.direct, ago: 40 * time.Minute, body: str("history that just landed"), fresh: true})
	photo := r.add(msg{chat: r.direct, ago: 87 * time.Minute, kind: "image", body: str("beach pic")})
	r.pass()
	for name, id := range map[string]int64{"deleted": deleted, "sticker": sticker, "blank": blank, "recent": recent, "fresh": fresh} {
		if r.indexed(id) {
			t.Errorf("%s was indexed", name)
		}
	}
	if !r.indexed(photo) || !strings.Contains(strings.Join(r.fake.texts(), "|"), "Rahul: (photo) beach pic") {
		t.Fatalf("caption not indexed with its label: %q", r.fake.texts())
	}
}

func TestMasksForACloudServiceAndNotForALocalOne(t *testing.T) {
	r := newRig(t)
	r.fake.locality = llm.Cloud
	r.say(r.direct, 90, "my number is 9123456780")
	r.pass()
	if got := strings.Join(r.fake.texts(), "|"); strings.Contains(got, "9123456780") || !strings.Contains(got, "[PHONE_") {
		t.Fatalf("sent %q", got)
	}
	local := newRig(t)
	local.say(local.direct, 90, "my number is 9123456780")
	local.pass()
	if got := strings.Join(local.fake.texts(), "|"); !strings.Contains(got, "9123456780") {
		t.Fatalf("local sent %q", got)
	}
}

func TestNamesKayasAnswersInTheOwnChat(t *testing.T) {
	r := newRig(t)
	self := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	q := r.add(msg{chat: self, fromMe: true, ago: 90 * time.Minute, body: str("who are you?")})
	a := r.add(msg{chat: self, fromMe: true, ago: 89 * time.Minute, body: str("I'm Kaya.")})
	var answerWaID string
	if err := r.admin.QueryRow(bg, "SELECT wa_id FROM messages WHERE id = $1", a).Scan(&answerWaID); err != nil {
		t.Fatal(err)
	}
	r.exec("INSERT INTO exchanges (question_message_id, answer_wa_id) VALUES ($1, $2)", q, answerWaID)
	r.pass()
	got := strings.Join(r.fake.texts(), "|")
	if !strings.Contains(got, "Me: who are you?") || !strings.Contains(got, "Kaya: I'm Kaya.") {
		t.Fatalf("sent %q", got)
	}
}

func TestMakesAChunkAgainOnlyWhenItsTextChanges(t *testing.T) {
	r := newRig(t)
	a := r.say(r.direct, 90, "dinner at 8?")
	r.say(r.direct, 89, "sure")
	r.pass()
	first := r.one("SELECT max(id) FROM chunks")

	r.exec("UPDATE messages SET source = source WHERE id = $1", a)
	r.pass()
	if r.one("SELECT max(id) FROM chunks") != first || len(r.fake.calls) != 1 {
		t.Fatalf("a change that is not text made the chunk again (%d calls)", len(r.fake.calls))
	}
	r.exec("UPDATE messages SET body = 'dinner at 9?', edited_at = now() WHERE id = $1", a)
	r.pass()
	if r.one("SELECT max(id) FROM chunks") == first || len(r.fake.calls) != 2 || r.count("SELECT 1 FROM chunks") != 1 {
		t.Fatalf("an edit did not make the chunk again (%d calls)", len(r.fake.calls))
	}
	if !strings.Contains(r.fake.calls[1][0], "dinner at 9?") {
		t.Fatalf("sent %q", r.fake.calls[1])
	}
}

func TestFreesTheChunkOfAHardDeletedMessage(t *testing.T) {
	r := newRig(t)
	a := r.say(r.direct, 90, "first")
	b := r.say(r.direct, 89, "second")
	r.pass()
	r.exec("DELETE FROM messages WHERE id = $1", b)
	r.pass()
	if !r.indexed(a) || r.count("SELECT 1 FROM message_chunks WHERE message_id = $1", b) != 0 || r.one("SELECT message_count FROM chunks") != 1 {
		t.Fatalf("index after a hard delete: %d mapped", r.count("SELECT 1 FROM message_chunks"))
	}
}

func TestFreesTheChunkOfAMessageDeletedForEveryone(t *testing.T) {
	r := newRig(t)
	a := r.say(r.direct, 90, "first")
	b := r.say(r.direct, 89, "second")
	r.pass()
	r.exec("UPDATE messages SET deleted_at = now() WHERE id = $1", a)
	r.pass()
	if !r.indexed(b) || r.indexed(a) || r.one("SELECT message_count FROM chunks") != 1 {
		t.Fatalf("index after a revoke: %d mapped", r.count("SELECT 1 FROM message_chunks"))
	}
}

func TestFollowsAMessageMovedToAnotherChatWithoutPayingAgain(t *testing.T) {
	r := newRig(t)
	twin := r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', '120363000000000002@g.us', 'College Gang') RETURNING id")
	a := r.say(r.group, 90, "hello all")
	r.pass()
	r.exec("UPDATE messages SET chat_id = $1 WHERE id = $2", twin, a)
	r.pass()
	if got := r.one("SELECT chat_id FROM chunks"); got != twin || len(r.fake.calls) != 1 {
		t.Fatalf("chunk chat %d after %d calls", got, len(r.fake.calls))
	}
}

func TestMakesChunksAgainForAnotherModel(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 90, "hello")
	r.pass()
	r.ix.o.Embeddings.(*fakeEmbeddings).model = "stub-words-v2"
	r.pass()
	if got := r.count("SELECT 1 FROM chunks WHERE model = 'stub-words-v2'"); got != 1 || r.count("SELECT 1 FROM chunks") != 1 {
		t.Fatalf("chunks of the new model: %d", got)
	}
}

func TestNoticesAnEditThatLandsWhileTheChunkIsBeingEmbedded(t *testing.T) {
	r := newRig(t)
	a := r.say(r.direct, 90, "meet at 5")
	r.fake.during = func() { r.exec("UPDATE messages SET body = 'meet at 6', edited_at = now() WHERE id = $1", a) }
	r.pass()
	r.pass()
	if len(r.fake.calls) != 2 || !strings.Contains(r.fake.calls[1][0], "meet at 6") {
		t.Fatalf("calls %q", r.fake.calls)
	}
}

func TestSkipsTheOneChunkTheServiceRefusesAndKeepsTheRest(t *testing.T) {
	r := newRig(t)
	for i := range 5 {
		chat := r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', $1, $2) RETURNING id", fmt.Sprintf("12036300000000010%d@g.us", i), fmt.Sprintf("group %d", i))
		body := "fine"
		if i == 3 {
			body = "poison"
		}
		r.say(chat, 90, body)
	}
	r.fake.fail = func(texts []string) error {
		if slices.ContainsFunc(texts, func(s string) bool { return strings.Contains(s, "poison") }) {
			return &embeddings.Error{Provider: "fake", Code: embeddings.Rejected, Status: 400}
		}
		return nil
	}
	if n := r.pass(); n != 5 {
		t.Fatalf("indexed %d", n)
	}
	if r.count("SELECT 1 FROM chunks WHERE skipped = 'rejected'") != 1 || r.count("SELECT 1 FROM chunks WHERE embedding IS NOT NULL") != 4 {
		t.Fatal("the refused chunk was not set aside alone")
	}
	calls := len(r.fake.calls)
	r.pass()
	if len(r.fake.calls) != calls {
		t.Fatal("a skipped chunk was tried again")
	}
}

func TestHalvesTheBatchOnARateLimitAndCarriesOn(t *testing.T) {
	r := newRig(t)
	for i := range 6 {
		chat := r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', $1, 'g') RETURNING id", fmt.Sprintf("12036300000000020%d@g.us", i))
		r.say(chat, 90, "hi")
	}
	limited := 1
	r.fake.fail = func([]string) error {
		if limited > 0 {
			limited--
			wait := time.Millisecond
			return &embeddings.Error{Provider: "fake", Code: embeddings.RateLimited, Status: 429, RetryAfter: &wait}
		}
		return nil
	}
	if err := r.ix.lock.Hold(bg); err != nil {
		t.Fatal(err)
	}
	_, err := r.ix.pass(bg)
	var e *embeddings.Error
	if !errors.As(err, &e) || e.Code != embeddings.RateLimited || r.ix.batch != 64 {
		t.Fatalf("err %v, batch %d", err, r.ix.batch)
	}
	if n := r.pass(); n != 6 {
		t.Fatalf("indexed %d after the limit", n)
	}
}

func TestStopsAtOnceWhenTheServiceRefusesTheKey(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 90, "hello")
	r.fake.fail = func([]string) error { return &embeddings.Error{Provider: "fake", Code: embeddings.Auth, Status: 401} }
	if err := r.ix.lock.Hold(bg); err != nil {
		t.Fatal(err)
	}
	_, err := r.ix.pass(bg)
	var e *embeddings.Error
	if !errors.As(err, &e) || e.Code != embeddings.Auth || r.count("SELECT 1 FROM chunks") != 0 {
		t.Fatalf("err %v", err)
	}
}

func TestWaitsWhilePausedEvenBetweenBatches(t *testing.T) {
	r := newRig(t)
	r.fake.max = 1
	r.ix.batch = 1
	a := r.say(r.direct, 90, "one")
	b := r.say(r.group, 90, "two")
	if _, err := r.switches.Pause(bg, "x"); err != nil {
		t.Fatal(err)
	}
	if err := r.ix.lock.Hold(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ix.pass(bg); !errors.Is(err, errPaused) || len(r.fake.calls) != 0 {
		t.Fatalf("paused pass: %v, %d calls", err, len(r.fake.calls))
	}
	if _, err := r.switches.Resume(bg, "y"); err != nil {
		t.Fatal(err)
	}
	r.fake.during = func() { _, _ = r.switches.Pause(bg, "z") }
	n, err := r.ix.pass(bg)
	if !errors.Is(err, errPaused) || n != 1 || len(r.fake.calls) != 1 || r.indexed(a) == r.indexed(b) {
		t.Fatalf("pause mid-pass: %v, %d indexed, %d calls", err, n, len(r.fake.calls))
	}
}

func TestMakesAChunkAgainWhenHistoryLandsInsideIt(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 90, "are you coming")
	r.say(r.direct, 84, "yes, on my way")
	r.pass()
	late := r.say(r.direct, 87, "where are you")
	r.pass()
	if !r.indexed(late) || r.count("SELECT 1 FROM chunks") != 1 || r.one("SELECT message_count FROM chunks") != 3 {
		t.Fatalf("late message: %d chunks", r.count("SELECT 1 FROM chunks"))
	}
}

func TestLeavesTheLastChatOfAFullBatchForTheNextPass(t *testing.T) {
	r := newRig(t, func(o *IndexerOptions) { o.BatchMessages = 3 })
	r.say(r.direct, 90, "a")
	r.say(r.direct, 89, "b")
	g1 := r.say(r.group, 90, "c")
	g2 := r.say(r.group, 89, "d")
	if n := r.pass(); n != 2 || r.indexed(g1) {
		t.Fatalf("first pass indexed %d", n)
	}
	if n := r.pass(); n != 2 || !r.indexed(g2) || r.count("SELECT 1 FROM chunks") != 2 {
		t.Fatalf("second pass indexed %d", n)
	}
}

func TestOnlyOneIndexerWorksAndAnotherTakesOverWhenItStops(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 90, "hello")
	other := NewIndexer(r.ix.o)
	r.ix.Start()
	other.Start()
	defer other.Stop()
	deadline := time.Now().Add(10 * time.Second)
	for r.count("SELECT 1 FROM message_chunks") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("nothing indexed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.ix.Stop()
	r.say(r.group, 90, "after the handover")
	other.Wake()
	for r.count("SELECT 1 FROM message_chunks") < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the second indexer never took over")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.count("SELECT 1 FROM chunks") != 2 {
		t.Fatalf("%d chunks", r.count("SELECT 1 FROM chunks"))
	}
}

func TestReportsProgress(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 90, "a")
	r.say(r.direct, 0, "b")
	r.pass()
	p, err := r.ix.Progress(bg)
	if err != nil || p != (Progress{Indexed: 1, Total: 2}) {
		t.Fatalf("progress %+v, %v", p, err)
	}
}

func TestTellsAWriteTheDatabaseWillNeverTakeFromAPassingFailure(t *testing.T) {
	for code, want := range map[string]bool{"22000": true, "23505": true, "42P01": true, "08006": false, "57P01": false, "": false} {
		if got := writeRefused(&pgconn.PgError{Code: code}); got != want {
			t.Errorf("%q: %v", code, got)
		}
	}
	if writeRefused(errors.New("plain")) {
		t.Error("a plain error counted as refused")
	}
}

func TestWaitsForABusyConversationToPauseBeforeChunkingIt(t *testing.T) {
	r := newRig(t)
	a := r.say(r.direct, 25, "are you up")
	b := r.say(r.direct, 18, "yes")
	still := r.add(msg{chat: r.direct, ago: 12 * time.Minute, body: str("coming?"), fresh: true})
	if n := r.pass(); n != 0 || r.indexed(a) {
		t.Fatalf("a conversation still going on was chunked (%d)", n)
	}
	r.exec("UPDATE messages SET created_at = now() - interval '1 hour' WHERE id = $1", still)
	if n := r.pass(); n != 3 || !r.indexed(b) || r.count("SELECT 1 FROM chunks") != 1 {
		t.Fatalf("indexed %d into %d chunks", n, r.count("SELECT 1 FROM chunks"))
	}
}

func TestMasksACodeWhoseRequestWasInAnEarlierChunk(t *testing.T) {
	r := newRig(t)
	r.fake.locality = llm.Cloud
	r.say(r.direct, 120, "OTP bhej jaldi")
	r.pass()
	r.say(r.direct, 90, "ye lo 482913")
	r.pass()
	if got := strings.Join(r.fake.texts(), "|"); strings.Contains(got, "482913") || !strings.Contains(got, "ye lo [OTP_1]") {
		t.Fatalf("sent %q", got)
	}
}

func TestDoesNotSkipEverythingWhenTheServiceRefusesEveryRequest(t *testing.T) {
	r := newRig(t)
	for i := range 4 {
		chat := r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', $1, 'g') RETURNING id", fmt.Sprintf("12036300000000030%d@g.us", i))
		r.say(chat, 90, "hi")
	}
	r.fake.fail = func([]string) error {
		return &embeddings.Error{Provider: "fake", Code: embeddings.Rejected, Status: 400}
	}
	if err := r.ix.lock.Hold(bg); err != nil {
		t.Fatal(err)
	}
	_, err := r.ix.pass(bg)
	var e *embeddings.Error
	if !errors.As(err, &e) || e.Code != embeddings.Misconfigured || r.count("SELECT 1 FROM chunks") != 0 {
		t.Fatalf("err %v, %d chunks", err, r.count("SELECT 1 FROM chunks"))
	}
}

func TestCountsSkippedMessagesInTheProgress(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 90, "poison")
	r.say(r.group, 90, "fine")
	r.fake.fail = func(texts []string) error {
		if slices.ContainsFunc(texts, func(s string) bool { return strings.Contains(s, "poison") }) {
			return &embeddings.Error{Provider: "fake", Code: embeddings.Rejected, Status: 400}
		}
		return nil
	}
	r.pass()
	if p, err := r.ix.Progress(bg); err != nil || p != (Progress{Indexed: 2, Skipped: 1, Total: 2}) {
		t.Fatalf("progress %+v, %v", p, err)
	}
}

func TestFollowsARealContactMerge(t *testing.T) {
	r := newRig(t)
	byLID := r.one("SELECT contact_id_for('100000000000099@lid', NULL)")
	lidChat := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", byLID)
	a := r.add(msg{chat: lidChat, sender: byLID, ago: 90 * time.Minute, body: str("hello from the lid")})
	r.pass()
	if !r.indexed(a) {
		t.Fatal("not indexed before the merge")
	}
	r.one("SELECT contact_id_for('100000000000099@lid', '919876543210@s.whatsapp.net')")
	r.pass()
	var chat, chunkChat int64
	if err := r.admin.QueryRow(bg, `SELECT m.chat_id, k.chat_id FROM messages m
		JOIN message_chunks mc ON mc.message_id = m.id JOIN chunks k ON k.id = mc.chunk_id
		WHERE m.body = 'hello from the lid'`).Scan(&chat, &chunkChat); err != nil {
		t.Fatalf("the merged message is not indexed: %v", err)
	}
	if chat != r.direct || chunkChat != chat {
		t.Fatalf("message in chat %d, its chunk in %d, want %d", chat, chunkChat, r.direct)
	}
	if r.count("SELECT 1 FROM message_chunks mc LEFT JOIN messages m ON m.id = mc.message_id WHERE m.id IS NULL") != 0 {
		t.Fatal("a merged-away message is still in the index")
	}
}

func TestGoesStraightOnWhileThereIsABacklog(t *testing.T) {
	r := newRig(t, func(o *IndexerOptions) { o.BatchMessages = 1; o.Poll = time.Hour })
	for i := range 4 {
		chat := r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', $1, 'g') RETURNING id", fmt.Sprintf("12036300000000040%d@g.us", i))
		r.say(chat, 90, "backlog")
	}
	r.ix.Start()
	deadline := time.Now().Add(5 * time.Second)
	for r.count("SELECT 1 FROM message_chunks") < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 4 indexed: it waited a poll between passes", r.count("SELECT 1 FROM message_chunks"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAChatStillTalkingDoesNotHoldUpTheHistoryBehindIt(t *testing.T) {
	r := newRig(t, func(o *IndexerOptions) { o.BatchMessages = 10 })
	busy := r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', '120363000000000501@g.us', 'busy') RETURNING id")
	old := r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', '120363000000000502@g.us', 'history') RETURNING id")
	r.say(busy, 15, "still")
	r.say(busy, 14, "talking")
	r.add(msg{chat: busy, ago: 8 * time.Minute, body: str("and more"), fresh: true})
	for i := range 40 {
		r.say(old, 2000-i*30, fmt.Sprintf("old message %d", i))
	}
	total := 0
	for range 6 {
		total += r.pass()
	}
	if total != 40 || r.count("SELECT 1 FROM message_chunks mc JOIN messages m ON m.id = mc.message_id WHERE m.chat_id = $1", busy) != 0 {
		t.Fatalf("indexed %d of the 40 old messages; the busy chat has %d", total,
			r.count("SELECT 1 FROM message_chunks mc JOIN messages m ON m.id = mc.message_id WHERE m.chat_id = $1", busy))
	}
}

func TestCuttingABatchInsideAChatWaitsOnlyForItsLastGroup(t *testing.T) {
	r := newRig(t, func(o *IndexerOptions) { o.BatchMessages = 5 })
	for i := range 8 {
		r.say(r.group, 600-i*60, fmt.Sprintf("hourly %d", i))
	}
	if n := r.pass(); n != 4 {
		t.Fatalf("first pass indexed %d, want the 4 groups before the cut", n)
	}
	if n := r.pass(); n != 4 {
		t.Fatalf("second pass indexed %d", n)
	}
}
