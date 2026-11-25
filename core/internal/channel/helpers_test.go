package channel

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

var quiet = slog.New(slog.DiscardHandler)

const ownPN = "919999999999@s.whatsapp.net"

func databases(t *testing.T) (core, admin *store.DB) {
	t.Helper()
	return testdb.MigratedPair(t, "kaya_core", store.WithAppName("kaya-core"))
}

func validator(t *testing.T) *contracts.Validator {
	t.Helper()
	v, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func exec(t *testing.T, db *store.DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func column(t *testing.T, db *store.DB, sql string, args ...any) []string {
	t.Helper()
	var out []string
	err := db.InTx(context.Background(), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), sql, args...)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// result runs fn in the background and hands back its outcome.
type result[T any] struct {
	value T
	err   error
}

func background[T any](fn func() (T, error)) <-chan result[T] {
	out := make(chan result[T], 1)
	go func() {
		v, err := fn()
		out <- result[T]{v, err}
	}()
	return out
}

func await[T any](t *testing.T, ch <-chan result[T]) result[T] {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for a result")
	}
	return result[T]{}
}
