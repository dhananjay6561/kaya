package facts

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/masking"
	"github.com/dhananjay6561/kaya/core/internal/pause"
)

var (
	quiet      = slog.New(slog.DiscardHandler)
	kolkata, _ = time.LoadLocation("Asia/Kolkata")
)

// model answers with the next scripted reply or error, and records what it was asked.
type model struct {
	locality llm.Locality
	mu       sync.Mutex
	replies  []any
	asked    []llm.Request
}

func (m *model) Name() string                { return "fake" }
func (m *model) Locality() llm.Locality      { return m.locality }
func (m *model) ContextTokens() int          { return 4000 }
func (m *model) CountTokens(text string) int { return len(text) / 4 }

func (m *model) requests() []llm.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.asked)
}

func (m *model) script(replies ...any) *model {
	m.replies = append(m.replies, replies...)
	return m
}

func (m *model) Complete(_ context.Context, r llm.Request) (llm.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = append(m.asked, r)
	if len(m.replies) == 0 {
		return llm.Completion{Text: `{"facts":[],"forget":[]}`}, nil
	}
	next := m.replies[0]
	m.replies = m.replies[1:]
	if err, ok := next.(error); ok {
		return llm.Completion{}, err
	}
	return llm.Completion{Text: next.(string)}, nil
}

func (r *rig) learner(m *model, sw pause.Switch) *Learner {
	r.t.Helper()
	if sw == nil {
		sw = &pause.Memory{}
	}
	l, err := NewLearner(LearnerOptions{
		DB: r.core, LLM: m, Pause: sw, Logger: quiet,
		NewMasker: func() Masker { return masking.NewSession() },
		Zone:      kolkata,
		Now:       func() time.Time { return time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC) },
		Poll:      20 * time.Millisecond, Gap: time.Millisecond, RetryMin: 10 * time.Millisecond, RetryMax: 20 * time.Millisecond,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return l
}

func (r *rig) step(l *Learner) (bool, error) {
	r.t.Helper()
	found, _, err := l.next(bg)
	return found, err
}

func (r *rig) cursor() int64 {
	r.t.Helper()
	return r.one("SELECT exchange_id FROM fact_learning")
}

func TestLearnerAddsWhatAnExchangeTaughtAndMovesPastIt(t *testing.T) {
	r := newRig(t)
	q := r.ask("main Keploy mein DevRel hoon", "nice, DevRel at Keploy!")
	m := (&model{locality: llm.Local}).script(`{"facts":[{"text":" Works at Keploy as DevRel ","replaces":0}],"forget":[]}`)
	l := r.learner(m, nil)
	if found, err := r.step(l); !found || err != nil {
		t.Fatalf("found %v, err %v", found, err)
	}
	same(t, r.texts(), []string{"learned: Works at Keploy as DevRel"})
	if got := r.one("SELECT message_id FROM facts"); got != q {
		t.Errorf("message_id = %d, want %d", got, q)
	}
	if found, err := r.step(l); found || err != nil {
		t.Fatalf("a second pass found %v, %v", found, err)
	}
	if len(m.requests()) != 1 {
		t.Fatalf("asked %d times", len(m.requests()))
	}
	req := m.requests()[0]
	if req.Tier != llm.Routine || req.Schema == nil || req.Messages[0].Role != llm.System ||
		!strings.HasSuffix(req.Messages[0].Content, "\n\nToday is Thursday 26 November 2026.") {
		t.Errorf("request = %+v", req)
	}
	if !strings.Contains(req.Messages[1].Content, `Their message, sent on Friday 20 November 2026: "main Keploy mein DevRel hoon"`) ||
		!strings.Contains(req.Messages[1].Content, "(none yet)") {
		t.Errorf("user message = %q", req.Messages[1].Content)
	}
}

func TestLearnerShowsKeptFactsAndChangesAndDropsOnlyLearnedOnes(t *testing.T) {
	r := newRig(t)
	city := r.one("INSERT INTO facts (body, source) VALUES ('Lives in Delhi', 'told') RETURNING id")
	pet := r.one("INSERT INTO facts (body, source) VALUES ('Has a cat', 'learned') RETURNING id")
	job := r.one("INSERT INTO facts (body, source) VALUES ('Works at Acme', 'learned') RETURNING id")
	food := r.one("INSERT INTO facts (body, source) VALUES ('Vegetarian', 'told') RETURNING id")
	r.ask("moved to Pune, joined Keploy, the cat is gone and I eat everything now", "big month")
	m := (&model{locality: llm.Local}).script(`{"facts":[{"text":"Lives in Pune","replaces":` + itoa(city) + `},` +
		`{"text":"Works at Keploy","replaces":` + itoa(job) + `}],"forget":[` + itoa(pet) + `,` + itoa(food) + `]}`)
	l := r.learner(m, nil)
	if _, err := r.step(l); err != nil {
		t.Fatal(err)
	}
	same(t, r.texts(), []string{"told: Vegetarian", "told: Lives in Delhi", "learned: Lives in Pune", "learned: Works at Keploy"})
	if got := r.one("SELECT id FROM facts WHERE body = 'Works at Keploy'"); got != job {
		t.Errorf("the changed fact got a new id %d", got)
	}
	asked := m.requests()[0].Messages[1].Content
	if !strings.Contains(asked, itoa(city)+". Lives in Delhi (theirs)\n") || !strings.Contains(asked, itoa(pet)+". Has a cat\n") {
		t.Errorf("kept facts not shown: %q", asked)
	}
}

func TestLearnerShowsOnlyTheFactsThatFitItsShare(t *testing.T) {
	r := newRig(t)
	r.exec("INSERT INTO facts (body, source) SELECT 'fact ' || n, 'learned' FROM generate_series(1, 50) n")
	r.ask("hi", "hey")
	m := &model{locality: llm.Local}
	l := r.learner(m, nil)
	l.o.FactTokens = 20
	if _, err := r.step(l); err != nil {
		t.Fatal(err)
	}
	shown := strings.Count(m.requests()[0].Messages[1].Content, ". fact ")
	if shown == 0 || shown >= 50 {
		t.Fatalf("%d facts shown", shown)
	}
}

func TestLearnerRefusesSecretsPlaceholdersDuplicatesAndTooMany(t *testing.T) {
	r := newRig(t)
	r.exec("INSERT INTO facts (body, source) VALUES ('Likes chai', 'told')")
	r.ask("remember stuff", "ok")
	m := (&model{locality: llm.Local}).script(`{"facts":[
		{"text":"likes CHAI","replaces":0},
		{"text":"Card number is 4111 1111 1111 1111","replaces":0},
		{"text":"OTP for bank is 482913","replaces":0},
		{"text":"Mother's number is [PHONE_1]","replaces":0},
		{"text":"Plays chess","replaces":424242},
		{"text":"Wifi password is kaya@1234","replaces":0},
		{"text":"` + strings.Repeat("a", maxLearnedLength+1) + `","replaces":0},
		{"text":"Seven","replaces":0}],"forget":[999]}`)
	if _, err := r.step(r.learner(m, nil)); err != nil {
		t.Fatal(err)
	}
	same(t, r.texts(), []string{"told: Likes chai", "learned: Plays chess"})
}

func TestLearnerMakesRoomByDroppingTheStalestLearnedFact(t *testing.T) {
	r := newRig(t)
	r.exec(`INSERT INTO facts (body, source, updated_at)
		SELECT 'fact ' || n, CASE WHEN n = 1 THEN 'told' ELSE 'learned' END, now() - n * interval '1 hour'
		FROM generate_series(1, $1) n`, MaxFacts)
	r.ask("I like tea", "ok")
	m := (&model{locality: llm.Local}).script(`{"facts":[{"text":"Likes tea","replaces":0}],"forget":[]}`)
	if _, err := r.step(r.learner(m, nil)); err != nil {
		t.Fatal(err)
	}
	if n := r.one("SELECT count(*) FROM facts"); n != MaxFacts {
		t.Fatalf("%d facts", n)
	}
	if r.one("SELECT count(*) FROM facts WHERE body IN ('Likes tea', 'fact 1')") != 2 ||
		r.one("SELECT count(*) FROM facts WHERE body = $1", "fact "+itoa(MaxFacts)) != 0 {
		t.Fatal("the wrong fact made room")
	}
}

func TestLearnerAddsNothingWhenEveryFactIsTold(t *testing.T) {
	r := newRig(t)
	r.exec("INSERT INTO facts (body, source) SELECT 'fact ' || n, 'told' FROM generate_series(1, $1) n", MaxFacts)
	r.ask("I like tea", "ok")
	m := (&model{locality: llm.Local}).script(`{"facts":[{"text":"Likes tea","replaces":0}],"forget":[]}`)
	if _, err := r.step(r.learner(m, nil)); err != nil {
		t.Fatal(err)
	}
	if r.one("SELECT count(*) FROM facts WHERE body = 'Likes tea'") != 0 || r.cursor() == 0 {
		t.Fatal("added past the limit, or stuck")
	}
}

func TestLearnerSkipsUnusableExchangesWithoutAskingTheModel(t *testing.T) {
	r := newRig(t)
	q := r.ask("deleted question", "answer")
	r.exec("UPDATE messages SET deleted_at = now() WHERE id = $1", q)
	edited := r.ask("edited after", "answer")
	r.exec("UPDATE messages SET edited_at = now() + interval '1 hour' WHERE id = $1", edited)
	r.exec("INSERT INTO exchanges (question_message_id, answer_wa_id) VALUES (987654, 'nowhere')")
	m := &model{locality: llm.Local}
	l := r.learner(m, nil)
	for range 3 {
		if found, err := r.step(l); !found || err != nil {
			t.Fatalf("found %v err %v", found, err)
		}
	}
	if len(m.requests()) != 0 {
		t.Fatalf("the model was asked %d times", len(m.requests()))
	}
	if r.cursor() != r.one("SELECT max(id) FROM exchanges") {
		t.Fatal("the cursor did not move past them")
	}
}

func TestLearnerSkipsAnswersItCannotReadButRetriesOutages(t *testing.T) {
	r := newRig(t)
	r.ask("one", "a")
	r.ask("two", "b")
	r.ask("three", "c")
	limited := &llm.Error{Code: llm.RateLimited}
	m := (&model{locality: llm.Local}).script(
		"not json",
		&llm.Error{Code: llm.Rejected},
		limited,
		`{"facts":[{"text":"Counts to three","replaces":0}],"forget":[]}`)
	l := r.learner(m, nil)
	for range 2 {
		if _, err := r.step(l); err != nil {
			t.Fatal(err)
		}
	}
	before := r.cursor()
	if _, err := r.step(l); !errors.Is(err, limited) {
		t.Fatalf("err = %v", err)
	}
	if r.cursor() != before {
		t.Fatal("an outage moved the cursor")
	}
	if _, err := r.step(l); err != nil {
		t.Fatal(err)
	}
	same(t, r.texts(), []string{"learned: Counts to three"})
	if !strings.Contains(m.requests()[3].Messages[1].Content, `"three"`) {
		t.Fatal("the retry asked about another exchange")
	}
}

func TestLearnerStopsMovingWhenTheModelKeepsRefusing(t *testing.T) {
	r := newRig(t)
	for range 4 {
		r.ask("q", "a")
	}
	m := (&model{locality: llm.Local}).script(
		&llm.Error{Code: llm.TooLong}, &llm.Error{Code: llm.Rejected}, &llm.Error{Code: llm.Rejected})
	l := r.learner(m, nil)
	for range 2 {
		if _, err := r.step(l); err != nil {
			t.Fatal(err)
		}
	}
	before := r.cursor()
	if _, err := r.step(l); !errors.Is(err, errRefused) {
		t.Fatalf("err = %v", err)
	}
	if r.cursor() != before {
		t.Fatal("a third refusal in a row moved the cursor")
	}
	if _, err := r.step(l); err != nil || r.cursor() == before {
		t.Fatalf("an answer did not let it move on: %v", err)
	}
}

func TestLearnerNeverAppliesAnExchangeTwice(t *testing.T) {
	r := newRig(t)
	r.ask("q", "a")
	e := exchange{id: r.one("SELECT id FROM exchanges")}
	r.exec("UPDATE fact_learning SET exchange_id = $1", e.id)
	out := learned{Facts: []struct {
		Text     string `json:"text"`
		Replaces int64  `json:"replaces"`
	}{{Text: "Likes tea"}}}
	l := r.learner(&model{locality: llm.Local}, nil)
	if err := l.apply(bg, e, out); !errors.Is(err, errLearned) {
		t.Fatalf("err = %v", err)
	}
	if err := l.save(bg, e, out); err != nil {
		t.Fatal(err)
	}
	if n := r.one("SELECT count(*) FROM facts"); n != 0 {
		t.Fatal("applied twice")
	}
}

func TestLearnerPacesByTheTokensItSpends(t *testing.T) {
	l := &Learner{o: LearnerOptions{Gap: 20 * time.Second, TokensPerMinute: 3000}}
	if got := l.pace(500); got != 20*time.Second {
		t.Errorf("a small exchange waits %v", got)
	}
	if got := l.pace(6000); got != 2*time.Minute {
		t.Errorf("a big exchange waits %v", got)
	}
}

func TestLearnerMasksKayasAnswerForACloudModel(t *testing.T) {
	r := newRig(t)
	r.ask("what was mom's number?", "It is 9876543210, from your chat with Mom.")
	m := &model{locality: llm.Cloud}
	if _, err := r.step(r.learner(m, nil)); err != nil {
		t.Fatal(err)
	}
	asked := m.requests()[0].Messages[1].Content
	if strings.Contains(asked, "9876543210") || !strings.Contains(asked, "[PHONE_1]") {
		t.Fatalf("answer not masked: %q", asked)
	}
	if _, err := NewLearner(LearnerOptions{DB: r.core, LLM: m, Pause: &pause.Memory{}, Logger: quiet}); err == nil {
		t.Fatal("a cloud model was accepted without masking")
	}
}

func TestLearnerWaitsWhilePaused(t *testing.T) {
	r := newRig(t)
	r.ask("hi", "hey")
	sw := &pause.Memory{}
	if _, err := sw.Pause(bg, "test"); err != nil {
		t.Fatal(err)
	}
	m := &model{locality: llm.Local}
	if _, err := r.step(r.learner(m, sw)); !errors.Is(err, errPaused) {
		t.Fatalf("err = %v", err)
	}
	if len(m.requests()) != 0 || r.cursor() != 0 {
		t.Fatal("learned while paused")
	}
}

func TestLearnerRunsInTheBackgroundUntilStopped(t *testing.T) {
	r := newRig(t)
	m := (&model{locality: llm.Local}).script(
		&llm.Error{Code: llm.Unavailable},
		`{"facts":[{"text":"Has a dog called Bruno","replaces":0}],"forget":[]}`)
	l := r.learner(m, nil)
	l.Start()
	defer l.Stop()
	r.ask("my dog Bruno is sick", "get well soon Bruno")
	deadline := time.Now().Add(5 * time.Second)
	for r.one("SELECT count(*) FROM facts") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("nothing was learned")
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.Stop()
	l.Stop()
	same(t, r.texts(), []string{"learned: Has a dog called Bruno"})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
