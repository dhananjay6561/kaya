package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/testdb"
)

func TestPublishStoresAnEventOnceHoweverOftenItIsRepeated(t *testing.T) {
	gateway, admin := testdb.MigratedPair(t, "kaya_gateway")
	ctx := context.Background()
	v, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	pub := NewPublisher(gateway, NewOutbox(v))

	happened := time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)
	event, err := contracts.NewEvent(contracts.EventConnectionState, contracts.ConnectionState{
		State: contracts.StateDisconnected, OwnJID: "919999999999@s.whatsapp.net", Reason: "connection lost, reconnecting",
	})
	if err != nil {
		t.Fatal(err)
	}
	event.OccurredAt = contracts.At(happened)
	for range 3 {
		if err := pub.Publish(ctx, event); err != nil {
			t.Fatal(err)
		}
	}

	var rows int
	var id, state, occurred, reason string
	err = admin.Query(ctx, `
		SELECT count(*) OVER (), id::text, state, payload ->> 'occurred_at', payload -> 'data' ->> 'reason'
		FROM gateway_events`, nil, &rows, &id, &state, &occurred, &reason)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 || id != event.ID || state != "pending" || occurred != "2026-11-22T09:30:00Z" || reason != "connection lost, reconnecting" {
		t.Fatalf("got %d rows: %s %s %s %q", rows, id, state, occurred, reason)
	}
}

func TestPublishReportsAnInvalidEventAndOtherFailures(t *testing.T) {
	gateway := testdb.MigratedAs(t, "kaya_gateway")
	v, _ := contracts.NewValidator()
	pub := NewPublisher(gateway, NewOutbox(v))

	bad, _ := contracts.NewEvent(contracts.EventConnectionState, contracts.ConnectionState{State: "sleeping"})
	if err := pub.Publish(context.Background(), bad); !errors.Is(err, contracts.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}

	good, _ := contracts.NewEvent(contracts.EventConnectionState, contracts.ConnectionState{State: contracts.StateConnected})
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pub.Publish(gone, good); err == nil {
		t.Fatal("a write that did not happen must be reported")
	}
}
