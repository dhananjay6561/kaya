package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/agent"
	"github.com/dhananjay6561/kaya/core/internal/jobs"
	"github.com/dhananjay6561/kaya/core/internal/text"
)

// JobBook keeps reminders and scheduled jobs.
type JobBook interface {
	Create(ctx context.Context, j jobs.Job, createdBy int64) (jobs.Job, bool, error)
	List(ctx context.Context) ([]jobs.Job, error)
	Cancel(ctx context.Context, ids []int64) ([]jobs.Job, error)
}

var weekdayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// SetReminder sets a reminder or a scheduled job from what DJ asked. The model only reads his
// words into a time and a repeat; code works out the exact time, and the line DJ sees under the
// answer is written here, so what he reads is when it will fire.
func SetReminder(book JobBook, zone *time.Location, now func() time.Time) agent.Tool {
	return agent.Tool{
		Name: "set_reminder",
		Description: "Set a reminder or a scheduled message for the owner: once, or repeating. It fires in their own chat " +
			"at exactly that time. Use it whenever they ask to be reminded, woken, nudged or sent something at a time " +
			"(\"remind me at 6 to call Shaurya\", \"kal subah 8 baje yaad dilana\", \"every weekday at 9 send me my tasks\"). " +
			"Give at as their local date and time, worked out from now and their words; for \"in 20 minutes\" give in_minutes " +
			"instead. If they give no time at all, ask them. Set ask to true when at that time they want you to do or " +
			"answer something (send their tasks, a summary, the time in New York), false for a plain reminder.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"text": {"type": "string", "description": "What to remind them of, short, in their words; for ask, what to do then."},
				"at": {"type": "string", "pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}$", "description": "Their local date and time it fires first, YYYY-MM-DD HH:MM."},
				"in_minutes": {"type": "integer", "minimum": 1, "maximum": 525600, "description": "Fires this many minutes from now, instead of at."},
				"repeat": {"type": "string", "enum": ["none", "hourly", "daily", "weekdays", "weekly", "monthly"], "description": "How it repeats; none for once."},
				"every": {"type": "integer", "minimum": 1, "maximum": 365, "description": "Every how many hours, days, weeks or months; 1 when left out."},
				"weekdays": {"type": "array", "items": {"type": "string", "enum": ["sun", "mon", "tue", "wed", "thu", "fri", "sat"]}, "description": "For weekly: the days it fires on."},
				"until": {"type": "string", "pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2}$", "description": "The last day it may fire, YYYY-MM-DD, if they said."},
				"ask": {"type": "boolean", "description": "true to do or answer the text when it fires, false for a plain reminder."}
			},
			"required": ["text"],
			"additionalProperties": false
		}`),
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Text      string   `json:"text"`
				At        string   `json:"at"`
				InMinutes int      `json:"in_minutes"`
				Repeat    string   `json:"repeat"`
				Every     int      `json:"every"`
				Weekdays  []string `json:"weekdays"`
				Until     string   `json:"until"`
				Ask       bool     `json:"ask"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			j, err := plan(args.Text, args.At, args.InMinutes, args.Repeat, args.Every, args.Weekdays, args.Until, args.Ask, zone, now())
			if err != nil {
				return "", err
			}
			made, created, err := book.Create(ctx, j, 0)
			if err != nil {
				return "", err
			}
			when := jobs.Describe(made.NextAt, made.Repeat, zone, now())
			agent.AddNote(ctx, "⏰ "+when+": "+made.Text)
			if !created {
				return fmt.Sprintf("That was already set (#%d) for %s. Tell them it is set; the exact time is shown under your answer.", made.ID, when), nil
			}
			return fmt.Sprintf("Set (#%d) for %s, %s from now. Tell them in a few words; the exact time is shown under your answer, so do not restate it differently.",
				made.ID, when, untilText(made.NextAt.Sub(now()))), nil
		},
	}
}

// plan works out a job from what the model read in DJ's words.
func plan(what, at string, inMinutes int, repeat string, every int, weekdays []string, until string, ask bool, zone *time.Location, now time.Time) (jobs.Job, error) {
	what = text.Clip(strings.TrimSpace(what), 400)
	if what == "" {
		return jobs.Job{}, agent.Problemf("say what to remind them of")
	}
	if repeat == "" {
		repeat = jobs.Once
	}
	r := jobs.Repeat{Kind: repeat, Every: max(every, 1)}
	for _, d := range weekdays {
		if i := slices.Index(weekdayNames, d); i >= 0 && !slices.Contains(r.Days, time.Weekday(i)) {
			r.Days = append(r.Days, time.Weekday(i))
		}
	}
	slices.Sort(r.Days)
	if repeat != jobs.Weekly {
		r.Days = nil
	}
	var first time.Time
	switch {
	case inMinutes > 0 && at != "":
		return jobs.Job{}, agent.Problemf("give either at or in_minutes, not both")
	case inMinutes > 0:
		first = now.Add(time.Duration(inMinutes) * time.Minute).Truncate(time.Second)
	case at != "":
		day, err := time.ParseInLocation("2006-01-02", at[:10], zone)
		if err != nil {
			return jobs.Job{}, agent.Problemf("%q is not a date; write YYYY-MM-DD HH:MM", at)
		}
		if first, err = jobs.At(day, at[11:], zone); err != nil {
			return jobs.Job{}, agent.Problemf("%q is not a time; write YYYY-MM-DD HH:MM", at)
		}
	default:
		return jobs.Job{}, agent.Problemf("give the time: at (their local YYYY-MM-DD HH:MM) or in_minutes")
	}
	clock := first.In(zone).Format("15:04")
	if repeat == jobs.Weekly && len(r.Days) > 0 && !slices.Contains(r.Days, first.In(zone).Weekday()) {
		for d := 1; d <= 7; d++ {
			day := first.In(zone).AddDate(0, 0, d)
			if slices.Contains(r.Days, day.Weekday()) {
				first, _ = jobs.At(day, clock, zone)
				break
			}
		}
	}
	if repeat == jobs.Monthly {
		r.MonthDay = first.In(zone).Day()
	}
	if until != "" {
		u, err := time.ParseInLocation("2006-01-02", until, zone)
		if err != nil {
			return jobs.Job{}, agent.Problemf("%q is not a date; write YYYY-MM-DD", until)
		}
		r.Until = u
	}
	if !first.After(now) {
		if repeat == jobs.Once {
			return jobs.Job{}, agent.Problemf("%s has already passed (it is now %s); if they meant the next one, use that date, and ask if unsure",
				first.In(zone).Format("Mon 2 Jan 15:04"), now.In(zone).Format("Mon 2 Jan 15:04"))
		}
		next := first
		for !next.After(now) {
			var err error
			if next, err = jobs.Next(next, clock, r, zone); err != nil {
				return jobs.Job{}, agent.Problemf("with that end date it would never fire")
			}
		}
		first = next
	}
	if !r.Until.IsZero() && first.In(zone).Format("2006-01-02") > r.Until.Format("2006-01-02") {
		return jobs.Job{}, agent.Problemf("it would first fire after its end date")
	}
	return jobs.Job{Text: what, Ask: ask, Repeat: r, Clock: clock, NextAt: first}, nil
}

func untilText(d time.Duration) string {
	d = d.Round(time.Minute)
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// ListReminders lists the reminders and scheduled jobs that are set, with their numbers.
func ListReminders(book JobBook, zone *time.Location, now func() time.Time) agent.Tool {
	return agent.Tool{
		Name:        "list_reminders",
		Description: "The owner's reminders and scheduled messages that are set, soonest first, each with its number for cancel_reminder.",
		Parameters:  json.RawMessage(`{"type": "object", "properties": {}, "additionalProperties": false}`),
		Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			list, err := book.List(ctx)
			if err != nil {
				return "", err
			}
			if len(list) == 0 {
				return "No reminders are set.", nil
			}
			lines := make([]string, len(list))
			for i, j := range list {
				lines[i] = fmt.Sprintf("#%d %s: %s", j.ID, jobs.Describe(j.NextAt, j.Repeat, zone, now()), j.Text)
			}
			return strings.Join(lines, "\n"), nil
		},
	}
}

// CancelReminder stops a reminder or scheduled job by its number.
func CancelReminder(book JobBook, zone *time.Location, now func() time.Time) agent.Tool {
	return agent.Tool{
		Name:        "cancel_reminder",
		Description: "Stop a reminder or scheduled message by its number from list_reminders. List them first if you do not know the number.",
		Parameters:  json.RawMessage(`{"type": "object", "properties": {"id": {"type": "integer", "minimum": 1}}, "required": ["id"], "additionalProperties": false}`),
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			gone, err := book.Cancel(ctx, []int64{args.ID})
			if err != nil {
				return "", err
			}
			if len(gone) == 0 {
				return "", agent.Problemf("no reminder #%d is set; list them to find the number", args.ID)
			}
			agent.AddNote(ctx, "🚫 Cancelled: "+gone[0].Text)
			return "Cancelled #" + fmt.Sprint(args.ID) + ": " + gone[0].Text + ".", nil
		},
	}
}
