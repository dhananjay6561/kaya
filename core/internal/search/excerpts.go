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
// model. A question that names someone is searched among their chats, where the topic alone is a
// weaker signal, so it may be NamedDistance away.
const (
	ExcerptDistance = 0.6
	NamedDistance   = 0.7
)

// commonWords are words of a question that are never taken for a name, though a contact may be
// called that: question words, pronouns and fillers in English and Hinglish.
var commonWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`what when where which who whom whose why how did does was were has have had the and but
		for from with about said say says tell told ask asked any some this that there their they them she her him his
		you your yours mine our are not can could would should will shall just only also very much more most kya kab kaha
		kahan kaun kisne kis kisko kyu kyun kaise kitna kitne kitni hai hain tha thi the hoga hogi bola boli bole kaha
		kahi baat bata batao bolo raha rahi rahe wala wali wale aur bhi abhi kal aaj parso mera meri mere tera teri tere
		uska uski uske unka unki unke apna apni apne yeh woh voh wo ye isko usko inko unko mujhe tujhe hum tum aap ka ki
		ke ko se me mein par pe tak liye group chat chats message messages msg today yesterday tomorrow week month`) {
		commonWords[w] = true
	}
}

// nameWords are the words of a question that may be someone's name.
func nameWords(question string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(question), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) >= 3 && !commonWords[w] && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// named finds the contacts and group chats a question names: a word of the question that is a
// whole word of a contact's saved, first, pushed or business name, or of a chat's name.
func (s *Searcher) named(ctx context.Context, question string) (contacts, chats []int64, err error) {
	words := nameWords(question)
	if len(words) == 0 {
		return nil, nil, nil
	}
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id FROM contacts c
			WHERE EXISTS (SELECT 1 FROM unnest(regexp_split_to_array(lower(concat_ws(' ', c.saved_name, c.first_name, c.push_name, c.business_name)), '[^[:alnum:]]+')) w
			              WHERE w = ANY($1))
			ORDER BY id LIMIT 20`, words)
		if err != nil {
			return err
		}
		if contacts, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT id FROM chats ch
			WHERE ch.name IS NOT NULL
			  AND EXISTS (SELECT 1 FROM unnest(regexp_split_to_array(lower(ch.name), '[^[:alnum:]]+')) w WHERE w = ANY($1))
			ORDER BY id LIMIT 20`, words)
		if err != nil {
			return err
		}
		chats, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		return err
	})
	return contacts, chats, err
}

// Excerpts finds up to limit chunks of DJ's other chats (never his own chat with Kaya, which
// Recall covers) that bear on question, best first. When the question names people or a group,
// their chats are searched first, and all chats only if that finds nothing close enough.
func (s *Searcher) Excerpts(ctx context.Context, question string, limit int) ([]Hit, error) {
	if limit <= 0 {
		return nil, nil
	}
	var self int64
	err := s.DB.QueryRow(ctx, `
		SELECT q.chat_id FROM exchanges e JOIN messages q ON q.id = e.question_message_id
		ORDER BY e.id DESC LIMIT 1`).Scan(&self)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	contacts, chats, err := s.named(ctx, question)
	if err != nil {
		return nil, err
	}
	type attempt struct {
		filter Filter
		bar    float64
	}
	var tries []attempt
	if len(contacts) > 0 {
		tries = append(tries, attempt{Filter{ContactIDs: contacts}, s.NamedDistance})
	}
	if len(chats) > 0 {
		tries = append(tries, attempt{Filter{ChatIDs: chats}, s.NamedDistance})
	}
	tries = append(tries, attempt{Filter{}, s.ExcerptDistance})
	for _, try := range tries {
		hits, err := s.Search(ctx, question, try.filter, limit+5)
		if err != nil {
			return nil, err
		}
		var out []Hit
		for _, h := range hits {
			if h.ChatID != self && h.Distance <= try.bar && len(out) < limit {
				out = append(out, h)
			}
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, nil
}
