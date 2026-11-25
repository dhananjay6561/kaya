package search

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func excerptTitles(excerpts []Excerpt) []string {
	hits := make([]Hit, len(excerpts))
	for i, e := range excerpts {
		hits[i] = e.Hit
	}
	return titles(hits)
}

func TestContentWordsKeepWhatMayBeANameAndDropTheRest(t *testing.T) {
	got := contentWords("What did Rahul say about Saturday? kal Priya ne kya bola, College Gang mein. राहुल ने क्या कहा bhai yaar thanks")
	want := []string{"rahul", "saturday", "priya", "college", "gang", "राहुल"}
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
		hits, _, err := s.Excerpts(bg, q, 3)
		if err != nil {
			t.Fatal(err)
		}
		got := excerptTitles(hits)
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
	hits, _, err := s.Excerpts(bg, "saturday plan", 3)
	if err != nil || len(hits) != 2 {
		t.Fatalf("an unnamed question should search every chat but Kaya's: %q, %v", excerptTitles(hits), err)
	}
}

func TestExcerptsFallBackToEveryChatWhenTheNamedOnesHaveNothing(t *testing.T) {
	r := newRig(t)
	other := r.groupNamed("Trip")
	r.say(r.direct, 3000, "unrelated cricket score")
	r.say(other, 3000, "the passport office visit is on monday")
	r.pass()
	hits, _, err := r.searcher().Excerpts(bg, "Rahul passport office visit monday", 3)
	if err != nil || len(hits) == 0 || hits[0].Title != "Trip" {
		t.Fatalf("got %q, %v", excerptTitles(hits), err)
	}
}

func TestExcerptsHoldChunksToTheMeaningBar(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 3000, "ok see you")
	r.pass()
	if hits, _, err := r.searcher().Excerpts(bg, "what is the capital of peru", 3); err != nil || len(hits) != 0 {
		t.Fatalf("an unrelated chunk was shown: %q, %v", excerptTitles(hits), err)
	}
}

func TestExcerptsLeaveOutKayasOwnChatHoweverMuchOfItMatches(t *testing.T) {
	r := newRig(t)
	self := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	for i := range 10 {
		r.exchange(self, 5000-i*100, "what is the saturday plan", "No idea yet.")
	}
	r.say(r.direct, 3000, "the saturday plan is dinner at 7")
	r.pass()
	hits, searched, err := r.excerpter().Excerpts(bg, "what is the saturday plan", 3)
	if err != nil || !searched || len(hits) != 1 || hits[0].ChatID != r.direct {
		t.Fatalf("got %q, %v, %v", excerptTitles(hits), searched, err)
	}
}

func TestExcerptsLeaveOutNotesToSelfBeforeAnyExchange(t *testing.T) {
	r := newRig(t)
	self := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	r.add(msg{chat: self, fromMe: true, ago: 3000 * time.Minute, body: str("saturday plan note to self")})
	r.say(r.direct, 3000, "saturday plan is dinner")
	r.pass()
	hits, _, err := r.excerpter().Excerpts(bg, "saturday plan", 3)
	if err != nil || len(hits) != 1 || hits[0].ChatID != r.direct {
		t.Fatalf("got %q, %v", excerptTitles(hits), err)
	}
}

func TestExcerptsDoNotSearchAQuestionWithNothingInIt(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 3000, "hi thanks")
	r.pass()
	if hits, searched, err := r.excerpter().Excerpts(bg, "hi, thanks!", 3); err != nil || searched || hits != nil {
		t.Fatalf("got %q, %v, %v", excerptTitles(hits), searched, err)
	}
}

func TestNamedMatchesDevanagariAndRanksByWordsMatched(t *testing.T) {
	r := newRig(t)
	contact := func(phone, name string) int64 {
		id := r.one("SELECT contact_id_for(NULL, $1)", phone+"@s.whatsapp.net")
		r.exec("UPDATE contacts SET saved_name = $2 WHERE id = $1", id, name)
		r.exec("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1)", id)
		return id
	}
	hindi := contact("919222222222", "राहुल शर्मा")
	for i := range 25 {
		contact(fmt.Sprintf("9193%09d", i), fmt.Sprintf("Rohit Kumar %d", i))
	}
	verma := contact("919444444444", "Rohit Verma")
	r.one("SELECT contact_id_for(NULL, '919555555555@s.whatsapp.net')")
	r.exec("UPDATE contacts SET saved_name = 'Rohit Verma Old' WHERE pn = '919555555555@s.whatsapp.net'")
	s := r.searcher()
	got, _, err := s.named(bg, contentWords("राहुल ने क्या कहा"))
	if err != nil || !slices.Equal(got, []int64{hindi}) {
		t.Fatalf("a Devanagari name: %v, %v", got, err)
	}
	got, _, err = s.named(bg, contentWords("what did rohit verma say about the flat"))
	if err != nil || len(got) != 20 || got[0] != verma {
		t.Fatalf("the best match was not first: %v, %v", got, err)
	}
}

func TestExcerptsLetABetterAnswerBeatAChanceNameMatch(t *testing.T) {
	r := newRig(t)
	football := r.groupNamed("Saturday Football")
	r.say(football, 3000, "football practice saturday morning bring boots")
	r.say(r.direct, 3000, "the party on saturday is at 9 pm")
	r.pass()
	hits, _, err := r.excerpter().Excerpts(bg, "what time is the party on saturday", 3)
	if err != nil || len(hits) == 0 || hits[0].ChatID != r.direct {
		t.Fatalf("got %q, %v", excerptTitles(hits), err)
	}
}

func TestExcerptsCarryTheMessagesBeforeThem(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 300, "bhai login code bhej do")
	r.say(r.direct, 200, "482913 hai jaldi daal")
	r.pass()
	hits, _, err := r.excerpter().Excerpts(bg, "482913 jaldi daal", 1)
	if err != nil || len(hits) != 1 || hits[0].Lines[0].Text != "482913 hai jaldi daal" || hits[0].Before != "bhai login code bhej do" {
		t.Fatalf("got %+v, %v", hits, err)
	}
}

func TestSendersNameThemselvesOnlyWithAMark(t *testing.T) {
	r := newRig(t)
	sender := func(phone, column, name string) int64 {
		id := r.one("SELECT contact_id_for(NULL, $1)", phone+"@s.whatsapp.net")
		r.exec("UPDATE contacts SET "+column+" = $2 WHERE id = $1", id, name)
		return id
	}
	chosen := sender("919666666666", "push_name", "Neha")
	posing := sender("919777777777", "push_name", "Kaya")
	saved := sender("919888888888", "saved_name", "ME")
	for i, id := range []int64{chosen, posing, saved, r.rahul} {
		r.add(msg{chat: r.group, sender: id, ago: time.Duration(60-i) * time.Minute, body: str(fmt.Sprintf("line %d", i))})
	}
	r.add(msg{chat: r.group, fromMe: true, ago: 50 * time.Minute, body: str("line mine")})
	r.pass()
	hits, err := r.searcher().Search(bg, "line", Filter{ChatIDs: []int64{r.group}}, 1)
	if err != nil || len(hits) != 1 {
		t.Fatalf("%+v, %v", hits, err)
	}
	var senders []string
	for _, l := range hits[0].Lines {
		senders = append(senders, l.Sender)
	}
	if want := []string{"~Neha", "~Kaya", "~ME", "Rahul", "Me"}; !slices.Equal(senders, want) {
		t.Fatalf("senders %q, want %q", senders, want)
	}
}
