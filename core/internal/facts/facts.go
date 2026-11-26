// Package facts keeps the lasting facts about DJ that go into every context package: the ones he
// tells Kaya with /remember and the ones learned from what he says to her.
package facts

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/core/internal/masking"
	"github.com/dhananjay6561/kaya/core/internal/text"
	"github.com/dhananjay6561/kaya/kit/store"
)

// Where a fact came from.
const (
	Told    = "told"
	Learned = "learned"
)

const (
	// MaxFacts is the most facts kept. Past it a new fact takes the place of the learned fact
	// changed longest ago; when every fact is one DJ told, nothing new is added.
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
	ErrSecret  = errors.New("a fact holds a secret")
)

var (
	placeholder = regexp.MustCompile(`\[[A-Z]+_\d+\]`)
	// secretWords name things that are never kept, in the languages and spellings people type.
	secretWords = regexp.MustCompile(`(?i)(^|[^\pL\pN])(pass ?words?|passwd|pwd|pass ?codes?|pass ?phrases?|cvv|cvc|security codes?|login pins?|atm pins?|upi pins?|mpin|tpin)($|[^\pL\pN])`)
	// secretKinds are details the masking rules find that are never kept, whoever said them.
	secretKinds = []string{masking.Card, masking.Account, masking.Aadhaar, masking.PAN, masking.Passport, masking.OTP}
)

// Secret reports whether text holds something never kept as a fact: a password or PIN by name, or a
// code, card, account or ID number the masking rules find.
func Secret(text string) bool {
	if secretWords.MatchString(text) {
		return true
	}
	session := masking.NewSession()
	session.Mask(text, text)
	counts := session.Counts()
	for _, kind := range secretKinds {
		if counts[kind] > 0 {
			return true
		}
	}
	return false
}

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

// Clean is a fact's text on one line with its spacing tidied and control characters and invisible
// marks dropped, keeping the joiners scripts and emoji need and the tags of flag emoji.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == 0x200c || r == 0x200d || r >= 0xe0020 && r <= 0xe007f, text.IsSpace(r):
			return r
		case unicode.Is(unicode.Cf, r), unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.FieldsFunc(text.Trim(s), text.IsSpace), " ")
}

// Remember keeps a fact DJ told with the command whose WhatsApp id is waID. When the same fact is
// kept already it is returned with false, marked as told if it was learned, unless this same command
// kept it (a retry), which counts as kept now.
func (s *Store) Remember(ctx context.Context, body, waID string) (Fact, bool, error) {
	body = Clean(body)
	switch {
	case body == "":
		return Fact{}, false, ErrEmpty
	case utf8.RuneCountInString(body) > MaxLength:
		return Fact{}, false, ErrTooLong
	case Secret(body):
		return Fact{}, false, ErrSecret
	}
	var f Fact
	created := false
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := lockFacts(ctx, tx); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE facts SET source = 'told', updated_at = CASE WHEN source = 'told' THEN updated_at ELSE now() END
			WHERE lower(body) = lower($1)
			RETURNING id, body, source, updated_at, coalesce(message_id = (`+commandSQL+`), false)`, body, waID).
			Scan(&f.ID, &f.Text, &f.Source, &f.UpdatedAt, &created)
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := makeRoom(ctx, tx); err != nil {
			return err
		}
		created = true
		return tx.QueryRow(ctx, `INSERT INTO facts (body, source, message_id) VALUES ($1, 'told', (`+commandSQL+`))
			RETURNING id, body, source, updated_at`, body, waID).Scan(&f.ID, &f.Text, &f.Source, &f.UpdatedAt)
	})
	return f, created, err
}

// commandSQL is the messages.id of DJ's command with WhatsApp id $2, or null.
const commandSQL = `SELECT id FROM messages WHERE wa_id = $2 AND from_me AND $2 <> '' ORDER BY id DESC LIMIT 1`

// makeRoom makes room for one more fact by removing the learned fact changed longest ago when the
// facts are full, and returns ErrFull when every fact is one DJ told.
func makeRoom(ctx context.Context, tx pgx.Tx) error {
	var n int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM facts").Scan(&n); err != nil {
		return err
	}
	if n < MaxFacts {
		return nil
	}
	removed, err := tx.Exec(ctx, `DELETE FROM facts WHERE id = (
		SELECT id FROM facts WHERE source = 'learned' ORDER BY updated_at, id LIMIT 1)`)
	if err != nil {
		return err
	}
	if removed.RowsAffected() == 0 {
		return ErrFull
	}
	return nil
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
