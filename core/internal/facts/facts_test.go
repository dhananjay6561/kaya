package facts

import (
	"context"
	"errors"
	"fmt"
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
	own, chat   int64
	seq         int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	core, admin := testdb.MigratedPair(t, "kaya_core")
	r := &rig{t: t, core: core, admin: admin}
	r.own = r.one("SELECT contact_id_for('100000000000001@lid', $1)", "919999999999@s.whatsapp.net")
	r.chat = r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.own)
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

// ask stores a question and Kaya's answer as one exchange and returns the question's message id.
func (r *rig) ask(question, answer string) int64 {
	r.t.Helper()
	r.seq++
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	stored := func(waID, body string) int64 {
		return r.one(`INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source)
		              VALUES ($1, $2, $3, true, $4, 'text', $5, 'live') RETURNING id`, r.chat, waID, r.own, at, body)
	}
	q := stored(fmt.Sprintf("3EB0ASK%d", r.seq), question)
	answerID := fmt.Sprintf("3EB0ANS%d", r.seq)
	stored(answerID, answer)
	r.exec("INSERT INTO exchanges (question_message_id, answer_wa_id, created_at) VALUES ($1, $2, now())", q, answerID)
	return q
}

func (r *rig) texts() []string {
	r.t.Helper()
	all, err := NewStore(r.core).List(bg)
	if err != nil {
		r.t.Fatal(err)
	}
	out := []string{}
	for _, f := range all {
		out = append(out, f.Source+": "+f.Text)
	}
	return out
}

func same(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestRememberKeepsAToldFactOnceWhateverItsCaseAndSpacing(t *testing.T) {
	r := newRig(t)
	s := NewStore(r.core)
	id := r.ask("x", "y")
	f, created, err := s.Remember(bg, "  Works at\nKeploy ", "3EB0ASK1")
	if err != nil || !created || f.Text != "Works at Keploy" || f.Source != Told {
		t.Fatalf("got %+v %v %v", f, created, err)
	}
	again, created, err := s.Remember(bg, "works at keploy", "")
	if err != nil || created || again.ID != f.ID {
		t.Fatalf("a repeat made %+v %v %v", again, created, err)
	}
	same(t, r.texts(), []string{"told: Works at Keploy"})
	if got := r.one("SELECT message_id FROM facts"); got != id {
		t.Fatalf("message_id = %d, want %d", got, id)
	}
}

func TestRememberMarksALearnedFactAsTold(t *testing.T) {
	r := newRig(t)
	r.exec("INSERT INTO facts (body, source) VALUES ('Lives in Delhi', 'learned')")
	f, created, err := NewStore(r.core).Remember(bg, "lives in delhi", "")
	if err != nil || created || f.Source != Told || f.Text != "Lives in Delhi" {
		t.Fatalf("got %+v %v %v", f, created, err)
	}
}

func TestRememberRefusesEmptyTooLongAndPastTheLimit(t *testing.T) {
	r := newRig(t)
	s := NewStore(r.core)
	if _, _, err := s.Remember(bg, " \n\u200b", ""); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty: %v", err)
	}
	if _, _, err := s.Remember(bg, strings.Repeat("é", MaxLength+1), ""); !errors.Is(err, ErrTooLong) {
		t.Errorf("too long: %v", err)
	}
	if _, _, err := s.Remember(bg, strings.Repeat("é", MaxLength), ""); err != nil {
		t.Errorf("the longest allowed: %v", err)
	}
	for _, secret := range []string{"my card is 4111 1111 1111 1111", "Netflix password: hunter2", "ATM PIN 4821", "CVV 123"} {
		if _, _, err := s.Remember(bg, secret, ""); !errors.Is(err, ErrSecret) {
			t.Errorf("%q: %v", secret, err)
		}
	}
	r.exec("INSERT INTO facts (body, source) SELECT 'fact ' || n, 'told' FROM generate_series(2, $1) n", MaxFacts)
	if _, _, err := s.Remember(bg, "one more", ""); !errors.Is(err, ErrFull) {
		t.Errorf("past the limit: %v", err)
	}
	if _, created, err := s.Remember(bg, "FACT 7", ""); err != nil || created {
		t.Errorf("a fact kept already is fine when full: %v %v", created, err)
	}
}

func TestRememberMakesRoomByDroppingTheStalestLearnedFact(t *testing.T) {
	r := newRig(t)
	r.exec(`INSERT INTO facts (body, source, updated_at)
		SELECT 'fact ' || n, 'learned', now() - n * interval '1 hour' FROM generate_series(1, $1) n`, MaxFacts)
	if _, created, err := NewStore(r.core).Remember(bg, "Likes tea", ""); err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	if r.one("SELECT count(*) FROM facts") != MaxFacts || r.one("SELECT count(*) FROM facts WHERE body = $1", "fact "+itoa(MaxFacts)) != 0 {
		t.Fatal("the wrong fact made room")
	}
}

func TestRememberRunAgainForTheSameCommandStillSaysKept(t *testing.T) {
	r := newRig(t)
	r.ask("x", "y")
	s := NewStore(r.core)
	first, created, err := s.Remember(bg, "Likes tea", "3EB0ASK1")
	if err != nil || !created {
		t.Fatal(created, err)
	}
	again, created, err := s.Remember(bg, "Likes tea", "3EB0ASK1")
	if err != nil || !created || again.ID != first.ID {
		t.Fatalf("a retry got %+v %v %v", again, created, err)
	}
	if _, created, _ := s.Remember(bg, "likes tea", "3EB0ANS1"); created {
		t.Fatal("another command repeating it counted as new")
	}
}

func TestCleanKeepsWhatScriptsAndEmojiNeedAndDropsControlCharacters(t *testing.T) {
	england := "\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F"
	for _, c := range [][2]string{
		{"क्\u200cष", "क्\u200cष"},
		{"Fan of " + england, "Fan of " + england},
		{"a\x00b\x07c\u200bd\u2060e", "abcde"},
		{" one\n\ttwo ", "one two"},
	} {
		if got := Clean(c[0]); got != c[1] {
			t.Errorf("Clean(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestListPutsToldFactsFirstThenNewestFirst(t *testing.T) {
	r := newRig(t)
	r.exec(`INSERT INTO facts (body, source, updated_at) VALUES
		('old learned', 'learned', now() - interval '2 days'), ('new learned', 'learned', now()),
		('old told', 'told', now() - interval '3 days'), ('new told', 'told', now() - interval '1 day')`)
	same(t, r.texts(), []string{"told: new told", "told: old told", "learned: new learned", "learned: old learned"})
	all, told, err := NewStore(r.core).Count(bg)
	if err != nil || all != 4 || told != 2 {
		t.Fatalf("count = %d %d %v", all, told, err)
	}
}

func TestForgetRemovesOnlyTheFactsThatExist(t *testing.T) {
	r := newRig(t)
	a := r.one("INSERT INTO facts (body, source) VALUES ('a', 'told') RETURNING id")
	b := r.one("INSERT INTO facts (body, source) VALUES ('b', 'learned') RETURNING id")
	r.exec("INSERT INTO facts (body, source) VALUES ('c', 'told')")
	gone, err := NewStore(r.core).Forget(bg, []int64{b, a, 999})
	if err != nil || len(gone) != 2 || gone[0].Text != "a" || gone[1].Text != "b" {
		t.Fatalf("forgot %+v %v", gone, err)
	}
	same(t, r.texts(), []string{"told: c"})
}
