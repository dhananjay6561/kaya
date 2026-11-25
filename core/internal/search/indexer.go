package search

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/dhananjay6561/kaya/core/internal/embeddings"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/masking"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/kit/logging"
	"github.com/dhananjay6561/kaya/kit/store"
)

// IndexerOptions configure the indexer. Zero values take the defaults.
type IndexerOptions struct {
	DB         *store.DB
	Embeddings embeddings.Provider
	Pause      pause.Switch
	Logger     *slog.Logger
	Zone       *time.Location
	Shape      Shape

	// Settle is how old a message must be, by when it was sent and when it was stored, before it is
	// indexed, so a conversation is chunked once it has paused and late history has landed.
	Settle time.Duration
	// BatchMessages is the most messages one pass reads.
	BatchMessages int
	// Poll is how long the indexer waits when there is nothing to do or Kaya is paused.
	Poll time.Duration
	// RetryMin and RetryMax bound the wait after a failure, which doubles each time.
	RetryMin time.Duration
	RetryMax time.Duration
	// Refused is how long indexing waits after the embedding service refuses the key, or the
	// database refuses what would be written: trying again at once would only fail again.
	Refused time.Duration
	// MaxTokens caps the estimated tokens one embedding call carries.
	MaxTokens int
}

// Progress is how much of the chat history is searchable. Skipped counts messages in chunks the
// embedding service would not take; they are in Indexed too.
type Progress struct {
	Indexed int
	Skipped int
	Total   int
}

// Indexer keeps the search index in step with the chats: one worker across every core process,
// under an advisory lock, turns settled messages into chunks and embeds them, and makes a chunk
// again when one of its messages changes.
type Indexer struct {
	o    IndexerOptions
	lock *store.Lock
	wake chan struct{}
	// batch is how many chunks one embedding call carries; it halves after a rate limit and grows
	// back after each call that goes through.
	batch int
	// probed is set once a pass has checked that the service takes requests at all.
	probed bool

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
}

const indexerLock = 5011604

var errPaused = errors.New("kaya is paused")

// NewIndexer returns a stopped indexer.
func NewIndexer(o IndexerOptions) *Indexer {
	if o.Shape == (Shape{}) {
		o.Shape = DefaultShape
	}
	if o.Settle == 0 {
		o.Settle = 10 * time.Minute
	}
	if o.BatchMessages == 0 {
		o.BatchMessages = 2000
	}
	if o.Poll == 0 {
		o.Poll = time.Minute
	}
	if o.RetryMin == 0 {
		o.RetryMin = 5 * time.Second
	}
	if o.RetryMax == 0 {
		o.RetryMax = 5 * time.Minute
	}
	if o.Refused == 0 {
		o.Refused = 10 * time.Minute
	}
	if o.MaxTokens == 0 {
		o.MaxTokens = 100_000
	}
	return &Indexer{o: o, lock: store.NewLock(o.DB, indexerLock), wake: make(chan struct{}, 1), batch: o.Embeddings.MaxBatch()}
}

// Start begins indexing in the background.
func (ix *Indexer) Start() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.running {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	ix.cancel, ix.done, ix.running = cancel, done, true
	go func() {
		defer close(done)
		ix.run(ctx)
	}()
}

// Stop ends indexing and waits for the worker. What it was embedding is left for next time.
func (ix *Indexer) Stop() {
	ix.mu.Lock()
	cancel, done := ix.cancel, ix.done
	ix.running = false
	ix.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Wake makes the worker look for work now instead of at its next poll. It never blocks.
func (ix *Indexer) Wake() {
	select {
	case ix.wake <- struct{}{}:
	default:
	}
}

func (ix *Indexer) sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-ix.wake:
	case <-timer.C:
	}
}

func (ix *Indexer) run(ctx context.Context) {
	defer ix.lock.Release()
	delay := ix.o.RetryMin
	failing := false
	for ctx.Err() == nil {
		n := 0
		err := ix.lock.Hold(ctx)
		if err == nil {
			n, err = ix.pass(ctx)
			if err == nil && n > 0 {
				ix.o.Logger.Info("messages added to the search index", "messages", n)
			}
		}
		if ctx.Err() != nil {
			return
		}
		var refused *embeddings.Error
		switch {
		case err == nil:
			if failing {
				ix.o.Logger.Info("indexing works again")
			}
			failing, delay = false, ix.o.RetryMin
			if n == 0 {
				ix.sleep(ctx, ix.o.Poll)
			}
		case errors.Is(err, errPaused):
			ix.sleep(ctx, ix.o.Poll)
		case errors.Is(err, store.ErrLockBusy):
			ix.sleep(ctx, delay)
			delay = min(delay*2, ix.o.RetryMax)
		case errors.As(err, &refused) && (refused.Code == embeddings.Auth || refused.Code == embeddings.Misconfigured):
			ix.o.Logger.Error("the embedding service refused the key or the request; indexing waits", "error_code", refused.Code, "status", refused.Status)
			ix.sleep(ctx, ix.o.Refused)
		case writeRefused(err):
			ix.o.Logger.Error("the database refused the index; indexing waits", logging.Err(err))
			ix.sleep(ctx, ix.o.Refused)
		default:
			if !failing {
				ix.o.Logger.Warn("indexing failed; trying again", logging.Err(err))
			}
			failing = true
			wait := delay
			if errors.As(err, &refused) && refused.RetryAfter != nil {
				wait = min(max(wait, *refused.RetryAfter), ix.o.Refused)
			}
			ix.sleep(ctx, wait)
			delay = min(delay*2, ix.o.RetryMax)
		}
	}
}

// writeRefused is a database error that trying again cannot fix: bad data, a broken constraint or
// a schema mismatch (SQLSTATE classes 22, 23 and 42). Retrying would pay for embeddings each time.
func writeRefused(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || len(pg.Code) < 2 {
		return false
	}
	return slices.Contains([]string{"22", "23", "42"}, pg.Code[:2])
}

const eligible = `
	m.kind IN ('text', 'image', 'video', 'document', 'poll')
	AND m.body IS NOT NULL AND btrim(m.body) <> '' AND m.deleted_at IS NULL`

const linesSQL = `
SELECT m.id, m.chat_id, m.sent_at, m.updated_at,
       CASE m.kind WHEN 'image' THEN '(photo) ' WHEN 'video' THEN '(video) '
                   WHEN 'document' THEN '(document) ' WHEN 'poll' THEN '(poll) ' ELSE '' END || m.body,
       CASE WHEN m.from_me AND EXISTS (SELECT 1 FROM exchanges e WHERE e.answer_wa_id = m.wa_id) THEN 'Kaya'
            WHEN m.from_me THEN 'Me'
            ELSE (SELECT CASE WHEN lower(n) IN ('me', 'kaya') THEN '~' || n ELSE n END FROM (
                    SELECT coalesce(nullif(c.saved_name, ''), nullif(c.first_name, ''), '~' || nullif(c.push_name, ''),
                                    '~' || nullif(c.business_name, ''), '+' || nullif(split_part(c.pn, '@', 1), ''), 'Someone') AS n) named) END,
       coalesce(nullif(ch.name, ''), nullif(cc.saved_name, ''), nullif(cc.first_name, ''), '~' || nullif(cc.push_name, ''),
                '~' || nullif(cc.business_name, ''), '+' || nullif(split_part(cc.pn, '@', 1), ''), 'a chat')
FROM messages m
JOIN chats ch ON ch.id = m.chat_id
LEFT JOIN contacts c ON c.id = m.sender_id
LEFT JOIN contacts cc ON cc.id = ch.contact_id
WHERE ` + eligible

const settled = `
	AND m.sent_at < now() - make_interval(secs => $1)
	AND m.created_at < now() - make_interval(secs => $1)
	AND NOT EXISTS (SELECT 1 FROM message_chunks mc WHERE mc.message_id = m.id)`

type titledLine struct {
	Line
	title string
}

func readLines(ctx context.Context, db *store.DB, sql string, args ...any) ([]titledLine, error) {
	var out []titledLine
	err := db.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (titledLine, error) {
			var l titledLine
			err := row.Scan(&l.MessageID, &l.ChatID, &l.SentAt, &l.UpdatedAt, &l.Text, &l.Sender, &l.title)
			return l, err
		})
		return err
	})
	return out, err
}

type pending struct {
	chatID int64
	lines  []Line
	hash   string
	text   string
}

func (ix *Indexer) paused(ctx context.Context) error {
	st, err := ix.o.Pause.State(ctx)
	if err != nil {
		return err
	}
	if st.Paused {
		return errPaused
	}
	return nil
}

// pass brings the index one step closer to the chats and reports how many messages it indexed.
func (ix *Indexer) pass(ctx context.Context) (int, error) {
	if err := ix.paused(ctx); err != nil {
		return 0, err
	}
	if err := ix.recheck(ctx); err != nil {
		return 0, err
	}
	settle := ix.o.Settle.Seconds()
	// A message that lands inside a chunk's time span (history arriving late) belongs in it, so
	// the chunk is made again with it.
	if _, err := ix.o.DB.Exec(ctx, `
		DELETE FROM chunks WHERE id IN (
			SELECT k.id FROM messages m
			CROSS JOIN LATERAL (
				SELECT id, last_sent_at FROM chunks k
				WHERE k.chat_id = m.chat_id AND k.first_sent_at < m.sent_at
				ORDER BY k.first_sent_at DESC LIMIT 1) k
			WHERE k.last_sent_at > m.sent_at AND `+eligible+settled+`
			LIMIT 200)`, settle); err != nil {
		return 0, err
	}
	items, err := ix.collect(ctx, settle)
	if err != nil {
		return 0, err
	}
	ix.probed = false
	indexed := 0
	for len(items) > 0 {
		if err := ix.paused(ctx); err != nil {
			return indexed, err
		}
		if err := ix.lock.Hold(ctx); err != nil {
			return indexed, err
		}
		n := ix.batchSize(items)
		stored, err := ix.embed(ctx, items[:n])
		indexed += stored
		if err != nil {
			return indexed, err
		}
		items = items[n:]
	}
	return indexed, nil
}

// collect reads settled messages chat by chat, the chats whose waiting messages are oldest first,
// until about BatchMessages are read, and turns them into chunks. Going by the oldest waiting
// message keeps a chat whose conversation is still going on (and so waits) from standing in front
// of the history behind it.
func (ix *Indexer) collect(ctx context.Context, settle float64) ([]pending, error) {
	var chats []int64
	err := ix.o.DB.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT m.chat_id FROM messages m WHERE `+eligible+settled+`
			GROUP BY m.chat_id ORDER BY min(m.sent_at), m.chat_id LIMIT 1000`, settle)
		if err != nil {
			return err
		}
		chats, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		return err
	})
	if err != nil {
		return nil, err
	}
	var out []pending
	budget := ix.o.BatchMessages
	for _, chat := range chats {
		if budget <= 0 {
			break
		}
		rows, err := readLines(ctx, ix.o.DB, linesSQL+settled+`
			AND m.chat_id = $2 ORDER BY m.sent_at, m.id LIMIT $3`, settle, chat, budget+1)
		if err != nil {
			return nil, err
		}
		cut := len(rows) > budget
		if cut {
			rows = rows[:budget]
		}
		items, err := ix.chunk(ctx, rows, cut)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		budget -= len(rows)
	}
	return out, nil
}

// chunk groups one chat's rows, oldest first, into chunks, masked when the embeddings leave the
// machine. The last group waits when the read was cut inside it, or while its conversation may
// still go on, so a chat is never chunked in the middle of a conversation.
func (ix *Indexer) chunk(ctx context.Context, rows []titledLine, cut bool) ([]pending, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	lines := make([]Line, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, r.Line)
	}
	title, chatID := rows[0].title, rows[0].ChatID
	groups := Group(lines, ix.o.Shape)
	if cut {
		groups = groups[:len(groups)-1]
	} else {
		closed, err := ix.closed(ctx, groups[len(groups)-1])
		if err != nil {
			return nil, err
		}
		if !closed {
			groups = groups[:len(groups)-1]
		}
	}
	if len(groups) == 0 {
		return nil, nil
	}
	before, err := ix.before(ctx, chatID, lines[0].SentAt)
	if err != nil {
		return nil, err
	}
	var out []pending
	for _, group := range groups {
		p := pending{chatID: chatID, lines: group}
		plain := Render(title, group, ix.o.Zone, ix.o.Shape, nil, "")
		p.hash, p.text = Hash(plain), plain
		if ix.o.Embeddings.Locality() == llm.Cloud {
			p.text = Render(title, group, ix.o.Zone, ix.o.Shape, masking.NewSession(), before)
		}
		out = append(out, p)
		before = ""
		for _, l := range group[max(0, len(group)-3):] {
			before += l.Text + "\n"
		}
	}
	return out, nil
}

// closed reports whether nothing more can join a chat's last group: it is full, or the chat's next
// message is more than the gap later, already indexed, or does not exist.
func (ix *Indexer) closed(ctx context.Context, group []Line) (bool, error) {
	last := group[len(group)-1]
	size := 0
	for _, l := range group {
		size += min(utf8.RuneCountInString(l.Text), ix.o.Shape.MaxChars) + utf8.RuneCountInString(l.Sender) + 8
	}
	if len(group) >= ix.o.Shape.MaxLines {
		return true, nil
	}
	var sentAt time.Time
	var length int
	var indexed bool
	err := ix.o.DB.QueryRow(ctx, `
		SELECT m.sent_at, length(m.body), EXISTS (SELECT 1 FROM message_chunks mc WHERE mc.message_id = m.id)
		FROM messages m WHERE m.chat_id = $1 AND m.sent_at > $2 AND `+eligible+`
		ORDER BY m.sent_at, m.id LIMIT 1`, last.ChatID, last.SentAt).Scan(&sentAt, &length, &indexed)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return indexed || sentAt.Sub(last.SentAt) > ix.o.Shape.Gap || size+min(length, ix.o.Shape.MaxChars)+8 > ix.o.Shape.MaxChars, nil
}

// before is the text of the few messages just before at in a chat, so a code asked for there masks
// the number that answers it here.
func (ix *Indexer) before(ctx context.Context, chatID int64, at time.Time) (string, error) {
	var text *string
	err := ix.o.DB.QueryRow(ctx, `
		SELECT string_agg(body, E'\n') FROM (
			SELECT m.body FROM messages m WHERE m.chat_id = $1 AND m.sent_at < $2 AND `+eligible+`
			ORDER BY m.sent_at DESC LIMIT 3) recent`, chatID, at).Scan(&text)
	if err != nil || text == nil {
		return "", err
	}
	return *text, nil
}

// batchSize is how many of items fit one embedding call: at most the current batch size and the
// token cap, at least one.
func (ix *Indexer) batchSize(items []pending) int {
	n, tokens := 0, 0
	for _, it := range items[:min(len(items), ix.batch)] {
		estimate := utf8.RuneCountInString(it.text)/3 + 1
		if n > 0 && tokens+estimate > ix.o.MaxTokens {
			break
		}
		n++
		tokens += estimate
	}
	return max(n, 1)
}

// embed embeds items and stores them. A rate limit halves the batch and waits as long as the
// service asks; a batch the service refuses is split until the chunk it will not take is found,
// and that chunk is stored as skipped so it is not tried again until its text changes.
func (ix *Indexer) embed(ctx context.Context, items []pending) (int, error) {
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = it.text
	}
	result, err := ix.o.Embeddings.Embed(ctx, texts, embeddings.Document)
	var failed *embeddings.Error
	switch {
	case err == nil:
		ix.batch = min(ix.batch*2, ix.o.Embeddings.MaxBatch())
		return ix.store(ctx, items, result.Vectors, "")
	case errors.As(err, &failed) && failed.Code == embeddings.RateLimited:
		ix.batch = max(1, ix.batch/2)
		return 0, err
	case errors.As(err, &failed) && failed.Code == embeddings.Rejected:
		if !ix.probed {
			// Find out once per pass whether the service refuses this chunk or every request:
			// a wrong model or setting must not mark the whole history as skipped.
			ix.probed = true
			if _, probe := ix.o.Embeddings.Embed(ctx, []string{"ok"}, embeddings.Document); probe != nil {
				var refused *embeddings.Error
				if errors.As(probe, &refused) && refused.Code == embeddings.Rejected {
					return 0, &embeddings.Error{Provider: refused.Provider, Code: embeddings.Misconfigured, Status: refused.Status}
				}
				return 0, probe
			}
		}
		if len(items) == 1 {
			ix.o.Logger.Warn("the embedding service would not take a chunk; it is skipped", "chat_id", items[0].chatID, "status", failed.Status)
			return ix.store(ctx, items, nil, failed.Code)
		}
		half := len(items) / 2
		first, err := ix.embed(ctx, items[:half])
		if err != nil {
			return first, err
		}
		second, err := ix.embed(ctx, items[half:])
		return first + second, err
	default:
		return 0, err
	}
}

func vectorText(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 8)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// store writes the chunks and which messages they hold in one transaction. Each message keeps the
// updated_at it was read with, so a change that landed while the chunk was being embedded is
// noticed on the next check. A message already in the index fails the whole batch rather than
// being skipped quietly.
func (ix *Indexer) store(ctx context.Context, items []pending, vectors [][]float32, skipped string) (int, error) {
	stored := 0
	err := ix.o.DB.InTx(ctx, func(tx pgx.Tx) error {
		stored = 0
		for i, it := range items {
			var embedding, reason any
			if vectors != nil {
				embedding = vectorText(vectors[i])
			} else {
				reason = skipped
			}
			ids := make([]int64, len(it.lines))
			updated := make([]time.Time, len(it.lines))
			for j, l := range it.lines {
				ids[j], updated[j] = l.MessageID, l.UpdatedAt
			}
			var id int64
			err := tx.QueryRow(ctx, `
				INSERT INTO chunks (chat_id, first_sent_at, last_sent_at, message_count, text_hash, model, embedding, skipped)
				VALUES ($1, $2, $3, $4, $5, $6, $7::halfvec, $8) RETURNING id`,
				it.chatID, it.lines[0].SentAt, it.lines[len(it.lines)-1].SentAt, len(it.lines), it.hash,
				ix.o.Embeddings.Model(), embedding, reason).Scan(&id)
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO message_chunks (message_id, chunk_id, message_updated_at)
				SELECT unnest($1::bigint[]), $2, unnest($3::timestamptz[])`, ids, id, updated)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != int64(len(ids)) {
				return errors.New("search index: not every message of a chunk was recorded")
			}
			stored += len(ids)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return stored, nil
}

// recheck finds chunks that no longer match their messages (a message was edited, deleted or
// moved to another chat by a contact merge, or the chunk was made by another model) and either
// confirms them or removes them, which frees their messages to be indexed again.
func (ix *Indexer) recheck(ctx context.Context) error {
	var stale []int64
	err := ix.o.DB.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT mc.chunk_id FROM message_chunks mc
			JOIN chunks k ON k.id = mc.chunk_id
			LEFT JOIN messages m ON m.id = mc.message_id
			WHERE m.id IS NULL OR m.updated_at <> mc.message_updated_at OR m.chat_id <> k.chat_id
			UNION
			SELECT id FROM chunks WHERE model <> $1
			LIMIT 500`, ix.o.Embeddings.Model())
		if err != nil {
			return err
		}
		stale, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		return err
	})
	if err != nil {
		return err
	}
	for _, id := range stale {
		if err := ix.recheckOne(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (ix *Indexer) recheckOne(ctx context.Context, id int64) error {
	var model, hash string
	var ids []int64
	err := ix.o.DB.QueryRow(ctx, `
		SELECT k.model, k.text_hash, array_agg(mc.message_id ORDER BY mc.message_id)
		FROM chunks k JOIN message_chunks mc ON mc.chunk_id = k.id
		WHERE k.id = $1 GROUP BY k.id`, id).Scan(&model, &hash, &ids)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := readLines(ctx, ix.o.DB, linesSQL+` AND m.id = ANY($1) ORDER BY m.sent_at, m.id`, ids)
	if err != nil {
		return err
	}
	same := model == ix.o.Embeddings.Model() && len(rows) == len(ids)
	if same {
		lines := make([]Line, len(rows))
		for i, r := range rows {
			lines[i] = r.Line
			same = same && r.ChatID == rows[0].ChatID
		}
		same = same && len(Group(lines, ix.o.Shape)) == 1 &&
			Hash(Render(rows[0].title, lines, ix.o.Zone, ix.o.Shape, nil, "")) == hash
	}
	if !same {
		_, err := ix.o.DB.Exec(ctx, "DELETE FROM chunks WHERE id = $1", id)
		return err
	}
	// The text is the same (a reaction or a status change touched the message, or a contact merge
	// moved the chat): keep the vector and remember what was checked.
	read := make([]int64, len(rows))
	updated := make([]time.Time, len(rows))
	for i, r := range rows {
		read[i], updated[i] = r.MessageID, r.UpdatedAt
	}
	return ix.o.DB.InTx(ctx, func(tx pgx.Tx) error {
		// The times as read, not as they are now: an edit that lands after the read is still
		// noticed on the next check.
		if _, err := tx.Exec(ctx, `
			UPDATE message_chunks mc SET message_updated_at = x.updated_at
			FROM unnest($2::bigint[], $3::timestamptz[]) AS x(message_id, updated_at)
			WHERE mc.message_id = x.message_id AND mc.chunk_id = $1`, id, read, updated); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE chunks SET chat_id = $2 WHERE id = $1", id, rows[0].ChatID)
		return err
	})
}

// Progress counts the messages in the index against every message that can be.
func (ix *Indexer) Progress(ctx context.Context) (Progress, error) {
	var p Progress
	err := ix.o.DB.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM message_chunks),
		       (SELECT count(*) FROM message_chunks mc JOIN chunks k ON k.id = mc.chunk_id WHERE k.skipped IS NOT NULL),
		       (SELECT count(*) FROM messages m WHERE `+eligible+`)`).Scan(&p.Indexed, &p.Skipped, &p.Total)
	p.Indexed = min(p.Indexed, p.Total)
	return p, err
}
