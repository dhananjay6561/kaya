package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/testdb"
)

func TestAddStoresTheEventWithItsTransaction(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	v, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	outbox := NewOutbox(v)

	committed, err := contracts.NewEvent(contracts.EventConnectionState, contracts.ConnectionState{State: contracts.StateConnected})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InTx(ctx, func(tx pgx.Tx) error { return outbox.Add(ctx, tx, committed) }); err != nil {
		t.Fatal(err)
	}

	rolledBack, _ := contracts.NewEvent(contracts.EventConnectionState, contracts.ConnectionState{State: contracts.StateConnecting})
	boom := errors.New("the rows it describes failed to store")
	err = db.InTx(ctx, func(tx pgx.Tx) error {
		if err := outbox.Add(ctx, tx, rolledBack); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}

	err = db.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT id::text, type, state, payload FROM gateway_events ORDER BY seq")
		if err != nil {
			return err
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id, typ, state string
			var payload json.RawMessage
			if err := rows.Scan(&id, &typ, &state, &payload); err != nil {
				return err
			}
			if _, err := v.DecodeEvent(payload); err != nil {
				t.Fatalf("stored payload is not a valid event: %v", err)
			}
			got = append(got, id+" "+typ+" "+state)
		}
		if len(got) != 1 || got[0] != committed.ID+" connection.state pending" {
			t.Fatalf("want only the committed event, pending; got %v", got)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAddRefusesAnInvalidEvent(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	v, _ := contracts.NewValidator()
	bad, err := contracts.NewEvent(contracts.EventConnectionState, contracts.ConnectionState{State: "sleeping"})
	if err != nil {
		t.Fatal(err)
	}
	err = db.InTx(ctx, func(tx pgx.Tx) error { return NewOutbox(v).Add(ctx, tx, bad) })
	if !errors.Is(err, contracts.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestEventsAreNumberedInTheOrderTheirTransactionsCommit(t *testing.T) {
	db := testdb.Migrated(t)
	ctx := context.Background()
	v, _ := contracts.NewValidator()
	outbox := NewOutbox(v)
	first, _ := contracts.NewEvent(contracts.EventConnectionState, contracts.ConnectionState{State: contracts.StateConnecting})
	second, _ := contracts.NewEvent(contracts.EventConnectionState, contracts.ConnectionState{State: contracts.StateConnected})

	added, release := make(chan struct{}), make(chan struct{})
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() {
		firstDone <- db.InTx(ctx, func(tx pgx.Tx) error {
			if err := outbox.Add(ctx, tx, first); err != nil {
				return err
			}
			close(added)
			<-release
			return nil
		})
	}()
	<-added
	go func() {
		secondDone <- db.InTx(ctx, func(tx pgx.Tx) error { return outbox.Add(ctx, tx, second) })
	}()

	select {
	case err := <-secondDone:
		t.Fatalf("a later event was stored while an earlier one was still uncommitted, so core could read it first (%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}

	var order string
	if err := db.Query(ctx, "SELECT string_agg(payload -> 'data' ->> 'state', ' ' ORDER BY seq) FROM gateway_events", nil, &order); err != nil || order != "connecting connected" {
		t.Fatalf("got %q %v", order, err)
	}
}
