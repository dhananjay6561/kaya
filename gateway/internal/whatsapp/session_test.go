package whatsapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/dhananjay6561/kaya/gateway/internal/store"
)

func savePaired(t *testing.T, s *Sessions, jid types.JID) {
	t.Helper()
	device := s.New()
	device.ID = &jid
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{1},
		AccountSignature:    make([]byte, 64),
		AccountSignatureKey: make([]byte, 32),
		DeviceSignature:     make([]byte, 64),
	}
	if err := device.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// testDB returns a pool whose search_path is a fresh schema, dropped when the test ends.
func testDB(t *testing.T) *store.DB {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	schema := "test_whatsapp_" + hex.EncodeToString(suffix)

	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := store.Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestSessionsWaitForTheDatabase(t *testing.T) {
	db, err := store.Open("postgres://127.0.0.1:1/kaya?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sqlDB := db.SQL()
	defer func() { _ = sqlDB.Close() }()

	sessions := NewSessions(sqlDB, waLog.Noop)
	if _, err := sessions.Load(context.Background()); err == nil {
		t.Fatal("want an error while the database is down")
	}
	if sessions.upgraded {
		t.Fatal("a failed upgrade must be retried on the next load")
	}
}

func TestSessionsAgainstRealPostgres(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	sqlDB := db.SQL()
	defer func() { _ = sqlDB.Close() }()

	sessions := NewSessions(sqlDB, waLog.Noop)
	if _, err := NewSessions(sqlDB, waLog.Noop).Load(ctx); err != nil {
		t.Fatal(err)
	}

	device, err := sessions.Load(ctx)
	if err != nil || device != nil {
		t.Fatalf("a fresh database has no session, got %v, %v", device, err)
	}

	jid := types.NewADJID("19995550100", 0, 7)
	savePaired(t, sessions, jid)

	reopened := NewSessions(sqlDB, waLog.Noop)
	device, err = reopened.Load(ctx)
	if err != nil || device == nil || device.ID == nil || *device.ID != jid {
		t.Fatalf("the session must survive a restart, got %v, %v", device, err)
	}

	savePaired(t, reopened, types.NewADJID("19995550101", 0, 3))
	if _, err := reopened.Load(ctx); !errors.Is(err, ErrSeveralSessions) {
		t.Fatalf("want ErrSeveralSessions, got %v", err)
	}
}
