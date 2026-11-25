package search

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
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
	// ExcludeChatIDs leaves out chunks of these chats.
	ExcludeChatIDs []int64
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
			LIMIT $8`, append([]any{vectorText(vector), p.Model}, append(filterArgs(f), limit)...)...)
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

// chunkFilter is the WHERE clause for a Filter on chunk alias k, with its five arguments starting
// at $n: chat ids, contact ids, from, to and chat ids to leave out.
func chunkFilter(k string, n int) string {
	arg := func(i int) string { return "$" + strconv.Itoa(n+i) }
	return `(` + arg(0) + `::bigint[] IS NULL OR ` + k + `.chat_id = ANY(` + arg(0) + `))
		AND (` + arg(4) + `::bigint[] IS NULL OR NOT ` + k + `.chat_id = ANY(` + arg(4) + `))
		AND (` + arg(2) + `::timestamptz IS NULL OR ` + k + `.last_sent_at >= ` + arg(2) + `)
		AND (` + arg(3) + `::timestamptz IS NULL OR ` + k + `.first_sent_at <= ` + arg(3) + `)
		AND (` + arg(1) + `::bigint[] IS NULL
		     OR EXISTS (SELECT 1 FROM chats c WHERE c.id = ` + k + `.chat_id AND c.contact_id = ANY(` + arg(1) + `))
		     OR EXISTS (SELECT 1 FROM message_chunks fc JOIN messages fm ON fm.id = fc.message_id
		                WHERE fc.chunk_id = ` + k + `.id AND NOT fm.from_me AND fm.deleted_at IS NULL AND fm.sender_id = ANY(` + arg(1) + `)))`
}

func filterArgs(f Filter) []any {
	var chats, contacts any
	if len(f.ChatIDs) > 0 {
		chats = f.ChatIDs
	}
	if len(f.ContactIDs) > 0 {
		contacts = f.ContactIDs
	}
	var from, to, exclude any
	if !f.From.IsZero() {
		from = f.From
	}
	if !f.To.IsZero() {
		to = f.To
	}
	if len(f.ExcludeChatIDs) > 0 {
		exclude = f.ExcludeChatIDs
	}
	return []any{chats, contacts, from, to, exclude}
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
	// RecallDistance, ExcerptDistance and NamedDistance are the bars Recall and Excerpts hold
	// chunks to; NewSearcher sets the constants of the same names.
	RecallDistance, ExcerptDistance, NamedDistance float64

	mu      sync.Mutex
	queries map[string]embedded
}

// NewSearcher searches the chunks in db, comparing vectors made by embedder's model.
func NewSearcher(db *store.DB, embedder embeddings.Provider) *Searcher {
	return &Searcher{
		DB: db, Embeddings: embedder, Vectors: &PgVectors{DB: db, Model: embedder.Model()},
		RecallDistance: RecallDistance, ExcerptDistance: ExcerptDistance, NamedDistance: NamedDistance,
	}
}

// queryTTL is how long a query's vector, or its failure, is kept: a question's recall and excerpts
// search with the same words, so it is embedded once, and a service that just failed is not asked
// again within the same reply.
const queryTTL = time.Minute

type embedded struct {
	vector []float32
	err    error
	at     time.Time
}

func (s *Searcher) embed(ctx context.Context, query string) ([]float32, error) {
	s.mu.Lock()
	if e, ok := s.queries[query]; ok && time.Since(e.at) < queryTTL {
		s.mu.Unlock()
		return e.vector, e.err
	}
	s.mu.Unlock()
	result, err := s.Embeddings.Embed(ctx, []string{query}, embeddings.Query)
	var vector []float32
	if err == nil {
		vector = result.Vectors[0]
	}
	if ctx.Err() != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queries == nil || len(s.queries) > 64 {
		s.queries = map[string]embedded{}
	}
	s.queries[query] = embedded{vector: vector, err: err, at: time.Now()}
	return vector, err
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
	vector, err := s.embed(ctx, query)
	if err != nil {
		return nil, err
	}
	near, err := s.Vectors.Nearest(ctx, vector, f, candidates)
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
	out := make([]Hit, 0, max(0, limit))
	for _, h := range ranked {
		if len(out) >= limit {
			break
		}
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
			LIMIT $7`, append([]any{query}, append(filterArgs(f), candidates)...)...)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		return err
	})
	return out, err
}

// RecallDistance is how far in meaning a chunk may be from the question and still have its
// exchanges recalled. The nearest chunks always come back, related or not, and even a shared word
// like "ok" is no sign of relevance, so the bar applies to every chunk.
const RecallDistance = 0.6

// Recall finds up to limit past exchanges between DJ and Kaya that bear on question, best first.
// It leaves out exclude (the questions' message ids, as the recent conversation already carries
// them) and looks only at chunks that start before before, when it is set, so a recent conversation
// on the same topic does not crowd out the older one. Within a chunk, exchanges whose own words
// match the question come first.
func (s *Searcher) Recall(ctx context.Context, question string, exclude []int64, before time.Time, limit int) ([]memory.Exchange, error) {
	if limit <= 0 {
		return nil, nil
	}
	self, err := s.selfChat(ctx)
	if err != nil || self == 0 {
		return nil, err
	}
	hits, err := s.Search(ctx, question, Filter{ChatIDs: []int64{self}, To: before}, 10)
	if err != nil {
		return nil, err
	}
	chunkRank := map[int64]int{}
	var ids []int64
	for i, h := range hits {
		if h.Distance > s.RecallDistance {
			continue
		}
		for _, l := range h.Lines {
			if _, seen := chunkRank[l.MessageID]; !seen {
				chunkRank[l.MessageID] = i
				ids = append(ids, l.MessageID)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	type found struct {
		memory.Exchange
		chunk, exchange int
		words           bool
		rank            float64
	}
	var all []found
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT e.id, q.id, a.id, q.body, a.body, q.sent_at,
			       to_tsvector('simple', q.body || ' ' || a.body) @@ websearch_to_tsquery('simple', $3),
			       ts_rank(to_tsvector('simple', q.body || ' ' || a.body), websearch_to_tsquery('simple', $3))
			FROM exchanges e
			JOIN messages q ON q.id = e.question_message_id
			JOIN messages a ON a.chat_id = q.chat_id AND a.wa_id = e.answer_wa_id AND a.from_me
			WHERE (q.id = ANY($1) OR a.id = ANY($1)) AND NOT q.id = ANY($2)
			  AND q.deleted_at IS NULL AND a.deleted_at IS NULL AND q.body IS NOT NULL AND a.body IS NOT NULL
			  AND (q.edited_at IS NULL OR q.edited_at <= e.created_at)`,
			ids, append([]int64{}, exclude...), question)
		if err != nil {
			return err
		}
		all, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (found, error) {
			var f found
			var exchangeID, a int64
			var rank float32
			if err := row.Scan(&exchangeID, &f.ID, &a, &f.Question, &f.Answer, &f.At, &f.words, &rank); err != nil {
				return f, err
			}
			f.Question = text.Trim(f.Question)
			f.chunk = min(rankOr(chunkRank, f.ID), rankOr(chunkRank, a))
			f.exchange, f.rank = int(exchangeID), float64(rank)
			return f, nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(all, func(a, b found) int {
		switch {
		case a.chunk != b.chunk:
			return a.chunk - b.chunk
		case a.words != b.words:
			if a.words {
				return -1
			}
			return 1
		case a.rank != b.rank:
			if a.rank > b.rank {
				return -1
			}
			return 1
		}
		return b.exchange - a.exchange
	})
	if len(all) > limit {
		all = all[:limit]
	}
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
