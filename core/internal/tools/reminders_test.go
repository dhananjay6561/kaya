package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/agent"
	"github.com/dhananjay6561/kaya/core/internal/jobs"
)

type jobBook struct{ jobs []jobs.Job }

func (b *jobBook) Create(_ context.Context, j jobs.Job, _ int64) (jobs.Job, bool, error) {
	for _, have := range b.jobs {
		if have.Text == j.Text && have.NextAt.Equal(j.NextAt) {
			return have, false, nil
		}
	}
	j.ID = int64(len(b.jobs) + 1)
	b.jobs = append(b.jobs, j)
	return j, true, nil
}
func (b *jobBook) List(context.Context) ([]jobs.Job, error) { return b.jobs, nil }
func (b *jobBook) Cancel(_ context.Context, ids []int64) ([]jobs.Job, error) {
	var gone []jobs.Job
	for i, j := range b.jobs {
		if len(ids) > 0 && j.ID == ids[0] {
			gone = append(gone, j)
			b.jobs = append(b.jobs[:i], b.jobs[i+1:]...)
			break
		}
	}
	return gone, nil
}

// noted runs a tool the way the agent loop does and returns its result and notes.
func noted(t *testing.T, tool agent.Tool, args string) (string, []string, error) {
	t.Helper()
	if _, err := agent.NewRegistry(tool); err != nil {
		t.Fatal(err)
	}
	ctx, notes := agent.CollectNotes(bg)
	result, err := tool.Run(ctx, json.RawMessage(args))
	return result, notes(), err
}

func TestSetReminderWorksOutTheExactTime(t *testing.T) {
	ist := kolkata(t)
	now := time.Date(2026, 11, 28, 15, 0, 30, 0, ist)
	clock := func() time.Time { return now }
	book := &jobBook{}
	set := SetReminder(book, ist, clock)
	cases := []struct {
		args, note string
	}{
		{`{"text":"Call Shaurya","at":"2026-11-28 18:00"}`, "⏰ Sat 28 Nov, 18:00: Call Shaurya"},
		{`{"text":"Check the oven","in_minutes":20}`, "⏰ Sat 28 Nov, 15:21: Check the oven"},
		{`{"text":"Stretch","at":"2026-11-28 09:00","repeat":"daily"}`, "⏰ Sun 29 Nov, 09:00, then every day: Stretch"},
		{`{"text":"Gym","at":"2026-11-28 07:00","repeat":"weekly","weekdays":["thu","mon"]}`, "⏰ Mon 30 Nov, 07:00, then every week on Mon, Thu: Gym"},
		{`{"text":"Pay rent","at":"2026-12-01 10:00","repeat":"monthly"}`, "⏰ Tue 1 Dec, 10:00, then every month: Pay rent"},
		{`{"text":"send me my open tasks","at":"2026-11-30 09:00","repeat":"weekdays","ask":true}`, "⏰ Mon 30 Nov, 09:00, then every weekday: send me my open tasks"},
		{`{"text":"standup notes","at":"2026-11-29 09:00","repeat":"weekdays"}`, "⏰ Mon 30 Nov, 09:00, then every weekday: standup notes"},
	}
	for _, c := range cases {
		result, notes, err := noted(t, set, c.args)
		if err != nil || len(notes) != 1 || notes[0] != c.note || !strings.HasPrefix(result, "Set (#") {
			t.Errorf("%s: result %q, notes %q, %v", c.args, result, notes, err)
		}
	}
	if !book.jobs[1].NextAt.Equal(now.Add(20*time.Minute).Round(time.Minute)) || book.jobs[4].Repeat.MonthDay != 1 || !book.jobs[5].Ask {
		t.Fatalf("jobs %+v", book.jobs)
	}
	if result, _, err := noted(t, set, `{"text":"Call Shaurya","at":"2026-11-28 18:00"}`); err != nil || !strings.HasPrefix(result, "That was already set") {
		t.Fatalf("again: %q, %v", result, err)
	}
	var problem *agent.Problem
	for args, want := range map[string]string{
		`{"text":"Lunch","at":"2026-11-28 13:00"}`:                                       "has already passed",
		`{"text":"Lunch"}`:                                                               "give the time",
		`{"text":"Lunch","at":"2026-11-28 18:00","in_minutes":5}`:                        "either at or in_minutes",
		`{"text":"Lunch","at":"2026-11-30 13:00","repeat":"daily","until":"2026-11-29"}`: "after its end date",
		`{"text":"  ","in_minutes":5}`:                                                   "say what to remind",
	} {
		if _, _, err := noted(t, set, args); !errors.As(err, &problem) || !strings.Contains(problem.Message, want) {
			t.Errorf("%s: err = %v", args, err)
		}
	}
}

func TestListAndCancelReminders(t *testing.T) {
	ist := kolkata(t)
	now := time.Date(2026, 11, 28, 15, 0, 0, 0, ist)
	clock := func() time.Time { return now }
	book := &jobBook{jobs: []jobs.Job{{ID: 7, Text: "Call Shaurya", Repeat: jobs.Repeat{Kind: jobs.Once}, NextAt: time.Date(2026, 11, 28, 18, 0, 0, 0, ist)}}}
	list, _, err := noted(t, ListReminders(book, ist, clock), `{}`)
	if err != nil || list != "#7 Sat 28 Nov, 18:00: Call Shaurya" {
		t.Fatalf("list %q, %v", list, err)
	}
	result, notes, err := noted(t, CancelReminder(book, ist, clock), `{"id":7}`)
	if err != nil || result != "Cancelled #7: Call Shaurya." || len(notes) != 1 || notes[0] != "🚫 Cancelled: Call Shaurya" {
		t.Fatalf("cancel %q %q %v", result, notes, err)
	}
	var problem *agent.Problem
	if _, _, err := noted(t, CancelReminder(book, ist, clock), `{"id":7}`); !errors.As(err, &problem) {
		t.Fatalf("cancel again: %v", err)
	}
	if list, _, _ := noted(t, ListReminders(book, ist, clock), `{}`); list != "No reminders are set." {
		t.Fatalf("list %q", list)
	}
}
