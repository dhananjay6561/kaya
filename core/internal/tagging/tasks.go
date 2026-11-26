// Package tagging reads each new message in DJ's chats once and tags it important, ignore, task or
// reminder, keeping the tasks and reminders it finds for /tasks.
package tagging

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/core/internal/search"
	"github.com/dhananjay6561/kaya/kit/store"
)

// Tags a message can have; KindTask and KindReminder are also the kinds of task.
const (
	Important    = "important"
	Ignore       = "ignore"
	KindTask     = "task"
	KindReminder = "reminder"
)

// Task is something DJ has to do or be reminded of, found in a message.
type Task struct {
	ID    int64
	Kind  string
	Title string
	// Due is when it is due, nil when the message did not say; DueHasTime is false when only the day
	// is known.
	Due        *time.Time
	DueHasTime bool
	Chat       string
	SentAt     time.Time
	Done       bool
}

// Tasks reads and closes tasks.
type Tasks struct{ db *store.DB }

// NewTasks returns tasks on db.
func NewTasks(db *store.DB) *Tasks { return &Tasks{db: db} }

const taskColumns = `t.id, t.kind, t.title, t.due_at, t.due_has_time, ` + search.ChatTitleSQL + `,
	coalesce(m.sent_at, t.created_at), t.state = 'done'`

const taskJoins = `
LEFT JOIN chats ch ON ch.id = t.chat_id
LEFT JOIN contacts cc ON cc.id = ch.contact_id
LEFT JOIN messages m ON m.id = t.message_id`

func collect(rows pgx.Rows, err error) ([]Task, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Task, error) {
		var t Task
		err := row.Scan(&t.ID, &t.Kind, &t.Title, &t.Due, &t.DueHasTime, &t.Chat, &t.SentAt, &t.Done)
		return t, err
	})
}

// Open returns up to limit open tasks: the ones with a due date first, soonest first, then the rest,
// newest first.
func (s *Tasks) Open(ctx context.Context, limit int) ([]Task, error) {
	var out []Task
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = collect(tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks t`+taskJoins+`
			WHERE t.state = 'open'
			ORDER BY t.due_at IS NULL, t.due_at, t.created_at DESC, t.id DESC
			LIMIT $1`, limit))
		return err
	})
	return out, err
}

// CountOpen is how many tasks are open.
func (s *Tasks) CountOpen(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, "SELECT count(*) FROM tasks WHERE state = 'open'").Scan(&n)
	return n, err
}

// Done closes the open tasks with these ids and returns the ones it closed.
func (s *Tasks) Done(ctx context.Context, ids []int64) ([]Task, error) {
	var out []Task
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = collect(tx.Query(ctx, `WITH closed AS (
				UPDATE tasks SET state = 'done', done_at = now() WHERE id = ANY($1) AND state = 'open' RETURNING *)
			SELECT `+taskColumns+` FROM closed t`+taskJoins+` ORDER BY t.id`, ids))
		return err
	})
	return out, err
}
