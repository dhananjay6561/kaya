// Package events writes gateway events to the outbox that core reads.
package events

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
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

// Add validates e and inserts it in tx. Core is woken once the transaction commits.
func (o *Outbox) Add(ctx context.Context, tx pgx.Tx, e contracts.Event) error {
	raw, err := o.validator.EncodeEvent(e)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", e.Type, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO gateway_events (id, type, payload) VALUES ($1, $2, $3)", e.ID, string(e.Type), raw); err != nil {
		return fmt.Errorf("store %s event: %w", e.Type, err)
	}
	return nil
}
