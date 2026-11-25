package status

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/core/internal/gateway"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

var (
	now = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	bg  = context.Background()
)

func ago(d time.Duration) time.Time { return now.Add(-d) }

func TestDurationsReadAtAGlance(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                                     "under a minute",
		59*time.Second + 999*time.Millisecond: "under a minute",
		time.Minute:                           "1m",
		59 * time.Minute:                      "59m",
		133 * time.Minute:                     "2h 13m",
		3*24*time.Hour + 5*time.Hour:          "3d 5h",
	} {
		if got := Duration(d); got != want {
			t.Errorf("%v: got %q, want %q", d, got, want)
		}
	}
}

func healthy() Status {
	return Status{
		Version:      "8fc3488",
		StartedAt:    ago(133 * time.Minute),
		Now:          now,
		Connection:   &gateway.Connection{State: "connected", OccurredAt: ago(12 * time.Minute)},
		LastStoredAt: ago(2 * time.Minute),
	}
}

func matches(t *testing.T, text, pattern string) {
	t.Helper()
	if !regexp.MustCompile(`(?m)` + pattern).MatchString(text) {
		t.Fatalf("%q does not match %s", text, pattern)
	}
}

func TestSaysKayaIsUpConnectedAnsweringAndStoring(t *testing.T) {
	want := strings.Join([]string{
		"Kaya 8fc3488, up 2h 13m.",
		"WhatsApp: connected for 12m.",
		"Answering messages.",
		"Last incoming message stored 2m ago.",
	}, "\n")
	if got := Format(healthy()); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestSaysHowLongKayaHasBeenPausedAndHowToUndoIt(t *testing.T) {
	st := healthy()
	st.Pause = pause.State{Paused: true, Since: ago(5 * time.Minute)}
	text := Format(st)
	matches(t, text, `^Paused for 5m\. Send /resume to start again\.$`)
	if strings.Contains(text, "Answering") {
		t.Fatal(text)
	}
}

func TestNamesTroubleOnlyWhenThereIsSome(t *testing.T) {
	if text := Format(healthy()); strings.Contains(text, "Events") {
		t.Fatal(text)
	}
	st := healthy()
	st.Connection = &gateway.Connection{State: "disconnected", Reason: "connection lost, reconnecting", OccurredAt: ago(3 * time.Minute)}
	st.EventsWaiting, st.EventsDead = 4, 2
	text := Format(st)
	matches(t, text, `^WhatsApp: disconnected for 3m \(connection lost, reconnecting\)\.$`)
	matches(t, text, `^Events waiting for core: 4\.$`)
	matches(t, text, `^Events core gave up on: 2\.$`)
}

func TestCopesWithAFreshInstallThatHasHeardNothingYet(t *testing.T) {
	st := healthy()
	st.Connection, st.LastStoredAt = nil, time.Time{}
	text := Format(st)
	matches(t, text, `^WhatsApp: no report from the gateway yet\.$`)
	matches(t, text, `^No incoming messages stored yet\.$`)
}

func TestNeverShowsANegativeAgeWhenClocksDisagree(t *testing.T) {
	st := healthy()
	st.LastStoredAt = ago(-5 * time.Minute)
	matches(t, Format(st), `stored under a minute ago`)
}

func TestReadsWhatCoreIsAllowedToRead(t *testing.T) {
	core, admin := testdb.MigratedPair(t, "kaya_core", store.WithAppName("kaya-core"))
	startedAt := time.Now()
	empty, err := Read(bg, Source{DB: core, Pause: pause.NewStored(core), Version: "test", StartedAt: startedAt})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Connection != nil || !empty.LastStoredAt.IsZero() || empty.Pause.Paused || empty.EventsWaiting != 0 {
		t.Fatalf("got %+v", empty)
	}

	one := func(sql string, args ...any) int64 {
		var id int64
		if err := admin.QueryRow(bg, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		if _, err := admin.Exec(bg, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	own := one("SELECT contact_id_for('100000000000001@lid', '919999999999@s.whatsapp.net')")
	chat := one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", own)
	friend := one("SELECT contact_id_for('100000000000002@lid', '919888888888@s.whatsapp.net')")
	exec(`INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, created_at)
	      VALUES ($1, '3EB0INCOMING', $3, false, now(), 'text', 'hi', 'live', now() - interval '2 hours'),
	             ($1, '3EB0STATUS', $2, true, now(), 'text', '/status', 'live', now())`, chat, own, friend)
	exec("INSERT INTO connection_state (state, own_jid, occurred_at) VALUES ('connected', '919999999999@s.whatsapp.net', now())")
	id := contracts.NewID()
	payload, _ := json.Marshal(map[string]string{"id": id, "type": "history.synced"})
	exec("INSERT INTO gateway_events (id, type, payload) VALUES ($1, 'history.synced', $2)", id, string(payload))

	full, err := Read(bg, Source{DB: core, Pause: &pause.Memory{}, Version: "test", StartedAt: startedAt})
	if err != nil {
		t.Fatal(err)
	}
	if full.Connection == nil || full.Connection.State != "connected" {
		t.Fatalf("got %+v", full.Connection)
	}
	if age := time.Since(full.LastStoredAt); age < 110*time.Minute || age > 130*time.Minute {
		t.Fatalf("the owner's own messages, like the /status itself, do not count: %v", age)
	}
	if full.EventsWaiting != 1 || full.EventsDead != 0 {
		t.Fatalf("got %d waiting, %d dead", full.EventsWaiting, full.EventsDead)
	}
}

func TestShowsHowMuchOfTheHistoryIsSearchable(t *testing.T) {
	if text := Format(healthy()); strings.Contains(text, "Search index") {
		t.Fatal(text)
	}
	st := healthy()
	st.Indexed, st.Searchable = 98123, 102000
	matches(t, Format(st), `^Search index: 98123 of 102000 messages \(96%\)\.$`)
}

func TestReadsTheIndexAndLeavesItOutWhenItCannot(t *testing.T) {
	core, _ := testdb.MigratedPair(t, "kaya_core")
	src := Source{DB: core, Pause: &pause.Memory{}, Version: "test", StartedAt: time.Now(),
		Index: func(context.Context) (int, int, error) { return 5, 8, nil }}
	st, err := Read(bg, src)
	if err != nil || st.Indexed != 5 || st.Searchable != 8 {
		t.Fatalf("got %+v, %v", st, err)
	}
	src.Index = func(context.Context) (int, int, error) { return 0, 0, errors.New("index away") }
	if st, err := Read(bg, src); err != nil || st.Searchable != 0 {
		t.Fatalf("a failing index broke the status: %+v, %v", st, err)
	}
}
