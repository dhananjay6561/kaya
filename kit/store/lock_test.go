package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLockLetsOneWorkerHoldItAndPassesItOn(t *testing.T) {
	db, err := Open(testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	key := time.Now().UnixNano()
	one, two := NewLock(db, key), NewLock(db, key)
	defer one.Release()
	defer two.Release()
	if err := one.Hold(ctx); err != nil {
		t.Fatal(err)
	}
	if err := one.Hold(ctx); err != nil {
		t.Fatalf("holding it again: %v", err)
	}
	if err := two.Hold(ctx); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("second worker: %v", err)
	}
	one.Release()
	if err := two.Hold(ctx); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestLockIsTakenAgainAfterItsConnectionIsLost(t *testing.T) {
	db, err := Open(testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	key := time.Now().UnixNano()
	l := NewLock(db, key)
	defer l.Release()
	if err := l.Hold(ctx); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := l.conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := l.Hold(ctx)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrLockBusy) || time.Now().After(deadline) {
			t.Fatalf("taking it again: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var newPid int
	if err := l.conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&newPid); err != nil || newPid == pid {
		t.Fatalf("still on the lost connection: %d, %v", newPid, err)
	}
}
