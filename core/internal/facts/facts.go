// Package facts keeps the lasting facts about DJ that go into every context package: the ones he
// tells Kaya with /remember and the ones learned from what he says to her.
package facts

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/core/internal/text"
	"github.com/dhananjay6561/kaya/kit/store"
)

// Where a fact came from.
const (
	Told    = "told"
	Learned = "learned"
)

const (
	// MaxFacts is the most facts kept; past it nothing new is added until some are removed.
	MaxFacts = 200
	// MaxLength is the most characters one fact may have.
	MaxLength = 500
)

// Fact is one lasting fact about DJ.
type Fact struct {
	ID        int64
	Text      string
	Source    string
	UpdatedAt time.Time
}

// Errors Remember reports.
var (
	ErrEmpty   = errors.New("a fact needs some text")
	ErrTooLong = errors.New("a fact is too long")
	ErrFull    = errors.New("no room for more facts")
)

// Store reads and writes facts.
type Store struct{ db *store.DB }

// NewStore returns a store on db.
func NewStore(db *store.DB) *Store { return &Store{db: db} }

const listSQL = `SELECT id, body, source, updated_at FROM facts
ORDER BY source = 'told' DESC, updated_at DESC, id DESC`

// List returns every fact: the ones DJ told first, then the rest, each newest first.
func (s *Store) List(ctx context.Context) ([]Fact, error) {
	var out []Fact
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = list(ctx, tx)
		return err
	})
	return out, err
}

func list(ctx context.Context, tx pgx.Tx) ([]Fact, error) {
	return collect(tx.Query(ctx, listSQL))
}

func collect(rows pgx.Rows, err error) ([]Fact, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Fact, error) {
		var f Fact
		err := row.Scan(&f.ID, &f.Text, &f.Source, &f.UpdatedAt)
		return f, err
	})
}

// lockFacts keeps facts from changing under a writer that checks for duplicates and room first.
func lockFacts(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "LOCK TABLE facts IN SHARE ROW EXCLUSIVE MODE")
	return err
}

// Clean is a fact's text on one line with its spacing tidied and invisible marks dropped (the
// emoji joiner kept).
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) && r != 0x200d {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.FieldsFunc(text.Trim(s), text.IsSpace), " ")
}

// Remember keeps a fact DJ told. When the same fact is kept already it is returned with false,
// marked as told if it was learned.
func (s *Store) Remember(ctx context.Context, body string, messageID *int64) (Fact, bool, error) {
	body = Clean(body)
	switch {
	case body == "":
		return Fact{}, false, ErrEmpty
	case utf8.RuneCountInString(body) > MaxLength:
		return Fact{}, false, ErrTooLong
	}
	var f Fact
	created := false
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := lockFacts(ctx, tx); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE facts SET source = 'told', updated_at = CASE WHEN source = 'told' THEN updated_at ELSE now() END
			WHERE lower(body) = lower($1) RETURNING id, body, source, updated_at`, body).
			Scan(&f.ID, &f.Text, &f.Source, &f.UpdatedAt)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM facts").Scan(&n); err != nil {
			return err
		}
		if n >= MaxFacts {
			return ErrFull
		}
		created = true
		return tx.QueryRow(ctx, `INSERT INTO facts (body, source, message_id) VALUES ($1, 'told', $2)
			RETURNING id, body, source, updated_at`, body, messageID).Scan(&f.ID, &f.Text, &f.Source, &f.UpdatedAt)
	})
	return f, created, err
}

// Forget removes the facts with these ids and returns the ones it removed.
func (s *Store) Forget(ctx context.Context, ids []int64) ([]Fact, error) {
	var out []Fact
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = collect(tx.Query(ctx, `WITH gone AS (DELETE FROM facts WHERE id = ANY($1) RETURNING id, body, source, updated_at)
			SELECT * FROM gone ORDER BY id`, ids))
		return err
	})
	return out, err
}

// Count is how many facts are kept and how many of them DJ told.
func (s *Store) Count(ctx context.Context) (all, told int, err error) {
	err = s.db.QueryRow(ctx, "SELECT count(*), count(*) FILTER (WHERE source = 'told') FROM facts").Scan(&all, &told)
	return all, told, err
}
