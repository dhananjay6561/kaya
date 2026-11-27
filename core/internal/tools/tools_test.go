package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/agent"
	"github.com/dhananjay6561/kaya/core/internal/tagging"
)

var bg = context.Background()

func kolkata(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func run(t *testing.T, tool agent.Tool, args string) (string, error) {
	t.Helper()
	if _, err := agent.NewRegistry(tool); err != nil {
		t.Fatal(err)
	}
	return tool.Run(bg, json.RawMessage(args))
}

func TestTheClockTellsTheTimeElsewhereNextToTheOwners(t *testing.T) {
	now := time.Date(2026, 11, 27, 7, 0, 0, 0, time.UTC)
	c := Clock(kolkata(t), func() time.Time { return now })
	got, err := run(t, c, `{"zones":["America/New_York","Asia/Tokyo","Asia/Kolkata","Mars/Base","Local"]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"The owner's zone, Asia/Kolkata: Fri 27 Nov 2026, 12:30 (UTC+05:30)",
		"America/New_York: Fri 27 Nov 2026, 02:00 (UTC-05:00), 10h30m behind the owner",
		"Asia/Tokyo: Fri 27 Nov 2026, 16:00 (UTC+09:00), 3h30m ahead of the owner",
		"Asia/Kolkata: Fri 27 Nov 2026, 12:30 (UTC+05:30), the same as the owner's",
		`"Mars/Base": not a time zone I know; use an IANA name such as Europe/Paris.`,
		`"Local": not a time zone I know; use an IANA name such as Europe/Paris.`,
	}, "\n")
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestTheClockConvertsAGivenTimeWithDaylightSaving(t *testing.T) {
	c := Clock(kolkata(t), time.Now)
	got, err := run(t, c, `{"zones":["Asia/Kolkata"],"at":"2026-07-01 21:00","from":"America/Los_Angeles"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := "Converting 2026-07-01 21:00 in America/Los_Angeles.\n" +
		"The owner's zone, Asia/Kolkata: Thu 2 Jul 2026, 09:30 (UTC+05:30)\n" +
		"Asia/Kolkata: Thu 2 Jul 2026, 09:30 (UTC+05:30), the same as the owner's"
	if got != want {
		t.Fatalf("got\n%s", got)
	}
	got, err = run(t, c, `{"zones":["America/New_York"],"at":"2026-03-08 02:30","from":"America/New_York"}`)
	if err != nil || !strings.Contains(got, "skipped there when the clocks go forward, so it is taken as 03:30") ||
		!strings.Contains(got, "America/New_York: Sun 8 Mar 2026, 03:30 (UTC-04:00), 9h30m behind the owner") {
		t.Fatalf("got %s, %v", got, err)
	}
	got, err = run(t, c, `{"zones":["UTC"],"at":"2026-11-01 01:30","from":"America/New_York"}`)
	if err != nil || !strings.Contains(got, "happens twice there when the clocks go back, so it is taken the first time, UTC-04:00") ||
		!strings.Contains(got, "UTC: Sun 1 Nov 2026, 05:30 (UTC+00:00)") {
		t.Fatalf("got %s, %v", got, err)
	}
	got, err = run(t, c, `{"zones":["UTC"],"at":"2026-11-01 03:30","from":"America/New_York"}`)
	if err != nil || strings.Contains(got, "twice") || !strings.Contains(got, "UTC: Sun 1 Nov 2026, 08:30") {
		t.Fatalf("got %s, %v", got, err)
	}
	got, err = run(t, c, `{"zones":["UTC"],"at":"2026-11-27 18:00"}`)
	if err != nil || !strings.Contains(got, "Converting 2026-11-27 18:00 in Asia/Kolkata.") || !strings.Contains(got, "UTC: Fri 27 Nov 2026, 12:30 (UTC+00:00), 5h30m behind") {
		t.Fatalf("got %s, %v", got, err)
	}
}

func TestTheClockSaysWhatItCouldNotRead(t *testing.T) {
	c := Clock(kolkata(t), time.Now)
	var problem *agent.Problem
	if _, err := run(t, c, `{"zones":["UTC"],"at":"2026-11-27 18:00","from":"Nowhere/City"}`); !errors.As(err, &problem) || !strings.Contains(problem.Message, "Nowhere/City") {
		t.Fatalf("err = %v", err)
	}
	if _, err := run(t, c, `{"zones":["UTC"],"at":"2026-13-45 99:00"}`); !errors.As(err, &problem) || !strings.Contains(problem.Message, "YYYY-MM-DD HH:MM") {
		t.Fatalf("err = %v", err)
	}
}

type book struct {
	open  []tagging.Task
	total int
	err   error
}

func (b book) Open(_ context.Context, limit int) ([]tagging.Task, error) {
	return b.open[:min(limit, len(b.open))], b.err
}
func (b book) CountOpen(context.Context) (int, error) { return b.total, b.err }

func TestOpenTasksListsWhatTasksShows(t *testing.T) {
	ist := kolkata(t)
	now := time.Date(2026, 11, 27, 7, 0, 0, 0, time.UTC)
	due := time.Date(2026, 11, 28, 10, 30, 0, 0, ist)
	tasks := []tagging.Task{
		{ID: 12, Kind: tagging.KindTask, Title: "Send the deck", Due: &due, DueHasTime: true, Chat: "Work", SentAt: now.Add(-time.Hour)},
		{ID: 13, Kind: tagging.KindReminder, Title: "Mom's birthday", Chat: "Family", SentAt: now.Add(-48 * time.Hour)},
	}
	clockNow := func() time.Time { return now }
	got, err := run(t, OpenTasks(book{open: tasks, total: 2}, ist, clockNow), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "12. Send the deck, due Sat 28 Nov 10:30 (Work, 27 Nov)\n13. Mom's birthday (Family, 25 Nov)" {
		t.Fatalf("got %q", got)
	}
	if got, err := run(t, OpenTasks(book{}, ist, clockNow), `{}`); err != nil || got != "No open tasks." {
		t.Fatalf("got %q, %v", got, err)
	}
	var many []tagging.Task
	for i := range 40 {
		many = append(many, tagging.Task{ID: int64(i + 1), Title: "t", Chat: "c", SentAt: now})
	}
	got, err = run(t, OpenTasks(book{open: many, total: 40}, ist, clockNow), `{}`)
	if err != nil || strings.Count(got, "\n") != 30 || !strings.HasSuffix(got, "(10 more not shown; /tasks lists more.)") {
		t.Fatalf("got %q, %v", got, err)
	}
	away := errors.New("db away")
	if _, err := run(t, OpenTasks(book{err: away}, ist, clockNow), `{}`); !errors.Is(err, away) {
		t.Fatalf("err = %v", err)
	}
}
