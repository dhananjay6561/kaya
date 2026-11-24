// Package events writes gateway events to the outbox that core reads.
package events

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/contracts"
)

// Outbox inserts events into gateway_events. Each insert runs inside the caller's transaction, so
// an event is stored exactly when the rows it describes are.
type Outbox struct {
	validator *contracts.Validator
}

// NewOutbox returns an outbox that validates every event against the contract before storing it.
func NewOutbox(validator *contracts.Validator) *Outbox {
	return &Outbox{validator: validator}
}

// orderLock is the advisory lock every transaction takes before it adds an event.
const orderLock = 5011602

// Add validates e and inserts it in tx. Core is woken once the transaction commits.
//
// A transaction that adds an event holds a lock from here until it ends, so events are numbered in
// the order their transactions commit. Without it a transaction that took a lower number but
// committed later would be read by core after a higher one.
func (o *Outbox) Add(ctx context.Context, tx pgx.Tx, e contracts.Event) error {
	raw, err := o.validator.EncodeEvent(e)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", e.Type, err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(orderLock)); err != nil {
		return fmt.Errorf("order %s event: %w", e.Type, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO gateway_events (id, type, payload) VALUES ($1, $2, $3)", e.ID, string(e.Type), raw); err != nil {
		return fmt.Errorf("store %s event: %w", e.Type, err)
	}
	return nil
}
