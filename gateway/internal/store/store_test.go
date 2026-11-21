package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func testURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return url
}

func TestOpenRejectsBadURL(t *testing.T) {
	if _, err := Open("postgres://%zz"); err == nil {
		t.Fatal("expected an error for a malformed URL")
	}
}

func TestCheckAgainstRealPostgres(t *testing.T) {
	db, err := Open(testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.Check(ctx); err != nil {
		t.Fatalf("check: %v", err)
	}
}

func TestCheckFailsWhenUnreachable(t *testing.T) {
	db, err := Open("postgres://127.0.0.1:1/kaya?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.Check(ctx); err == nil {
		t.Fatal("expected an error for an unreachable db")
	}
}
