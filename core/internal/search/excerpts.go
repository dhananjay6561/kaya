package search

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dhananjay6561/kaya/kit/store"
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

// Limits on a plan, which a model writes: how many names it may give, how long each may be, and how
// many contacts or groups one name may stand for.
const (
	MaxNames    = 5
	maxNameLen  = 80
	perName     = 3
	maxQueryLen = 500
)

// newest is how many of the latest chunks come first when a plan asks for the newest messages.
const newest = 2

// Plan is what to search DJ's other chats for, worked out from his question by a model, so the
// search understands any language and any phrasing: what the question is about, the people and
// groups it names (pronouns already resolved), whether it asks for the newest messages, and the
// time it covers.
type Plan struct {
	Query  string
	People []string
	Groups []string
	Newest bool
	From   time.Time
	To     time.Time
}

// Excerpt is a chunk found for a question, with the text of the messages just before it, which
// masking needs to tell a code from any number.
type Excerpt struct {
	Hit
	Before string
	// Newest says the chunk is among the newest of the people or groups the plan names (or of all
	// chats when it names none), put first because the question asked for the newest messages.
	Newest bool
}

// Found is what a search for a plan found, and the names in it that match no contact or group.
type Found struct {
	Excerpts []Excerpt
	Unknown  []string
}

// contactName is the name DJ saved for a contact or, only when he saved none, the names they chose
// for themselves, lower-cased, so nobody can steer the search toward their chats with a name of
// their choosing. The trigram index on contacts is built on the same expression.
const contactName = `lower(CASE WHEN coalesce(c.saved_name, '') = '' AND coalesce(c.first_name, '') = ''
	THEN coalesce(c.push_name, '') || ' ' || coalesce(c.business_name, '')
	ELSE coalesce(c.saved_name, '') || ' ' || coalesce(c.first_name, '') END)`

// people finds the contacts each name stands for (never self, DJ's own chat with Kaya), up to
// perName contacts with a chat or a message, the closest names first and, among names as close, the
// people DJ talked to most recently. See match for how a name matches.
func (s *Searcher) people(ctx context.Context, names []string, self int64) (best, all []int64, unknown []string, err error) {
	return s.resolve(ctx, names, func(condition, similarity string) string {
		return `
			SELECT c.id FROM contacts c
			WHERE ` + strings.ReplaceAll(condition, "NAME", contactName) + `
			  AND (EXISTS (SELECT 1 FROM chats ch WHERE ch.contact_id = c.id)
			       OR EXISTS (SELECT 1 FROM messages m WHERE m.sender_id = c.id))
			  AND NOT EXISTS (SELECT 1 FROM chats sc WHERE sc.id = $3 AND sc.contact_id = c.id)
			ORDER BY round((` + strings.ReplaceAll(similarity, "NAME", contactName) + `)::numeric, 1) DESC,
			         greatest((SELECT max(m.sent_at) FROM messages m WHERE m.sender_id = c.id),
			                  (SELECT max(m.sent_at) FROM chats ch JOIN messages m ON m.chat_id = ch.id WHERE ch.contact_id = c.id)) DESC NULLS LAST,
			         c.id
			LIMIT $2`
	}, self)
}

// groups finds the group chats each name stands for, like people.
func (s *Searcher) groups(ctx context.Context, names []string) (best, all []int64, unknown []string, err error) {
	const groupName = `lower(coalesce(ch.name, ''))`
	return s.resolve(ctx, names, func(condition, similarity string) string {
		return `
			SELECT ch.id FROM chats ch
			WHERE ch.kind = 'group' AND ch.id <> $3 AND ` + strings.ReplaceAll(condition, "NAME", groupName) + `
			ORDER BY round((` + strings.ReplaceAll(similarity, "NAME", groupName) + `)::numeric, 1) DESC,
			         (SELECT max(m.sent_at) FROM messages m WHERE m.chat_id = ch.id) DESC NULLS LAST,
			         ch.id
			LIMIT $2`
	}, 0)
}

// How a name is matched against the names of contacts and groups (NAME stands for the name
// expression): a short name only as a whole word, so "Om" is not Omkar; a longer one by trigram
// word similarity, so misspellings and any alphabet match; and, for a script written without
// spaces, as part of a name, so 小明 finds 王小明.
const (
	wholeWord   = `lower($1) = ANY(regexp_split_to_array(NAME, '[[:space:][:punct:]]+'))`
	similar     = `lower($1) <% NAME`
	inside      = `strpos(NAME, lower($1)) > 0`
	similarity  = `word_similarity(lower($1), NAME)`
	exactly     = `1`
	shortName   = 3
	wordsOfName = 2
)

// resolve finds what each name stands for with the query query writes for a condition and an order
// of similarity. A name made of several words that matches nothing as a whole is tried word by word
// ("Priya Sharma" finds a contact saved as "Priya").
func (s *Searcher) resolve(ctx context.Context, names []string, query func(condition, similarity string) string, self int64) (best, all []int64, unknown []string, err error) {
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		find := func(name string) ([]int64, error) {
			ways := [][2]string{{similar, similarity}}
			if len([]rune(name)) <= shortName {
				ways = [][2]string{{wholeWord, exactly}}
			}
			if !isASCII(name) {
				ways = append(ways, [2]string{inside, exactly})
			}
			for _, way := range ways {
				rows, err := tx.Query(ctx, query(way[0], way[1]), name, perName, self)
				if err != nil {
					return nil, err
				}
				ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
				if err != nil || len(ids) > 0 {
					return ids, err
				}
			}
			return nil, nil
		}
		for _, name := range names {
			ids, err := find(name)
			if err != nil {
				return err
			}
			if words := strings.Fields(name); len(ids) == 0 && len(words) >= wordsOfName {
				for _, w := range words {
					if ids, err = find(w); err != nil || len(ids) > 0 {
						break
					}
				}
				if err != nil {
					return err
				}
			}
			if len(ids) == 0 {
				unknown = append(unknown, name)
				continue
			}
			if !slices.Contains(best, ids[0]) {
				best = append(best, ids[0])
			}
			for _, id := range ids {
				if !slices.Contains(all, id) {
					all = append(all, id)
				}
			}
		}
		return nil
	})
	return best, all, unknown, err
}

func isASCII(s string) bool {
	for _, r := range s {
		if r >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// clean is text a model wrote, on one line, without control characters, cut to limit.
func clean(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	return cut(oneLine(text), limit)
}

// cleanNames keeps up to MaxNames distinct names of a plan, each cleaned and cut to maxNameLen.
func cleanNames(names []string) []string {
	var out []string
	for _, n := range names {
		n = clean(n, maxNameLen)
		if n != "" && len(out) < MaxNames && !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, n) }) {
			out = append(out, n)
		}
	}
	return out
}

func (s *Searcher) selfChat(ctx context.Context) (int64, error) { return SelfChat(ctx, s.DB) }

// SelfChat is DJ's own chat with Kaya: the direct chat with the account the gateway is linked to
// or, while it is not linked (logged out), the chat of the latest exchange. It is zero before
// either is known.
func SelfChat(ctx context.Context, db *store.DB) (int64, error) {
	var id int64
	err := db.QueryRow(ctx, `
		SELECT id FROM (
			(SELECT ch.id, 0 AS rank FROM connection_state cs
			 JOIN contacts c ON cs.own_jid IN (c.pn, c.lid)
			 JOIN chats ch ON ch.kind = 'direct' AND ch.contact_id = c.id
			 WHERE cs.own_jid IS NOT NULL
			 ORDER BY cs.occurred_at DESC LIMIT 1)
			UNION ALL
			(SELECT m.chat_id, 1 FROM exchanges e JOIN messages m ON m.id = e.question_message_id
			 ORDER BY e.id DESC LIMIT 1)) known
		ORDER BY rank LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// Excerpts finds up to limit chunks of DJ's other chats (never his own chat with Kaya, which
// Recall covers) for a plan, best first, within the plan's time. Chunks of the people and groups it
// names are searched alongside every chat, with a looser bar and a head start, so a name that
// matches by chance cannot hide a better answer elsewhere. When the plan asks for the newest
// messages, the newest chunks of the best match for each name (or of all chats, when it names
// nobody) come first, whatever they are about, since meaning cannot tell new from old.
func (s *Searcher) Excerpts(ctx context.Context, p Plan, limit int) (Found, error) {
	if limit <= 0 {
		return Found{}, nil
	}
	query := clean(p.Query, maxQueryLen)
	people, groups := cleanNames(p.People), cleanNames(p.Groups)
	if query == "" {
		query = strings.Join(append(slices.Clone(people), groups...), " ")
	}
	self, err := s.selfChat(ctx)
	if err != nil {
		return Found{}, err
	}
	window := Filter{From: p.From, To: p.To}
	if self != 0 {
		window.ExcludeChatIDs = []int64{self}
	}
	bestContacts, contacts, unknownPeople, err := s.people(ctx, people, self)
	if err != nil {
		return Found{}, err
	}
	bestChats, chats, unknownGroups, err := s.groups(ctx, groups)
	if err != nil {
		return Found{}, err
	}
	// A model may put a group among people or the other way round, so a name that matches neither
	// kind it was given is looked for as the other.
	moreChats, chatsToo, unknownPeople, err := s.groups(ctx, unknownPeople)
	if err != nil {
		return Found{}, err
	}
	moreContacts, contactsToo, unknownGroups, err := s.people(ctx, unknownGroups, self)
	if err != nil {
		return Found{}, err
	}
	bestContacts, contacts = merge(bestContacts, moreContacts), merge(contacts, contactsToo)
	bestChats, chats = merge(bestChats, moreChats), merge(chats, chatsToo)
	found := Found{Unknown: append(unknownPeople, unknownGroups...)}

	type candidate struct {
		hit    Hit
		score  float64
		newest bool
	}
	best := map[int64]candidate{}
	consider := func(f Filter, bar, bonus float64) error {
		hits, err := s.Search(ctx, query, f, limit+5)
		if err != nil {
			return err
		}
		for _, h := range hits {
			if h.Distance > bar {
				continue
			}
			if prev, ok := best[h.ChunkID]; !ok || h.Distance-bonus < prev.score {
				best[h.ChunkID] = candidate{hit: h, score: h.Distance - bonus}
			}
		}
		return nil
	}
	if query != "" {
		if len(contacts) > 0 {
			f := window
			f.ContactIDs = contacts
			if err := consider(f, s.NamedDistance, namedBonus); err != nil {
				return Found{}, err
			}
		}
		if len(chats) > 0 {
			f := window
			f.ChatIDs = chats
			if err := consider(f, s.NamedDistance, namedBonus); err != nil {
				return Found{}, err
			}
		}
		if err := consider(window, s.ExcerptDistance, 0); err != nil {
			return Found{}, err
		}
	}
	if p.Newest {
		var filters []Filter
		for _, id := range bestContacts {
			f := window
			f.ContactIDs = []int64{id}
			filters = append(filters, f)
		}
		if len(bestChats) > 0 {
			f := window
			f.ChatIDs = bestChats
			filters = append(filters, f)
		}
		if len(people)+len(groups) == 0 {
			filters = append(filters, window)
		}
		var latest []Hit
		for _, f := range filters {
			hits, err := s.Latest(ctx, f, newest)
			if err != nil {
				return Found{}, err
			}
			latest = append(latest, hits...)
		}
		slices.SortStableFunc(latest, func(a, b Hit) int {
			return b.Lines[len(b.Lines)-1].SentAt.Compare(a.Lines[len(a.Lines)-1].SentAt)
		})
		taken := 0
		for _, h := range latest {
			if taken == newest {
				break
			}
			if prev, ok := best[h.ChunkID]; ok && prev.newest {
				continue
			}
			best[h.ChunkID] = candidate{hit: h, score: -10 + float64(taken), newest: true}
			taken++
		}
	}
	all := make([]candidate, 0, len(best))
	for _, c := range best {
		all = append(all, c)
	}
	slices.SortFunc(all, func(a, b candidate) int {
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
	for _, c := range all {
		before, err := s.before(ctx, c.hit.ChatID, c.hit.Lines[0])
		if err != nil {
			return Found{}, err
		}
		found.Excerpts = append(found.Excerpts, Excerpt{Hit: c.hit, Before: before, Newest: c.newest})
	}
	return found, nil
}

func merge(a, b []int64) []int64 {
	for _, id := range b {
		if !slices.Contains(a, id) {
			a = append(a, id)
		}
	}
	return a
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
