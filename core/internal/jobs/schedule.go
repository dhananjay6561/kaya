// Package jobs keeps DJ's reminders and scheduled jobs and fires each at its time in his own chat:
// a scheduler that sleeps until the next one is due and wakes when one is added or changed.
package jobs

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Ways a job repeats.
const (
	Once     = "none"
	Hourly   = "hourly"
	Daily    = "daily"
	Weekdays = "weekdays"
	Weekly   = "weekly"
	Monthly  = "monthly"
)

// Repeat is how a job comes back after it fires.
type Repeat struct {
	Kind string
	// Every is every how many hours, days, weeks or months; at least 1.
	Every int
	// Days are the weekdays a weekly job fires on.
	Days []time.Weekday
	// MonthDay is the day of the month a monthly job fires on.
	MonthDay int
	// Until is the last day it may fire, in DJ's time zone; zero for no end.
	Until time.Time
}

const wallLayout = "2006-01-02 15:04"

// At is the instant a wall clock time in zone stands for: a time the clocks skip moves forward by
// the gap, and a time they repeat is taken the first time.
func At(date time.Time, clock string, zone *time.Location) (time.Time, error) {
	text := date.Format("2006-01-02") + " " + clock
	wall, err := time.ParseInLocation(wallLayout, text, zone)
	if err != nil {
		return time.Time{}, err
	}
	asUTC, _ := time.Parse(wallLayout, text)
	_, before := wall.Add(-24 * time.Hour).Zone()
	_, after := wall.Add(24 * time.Hour).Zone()
	first := asUTC.Add(-time.Duration(before) * time.Second)
	second := asUTC.Add(-time.Duration(after) * time.Second)
	firstFits := first.In(zone).Format(wallLayout) == text
	secondFits := second.In(zone).Format(wallLayout) == text
	switch {
	case firstFits && secondFits && second.Before(first):
		return second, nil
	case firstFits:
		return first, nil
	case secondFits:
		return second, nil
	default:
		return first, nil
	}
}

// ErrNoMore means a job does not come back.
var ErrNoMore = errors.New("the job does not repeat any more")

// Next is the time after due a job with r fires again, at clock in zone.
func Next(due time.Time, clock string, r Repeat, zone *time.Location) (time.Time, error) {
	every := max(r.Every, 1)
	local := due.In(zone)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	var next time.Time
	var err error
	switch r.Kind {
	case Once:
		return time.Time{}, ErrNoMore
	case Hourly:
		next = due.Add(time.Duration(every) * time.Hour)
	case Daily:
		next, err = At(day.AddDate(0, 0, every), clock, zone)
	case Weekdays:
		d := day.AddDate(0, 0, 1)
		for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			d = d.AddDate(0, 0, 1)
		}
		next, err = At(d, clock, zone)
	case Weekly:
		days := r.Days
		if len(days) == 0 {
			days = []time.Weekday{local.Weekday()}
		}
		d := day.AddDate(0, 0, 1)
		for !slices.Contains(days, d.Weekday()) {
			d = d.AddDate(0, 0, 1)
		}
		if weekStart(d) != weekStart(day) {
			d = d.AddDate(0, 0, 7*(every-1))
		}
		next, err = At(d, clock, zone)
	case Monthly:
		first := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, zone).AddDate(0, every, 0)
		last := first.AddDate(0, 1, -1).Day()
		next, err = At(first.AddDate(0, 0, min(r.MonthDay, last)-1), clock, zone)
	default:
		return time.Time{}, fmt.Errorf("unknown repeat %q", r.Kind)
	}
	if err != nil {
		return time.Time{}, err
	}
	if !r.Until.IsZero() && next.In(zone).Format("2006-01-02") > r.Until.Format("2006-01-02") {
		return time.Time{}, ErrNoMore
	}
	return next, nil
}

// weekStart is the Sunday that starts d's week.
func weekStart(d time.Time) time.Time { return d.AddDate(0, 0, -int(d.Weekday())) }

// Describe says when a job fires and how it repeats, the way DJ reads it.
func Describe(at time.Time, r Repeat, zone *time.Location, now time.Time) string {
	local := at.In(zone)
	layout := "Mon 2 Jan, 15:04"
	if local.Year() != now.In(zone).Year() {
		layout = "Mon 2 Jan 2006, 15:04"
	}
	out := local.Format(layout)
	every := max(r.Every, 1)
	unit := func(one, many string) string {
		if every == 1 {
			return "every " + one
		}
		return fmt.Sprintf("every %d %s", every, many)
	}
	switch r.Kind {
	case Hourly:
		out += ", then " + unit("hour", "hours")
	case Daily:
		out += ", then " + unit("day", "days")
	case Weekdays:
		out += ", then every weekday"
	case Weekly:
		names := make([]string, len(r.Days))
		for i, d := range r.Days {
			names[i] = d.String()[:3]
		}
		out += ", then " + unit("week", "weeks")
		if len(names) > 0 {
			out += " on " + strings.Join(names, ", ")
		}
	case Monthly:
		out += ", then " + unit("month", "months")
	}
	if !r.Until.IsZero() && r.Kind != Once {
		out += " until " + r.Until.Format("2 Jan")
	}
	return out
}
