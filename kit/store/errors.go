package store

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// IsConnectionError reports whether err means the database could not be reached or dropped the
// connection, so the same work may succeed later. A refused value, a broken constraint or a bad
// query fails the same way every time and is not one.
func IsConnectionError(err error) bool {
	if err == nil {
		return false
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return strings.HasPrefix(pg.Code, "08") || strings.HasPrefix(pg.Code, "53") ||
			strings.HasPrefix(pg.Code, "57") || pg.Code == "40001" || pg.Code == "40P01"
	}
	var connect *pgconn.ConnectError
	var network net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, pgx.ErrTxClosed) ||
		errors.Is(err, pgx.ErrTxCommitRollback) || errors.As(err, &connect) || errors.As(err, &network) ||
		pgconn.SafeToRetry(err) || strings.Contains(err.Error(), "conn closed") ||
		strings.Contains(err.Error(), "closed pool")
}
