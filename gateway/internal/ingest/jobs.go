package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"google.golang.org/protobuf/proto"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
)

// History job states, as stored in history_jobs.state.
const (
	JobPending = "pending"
	JobStored  = "stored"
	JobDone    = "done"
	JobDead    = "dead"
)

const (
	historyBatch       = 200
	historyMaxAttempts = 5
	historyMaxWait     = 7 * 24 * time.Hour
)

// ErrUnavailable means the database could not be reached; the work is safe to try again later.
var ErrUnavailable = errors.New("database unavailable")

// HistoryJob is one history sync chunk WhatsApp announced.
type HistoryJob struct {
	ID           int64
	WAID         string
	Notification []byte
	SyncType     string
	ChunkOrder   int
	// Progress is the percentage of the whole sync WhatsApp says is done, when it says.
	Progress *int
	State    string
}

// AddHistoryJob queues a chunk. A chunk that was announced before is left as it is.
func (s *Store) AddHistoryJob(ctx context.Context, tx pgx.Tx, job HistoryJob) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO history_jobs (wa_id, notification, sync_type, chunk_order, progress)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (wa_id) DO NOTHING`,
		job.WAID, job.Notification, job.SyncType, job.ChunkOrder, job.Progress)
	if err != nil {
		return fmt.Errorf("queue history job: %w", err)
	}
	return nil
}

// NextHistoryJob returns the oldest job that still needs work, or nil. Jobs that have waited too
// long are given up on first, since WhatsApp no longer holds their data.
func (s *Store) NextHistoryJob(ctx context.Context, tx pgx.Tx) (*HistoryJob, error) {
	_, err := tx.Exec(ctx, `
		UPDATE history_jobs SET state = 'dead', notification = NULL, error_code = 'waited_too_long', finished_at = now()
		WHERE state = 'pending' AND created_at < $1`, time.Now().Add(-historyMaxWait))
	if err != nil {
		return nil, fmt.Errorf("expire history jobs: %w", err)
	}
	var job HistoryJob
	var chunkOrder *int
	err = tx.QueryRow(ctx, `
		SELECT id, wa_id, notification, sync_type, chunk_order, progress, state FROM history_jobs
		WHERE state IN ('pending', 'stored') ORDER BY id LIMIT 1`).
		Scan(&job.ID, &job.WAID, &job.Notification, &job.SyncType, &chunkOrder, &job.Progress, &job.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read history job: %w", err)
	}
	if chunkOrder != nil {
		job.ChunkOrder = *chunkOrder
	}
	return &job, nil
}

// storeHistoryBatch stores records in one transaction, each behind a savepoint, so one message the
// database refuses becomes a dead letter and the rest still go in.
func (s *Store) storeHistoryBatch(ctx context.Context, tx pgx.Tx, jobID int64, records []Record) error {
	chats, messages := 0, 0
	for _, rec := range records {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		applied, err := s.Apply(ctx, sp, rec)
		if err == nil {
			err = sp.Commit(ctx)
		}
		if err != nil {
			_ = sp.Rollback(ctx)
			code, permanent := classify(err)
			if !permanent {
				return err
			}
			d := DeadLetter{WAID: rec.WAID, ChatJID: rec.Chat.JID, SenderJID: firstOf(rec.Sender), ErrorCode: code, Info: historyInfo}
			if d.ChatJID == "" {
				d.ChatJID = firstOf(rec.Chat.Contact)
			}
			if rec.raw != nil {
				d.Raw, _ = proto.Marshal(rec.raw)
			}
			if err := s.AddDeadLetter(ctx, tx, d); err != nil {
				return err
			}
			continue
		}
		if applied.NewChat {
			chats++
		}
		if applied.Changed {
			messages++
		}
	}
	_, err := tx.Exec(ctx, "UPDATE history_jobs SET chats = chats + $2, messages = messages + $3 WHERE id = $1", jobID, chats, messages)
	if err != nil {
		return fmt.Errorf("count history rows: %w", err)
	}
	return nil
}

func firstOf(a Address) string {
	switch {
	case a.LID != "":
		return a.LID
	case a.PN != "":
		return a.PN
	}
	return a.Bot
}

// finishHistoryJob marks a job stored once all its rows are in: it applies the edits and revokes
// that were waiting for those rows, clears the announcement, and tells core, if the chunk carried
// any chats.
func (s *Store) finishHistoryJob(ctx context.Context, tx pgx.Tx, job *HistoryJob, carriedChats bool) error {
	if _, err := s.ReplayParked(ctx, tx); err != nil {
		return err
	}
	var chats, messages int
	err := tx.QueryRow(ctx, `
		UPDATE history_jobs SET state = 'stored', notification = NULL, finished_at = now()
		WHERE id = $1 AND state = 'pending' RETURNING chats, messages`, job.ID).Scan(&chats, &messages)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("finish history job: %w", err)
	}
	if !carriedChats {
		return nil
	}
	event, err := contracts.NewEvent(contracts.EventHistorySynced, contracts.HistorySynced{
		Chats: chats, Messages: messages, Progress: job.Progress, Complete: job.Progress != nil && *job.Progress >= 100,
	})
	if err != nil {
		return err
	}
	return s.outbox.Add(ctx, tx, event)
}

// NextHistoryJob returns the oldest history job that still needs work, or nil.
func (i *Ingester) NextHistoryJob(ctx context.Context) (*HistoryJob, error) {
	var job *HistoryJob
	err := i.db.InTx(ctx, func(tx pgx.Tx) (err error) {
		job, err = i.store.NextHistoryJob(ctx, tx)
		return err
	})
	return job, scrub(err)
}

// StoreHistory stores one downloaded history chunk and marks its job stored. It returns
// ErrUnavailable when the database stayed away; calling it again with the same chunk is safe, since
// rows that are already in are left alone.
func (i *Ingester) StoreHistory(ctx context.Context, job *HistoryJob, data *waHistorySync.HistorySync, parser Parser, own Own) error {
	chunk := FromHistory(data, parser, own)

	steps := []func(pgx.Tx) error{}
	for start := 0; start < len(chunk.Mappings); start += historyBatch {
		batch := chunk.Mappings[start:min(start+historyBatch, len(chunk.Mappings))]
		steps = append(steps, func(tx pgx.Tx) error {
			for _, a := range batch {
				if err := i.linkAddresses(ctx, tx, a); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for start := 0; start < len(chunk.Dead); start += historyBatch {
		batch := chunk.Dead[start:min(start+historyBatch, len(chunk.Dead))]
		steps = append(steps, func(tx pgx.Tx) error {
			for _, d := range batch {
				if err := i.store.AddDeadLetter(ctx, tx, d); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for start := 0; start < len(chunk.Records); start += historyBatch {
		batch := chunk.Records[start:min(start+historyBatch, len(chunk.Records))]
		steps = append(steps, func(tx pgx.Tx) error { return i.store.storeHistoryBatch(ctx, tx, job.ID, batch) })
	}
	for start := 0; start < len(chunk.Names); start += historyBatch {
		batch := chunk.Names[start:min(start+historyBatch, len(chunk.Names))]
		steps = append(steps, func(tx pgx.Tx) error {
			for _, n := range batch {
				if err := i.optional(ctx, tx, func(sp pgx.Tx) error {
					return i.store.SetChatName(ctx, sp, n.Chat, n.Name, time.Time{})
				}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	steps = append(steps, func(tx pgx.Tx) error {
		return i.store.finishHistoryJob(ctx, tx, job, chunk.Conversations > 0)
	})

	for _, step := range steps {
		switch result, code := i.run(ctx, "history", step); result {
		case unavailable:
			return ErrUnavailable
		case rejected:
			return fmt.Errorf("history chunk refused by the database: %s", code)
		}
	}
	if len(chunk.Dead) > 0 {
		i.logger.Warn("some history messages could not be mapped and were set aside in ingest_dead", "messages", len(chunk.Dead))
	}
	i.logger.Info("history chunk stored", "sync_type", job.SyncType, "conversations", chunk.Conversations, "messages", len(chunk.Records))
	return nil
}

// linkAddresses makes sure a LID and its phone number are one contact.
func (i *Ingester) linkAddresses(ctx context.Context, tx pgx.Tx, a Address) error {
	return i.optional(ctx, tx, func(sp pgx.Tx) error {
		_, err := i.store.contact(ctx, sp, a)
		return err
	})
}

// optional runs a step that history can do without (a name, an address pairing) behind a
// savepoint. If the database refuses it, the step is skipped: old data being odd is no reason to
// fail the whole chunk. Only the database being away is passed on.
func (i *Ingester) optional(ctx context.Context, tx pgx.Tx, step func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if err = step(sp); err == nil {
		return sp.Commit(ctx)
	}
	_ = sp.Rollback(ctx)
	if _, permanent := classify(err); permanent {
		return nil
	}
	return err
}

// FailHistory records a failed attempt at a job. The job is given up on when final is set or after
// too many attempts; dead reports whether it was.
func (i *Ingester) FailHistory(ctx context.Context, job *HistoryJob, code string, final bool) (dead bool, err error) {
	err = i.db.InTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			UPDATE history_jobs SET
				attempts = attempts + 1,
				state = CASE WHEN $3 OR attempts + 1 >= $4 THEN 'dead' ELSE state END,
				notification = CASE WHEN $3 OR attempts + 1 >= $4 THEN NULL ELSE notification END,
				error_code = $2,
				finished_at = CASE WHEN $3 OR attempts + 1 >= $4 THEN now() END
			WHERE id = $1 AND state = 'pending' RETURNING state = 'dead'`,
			job.ID, code, final, historyMaxAttempts).Scan(&dead)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if dead {
		i.logger.Error("gave up on a history chunk; linking again is the only way to get it", "sync_type", job.SyncType, "error_code", code)
	}
	return dead, scrub(err)
}

// FinishHistory marks a stored or dead job as fully handled, once WhatsApp has been told.
func (i *Ingester) FinishHistory(ctx context.Context, job *HistoryJob) error {
	err := i.db.InTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE history_jobs SET state = 'done' WHERE id = $1 AND state = 'stored'", job.ID)
		return err
	})
	return scrub(err)
}
