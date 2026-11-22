package events

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
)

const uniqueViolation = "23505"

// DB runs a function inside a transaction.
type DB interface {
	InTx(ctx context.Context, fn func(pgx.Tx) error) error
}

// Publisher stores events that describe no row of their own, such as connection.state, each in
// its own transaction.
type Publisher struct {
	db     DB
	outbox *Outbox
}

// NewPublisher returns a publisher that writes through outbox.
func NewPublisher(db DB, outbox *Outbox) *Publisher {
	return &Publisher{db: db, outbox: outbox}
}

// Publish stores e. Storing the same event again changes nothing, so a caller that could not tell
// whether an earlier attempt went through can simply repeat it.
func (p *Publisher) Publish(ctx context.Context, e contracts.Event) error {
	err := p.db.InTx(ctx, func(tx pgx.Tx) error { return p.outbox.Add(ctx, tx, e) })
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "gateway_events_id_key" {
		return nil
	}
	return err
}
