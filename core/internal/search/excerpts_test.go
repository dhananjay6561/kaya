package search

import (
	"fmt"
	"slices"
	"strings"
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

// excerpter is a searcher whose bars suit the stub's word vectors, which are far apart unless the
// texts share most of their words.
func (r *rig) excerpter() *Searcher {
	s := r.searcher()
	s.ExcerptDistance, s.NamedDistance = 0.95, 0.95
	return s
}

// contact makes a contact with a direct chat; saved and push may be empty.
func (r *rig) contact(saved, push string) (contact, chat int64) {
	r.t.Helper()
	r.seq++
	contact = r.one("SELECT contact_id_for(NULL, $1)", fmt.Sprintf("9198%08d@s.whatsapp.net", r.seq))
	r.exec("UPDATE contacts SET saved_name = nullif($2, ''), push_name = nullif($3, '') WHERE id = $1", contact, saved, push)
	chat = r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", contact)
	return contact, chat
}

func (r *rig) excerpts(s *Searcher, p Plan) []Excerpt {
	r.t.Helper()
	found, err := s.Excerpts(bg, p, 3)
	if err != nil {
		r.t.Fatal(err)
	}
	return found.Excerpts
}

func TestPeopleAreFoundInAnyScriptDespiteTypos(t *testing.T) {
	r := newRig(t)
	hindi, _ := r.contact("राहुल शर्मा", "")
	teja, _ := r.contact("Tejashree Kulkarni", "")
	tanaka, _ := r.contact("田中 太郎", "")
	s := r.searcher()
	for name, want := range map[string]int64{"राहुल": hindi, "tejashri": teja, "TEJASHREE": teja, "田中": tanaka} {
		best, _, unknown, err := s.people(bg, []string{name}, 0)
		if err != nil || !slices.Equal(best, []int64{want}) || unknown != nil {
			t.Fatalf("%q: best %v, unknown %q, %v", name, best, unknown, err)
		}
	}
	if best, _, unknown, err := s.people(bg, []string{"Zorawar"}, 0); err != nil || best != nil || !slices.Equal(unknown, []string{"Zorawar"}) {
		t.Fatalf("an unknown name: %v, %q, %v", best, unknown, err)
	}
}

func TestPeopleMatchOnlySavedNamesAndPreferWhoDJTalksToMost(t *testing.T) {
	r := newRig(t)
	stuffed, _ := r.contact("Amit Office", "saturday plan party")
	unsaved, _ := r.contact("", "Neha Saturday")
	s := r.searcher()
	best, _, _, err := s.people(bg, []string{"saturday"}, 0)
	if err != nil || !slices.Equal(best, []int64{unsaved}) {
		t.Fatalf("got %v, want only %d (not %d)", best, unsaved, stuffed)
	}
	gym, gymChat := r.contact("Rahul Gym", "")
	r.say(r.direct, 9000, "old")
	r.add(msg{chat: gymChat, sender: gym, ago: 60 * time.Minute, body: str("new")})
	if best, all, _, err := s.people(bg, []string{"rahul"}, 0); err != nil || !slices.Equal(best, []int64{gym}) || !slices.Equal(all, []int64{gym, r.rahul}) {
		t.Fatalf("best %v, all %v, %v", best, all, err)
	}
	for i := range 6 {
		r.contact(fmt.Sprintf("Rohit Kumar %d", i), "")
	}
	verma, _ := r.contact("Rohit Verma", "")
	if best, all, _, err := s.people(bg, []string{"rohit verma"}, 0); err != nil || best[0] != verma || len(all) > perName {
		t.Fatalf("best %v, all %v, %v", best, all, err)
	}
}

func TestGroupsAreFoundByTheirName(t *testing.T) {
	r := newRig(t)
	trip := r.groupNamed("Goa Trip 2026")
	best, _, unknown, err := r.searcher().groups(bg, []string{"goa trip", "office"})
	if err != nil || !slices.Equal(best, []int64{trip}) || !slices.Equal(unknown, []string{"office"}) {
		t.Fatalf("best %v, unknown %q, %v", best, unknown, err)
	}
}

func TestCleanNamesKeepsAFewDistinctShortNames(t *testing.T) {
	got := cleanNames([]string{"Rahul", "rahul", " ", "a\nb", strings.Repeat("x", 200), "c", "d", "e", "f"})
	if len(got) != MaxNames || got[0] != "Rahul" || got[1] != "a b" || len([]rune(got[2])) > maxNameLen+1 {
		t.Fatalf("got %q", got)
	}
}

func TestExcerptsSearchTheNamedChatsButNeverKayasOwn(t *testing.T) {
	r := newRig(t)
	priya, _ := r.contact("Priya Sharma", "")
	gang := r.groupNamed("College Gang")
	r.say(r.direct, 3000, "saturday plan is dinner at 7")
	r.add(msg{chat: gang, sender: priya, ago: 2000 * time.Minute, body: str("saturday plan is a movie")})
	self := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	r.exchange(self, 1000, "saturday plan kya hai", "No idea.")
	r.pass()
	s := r.excerpter()
	for _, c := range []struct {
		p    Plan
		want string
	}{
		{Plan{Query: "saturday plan", People: []string{"Rahul"}}, "Rahul: saturday plan is dinner at 7"},
		{Plan{Query: "saturday plan", People: []string{"Priya"}}, "College Gang: saturday plan is a movie"},
		{Plan{Query: "saturday plan", Groups: []string{"college gang"}}, "College Gang: saturday plan is a movie"},
	} {
		got := r.excerpts(s, c.p)
		if !slices.Contains(excerptTitles(got), c.want) || slices.ContainsFunc(got, func(e Excerpt) bool { return e.ChatID == self }) {
			t.Fatalf("%+v: got %q", c.p, excerptTitles(got))
		}
	}
	if got := r.excerpts(s, Plan{Query: "saturday plan"}); len(got) != 2 {
		t.Fatalf("a plan naming nobody should search every chat but Kaya's: %q", excerptTitles(got))
	}
}

func TestExcerptsLeaveOutKayasOwnChatHoweverMuchOfItMatches(t *testing.T) {
	r := newRig(t)
	self := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	for i := range 10 {
		r.exchange(self, 5000-i*100, "what is the saturday plan", "No idea yet.")
	}
	r.add(msg{chat: self, fromMe: true, ago: 9000 * time.Minute, body: str("saturday plan note to self")})
	r.say(r.direct, 3000, "the saturday plan is dinner at 7")
	r.pass()
	if got := r.excerpts(r.excerpter(), Plan{Query: "what is the saturday plan"}); len(got) != 1 || got[0].ChatID != r.direct {
		t.Fatalf("got %q", excerptTitles(got))
	}
	r.exec("UPDATE connection_state SET own_jid = NULL")
	if got := r.excerpts(r.excerpter(), Plan{Query: "what is the saturday plan", People: []string{"Me"}, Newest: true}); slices.ContainsFunc(got, func(e Excerpt) bool { return e.ChatID == self }) {
		t.Fatalf("logged out: got %q", excerptTitles(got))
	}
	if got, err := r.recaller().Recall(bg, "saturday plan", nil, time.Time{}, 3); err != nil || len(got) == 0 {
		t.Fatalf("recall while logged out: %q, %v", questions(got), err)
	}
}

func TestExcerptsLeaveOutNotesToSelfBeforeAnyExchange(t *testing.T) {
	r := newRig(t)
	self := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	r.add(msg{chat: self, fromMe: true, ago: 3000 * time.Minute, body: str("saturday plan note to self")})
	r.say(r.direct, 3000, "saturday plan is dinner")
	r.pass()
	if got := r.excerpts(r.excerpter(), Plan{Query: "saturday plan", Newest: true}); len(got) != 1 || got[0].ChatID != r.direct {
		t.Fatalf("got %q", excerptTitles(got))
	}
}

func TestExcerptsFallBackToEveryChatAndReportUnknownNames(t *testing.T) {
	r := newRig(t)
	trip := r.groupNamed("Trip")
	r.say(r.direct, 3000, "unrelated cricket score")
	r.say(trip, 3000, "the passport office visit is on monday")
	r.pass()
	found, err := r.searcher().Excerpts(bg, Plan{Query: "passport office visit monday", People: []string{"Rahul", "Zorawar"}}, 3)
	if err != nil || len(found.Excerpts) == 0 || found.Excerpts[0].Title != "Trip" || !slices.Equal(found.Unknown, []string{"Zorawar"}) {
		t.Fatalf("got %q, unknown %q, %v", excerptTitles(found.Excerpts), found.Unknown, err)
	}
}

func TestExcerptsHoldChunksToTheMeaningBar(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 3000, "ok see you")
	r.pass()
	if got := r.excerpts(r.searcher(), Plan{Query: "what is the capital of peru"}); len(got) != 0 {
		t.Fatalf("an unrelated chunk was shown: %q", excerptTitles(got))
	}
}

func TestExcerptsLetABetterAnswerBeatAChanceNameMatch(t *testing.T) {
	r := newRig(t)
	football := r.groupNamed("Saturday Football")
	r.say(football, 3000, "football practice saturday morning bring boots")
	r.say(r.direct, 3000, "the party on saturday is at 9 pm")
	r.pass()
	got := r.excerpts(r.excerpter(), Plan{Query: "what time is the party on saturday", Groups: []string{"saturday"}})
	if len(got) == 0 || got[0].ChatID != r.direct {
		t.Fatalf("got %q", excerptTitles(got))
	}
}

func TestExcerptsCarryTheMessagesBeforeThem(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 300, "bhai login code bhej do")
	r.say(r.direct, 200, "482913 hai jaldi daal")
	r.pass()
	found, err := r.excerpter().Excerpts(bg, Plan{Query: "482913 jaldi daal"}, 1)
	if err != nil || len(found.Excerpts) != 1 || found.Excerpts[0].Lines[0].Text != "482913 hai jaldi daal" || found.Excerpts[0].Before != "bhai login code bhej do" {
		t.Fatalf("got %+v, %v", found, err)
	}
}

func TestExcerptsKeepToThePlansTime(t *testing.T) {
	r := newRig(t)
	r.say(r.direct, 9000, "dinner plan old")
	r.say(r.group, 120, "dinner plan new")
	r.pass()
	got := r.excerpts(r.excerpter(), Plan{Query: "dinner plan", From: time.Now().Add(-3 * time.Hour)})
	if len(got) != 1 || got[0].ChatID != r.group {
		t.Fatalf("got %q", excerptTitles(got))
	}
}

func TestExcerptsForTheNewestPutTheNewestChunksOfTheBestMatchFirst(t *testing.T) {
	r := newRig(t)
	teja, chat := r.contact("Tejashree", "")
	r.add(msg{chat: chat, sender: teja, ago: 9000 * time.Minute, body: str("tejashree asked what happened are you fine")})
	r.add(msg{chat: chat, sender: teja, ago: 3000 * time.Minute, body: str("can you send the notes")})
	r.add(msg{chat: chat, sender: teja, ago: 600 * time.Minute, body: str("coming to college tomorrow?")})
	r.say(r.direct, 300, "rahul is newer than all of them")
	r.pass()
	s := r.excerpter()
	got := r.excerpts(s, Plan{Query: "what did tejashree ask", People: []string{"Tejashree"}, Newest: true})
	if len(got) < 2 || got[0].Lines[0].Text != "coming to college tomorrow?" || got[1].Lines[0].Text != "can you send the notes" ||
		!got[0].Newest || !got[1].Newest || len(got) > 2 && got[2].Newest {
		t.Fatalf("got %q", excerptTitles(got))
	}
	got = r.excerpts(s, Plan{Query: "tejashree asked what happened are you fine", People: []string{"Tejashree"}})
	if len(got) == 0 || got[0].Lines[0].Text != "tejashree asked what happened are you fine" || got[0].Newest {
		t.Fatalf("a plan not asking for the newest: %q", excerptTitles(got))
	}
	got = r.excerpts(s, Plan{Query: "new messages", Newest: true})
	if len(got) == 0 || got[0].ChatID != r.direct || !got[0].Newest {
		t.Fatalf("the newest of every chat: %q", excerptTitles(got))
	}
	got = r.excerpts(s, Plan{Query: "new messages", People: []string{"Zorawar"}, Newest: true})
	if slices.ContainsFunc(got, func(e Excerpt) bool { return e.Newest }) {
		t.Fatalf("an unknown name brought the newest of every chat: %q", excerptTitles(got))
	}
}

func TestExcerptsForTheNewestTakeAPersonAndAGroupTogether(t *testing.T) {
	r := newRig(t)
	priya, direct := r.contact("Priya", "")
	gang := r.groupNamed("Priya Birthday")
	other, otherChat := r.contact("Priyanka", "")
	r.add(msg{chat: otherChat, sender: other, ago: 100 * time.Minute, body: str("priyanka is not priya")})
	if _, all, _, err := r.searcher().people(bg, []string{"Priya"}, 0); err != nil || !slices.Contains(all, other) {
		t.Fatalf("priyanka should be a weaker match: %v, %v", all, err)
	}
	r.add(msg{chat: direct, sender: priya, ago: 9000 * time.Minute, body: str("oldest from priya")})
	r.add(msg{chat: gang, sender: r.rahul, ago: 600 * time.Minute, body: str("newest in the group")})
	r.add(msg{chat: direct, sender: priya, ago: 3000 * time.Minute, body: str("newer from priya")})
	r.pass()
	got := r.excerpts(r.excerpter(), Plan{Query: "priya", People: []string{"Priya"}, Groups: []string{"Priya Birthday"}, Newest: true})
	if len(got) < 2 || got[0].Lines[0].Text != "newest in the group" || got[1].Lines[0].Text != "newer from priya" {
		t.Fatalf("got %q", excerptTitles(got))
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

func TestChatTitlesMarkANameTheContactChose(t *testing.T) {
	r := newRig(t)
	id, chat := r.contact("", "Kaya")
	r.add(msg{chat: chat, sender: id, ago: time.Hour, body: str("trust me")})
	r.pass()
	hits, err := r.searcher().Search(bg, "trust me", Filter{ChatIDs: []int64{chat}}, 1)
	if err != nil || len(hits) != 1 || hits[0].Title != "~Kaya" || hits[0].Lines[0].Sender != "~Kaya" {
		t.Fatalf("%+v, %v", hits, err)
	}
}

func TestNamesMatchAsAWholeAsWordsOrInsideAsTheirScriptNeeds(t *testing.T) {
	r := newRig(t)
	priya, _ := r.contact("Priya", "")
	omkar, _ := r.contact("Omkar", "")
	ming, _ := r.contact("王小明", "")
	s := r.searcher()
	for name, want := range map[string]int64{"Priya Sharma": priya, "小明": ming, "omkar": omkar} {
		if best, _, _, err := s.people(bg, []string{name}, 0); err != nil || !slices.Equal(best, []int64{want}) {
			t.Fatalf("%q: best %v, %v", name, best, err)
		}
	}
	if best, _, unknown, err := s.people(bg, []string{"Om"}, 0); err != nil || best != nil || !slices.Equal(unknown, []string{"Om"}) {
		t.Fatalf("a short name matched part of another: %v, %q, %v", best, unknown, err)
	}
	om, _ := r.contact("Om Prakash", "")
	if best, _, _, err := s.people(bg, []string{"om"}, 0); err != nil || !slices.Equal(best, []int64{om}) {
		t.Fatalf("a short name as a whole word: %v, %v", best, err)
	}
}

func TestPeopleNeverMatchDJHimself(t *testing.T) {
	r := newRig(t)
	r.exec("UPDATE contacts SET saved_name = 'Dhananjay' WHERE id = $1", r.me)
	self := r.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.me)
	if best, _, unknown, err := r.searcher().people(bg, []string{"Dhananjay"}, self); err != nil || best != nil || len(unknown) != 1 {
		t.Fatalf("best %v, unknown %q, %v", best, unknown, err)
	}
}

func TestExcerptsLookForAGroupAmongPeopleAndCleanWhatTheModelWrote(t *testing.T) {
	r := newRig(t)
	team := r.groupNamed("Office Team")
	r.say(team, 3000, "standup moved to ten")
	r.pass()
	found, err := r.excerpter().Excerpts(bg, Plan{Query: "standup\x00 moved", People: []string{"Office Team", "nobody\x00here"}}, 3)
	if err != nil || len(found.Excerpts) == 0 || found.Excerpts[0].ChatID != team || !slices.Equal(found.Unknown, []string{"nobody here"}) {
		t.Fatalf("got %q, unknown %q, %v", excerptTitles(found.Excerpts), found.Unknown, err)
	}
}

func TestFindChatFindsAPersonOrAGroupByAnyName(t *testing.T) {
	r := newRig(t)
	_, teja := r.contact("Tejashree Kulkarni", "")
	_, quiet := r.contact("Tejas Old", "")
	r.say(teja, 10, "hi")
	r.say(quiet, 9000, "long ago")
	r.say(r.group, 5, "hello gang")
	s := r.searcher()
	for name, want := range map[string]int64{"tejashri": teja, "college gang": r.group, "GANG": r.group} {
		id, _, ok, err := s.FindChat(bg, name)
		if err != nil || !ok || id != want {
			t.Fatalf("%q: got %d %v %v, want %d", name, id, ok, err, want)
		}
	}
	if _, title, ok, _ := s.FindChat(bg, "college"); !ok || title != "College Gang" {
		t.Fatalf("title %q", title)
	}
	if _, _, ok, err := s.FindChat(bg, "Zorawar"); ok || err != nil {
		t.Fatalf("an unknown name was found: %v", err)
	}
	if _, _, ok, err := s.FindChat(bg, " \n"); ok || err != nil {
		t.Fatal("an empty name was found")
	}
}
