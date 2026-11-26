package store

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsConnectionError reports whether err means the database could not be reached or dropped the
// connection, so the same work may succeed later. A refused value, a broken constraint, a bad query,
// a statement timeout, the caller's own deadline or a connection still busy with rows the caller
// never closed is not one: those are for the caller to judge.
func IsConnectionError(err error) bool {
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		strings.Contains(err.Error(), "conn busy") {
		return false
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "57P01", "57P02", "57P03", "40001", "40P01":
			return true
		}
		return strings.HasPrefix(pg.Code, "08") || strings.HasPrefix(pg.Code, "53")
	}
	var connect *pgconn.ConnectError
	var network *net.OpError
	return errors.As(err, &connect) || errors.As(err, &network) || pgconn.SafeToRetry(err) ||
		strings.Contains(err.Error(), "conn closed") || strings.Contains(err.Error(), "closed pool")
}

// IsRefused reports whether err is a database error that trying again cannot fix: bad data, a
// broken constraint or a schema mismatch (SQLSTATE classes 22, 23 and 42).
func IsRefused(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || len(pg.Code) < 2 {
		return false
	}
	switch pg.Code[:2] {
	case "22", "23", "42":
		return true
	}
	return false
}
