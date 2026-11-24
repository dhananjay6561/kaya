package store

import (
	"errors"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsConnectionError reports whether err means the database could not be reached or dropped the
// connection, so the same work may succeed later. A refused value, a broken constraint, a bad query,
// a statement timeout or the caller's own deadline is not one: those are for the caller to judge.
func IsConnectionError(err error) bool {
	if err == nil {
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
