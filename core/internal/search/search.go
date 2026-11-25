package search

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/core/internal/embeddings"
	"github.com/dhananjay6561/kaya/core/internal/memory"
	"github.com/dhananjay6561/kaya/core/internal/text"
	"github.com/dhananjay6561/kaya/kit/store"
)

// Filter narrows a search. Zero values do not filter.
type Filter struct {
	// ChatIDs keeps chunks of these chats.
	ChatIDs []int64
	// ContactIDs keeps chunks where one of these people wrote, or that are their direct chat.
	ContactIDs []int64
	// From and To keep chunks with a message sent between them.
	From, To time.Time
}

// Scored is a chunk and how close it is to what was searched for; lower is closer.
type Scored struct {
	ChunkID  int64
	Distance float64
}

// VectorStore finds the chunks whose vectors are nearest to a query vector.
type VectorStore interface {
	Nearest(ctx context.Context, vector []float32, f Filter, limit int) ([]Scored, error)
}

// PgVectors keeps the vectors in Postgres with pgvector, in the chunks table.
type PgVectors struct {
	DB    *store.DB
	Model string
}

// Nearest walks the HNSW index. Iterative scanning keeps it finding chunks when a filter rules out
// most of the nearest ones, and the result is sorted again because that scan may return them
// slightly out of order. Only chunks made by Model are compared.
func (p *PgVectors) Nearest(ctx context.Context, vector []float32, f Filter, limit int) ([]Scored, error) {
	var out []Scored
	err := p.DB.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL hnsw.iterative_scan = relaxed_order"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL hnsw.ef_search = 100"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT k.id, k.embedding <=> $1::halfvec
			FROM chunks k
			WHERE k.embedding IS NOT NULL AND k.model = $2 AND `+chunkFilter("k", 3)+`
			ORDER BY k.embedding <=> $1::halfvec
			LIMIT $7`, append([]any{vectorText(vector), p.Model}, append(filterArgs(f), limit)...)...)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Scored, error) {
			var s Scored
			err := row.Scan(&s.ChunkID, &s.Distance)
			return s, err
		})
		return err
	})
	slices.SortStableFunc(out, func(a, b Scored) int {
		switch {
		case a.Distance < b.Distance:
			return -1
		case a.Distance > b.Distance:
			return 1
		}
		return 0
	})
	return out, err
}

// chunkFilter is the WHERE clause for a Filter on chunk alias k, with its four arguments starting
// at $n: chat ids, contact ids, from and to.
func chunkFilter(k string, n int) string {
	arg := func(i int) string { return "$" + strconv.Itoa(n+i) }
	return `(` + arg(0) + `::bigint[] IS NULL OR ` + k + `.chat_id = ANY(` + arg(0) + `))
		AND (` + arg(2) + `::timestamptz IS NULL OR ` + k + `.last_sent_at >= ` + arg(2) + `)
		AND (` + arg(3) + `::timestamptz IS NULL OR ` + k + `.first_sent_at <= ` + arg(3) + `)
		AND (` + arg(1) + `::bigint[] IS NULL
		     OR EXISTS (SELECT 1 FROM chats c WHERE c.id = ` + k + `.chat_id AND c.contact_id = ANY(` + arg(1) + `))
		     OR EXISTS (SELECT 1 FROM message_chunks fc JOIN messages fm ON fm.id = fc.message_id
		                WHERE fc.chunk_id = ` + k + `.id AND NOT fm.from_me AND fm.sender_id = ANY(` + arg(1) + `)))`
}

func filterArgs(f Filter) []any {
	var chats, contacts any
	if len(f.ChatIDs) > 0 {
		chats = f.ChatIDs
	}
	if len(f.ContactIDs) > 0 {
		contacts = f.ContactIDs
	}
	var from, to any
	if !f.From.IsZero() {
		from = f.From
	}
	if !f.To.IsZero() {
		to = f.To
	}
	return []any{chats, contacts, from, to}
}

// Hit is one chunk a search found, with its messages, best first.
type Hit struct {
	ChunkID int64
	ChatID  int64
	Title   string
	Lines   []Line
	// Score is the fused rank: higher is better.
	Score float64
	// Distance is the cosine distance of the chunk's vector from the query's, from 0 (the same
	// direction) to 2; it is 2 when the chunk was found by words only.
	Distance float64
	// Meaning and Words say which search found it.
	Meaning, Words bool
}

// Searcher finds chunks by meaning and by words together.
type Searcher struct {
	DB         *store.DB
	Embeddings embeddings.Provider
	Vectors    VectorStore
}

// NewSearcher searches the chunks in db, comparing vectors made by embedder's model.
func NewSearcher(db *store.DB, embedder embeddings.Provider) *Searcher {
	return &Searcher{DB: db, Embeddings: embedder, Vectors: &PgVectors{DB: db, Model: embedder.Model()}}
}

// ErrEmptyQuery refuses a search for nothing.
var ErrEmptyQuery = errors.New("search: nothing to search for")

const (
	candidates = 50
	// fusion is the k of reciprocal rank fusion: a chunk's score is the sum of 1/(k+rank) over the
	// searches that found it, so one found by both beats one found high by only one.
	fusion = 60
)

// Search finds up to limit chunks for query. Meaning comes from the embedding service's vectors,
// words from Postgres full text search over the messages; the two rankings are fused. The query
// goes to the embedding service as it is: it is DJ's own words.
func (s *Searcher) Search(ctx context.Context, query string, f Filter, limit int) ([]Hit, error) {
	if oneLine(query) == "" {
		return nil, ErrEmptyQuery
	}
	embedded, err := s.Embeddings.Embed(ctx, []string{query}, embeddings.Query)
	if err != nil {
		return nil, err
	}
	near, err := s.Vectors.Nearest(ctx, embedded.Vectors[0], f, candidates)
	if err != nil {
		return nil, err
	}
	words, err := s.words(ctx, query, f)
	if err != nil {
		return nil, err
	}
	hits := map[int64]*Hit{}
	add := func(id int64, rank int, byMeaning bool) {
		h, ok := hits[id]
		if !ok {
			h = &Hit{ChunkID: id, Distance: 2}
			hits[id] = h
		}
		h.Score += 1 / float64(fusion+rank+1)
		if byMeaning {
			h.Meaning = true
		} else {
			h.Words = true
		}
	}
	for i, n := range near {
		add(n.ChunkID, i, true)
		hits[n.ChunkID].Distance = n.Distance
	}
	for i, id := range words {
		add(id, i, false)
	}
	ranked := make([]*Hit, 0, len(hits))
	for _, h := range hits {
		ranked = append(ranked, h)
	}
	slices.SortFunc(ranked, func(a, b *Hit) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return int(a.ChunkID - b.ChunkID)
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]Hit, 0, len(ranked))
	for _, h := range ranked {
		lines, err := readLines(ctx, s.DB, linesSQL+`
			AND m.id IN (SELECT message_id FROM message_chunks WHERE chunk_id = $1)
			ORDER BY m.sent_at, m.id`, h.ChunkID)
		if err != nil {
			return nil, err
		}
		if len(lines) == 0 {
			continue
		}
		h.ChatID, h.Title = lines[0].ChatID, lines[0].title
		for _, l := range lines {
			h.Lines = append(h.Lines, l.Line)
		}
		out = append(out, *h)
	}
	return out, nil
}

// words ranks chunks by how well their messages match the query's words.
func (s *Searcher) words(ctx context.Context, query string, f Filter) ([]int64, error) {
	var out []int64
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT k.id
			FROM websearch_to_tsquery('simple', $1) q,
			     messages m
			JOIN message_chunks mc ON mc.message_id = m.id
			JOIN chunks k ON k.id = mc.chunk_id
			WHERE to_tsvector('simple', coalesce(m.body, '')) @@ q AND m.deleted_at IS NULL AND `+chunkFilter("k", 2)+`
			GROUP BY k.id
			ORDER BY max(ts_rank(to_tsvector('simple', coalesce(m.body, '')), q)) DESC, k.id
			LIMIT $6`, append([]any{query}, append(filterArgs(f), candidates)...)...)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		return err
	})
	return out, err
}

// RecallDistance is how far in meaning an old exchange found by meaning alone may be from the
// question and still be recalled.
const RecallDistance = 0.6

// Recall finds up to limit past exchanges between DJ and Kaya that match question, leaving out
// the latest recent ones (which the conversation memory already carries), oldest first.
func (s *Searcher) Recall(ctx context.Context, question string, recent, limit int) ([]memory.Exchange, error) {
	var self int64
	err := s.DB.QueryRow(ctx, `
		SELECT q.chat_id FROM exchanges e JOIN messages q ON q.id = e.question_message_id
		ORDER BY e.id DESC LIMIT 1`).Scan(&self)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	hits, err := s.Search(ctx, question, Filter{ChatIDs: []int64{self}}, 10)
	if err != nil {
		return nil, err
	}
	rank := map[int64]int{}
	var ids []int64
	for i, h := range hits {
		// The nearest chunks always come back, related or not; an old exchange must share words
		// with the question or be close in meaning to be worth the model's room.
		if !h.Words && h.Distance > RecallDistance {
			continue
		}
		for _, l := range h.Lines {
			if _, seen := rank[l.MessageID]; !seen {
				rank[l.MessageID] = i
				ids = append(ids, l.MessageID)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	type found struct {
		memory.Exchange
		best int
	}
	var all []found
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT q.id, a.id, q.body, a.body, q.sent_at
			FROM exchanges e
			JOIN messages q ON q.id = e.question_message_id
			JOIN messages a ON a.chat_id = q.chat_id AND a.wa_id = e.answer_wa_id AND a.from_me
			WHERE (q.id = ANY($1) OR a.id = ANY($1))
			  AND q.deleted_at IS NULL AND a.deleted_at IS NULL AND q.body IS NOT NULL AND a.body IS NOT NULL
			  AND (q.edited_at IS NULL OR q.edited_at <= e.created_at)
			  AND e.id < (SELECT coalesce(min(id), 0) FROM (SELECT id FROM exchanges ORDER BY id DESC LIMIT $2) latest)`,
			ids, recent)
		if err != nil {
			return err
		}
		all, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (found, error) {
			var f found
			var q, a int64
			if err := row.Scan(&q, &a, &f.Question, &f.Answer, &f.At); err != nil {
				return f, err
			}
			f.Question = text.Trim(f.Question)
			f.best = min(rankOr(rank, q), rankOr(rank, a))
			return f, nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(all, func(a, b found) int { return a.best - b.best })
	if len(all) > limit {
		all = all[:limit]
	}
	slices.SortFunc(all, func(a, b found) int { return a.At.Compare(b.At) })
	out := make([]memory.Exchange, len(all))
	for i, f := range all {
		out[i] = f.Exchange
	}
	return out, nil
}

func rankOr(rank map[int64]int, id int64) int {
	if r, ok := rank[id]; ok {
		return r
	}
	return int(^uint(0) >> 1)
}
