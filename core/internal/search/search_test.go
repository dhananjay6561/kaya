package search

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/embeddings"
)

func (r *rig) searcher() *Searcher { return NewSearcher(r.core, r.fake) }

func titles(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		var texts []string
		for _, l := range h.Lines {
			texts = append(texts, l.Text)
		}
		out = append(out, h.Title+": "+strings.Join(texts, " / "))
	}
	return out
}

func (r *rig) groupNamed(name string) int64 {
	r.seq++
	return r.one("INSERT INTO chats (kind, jid, name) VALUES ('group', $1, $2) RETURNING id", fmt.Sprintf("1203630000009%05d@g.us", r.seq), name)
}

func TestSearchFindsChunksByWordsAndByMeaningAndRanksBothFirst(t *testing.T) {
	r := newRig(t)
	trip := r.groupNamed("Trip")
	money := r.groupNamed("Flat")
	other := r.groupNamed("Other")
	r.say(trip, 300, "goa trip plan for december")
	r.say(money, 300, "rent and electricity bill split")
	r.say(other, 300, "december rent is due")
	r.pass()
	hits, err := r.searcher().Search(bg, "rent", Filter{}, 5)
	if err != nil {
		t.Fatal(err)
	}
	got := titles(hits)
	if len(got) < 2 || !slices.Contains(got[:2], "Flat: rent and electricity bill split") || !slices.Contains(got[:2], "Other: december rent is due") {
		t.Fatalf("got %q", got)
	}
	if !hits[0].Words || !hits[0].Meaning {
		t.Fatalf("the top hit was not found both ways: %+v", hits[0])
	}
}

func TestSearchFiltersByChatContactAndTime(t *testing.T) {
	r := newRig(t)
	priya := r.one("SELECT contact_id_for(NULL, '919111111111@s.whatsapp.net')")
	a := r.groupNamed("A")
	b := r.groupNamed("B")
	r.say(a, 3000, "dinner plan saturday")
	r.add(msg{chat: b, sender: priya, ago: 60 * time.Minute, body: str("dinner plan sunday")})
	r.say(r.direct, 120, "dinner plan friday")
	r.pass()
	s := r.searcher()
	check := func(f Filter, want ...string) {
		t.Helper()
		hits, err := s.Search(bg, "dinner plan", f, 10)
		if err != nil {
			t.Fatal(err)
		}
		got := titles(hits)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("filter %+v: got %q, want %q", f, got, want)
		}
	}
	check(Filter{ChatIDs: []int64{a}}, "A: dinner plan saturday")
	check(Filter{ContactIDs: []int64{priya}}, "B: dinner plan sunday")
	check(Filter{ContactIDs: []int64{r.rahul}}, "A: dinner plan saturday", "Rahul: dinner plan friday")
	check(Filter{From: time.Now().Add(-3 * time.Hour)}, "B: dinner plan sunday", "Rahul: dinner plan friday")
	check(Filter{To: time.Now().Add(-24 * time.Hour)}, "A: dinner plan saturday")
}

func TestSearchFindsASkippedChunkByItsWordsAndIgnoresAnotherModel(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 90, "poison but searchable")
	r.fake.fail = func(texts []string) error {
		if slices.ContainsFunc(texts, func(s string) bool { return strings.Contains(s, "poison") }) {
			return &embeddings.Error{Provider: "fake", Code: embeddings.Rejected, Status: 400}
		}
		return nil
	}
	r.pass()
	r.fake.fail = nil
	hits, err := r.searcher().Search(bg, "searchable", Filter{}, 5)
	if err != nil || len(hits) != 1 || hits[0].Meaning || !hits[0].Words {
		t.Fatalf("hits %+v, %v", hits, err)
	}
	r.say(r.group, 90, "vectors from an older model")
	r.pass()
	r.exec("UPDATE chunks SET model = 'old-model' WHERE chat_id = $1", r.group)
	hits, err = r.searcher().Search(bg, "vectors older model", Filter{ChatIDs: []int64{r.group}}, 5)
	if err != nil || len(hits) != 1 || hits[0].Meaning {
		t.Fatalf("a vector from another model was compared: %+v, %v", hits, err)
	}
}

func TestSearchRefusesNothingAndPassesOnTheServicesError(t *testing.T) {
	r := newRig(t)
	if _, err := r.searcher().Search(bg, "  \n ", Filter{}, 5); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("err %v", err)
	}
	r.fake.fail = func([]string) error { return &embeddings.Error{Provider: "fake", Code: embeddings.Unavailable} }
	if _, err := r.searcher().Search(bg, "x", Filter{}, 5); err == nil {
		t.Fatal("a failing service gave results")
	}
}

func TestSearchAsksTheServiceWithTheQueryAsWritten(t *testing.T) {
	r := newRig(t)
	var kinds []embeddings.Kind
	r.ix.o.Embeddings = &kindRecorder{fakeEmbeddings: r.fake, kinds: &kinds}
	s := NewSearcher(r.core, r.ix.o.Embeddings)
	if _, err := s.Search(bg, "call me on 9876543210", Filter{}, 5); err != nil {
		t.Fatal(err)
	}
	if got := r.fake.texts(); len(got) != 1 || got[0] != "call me on 9876543210" || kinds[0] != embeddings.Query {
		t.Fatalf("sent %q as %v", got, kinds)
	}
}

type kindRecorder struct {
	*fakeEmbeddings
	kinds *[]embeddings.Kind
}

func (k *kindRecorder) Embed(ctx context.Context, texts []string, kind embeddings.Kind) (embeddings.Result, error) {
	*k.kinds = append(*k.kinds, kind)
	return k.fakeEmbeddings.Embed(ctx, texts, kind)
}

// exchange stores a question DJ asked in his own chat and Kaya's answer, minutesAgo.
func (r *rig) exchange(self int64, minutesAgo int, question, answer string) {
	r.t.Helper()
	q := r.add(msg{chat: self, fromMe: true, ago: time.Duration(minutesAgo) * time.Minute, body: str(question)})
	a := r.add(msg{chat: self, fromMe: true, ago: time.Duration(minutesAgo)*time.Minute - 30*time.Second, body: str(answer)})
	var answerWaID string
	if err := r.admin.QueryRow(bg, "SELECT wa_id FROM messages WHERE id = $1", a).Scan(&answerWaID); err != nil {
		r.t.Fatal(err)
	}
	r.exec("INSERT INTO exchanges (question_message_id, answer_wa_id) VALUES ($1, $2)", q, answerWaID)
}

func TestRecallFindsOlderExchangesBeyondTheRecentOnes(t *testing.T) {
	r := newRig(t)
	self := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	r.exchange(self, 90*24*60, "what is the wifi password", "It is sunflower42.")
	r.exchange(self, 60*24*60, "best biryani place", "Try Paradise.")
	r.exchange(self, 30*24*60, "wifi router model", "It is a TP-Link Archer.")
	r.exchange(self, 60, "wifi slow today", "Restart the router.")
	r.say(r.direct, 300, "wifi at the office is down")
	r.pass()
	got, err := r.searcher().Recall(bg, "wifi", 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	var questions []string
	for _, e := range got {
		questions = append(questions, e.Question)
	}
	want := []string{"what is the wifi password", "wifi router model"}
	if !slices.Equal(questions, want) {
		t.Fatalf("recalled %q, want %q (oldest first, the latest left to the conversation, other chats left out)", questions, want)
	}
	if got[0].Answer != "It is sunflower42." {
		t.Fatalf("answer %q", got[0].Answer)
	}
	if limited, _ := r.searcher().Recall(bg, "wifi", 1, 1); len(limited) != 1 {
		t.Fatalf("limit not kept: %d", len(limited))
	}
	if none, err := newRig(t).searcher().Recall(bg, "wifi", 50, 3); err != nil || none != nil {
		t.Fatalf("recall with no exchanges: %v, %v", none, err)
	}
}
