// Package status answers /status: whether Kaya is up, connected, answering and keeping up.
package status

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/core/internal/facts"
	"github.com/dhananjay6561/kaya/core/internal/gateway"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/kit/store"
)

// Status is what /status reports.
type Status struct {
	Version       string
	StartedAt     time.Time
	Now           time.Time
	Pause         pause.State
	Connection    *gateway.Connection
	LastStoredAt  time.Time
	EventsWaiting int
	EventsDead    int
	// Indexed and Searchable are how many messages are in the search index and could be; both zero
	// when the index could not be read.
	Indexed    int
	Skipped    int
	Searchable int
	// Facts and FactsTold are how many facts Kaya keeps about DJ and how many he told her.
	Facts     int
	FactsTold int
	// Tagging is set when tagging is on: how many new messages wait for tags and how many tasks
	// are open.
	Tagging *Tagging
}

// Tagging is how tagging stands.
type Tagging struct {
	Waiting   int
	OpenTasks int
}

// Source is where a status is read from.
type Source struct {
	DB        *store.DB
	Pause     pause.Switch
	Version   string
	StartedAt time.Time
	// Index reports the search index's progress; nil leaves the line out.
	Index func(ctx context.Context) (indexed, skipped, total int, err error)
	// Facts counts the facts kept and how many DJ told; nil leaves the line out.
	Facts func(ctx context.Context) (all, told int, err error)
	// Tagging reports what waits for tags and how many tasks are open; nil leaves the line out.
	Tagging func(ctx context.Context) (waiting, openTasks int, err error)
}

// Read gathers the status.
func Read(ctx context.Context, src Source) (Status, error) {
	st := Status{Version: src.Version, StartedAt: src.StartedAt}
	var err error
	if st.Pause, err = src.Pause.State(ctx); err != nil {
		return Status{}, err
	}
	if st.Connection, err = gateway.ReadConnection(ctx, src.DB); err != nil {
		return Status{}, err
	}
	var last *time.Time
	err = src.DB.QueryRow(ctx, "SELECT created_at FROM messages WHERE NOT from_me ORDER BY id DESC LIMIT 1").Scan(&last)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Status{}, err
	}
	if last != nil {
		st.LastStoredAt = *last
	}
	err = src.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM gateway_events WHERE state = 'pending'),
	                                   (SELECT count(*) FROM gateway_events WHERE state = 'dead')`).
		Scan(&st.EventsWaiting, &st.EventsDead)
	if err != nil {
		return Status{}, err
	}
	if src.Index != nil {
		if indexed, skipped, total, err := src.Index(ctx); err == nil {
			st.Indexed, st.Skipped, st.Searchable = indexed, skipped, total
		}
	}
	if src.Facts != nil {
		if all, told, err := src.Facts(ctx); err == nil {
			st.Facts, st.FactsTold = all, told
		}
	}
	if src.Tagging != nil {
		if waiting, open, err := src.Tagging(ctx); err == nil {
			st.Tagging = &Tagging{Waiting: waiting, OpenTasks: open}
		}
	}
	st.Now = time.Now()
	return st, nil
}

// Duration writes d the way a person reads it at a glance.
func Duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	day := 24 * time.Hour
	return fmt.Sprintf("%dd %dh", int(d/day), int(d%day/time.Hour))
}

// Format writes the status as /status answers it.
func Format(st Status) string {
	since := func(at time.Time) string { return Duration(st.Now.Sub(at)) }
	lines := []string{fmt.Sprintf("Kaya %s, up %s.", st.Version, since(st.StartedAt))}
	if c := st.Connection; c != nil {
		reason := ""
		if c.Reason != "" {
			reason = " (" + c.Reason + ")"
		}
		lines = append(lines, fmt.Sprintf("WhatsApp: %s for %s%s.", c.State, since(c.OccurredAt), reason))
	} else {
		lines = append(lines, "WhatsApp: no report from the gateway yet.")
	}
	if st.Pause.Paused {
		lines = append(lines, fmt.Sprintf("Paused for %s. Send /resume to start again.", since(st.Pause.Since)))
	} else {
		lines = append(lines, "Answering messages.")
	}
	if !st.LastStoredAt.IsZero() {
		lines = append(lines, fmt.Sprintf("Last incoming message stored %s ago.", since(st.LastStoredAt)))
	} else {
		lines = append(lines, "No incoming messages stored yet.")
	}
	if st.Searchable > 0 {
		line := fmt.Sprintf("Search index: %d of %d messages (%d%%)", st.Indexed, st.Searchable, st.Indexed*100/st.Searchable)
		if st.Skipped > 0 {
			line += fmt.Sprintf(", %d the embedding service would not take", st.Skipped)
		}
		lines = append(lines, line+".")
	}
	if st.Facts > 0 {
		line := fmt.Sprintf("Facts about you: %d, %d of them you told me", st.Facts, st.FactsTold)
		if st.Facts >= facts.MaxFacts {
			line += " (the most I keep)"
		}
		lines = append(lines, line+".")
	}
	if tg := st.Tagging; tg != nil {
		line := fmt.Sprintf("Open tasks: %d", tg.OpenTasks)
		if tg.Waiting > 0 {
			line += fmt.Sprintf("; %d new messages waiting for tags", tg.Waiting)
		}
		lines = append(lines, line+".")
	}
	if st.EventsWaiting > 0 {
		lines = append(lines, fmt.Sprintf("Events waiting for core: %d.", st.EventsWaiting))
	}
	if st.EventsDead > 0 {
		lines = append(lines, fmt.Sprintf("Events core gave up on: %d.", st.EventsDead))
	}
	return strings.Join(lines, "\n")
}
