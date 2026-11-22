// Package testdb gives tests a throwaway Postgres database, optionally with the repo's migrations
// applied. Tests that use it are skipped unless TEST_DATABASE_URL is set.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/gateway/internal/store"
)

const migrationLock = 5011601

// New returns a pool on a fresh, empty database that is dropped when the test ends.
func New(t *testing.T) *store.DB {
	t.Helper()
	return open(t, false)
}

// Migrated is New with every migration in db/migrations applied.
func Migrated(t *testing.T) *store.DB {
	t.Helper()
	return open(t, true)
}

// MigratedAs is Migrated, with every connection acting as role (for example kaya_gateway), so a
// test runs with exactly the rights that service has.
func MigratedAs(t *testing.T, role string) *store.DB {
	t.Helper()
	return openAs(t, true, role)
}

// MigratedPair returns two pools on one migrated database: service acts as role, admin as the
// test login, for checking what the service wrote to tables it cannot read back.
func MigratedPair(t *testing.T, role string) (service, admin *store.DB) {
	t.Helper()
	service = openAs(t, true, role)
	u, err := url.Parse(service.URL())
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Del("role")
	u.RawQuery = q.Encode()
	admin, err = store.Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	return service, admin
}

func open(t *testing.T, migrate bool) *store.DB {
	t.Helper()
	return openAs(t, migrate, "")
}

func openAs(t *testing.T, migrate bool, role string) *store.DB {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "kaya_test_" + hex.EncodeToString(suffix)

	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	if migrate {
		// The migrations create the service roles, which every database on the server shares.
		// Tests in other packages run at the same time, so only one may migrate at once.
		if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
			t.Fatal(err)
		}
		applyMigrations(t, u.String())
		if _, err := admin.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrationLock); err != nil {
			t.Fatal(err)
		}
	}
	if role != "" {
		q := u.Query()
		q.Set("role", role)
		u.RawQuery = q.Encode()
	}
	db, err := store.Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func applyMigrations(t *testing.T, dsn string) {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	migrations := os.DirFS(filepath.Join(filepath.Dir(here), "..", "..", "..", "db", "migrations"))
	names, err := fs.Glob(migrations, "*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(names)

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, name := range names {
		raw, err := fs.ReadFile(migrations, name)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(raw), "-- migrate:down")
		if _, err := conn.Exec(ctx, up); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
}
