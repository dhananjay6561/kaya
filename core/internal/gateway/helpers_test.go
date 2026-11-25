package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

var quiet = slog.New(slog.DiscardHandler)

func validator(t *testing.T) *contracts.Validator {
	t.Helper()
	v, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// databases returns a pool acting as core's role and an admin pool on one fresh, migrated database.
func databases(t *testing.T) (core, admin *store.DB) {
	t.Helper()
	return testdb.MigratedPair(t, "kaya_core", store.WithAppName("kaya-core"))
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

func event(t *testing.T, typ contracts.EventType, data any, at time.Time) contracts.Event {
	t.Helper()
	e, err := contracts.NewEvent(typ, data)
	if err != nil {
		t.Fatal(err)
	}
	e.OccurredAt = contracts.At(at)
	return e
}

func connectionEvent(t *testing.T, state string) contracts.Event {
	t.Helper()
	return event(t, contracts.EventConnectionState, contracts.ConnectionState{State: state}, time.Now())
}

func exec(t *testing.T, db *store.DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func add(t *testing.T, admin *store.DB, e contracts.Event) {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, admin, "INSERT INTO gateway_events (id, type, payload) VALUES ($1, $2, $3)", e.ID, string(e.Type), raw)
}
