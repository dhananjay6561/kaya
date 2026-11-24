package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsConnectionError(t *testing.T) {
	away := []error{
		&pgconn.PgError{Code: "08006"},
		&pgconn.PgError{Code: "57P01"},
		&pgconn.PgError{Code: "53300"},
		&pgconn.PgError{Code: "40001"},
		fmt.Errorf("query: %w", io.ErrUnexpectedEOF),
		&net.OpError{Op: "dial", Err: errors.New("connection refused")},
		context.DeadlineExceeded,
		errors.New("closed pool"),
	}
	for _, err := range away {
		if !IsConnectionError(err) {
			t.Errorf("%v should count as the database being away", err)
		}
	}
	for _, err := range []error{nil, &pgconn.PgError{Code: "23505"}, &pgconn.PgError{Code: "42P01"}, errors.New("bad input")} {
		if IsConnectionError(err) {
			t.Errorf("%v should not count as the database being away", err)
		}
	}
}

func TestIsConnectionErrorForAnUnreachableServer(t *testing.T) {
	db, err := Open("postgres://127.0.0.1:1/kaya?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.Check(ctx); !IsConnectionError(err) {
		t.Fatalf("an unreachable server should be a connection error, got %v", err)
	}
}
