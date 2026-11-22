package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// DeviceName is how Kaya shows up under Linked devices on the phone.
const DeviceName = "Kaya"

// ErrSeveralSessions means the session tables hold more than one linked device, which Kaya never creates.
var ErrSeveralSessions = errors.New("more than one WhatsApp session is stored")

var deviceProps sync.Once

// Sessions keeps the WhatsApp session in Postgres through whatsmeow's own tables (whatsmeow_*).
type Sessions struct {
	container *sqlstore.Container
}

// OpenSessions creates or upgrades whatsmeow's session tables in db.
func OpenSessions(ctx context.Context, db *sql.DB, log waLog.Logger) (*Sessions, error) {
	deviceProps.Do(func() {
		name := DeviceName
		store.DeviceProps.Os = &name
		store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
	})
	container := sqlstore.NewWithDB(db, "postgres", log)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("prepare whatsapp session tables: %w", err)
	}
	return &Sessions{container: container}, nil
}

// Load returns the linked device, or nil when Kaya has not been paired.
func (s *Sessions) Load(ctx context.Context) (*store.Device, error) {
	devices, err := s.container.GetAllDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("load whatsapp session: %w", err)
	}
	switch len(devices) {
	case 0:
		return nil, nil
	case 1:
		return devices[0], nil
	default:
		return nil, ErrSeveralSessions
	}
}

// New returns a fresh, unsaved device for pairing.
func (s *Sessions) New() *store.Device {
	return s.container.NewDevice()
}
