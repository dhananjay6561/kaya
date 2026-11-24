package gateway

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/kit/store"
)

func handleOne(t *testing.T, core *store.DB, events *Events, e contracts.Event) error {
	t.Helper()
	var after AfterCommit
	err := core.InTx(context.Background(), func(tx pgx.Tx) error {
		var err error
		after, err = events.Handle(context.Background(), tx, e)
		return err
	})
	if err == nil && after != nil {
		after()
	}
	return err
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestKeepsTheLatestConnectionStateWhateverTheOrder(t *testing.T) {
	core, _ := databases(t)
	events := NewEvents(quiet)
	ctx := context.Background()
	if c, err := ReadConnection(ctx, core); err != nil || c != nil {
		t.Fatalf("want no state yet, got %v %v", c, err)
	}
	own := "919999999999@s.whatsapp.net"
	for _, e := range []contracts.Event{
		event(t, contracts.EventConnectionState, contracts.ConnectionState{State: "connected", OwnJID: own}, at("2026-11-22T10:00:00Z")),
		event(t, contracts.EventConnectionState, contracts.ConnectionState{State: "disconnected", OwnJID: own, Reason: "connection lost, reconnecting"}, at("2026-11-22T10:05:00Z")),
		event(t, contracts.EventConnectionState, contracts.ConnectionState{State: "connecting", OwnJID: own}, at("2026-11-22T09:00:00Z")),
	} {
		if err := handleOne(t, core, events, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadConnection(ctx, core)
	if err != nil {
		t.Fatal(err)
	}
	want := Connection{State: "disconnected", Reason: "connection lost, reconnecting", OwnJID: own, OccurredAt: at("2026-11-22T10:05:00Z")}
	if got == nil || got.State != want.State || got.Reason != want.Reason || got.OwnJID != want.OwnJID || !got.OccurredAt.Equal(want.OccurredAt) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	if err := handleOne(t, core, events, event(t, contracts.EventConnectionState, contracts.ConnectionState{State: "disconnected", Reason: "session store unavailable"}, at("2026-11-22T10:30:00Z"))); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadConnection(ctx, core); got.OwnJID != own {
		t.Fatal("a state that does not name the account does not make core forget whose it is")
	}

	if err := handleOne(t, core, events, event(t, contracts.EventConnectionState, contracts.ConnectionState{State: "logged_out"}, at("2026-11-22T11:00:00Z"))); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadConnection(ctx, core); got.State != "logged_out" || got.OwnJID != "" || got.Reason != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestTellsWhoeverWaitsForASendAndNobodyElse(t *testing.T) {
	core, _ := databases(t)
	events := NewEvents(quiet)
	mine := contracts.NewID()
	var got []string
	forget := events.OnSendResult(mine, func(r contracts.SendResult) { got = append(got, r.Status+" "+r.WAID) })
	events.OnSendResult(contracts.NewID(), func(contracts.SendResult) { got = append(got, "someone else's") })

	result := event(t, contracts.EventSendResult, contracts.SendResult{RequestID: mine, Status: "sent", WAID: "3EB0AAAA"}, time.Now())
	var after AfterCommit
	err := core.InTx(context.Background(), func(tx pgx.Tx) error {
		var err error
		after, err = events.Handle(context.Background(), tx, result)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatal("nobody is told before the event is committed as handled")
	}
	after()
	if !slices.Equal(got, []string{"sent 3EB0AAAA"}) {
		t.Fatalf("got %v", got)
	}

	forget()
	if err := handleOne(t, core, events, result); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"sent 3EB0AAAA"}) {
		t.Fatalf("a forgotten listener was told again: %v", got)
	}
}

func TestHandsEachStoredMessageToItsListenersInsideTheTransaction(t *testing.T) {
	core, _ := databases(t)
	events := NewEvents(quiet)
	var seen []int64
	off := events.OnMessage(func(ctx context.Context, tx pgx.Tx, m contracts.MessageStored) (AfterCommit, error) {
		var one int64
		if err := tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return nil, err
		}
		seen = append(seen, m.MessageID*one)
		return nil, nil
	})
	stored := func(id int64) contracts.Event {
		return event(t, contracts.EventMessageStored, contracts.MessageStored{
			MessageID: id, ChatID: 1, SenderID: 1, WAID: "3EB0BBBB", Kind: "text", Source: "live", Change: "new",
			SentAt: contracts.At(at("2026-11-22T10:00:00Z")),
		}, time.Now())
	}
	if err := handleOne(t, core, events, stored(7)); err != nil {
		t.Fatal(err)
	}
	off()
	if err := handleOne(t, core, events, stored(8)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []int64{7}) {
		t.Fatalf("got %v", seen)
	}

	bug := errors.New("listener bug")
	events.OnMessage(func(context.Context, pgx.Tx, contracts.MessageStored) (AfterCommit, error) { return nil, bug })
	if err := handleOne(t, core, events, stored(9)); !errors.Is(err, bug) {
		t.Fatalf("want the listener's error, got %v", err)
	}
}
