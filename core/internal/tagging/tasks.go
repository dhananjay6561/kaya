// Package tagging reads each new message in DJ's chats once and tags it important, ignore, task or
// reminder, keeping the tasks and reminders it finds for /tasks.
package tagging

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/core/internal/search"
	"github.com/dhananjay6561/kaya/core/internal/text"
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
type Tasks struct {
	db  *store.DB
	now func() time.Time
}

// NewTasks returns tasks on db, judging which reminders are over by now; nil means time.Now.
func NewTasks(db *store.DB, now func() time.Time) *Tasks {
	if now == nil {
		now = time.Now
	}
	return &Tasks{db: db, now: now}
}

const taskColumns = `t.id, t.kind, t.title, t.due_at, t.due_has_time, ` + search.ChatTitleSQL + `,
	coalesce(m.sent_at, t.created_at), t.state = 'done'`

// current leaves out tasks whose message was deleted and reminders that are over: two hours past
// their time, or past their day when only the day is known.
const current = `t.state = 'open' AND m.deleted_at IS NULL AND NOT (t.kind = 'reminder' AND t.due_at IS NOT NULL AND
	t.due_at + CASE WHEN t.due_has_time THEN interval '2 hours' ELSE interval '1 day' END < @now::timestamptz)`

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

// Open returns up to limit open tasks, leaving out those whose message was deleted and reminders
// that are over: the ones with a due date first, soonest first, then the rest, newest first.
func (s *Tasks) Open(ctx context.Context, limit int) ([]Task, error) {
	var out []Task
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = collect(tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks t`+taskJoins+`
			WHERE `+current+`
			ORDER BY t.due_at IS NULL, t.due_at, t.created_at DESC, t.id DESC
			LIMIT @limit`, pgx.NamedArgs{"now": s.now(), "limit": limit}))
		return err
	})
	return out, err
}

// CountOpen is how many tasks Open would list without a limit.
func (s *Tasks) CountOpen(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM tasks t LEFT JOIN messages m ON m.id = t.message_id
		WHERE `+current, pgx.NamedArgs{"now": s.now()}).Scan(&n)
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

// Describe is one task as a line: its number and title, when it is due (or that it is overdue), and
// where it came from.
func Describe(t Task, zone *time.Location, now time.Time) string {
	line := fmt.Sprintf("%d. %s", t.ID, text.Clip(t.Title, 200))
	if due := dueText(t, zone, now); due != "" {
		line += ", " + due
	}
	return line + " (" + t.Chat + ", " + t.SentAt.In(zone).Format("2 Jan") + ")"
}

// dueText says when a task is due, or that it is overdue; it is empty for a task with no due date.
func dueText(t Task, zone *time.Location, now time.Time) string {
	if t.Due == nil {
		return ""
	}
	layout := "Mon 2 Jan"
	if t.Due.In(zone).Year() != now.In(zone).Year() {
		layout += " 2006"
	}
	if t.DueHasTime {
		layout += " 15:04"
	}
	deadline := *t.Due
	if !t.DueHasTime {
		deadline = deadline.AddDate(0, 0, 1)
	}
	if now.After(deadline) {
		return "overdue: was due " + t.Due.In(zone).Format(layout)
	}
	return "due " + t.Due.In(zone).Format(layout)
}
