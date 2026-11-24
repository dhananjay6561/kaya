// Package testdb gives gateway tests a throwaway Postgres database, opened with the gateway's
// connection settings. Tests that use it are skipped unless TEST_DATABASE_URL is set.
package testdb

import (
	"testing"

	"github.com/dhananjay6561/kaya/kit/store"
	kit "github.com/dhananjay6561/kaya/kit/testdb"
)

// Settings are the pool options the gateway itself uses.
var Settings = []store.Option{store.WithAppName("kaya-gateway"), store.WithSearchPath("whatsmeow,public")}

// New returns a pool on a fresh, empty database that is dropped when the test ends.
func New(t *testing.T) *store.DB {
	t.Helper()
	return kit.New(t, Settings...)
}

// Migrated is New with every migration in db/migrations applied.
func Migrated(t *testing.T) *store.DB {
	t.Helper()
	return kit.Migrated(t, Settings...)
}

// MigratedAs is Migrated, with every connection acting as role.
func MigratedAs(t *testing.T, role string) *store.DB {
	t.Helper()
	return kit.MigratedAs(t, role, Settings...)
}

// MigratedPair returns two pools on one migrated database: service acts as role, admin as the test login.
func MigratedPair(t *testing.T, role string) (service, admin *store.DB) {
	t.Helper()
	return kit.MigratedPair(t, role, Settings...)
}
