package whatsapp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/dhananjay6561/kaya/gateway/internal/store"
	"github.com/dhananjay6561/kaya/gateway/internal/testdb"
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
	db := testdb.New(t)
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

func TestTheGatewayRoleKeepsSessionsInItsOwnSchema(t *testing.T) {
	db := testdb.MigratedAs(t, "kaya_gateway")
	ctx := context.Background()
	sqlDB := db.SQL()
	defer func() { _ = sqlDB.Close() }()

	sessions := NewSessions(sqlDB, waLog.Noop)
	if _, err := sessions.Load(ctx); err != nil {
		t.Fatal(err)
	}
	savePaired(t, sessions, types.NewADJID("19995550100", 0, 7))

	var inWhatsmeow, inPublic int
	err := sqlDB.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE schemaname = 'whatsmeow'),
		count(*) FILTER (WHERE schemaname = 'public')
		FROM pg_tables WHERE tablename LIKE 'whatsmeow\_%'`).Scan(&inWhatsmeow, &inPublic)
	if err != nil {
		t.Fatal(err)
	}
	if inWhatsmeow == 0 || inPublic != 0 {
		t.Fatalf("whatsmeow tables: %d in its schema, %d in public", inWhatsmeow, inPublic)
	}
}

func TestCoreCannotReadTheSession(t *testing.T) {
	gateway := testdb.MigratedAs(t, "kaya_gateway")
	ctx := context.Background()
	sqlDB := gateway.SQL()
	defer func() { _ = sqlDB.Close() }()
	if _, err := NewSessions(sqlDB, waLog.Noop).Load(ctx); err != nil {
		t.Fatal(err)
	}

	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "RESET ROLE; SET ROLE kaya_core"); err != nil {
		t.Fatal(err)
	}
	_, err = conn.ExecContext(ctx, "SELECT count(*) FROM whatsmeow.whatsmeow_device")
	if err == nil || !strings.Contains(err.Error(), "42501") {
		t.Fatalf("want permission denied (42501), got %v", err)
	}
	_, _ = conn.ExecContext(ctx, "RESET ROLE; SET ROLE kaya_gateway")
}
