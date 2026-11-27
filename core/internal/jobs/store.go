package jobs

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/kit/store"
)

// Job is a reminder or scheduled job.
type Job struct {
	ID     int64
	Text   string
	Ask    bool
	Repeat Repeat
	Clock  string
	NextAt time.Time
}

// Store keeps jobs.
type Store struct{ db *store.DB }

// NewStore returns jobs on db.
func NewStore(db *store.DB) *Store { return &Store{db: db} }

const jobColumns = `id, text, ask, repeat, every, weekdays, coalesce(month_day, 0), local_time, until, next_at`

func scanJob(row pgx.CollectableRow) (Job, error) {
	var j Job
	var days []int16
	var until *time.Time
	err := row.Scan(&j.ID, &j.Text, &j.Ask, &j.Repeat.Kind, &j.Repeat.Every, &days, &j.Repeat.MonthDay, &j.Clock, &until, &j.NextAt)
	for _, d := range days {
		j.Repeat.Days = append(j.Repeat.Days, time.Weekday(d))
	}
	if until != nil {
		j.Repeat.Until = *until
	}
	return j, err
}

func (s *Store) query(ctx context.Context, sql string, args ...any) ([]Job, error) {
	var out []Job
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, scanJob)
		return err
	})
	return out, err
}

// Create schedules j to fire first at j.NextAt. A job just like an active one, same text, same
// time and same repeat, is not made twice: the active one is returned, with false.
func (s *Store) Create(ctx context.Context, j Job, createdBy int64) (Job, bool, error) {
	days := make([]int, len(j.Repeat.Days))
	for i, d := range j.Repeat.Days {
		days[i] = int(d)
	}
	var until, monthDay any
	if !j.Repeat.Until.IsZero() {
		until = j.Repeat.Until.Format("2006-01-02")
	}
	if j.Repeat.Kind == Monthly {
		monthDay = j.Repeat.MonthDay
	}
	var by any
	if createdBy != 0 {
		by = createdBy
	}
	same, err := s.query(ctx, `SELECT `+jobColumns+` FROM jobs
		WHERE state = 'active' AND lower(text) = lower($1) AND abs(extract(epoch FROM next_at - $2::timestamptz)) < 60
		  AND repeat = $3 AND ask = $4 AND every = $5 AND weekdays = $6::smallint[]
		  AND month_day IS NOT DISTINCT FROM $7::smallint AND until IS NOT DISTINCT FROM $8::date`,
		strings.TrimSpace(j.Text), j.NextAt, j.Repeat.Kind, j.Ask, max(j.Repeat.Every, 1), days, monthDay, until)
	if err != nil || len(same) > 0 {
		if len(same) > 0 {
			return same[0], false, nil
		}
		return Job{}, false, err
	}
	made, err := s.query(ctx, `INSERT INTO jobs (text, ask, repeat, every, weekdays, month_day, local_time, until, next_at, created_by)
		VALUES ($1, $2, $3, $4, $5::smallint[], $6, $7, $8::date, $9, $10) RETURNING `+jobColumns,
		strings.TrimSpace(j.Text), j.Ask, j.Repeat.Kind, max(j.Repeat.Every, 1), days, monthDay, j.Clock, until, j.NextAt, by)
	if err != nil {
		return Job{}, false, err
	}
	return made[0], true, nil
}

// List is the active jobs, soonest first.
func (s *Store) List(ctx context.Context) ([]Job, error) {
	return s.query(ctx, `SELECT `+jobColumns+` FROM jobs WHERE state = 'active' ORDER BY next_at, id`)
}

// Cancel stops the active jobs with these ids and returns the ones it stopped.
func (s *Store) Cancel(ctx context.Context, ids []int64) ([]Job, error) {
	return s.query(ctx, `UPDATE jobs SET state = 'cancelled', next_at = NULL WHERE id = ANY($1) AND state = 'active'
		RETURNING id, text, ask, repeat, every, weekdays, coalesce(month_day, 0), local_time, until, now()`, ids)
}
