package search

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// ExcerptDistance is how far in meaning a chunk may be from the question and still be shown to the
// model. A chunk of a person or group the question names may be NamedDistance away, since inside
// their chats the topic alone is a weaker signal.
const (
	ExcerptDistance = 0.6
	NamedDistance   = 0.7
)

// namedBonus is how much closer a chunk of a person or group the question names counts when named
// and unnamed results are merged.
const namedBonus = 0.1

// commonWords are words of a question that are never taken for a name or counted as its content,
// though a contact may be called that: question words, pronouns, fillers, greetings and words that
// people add to names in an address book, in English and Hinglish.
var commonWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`what whats when where which who whom whose why how did does was were has have had the and but
		for from with about said say says tell told ask asked any some this that there their they them she her him his
		you your yours mine our are not can could would should will shall just only also very much more most anyone
		someone something anything everyone everything mention mentioned remember thanks thank please pls okay hey hello
		hii hiii cool nice good great yes yeah yep nope new old number num phone call called time times plan plans party
		ignore last next first today yesterday tomorrow week weeks month months year years day days morning night evening
		message messages msg msgs chat chats group groups sent send sending share shared talk talked talking discuss
		kya kab kaha kahan kaun kisne kis kisko kyu kyun kaise kitna kitne kitni hai hain tha thi the hoga hogi bola boli
		bole kahi baat bata batao bataya bataye bolo raha rahi rahe wala wali wale aur bhi abhi kal aaj parso mera meri
		mere tera teri tere uska uski uske unka unki unke apna apni apne yeh woh voh wo ye isko usko inko unko mujhe tujhe
		hum tum aap ka ki ke ko se me mein par pe tak liye bhai bhaiya yaar yar kuch nahi nhi haan han acha accha achha
		thik theek sab log logo koi kisi sir madam bro dude ji jee bhej bhejo bheja bheji diya diye kiya kiye kar karo
		krna karna hua hui hue gaya gayi gaye dekh dekho suno sun क्या कहा कहाँ कहां कब कौन किसने किसको क्यों कैसे
		कितना है हैं था थी थे और भी बोला बोली बोले बताया बताओ बताना मेरा मेरी तेरा उसका उसकी आज कल`) {
		commonWords[w] = true
	}
}

// contentWords are the words of a question that carry meaning: at least three characters (a mark
// such as a Devanagari vowel sign counts with its letter) and not a common word.
func contentWords(question string) []string {
	split := func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.Is(unicode.Mn, r) && !unicode.Is(unicode.Mc, r)
	}
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(question), split) {
		if len([]rune(w)) >= 3 && !commonWords[w] && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

const contactNames = `to_tsvector('simple', coalesce(c.saved_name, '') || ' ' || coalesce(c.first_name, '') || ' ' ||
	coalesce(c.push_name, '') || ' ' || coalesce(c.business_name, ''))`

// named finds who a question names: contacts with a chat or a message, and group chats, whose
// names have the most of the question's words, at most 20 of each.
func (s *Searcher) named(ctx context.Context, words []string) (contacts, chats []int64, err error) {
	if len(words) == 0 {
		return nil, nil, nil
	}
	query := strings.Join(words, " | ")
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.id FROM contacts c, to_tsquery('simple', $1) q
			WHERE `+contactNames+` @@ q
			  AND (EXISTS (SELECT 1 FROM chats ch WHERE ch.contact_id = c.id)
			       OR EXISTS (SELECT 1 FROM messages m WHERE m.sender_id = c.id))
			ORDER BY ts_rank(`+contactNames+`, q) DESC, c.id
			LIMIT 20`, query)
		if err != nil {
			return err
		}
		if contacts, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT ch.id FROM chats ch, to_tsquery('simple', $1) q
			WHERE ch.kind = 'group' AND to_tsvector('simple', coalesce(ch.name, '')) @@ q
			ORDER BY ts_rank(to_tsvector('simple', coalesce(ch.name, '')), q) DESC, ch.id
			LIMIT 20`, query)
		if err != nil {
			return err
		}
		chats, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		return err
	})
	return contacts, chats, err
}

// selfChat is DJ's own chat with Kaya: the direct chat with the account the gateway is linked to.
// It is zero before the gateway has said which account that is.
func (s *Searcher) selfChat(ctx context.Context) (int64, error) {
	var id int64
	err := s.DB.QueryRow(ctx, `
		SELECT ch.id FROM connection_state cs
		JOIN contacts c ON cs.own_jid IN (c.pn, c.lid)
		JOIN chats ch ON ch.kind = 'direct' AND ch.contact_id = c.id
		WHERE cs.own_jid IS NOT NULL
		ORDER BY cs.occurred_at DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// Excerpt is a chunk found for a question, with the text of the messages just before it, which
// masking needs to tell a code from any number.
type Excerpt struct {
	Hit
	Before string
}

// Excerpts finds up to limit chunks of DJ's other chats (never his own chat with Kaya, which
// Recall covers) that bear on question, best first, and says whether it searched: a question with
// no words of substance ("hi", "thanks") is not searched. Chunks of the people or groups the question names are searched alongside
// every chat, with a looser bar and a head start, so a name that matches by chance cannot hide a
// better answer elsewhere.
func (s *Searcher) Excerpts(ctx context.Context, question string, limit int) ([]Excerpt, bool, error) {
	words := contentWords(question)
	if limit <= 0 || len(words) == 0 {
		return nil, false, nil
	}
	self, err := s.selfChat(ctx)
	if err != nil {
		return nil, true, err
	}
	var exclude []int64
	if self != 0 {
		exclude = []int64{self}
	}
	contacts, chats, err := s.named(ctx, words)
	if err != nil {
		return nil, true, err
	}
	type found struct {
		hit   Hit
		score float64
	}
	best := map[int64]found{}
	consider := func(f Filter, bar, bonus float64) error {
		f.ExcludeChatIDs = exclude
		hits, err := s.Search(ctx, question, f, limit+5)
		if err != nil {
			return err
		}
		for _, h := range hits {
			if h.Distance > bar {
				continue
			}
			if prev, ok := best[h.ChunkID]; !ok || h.Distance-bonus < prev.score {
				best[h.ChunkID] = found{hit: h, score: h.Distance - bonus}
			}
		}
		return nil
	}
	if len(contacts) > 0 {
		if err := consider(Filter{ContactIDs: contacts}, s.NamedDistance, namedBonus); err != nil {
			return nil, true, err
		}
	}
	if len(chats) > 0 {
		if err := consider(Filter{ChatIDs: chats}, s.NamedDistance, namedBonus); err != nil {
			return nil, true, err
		}
	}
	if err := consider(Filter{}, s.ExcerptDistance, 0); err != nil {
		return nil, true, err
	}
	all := make([]found, 0, len(best))
	for _, f := range best {
		all = append(all, f)
	}
	slices.SortFunc(all, func(a, b found) int {
		switch {
		case a.score < b.score:
			return -1
		case a.score > b.score:
			return 1
		}
		return int(a.hit.ChunkID - b.hit.ChunkID)
	})
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]Excerpt, len(all))
	for i, f := range all {
		before, err := s.before(ctx, f.hit.ChatID, f.hit.Lines[0])
		if err != nil {
			return nil, true, err
		}
		out[i] = Excerpt{Hit: f.hit, Before: before}
	}
	return out, true, nil
}

// before is the text of the three messages before first in its chat, as the indexer reads it.
func (s *Searcher) before(ctx context.Context, chatID int64, first Line) (string, error) {
	var text *string
	err := s.DB.QueryRow(ctx, `
		SELECT string_agg(body, E'\n') FROM (
			SELECT m.body FROM messages m WHERE m.chat_id = $1 AND (m.sent_at, m.id) < ($2, $3) AND `+eligible+`
			ORDER BY m.sent_at DESC, m.id DESC LIMIT 3) recent`, chatID, first.SentAt, first.MessageID).Scan(&text)
	if err != nil || text == nil {
		return "", err
	}
	return *text, nil
}
