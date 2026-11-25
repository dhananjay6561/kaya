// Package pause is the switch DJ flips with /pause and /resume. While it is on, Kaya answers only
// its slash commands and does nothing else on its own (decision 65).
package pause

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/kit/store"
)

// State says whether Kaya is paused, and since when.
type State struct {
	Paused bool
	Since  time.Time
}

// Switch pauses and resumes Kaya. by names the command that asked; asking again with the same one
// gives the same answer.
type Switch interface {
	State(ctx context.Context) (State, error)
	// Pause reports true when this command paused Kaya, false when it already was.
	Pause(ctx context.Context, by string) (bool, error)
	// Resume reports true when this command resumed Kaya, false when it was not paused.
	Resume(ctx context.Context, by string) (bool, error)
}

// ErrMissing means the pause_state row is gone, so the migrations have not run.
var ErrMissing = errors.New("the pause_state row is missing; the migrations have not run")

// Stored keeps the switch in Postgres, so it holds across restarts and every core process.
type Stored struct {
	db *store.DB
}

// NewStored returns the switch kept in pause_state.
func NewStored(db *store.DB) *Stored { return &Stored{db: db} }

// State reads the switch.
func (s *Stored) State(ctx context.Context) (State, error) {
	var at *time.Time
	err := s.db.QueryRow(ctx, "SELECT paused_at FROM pause_state").Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, ErrMissing
	}
	if err != nil || at == nil {
		return State{}, err
	}
	return State{Paused: true, Since: *at}, nil
}

// Pause turns the switch on.
func (s *Stored) Pause(ctx context.Context, by string) (bool, error) {
	return s.flip(ctx, by, `
		UPDATE pause_state
		SET paused_at = coalesce(paused_at, now()),
		    changed_by = CASE WHEN paused_at IS NULL THEN $1 ELSE changed_by END,
		    changed_at = CASE WHEN paused_at IS NULL THEN now() ELSE changed_at END
		RETURNING changed_by IS NOT DISTINCT FROM $1`)
}

// Resume turns the switch off.
func (s *Stored) Resume(ctx context.Context, by string) (bool, error) {
	return s.flip(ctx, by, `
		UPDATE pause_state
		SET paused_at = NULL,
		    changed_by = CASE WHEN paused_at IS NOT NULL THEN $1 ELSE changed_by END,
		    changed_at = CASE WHEN paused_at IS NOT NULL THEN now() ELSE changed_at END
		RETURNING changed_by IS NOT DISTINCT FROM $1`)
}

func (s *Stored) flip(ctx context.Context, by, sql string) (bool, error) {
	var thisOne bool
	err := s.db.QueryRow(ctx, sql, by).Scan(&thisOne)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrMissing
	}
	return thisOne, err
}

// Memory keeps the switch in memory, for tests.
type Memory struct {
	mu        sync.Mutex
	pausedAt  time.Time
	changedBy string
}

// State reads the switch.
func (m *Memory) State(context.Context) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return State{Paused: !m.pausedAt.IsZero(), Since: m.pausedAt}, nil
}

// Pause turns the switch on.
func (m *Memory) Pause(_ context.Context, by string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pausedAt.IsZero() {
		m.pausedAt, m.changedBy = time.Now(), by
	}
	return m.changedBy == by, nil
}

// Resume turns the switch off.
func (m *Memory) Resume(_ context.Context, by string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.pausedAt.IsZero() {
		m.pausedAt, m.changedBy = time.Time{}, by
	}
	return m.changedBy == by, nil
}
