package search

import (
	"slices"
	"testing"
	"time"
)

func TestNameWordsKeepsWhatMayBeANameAndDropsTheRest(t *testing.T) {
	got := nameWords("What did Rahul say about Saturday? kal Priya ne kya bola, College Gang mein")
	want := []string{"rahul", "saturday", "priya", "college", "gang"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// excerpter is a searcher whose bars suit the stub's word vectors, which are far apart unless the
// texts share most of their words.
func (r *rig) excerpter() *Searcher {
	s := r.searcher()
	s.ExcerptDistance, s.NamedDistance = 0.95, 0.95
	return s
}

func TestExcerptsSearchTheChatsAQuestionNamesFirst(t *testing.T) {
	r := newRig(t)
	priya := r.one("SELECT contact_id_for(NULL, '919111111111@s.whatsapp.net')")
	r.exec("UPDATE contacts SET saved_name = 'Priya Sharma' WHERE id = $1", priya)
	gang := r.groupNamed("College Gang")
	r.say(r.direct, 3000, "saturday plan is dinner at 7")
	r.add(msg{chat: gang, sender: priya, ago: 2000 * time.Minute, body: str("saturday plan is a movie")})
	self := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	r.exchange(self, 1000, "saturday plan kya hai", "No idea.")
	r.pass()
	s := r.excerpter()
	only := func(q string, want string) {
		t.Helper()
		hits, err := s.Excerpts(bg, q, 3)
		if err != nil {
			t.Fatal(err)
		}
		got := titles(hits)
		if len(got) == 0 || got[0] != want {
			t.Fatalf("%q: got %q, want %q first", q, got, want)
		}
		for _, h := range hits {
			if h.ChatID == self {
				t.Fatalf("%q: Kaya's own chat came back as an excerpt", q)
			}
		}
	}
	only("what did Rahul say about the saturday plan", "Rahul: saturday plan is dinner at 7")
	only("saturday plan from priya", "College Gang: saturday plan is a movie")
	only("college gang saturday plan", "College Gang: saturday plan is a movie")
	hits, err := s.Excerpts(bg, "saturday plan", 3)
	if err != nil || len(hits) != 2 {
		t.Fatalf("an unnamed question should search every chat but Kaya's: %q, %v", titles(hits), err)
	}
}

func TestExcerptsFallBackToEveryChatWhenTheNamedOnesHaveNothing(t *testing.T) {
	r := newRig(t)
	other := r.groupNamed("Trip")
	r.say(r.direct, 3000, "unrelated cricket score")
	r.say(other, 3000, "the passport office visit is on monday")
	r.pass()
	hits, err := r.searcher().Excerpts(bg, "Rahul passport office visit monday", 3)
	if err != nil || len(hits) == 0 || hits[0].Title != "Trip" {
		t.Fatalf("got %q, %v", titles(hits), err)
	}
}

func TestExcerptsHoldChunksToTheMeaningBar(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 3000, "ok see you")
	r.pass()
	if hits, err := r.searcher().Excerpts(bg, "what is the capital of peru", 3); err != nil || len(hits) != 0 {
		t.Fatalf("an unrelated chunk was shown: %q, %v", titles(hits), err)
	}
}
