package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrLockBusy means another worker holds the lock.
var ErrLockBusy = errors.New("another worker holds the lock")

// Lock is a Postgres session advisory lock on a connection of its own, for one worker across every
// process: it is gone with its connection, so a crashed worker never blocks the next. It is not
// safe for concurrent use.
type Lock struct {
	db   *DB
	key  int64
	conn *pgx.Conn
}

// NewLock returns a lock on key that is not held yet.
func NewLock(db *DB, key int64) *Lock { return &Lock{db: db, key: key} }

// Hold takes the lock if this worker does not hold it yet, and checks that a held lock's connection
// still answers, taking it again if not. It returns ErrLockBusy when another worker holds it.
func (l *Lock) Hold(ctx context.Context) error {
	if l.conn != nil && !l.conn.IsClosed() {
		ping, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := l.conn.Ping(ping)
		cancel()
		if err == nil {
			return nil
		}
	}
	l.Release()
	conn, err := l.db.Hijack(ctx)
	if err != nil {
		return err
	}
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", l.key).Scan(&locked); err != nil {
		_ = conn.Close(context.Background())
		return err
	}
	if !locked {
		_ = conn.Close(context.Background())
		return ErrLockBusy
	}
	l.conn = conn
	return nil
}

// Release unlocks before closing the connection, so the next worker can take the lock at once
// instead of waiting for Postgres to notice the connection is gone.
func (l *Lock) Release() {
	if l.conn == nil {
		return
	}
	unlock, cancel := context.WithTimeout(context.Background(), time.Second)
	_, _ = l.conn.Exec(unlock, "SELECT pg_advisory_unlock($1)", l.key)
	cancel()
	_ = l.conn.Close(context.Background())
	l.conn = nil
}
