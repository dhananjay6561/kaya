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
		hits, _, err := s.Excerpts(bg, q, "", 3)
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
	hits, _, err := s.Excerpts(bg, "saturday plan", "", 3)
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
	hits, _, err := r.searcher().Excerpts(bg, "Rahul passport office visit monday", "", 3)
	if err != nil || len(hits) == 0 || hits[0].Title != "Trip" {
		t.Fatalf("got %q, %v", excerptTitles(hits), err)
	}
}

func TestExcerptsHoldChunksToTheMeaningBar(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 3000, "ok see you")
	r.pass()
	if hits, _, err := r.searcher().Excerpts(bg, "what is the capital of peru", "", 3); err != nil || len(hits) != 0 {
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
	hits, searched, err := r.excerpter().Excerpts(bg, "what is the saturday plan", "", 3)
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
	hits, _, err := r.excerpter().Excerpts(bg, "saturday plan", "", 3)
	if err != nil || len(hits) != 1 || hits[0].ChatID != r.direct {
		t.Fatalf("got %q, %v", excerptTitles(hits), err)
	}
}

func TestExcerptsDoNotSearchAQuestionWithNothingInIt(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 3000, "hi thanks")
	r.pass()
	if hits, searched, err := r.excerpter().Excerpts(bg, "hi, thanks!", "", 3); err != nil || searched || hits != nil {
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
	hits, _, err := r.excerpter().Excerpts(bg, "what time is the party on saturday", "", 3)
	if err != nil || len(hits) == 0 || hits[0].ChatID != r.direct {
		t.Fatalf("got %q, %v", excerptTitles(hits), err)
	}
}

func TestExcerptsCarryTheMessagesBeforeThem(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 300, "bhai login code bhej do")
	r.say(r.direct, 200, "482913 hai jaldi daal")
	r.pass()
	hits, _, err := r.excerpter().Excerpts(bg, "482913 jaldi daal", "", 1)
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

func TestSearchableTellsSmallTalkFromAQuestion(t *testing.T) {
	for q, want := range map[string]bool{
		"hi": false, "thanks!": false, "ok cool": false, "how are you": false, "kya haal hai": false,
		"kal ka plan kya hai": true, "what time is the party": true, "did anyone send the phone number?": true,
		"any new messages today?": true, "wi-fi pw?": true, "राहुल ने क्या कहा": true,
	} {
		if got := searchable(q); got != want {
			t.Errorf("%q: searchable %v, want %v", q, got, want)
		}
	}
}

func TestNamedMatchesTheNamesAContactChoseOnlyWhenDJSavedNone(t *testing.T) {
	r := newRig(t)
	contact := func(phone, saved, push string) int64 {
		id := r.one("SELECT contact_id_for(NULL, $1)", phone+"@s.whatsapp.net")
		r.exec("UPDATE contacts SET saved_name = nullif($2, ''), push_name = $3 WHERE id = $1", id, saved, push)
		r.exec("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1)", id)
		return id
	}
	stuffed := contact("919111111112", "Amit Office", "saturday plan party trip")
	unsaved := contact("919111111113", "", "Neha Saturday")
	got, _, err := r.searcher().named(bg, contentWords("saturday plan"))
	if err != nil || !slices.Equal(got, []int64{unsaved}) {
		t.Fatalf("got %v, want only %d (not %d)", got, unsaved, stuffed)
	}
}

func TestChatTitlesMarkANameTheContactChose(t *testing.T) {
	r := newRig(t)
	id := r.one("SELECT contact_id_for(NULL, '919111111114@s.whatsapp.net')")
	r.exec("UPDATE contacts SET push_name = 'Kaya' WHERE id = $1", id)
	chat := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", id)
	r.add(msg{chat: chat, sender: id, ago: time.Hour, body: str("trust me")})
	r.pass()
	hits, err := r.searcher().Search(bg, "trust me", Filter{ChatIDs: []int64{chat}}, 1)
	if err != nil || len(hits) != 1 || hits[0].Title != "~Kaya" || hits[0].Lines[0].Sender != "~Kaya" {
		t.Fatalf("%+v, %v", hits, err)
	}
}

func TestExcerptsLeaveOutKayasChatWhileTheGatewayIsLoggedOut(t *testing.T) {
	r := newRig(t)
	r.exec("UPDATE connection_state SET own_jid = NULL")
	self := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	r.exchange(self, 5000, "what is the saturday plan", "Dinner at 7.")
	r.say(r.direct, 3000, "the saturday plan is dinner at 7")
	r.pass()
	hits, _, err := r.excerpter().Excerpts(bg, "what is the saturday plan", "", 3)
	if err != nil || len(hits) != 1 || hits[0].ChatID != r.direct {
		t.Fatalf("got %q, %v", excerptTitles(hits), err)
	}
	if got, err := r.recaller().Recall(bg, "saturday plan", nil, time.Time{}, 3); err != nil || len(got) != 1 {
		t.Fatalf("recall while logged out: %q, %v", questions(got), err)
	}
}

func TestExcerptsTakeTheNamesOfAFollowUpFromTheQuestionBefore(t *testing.T) {
	r := newRig(t)
	teja := r.one("SELECT contact_id_for(NULL, '919111111115@s.whatsapp.net')")
	r.exec("UPDATE contacts SET saved_name = 'Tejashree' WHERE id = $1", teja)
	chat := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", teja)
	r.add(msg{chat: chat, sender: teja, ago: 3000 * time.Minute, body: str("did you submit the assignment")})
	r.say(r.direct, 3000, "did you submit the form")
	r.pass()
	s := r.excerpter()
	s.ExcerptDistance = 0
	hits, _, err := s.Excerpts(bg, "did you submit", "tejashree ne kya pucha", 3)
	if err != nil || len(hits) != 1 || hits[0].ChatID != chat {
		t.Fatalf("got %q, %v", excerptTitles(hits), err)
	}
	hits, _, err = s.Excerpts(bg, "rahul did you submit", "tejashree ne kya pucha", 3)
	if err != nil || len(hits) != 1 || hits[0].ChatID != r.direct {
		t.Fatalf("a question that names someone took the names before it: %q, %v", excerptTitles(hits), err)
	}
}

func TestExcerptsAboutRecentMessagesPutTheNewestFirst(t *testing.T) {
	r := newRig(t)
	teja := r.one("SELECT contact_id_for(NULL, '919111111116@s.whatsapp.net')")
	r.exec("UPDATE contacts SET saved_name = 'Tejashree' WHERE id = $1", teja)
	chat := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", teja)
	r.add(msg{chat: chat, sender: teja, ago: 9000 * time.Minute, body: str("tejashree asked what happened are you fine")})
	r.add(msg{chat: chat, sender: teja, ago: 3000 * time.Minute, body: str("can you send the notes")})
	r.add(msg{chat: chat, sender: teja, ago: 600 * time.Minute, body: str("coming to college tomorrow?")})
	r.pass()
	s := r.excerpter()
	for _, q := range [][2]string{{"recentlykya pucha", "tejashree ne kya pucha"}, {"usne ab kya msg kiya hai", "tejashree ne recently kya pucha"}} {
		hits, _, err := s.Excerpts(bg, q[0], q[1], 3)
		if err != nil || len(hits) < 2 || hits[0].Lines[0].Text != "coming to college tomorrow?" || hits[1].Lines[0].Text != "can you send the notes" ||
			!hits[0].Newest || !hits[1].Newest || len(hits) > 2 && hits[2].Newest {
			t.Fatalf("%q: got %q, %v", q[0], excerptTitles(hits), err)
		}
	}
	hits, _, err := s.Excerpts(bg, "what did tejashree ask", "", 3)
	if err != nil || len(hits) == 0 || hits[0].Lines[0].Text != "tejashree asked what happened are you fine" || hits[0].Newest {
		t.Fatalf("a question not about recent messages: %q, %v", excerptTitles(hits), err)
	}
}

func TestRecentlyAndFollowUpsAreToldFromOrdinaryWords(t *testing.T) {
	for q, want := range map[string]bool{
		"recentlykya pucha": true, "what did she ask recently": true, "latest from rahul": true, "uska last message": true,
		"abhi kya bola": true, "haal hi mein kya hua": true, "usne ab kya msg kiya hai": true, "koi new msg": true, "ab tak bataya": false,
		"what did rahul say last week": false, "at last": false, "recentre the map": false, "abhi tak bataya": false,
		"blast": false, "abhishek": false,
	} {
		if got := recently.MatchString(q); got != want {
			t.Errorf("recently %q: %v, want %v", q, got, want)
		}
	}
	for q, want := range map[string]bool{
		"recentlykya pucha": true, "and the notes?": true, "usne aur kya kaha bhai yaar": true,
		"party saturday kab hai": false, "what is the capital of peru": false,
	} {
		if got := followsUp(q); got != want {
			t.Errorf("followsUp %q: %v, want %v", q, got, want)
		}
	}
}

func TestExcerptsDoNotCarryAPersonIntoAnUnrelatedQuestion(t *testing.T) {
	r := newRig(t)
	teja := r.one("SELECT contact_id_for(NULL, '919111111117@s.whatsapp.net')")
	r.exec("UPDATE contacts SET saved_name = 'Tejashree' WHERE id = $1", teja)
	chat := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", teja)
	r.add(msg{chat: chat, sender: teja, ago: 3000 * time.Minute, body: str("tejashree pucha are you coming")})
	r.add(msg{chat: chat, sender: teja, ago: 600 * time.Minute, body: str("send my 500 back")})
	r.pass()
	s := r.excerpter()
	s.ExcerptDistance = 0
	for _, q := range []string{"party saturday kab hai", "abhi party saturday kab hai"} {
		if hits, _, err := s.Excerpts(bg, q, "tejashree ne kya pucha", 3); err != nil || len(hits) != 0 {
			t.Fatalf("%q: got %q, %v", q, excerptTitles(hits), err)
		}
	}
}

func TestExcerptsAboutATopicKeepTheBestMatchFirstEvenWhenRecent(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 9000, "assignment deadline is friday submit before noon")
	for i, text := range []string{"ok bro", "lol", "gm"} {
		r.say(r.direct, 3000-i*1000, text)
	}
	r.pass()
	s := r.excerpter()
	for _, q := range []string{
		"what did rahul say last week about the assignment deadline",
		"rahul ne recently assignment deadline kya bataya",
	} {
		hits, _, err := s.Excerpts(bg, q, "", 3)
		if err != nil || len(hits) == 0 || hits[0].Lines[0].Text != "assignment deadline is friday submit before noon" {
			t.Fatalf("%q: got %q, %v", q, excerptTitles(hits), err)
		}
	}
}

func TestExcerptsAboutRecentMessagesNeverShowKayasOwnChat(t *testing.T) {
	r := newRig(t)
	r.exec("UPDATE contacts SET saved_name = 'Dhananjay' WHERE id = $1", r.me)
	self := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	r.exchange(self, 60, "dhananjay recently", "Nothing.")
	r.add(msg{chat: r.group, sender: r.rahul, ago: 3000 * time.Minute, body: str("dhananjay is coming")})
	r.pass()
	hits, _, err := r.excerpter().Excerpts(bg, "dhananjay recently", "", 3)
	if err != nil || slices.ContainsFunc(hits, func(e Excerpt) bool { return e.ChatID == self }) {
		t.Fatalf("got %q, %v", excerptTitles(hits), err)
	}
}

func TestExcerptsAboutRecentMessagesTakeTheNewestOfAPersonAndAGroup(t *testing.T) {
	r := newRig(t)
	priya := r.one("SELECT contact_id_for(NULL, '919111111118@s.whatsapp.net')")
	r.exec("UPDATE contacts SET saved_name = 'Priya' WHERE id = $1", priya)
	direct := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", priya)
	gang := r.groupNamed("Priya Birthday")
	r.add(msg{chat: direct, sender: priya, ago: 9000 * time.Minute, body: str("oldest from priya")})
	r.add(msg{chat: gang, sender: r.rahul, ago: 600 * time.Minute, body: str("newest in the group")})
	r.add(msg{chat: direct, sender: priya, ago: 3000 * time.Minute, body: str("newer from priya")})
	r.pass()
	hits, _, err := r.excerpter().Excerpts(bg, "priya recently", "", 3)
	if err != nil || len(hits) < 2 || hits[0].Lines[0].Text != "newest in the group" || hits[1].Lines[0].Text != "newer from priya" {
		t.Fatalf("got %q, %v", excerptTitles(hits), err)
	}
}

func TestExcerptsAboutRecentMessagesTakeOnlyTheBestMatchingPerson(t *testing.T) {
	r := newRig(t)
	gym := r.one("SELECT contact_id_for(NULL, '919111111119@s.whatsapp.net')")
	r.exec("UPDATE contacts SET saved_name = 'Gym Trainer', first_name = 'Rahul Gym Trainer' WHERE id = $1", gym)
	gymChat := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", gym)
	r.say(r.direct, 9000, "from rahul himself")
	r.add(msg{chat: gymChat, sender: gym, ago: 600 * time.Minute, body: str("leg day tomorrow")})
	r.pass()
	hits, _, err := r.excerpter().Excerpts(bg, "rahul recently", "", 3)
	if err != nil || len(hits) == 0 || hits[0].ChatID != r.direct {
		t.Fatalf("got %q, %v", excerptTitles(hits), err)
	}
}

func TestExcerptsSearchEveryChatWithAFollowUpAlone(t *testing.T) {
	r := newRig(t)
	teja := r.one("SELECT contact_id_for(NULL, '919111111120@s.whatsapp.net')")
	r.exec("UPDATE contacts SET saved_name = 'Tejashree' WHERE id = $1", teja)
	r.say(r.direct, 3000, "the notes are ready")
	r.pass()
	s := r.excerpter()
	distance := func(q string) float64 {
		hits, err := s.Search(bg, q, Filter{ChatIDs: []int64{r.direct}}, 1)
		if err != nil || len(hits) != 1 {
			t.Fatalf("%v, %v", hits, err)
		}
		return hits[0].Distance
	}
	alone, joined := distance("and the notes?"), distance("tejashree ne kya pucha\nand the notes?")
	if alone >= joined {
		t.Fatalf("the stub does not tell them apart: %v, %v", alone, joined)
	}
	s.ExcerptDistance = (alone + joined) / 2
	hits, _, err := s.Excerpts(bg, "and the notes?", "tejashree ne kya pucha", 3)
	if err != nil || len(hits) != 1 || hits[0].ChatID != r.direct {
		t.Fatalf("got %q, %v", excerptTitles(hits), err)
	}
}
