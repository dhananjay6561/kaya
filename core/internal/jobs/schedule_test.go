package jobs

import (
	"errors"
	"testing"
	"time"
)

func zoneOf(t *testing.T, name string) *time.Location {
	t.Helper()
	z, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func TestNextWalksEachRepeat(t *testing.T) {
	ist := zoneOf(t, "Asia/Kolkata")
	at := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, ist) }
	cases := []struct {
		name string
		due  time.Time
		r    Repeat
		want []time.Time
	}{
		{"daily", at(2026, 11, 28, 18, 0), Repeat{Kind: Daily}, []time.Time{at(2026, 11, 29, 18, 0), at(2026, 11, 30, 18, 0)}},
		{"every 2 days", at(2026, 11, 28, 8, 0), Repeat{Kind: Daily, Every: 2}, []time.Time{at(2026, 11, 30, 8, 0), at(2026, 12, 2, 8, 0)}},
		{"weekdays over a weekend", at(2026, 11, 27, 9, 30), Repeat{Kind: Weekdays}, []time.Time{at(2026, 11, 30, 9, 30), at(2026, 12, 1, 9, 30)}},
		{
			"weekly mon and thu", at(2026, 11, 30, 7, 0),
			Repeat{Kind: Weekly, Days: []time.Weekday{time.Monday, time.Thursday}},
			[]time.Time{at(2026, 12, 3, 7, 0), at(2026, 12, 7, 7, 0)},
		},
		{
			"every 2 weeks on sat", at(2026, 11, 28, 10, 0),
			Repeat{Kind: Weekly, Every: 2, Days: []time.Weekday{time.Saturday}},
			[]time.Time{at(2026, 12, 12, 10, 0), at(2026, 12, 26, 10, 0)},
		},
		{"every 2 weeks on sat and sun", at(2026, 11, 28, 10, 0), Repeat{Kind: Weekly, Every: 2, Days: []time.Weekday{time.Saturday, time.Sunday}},
			[]time.Time{at(2026, 11, 29, 10, 0), at(2026, 12, 12, 10, 0), at(2026, 12, 13, 10, 0)}},
		{"monthly on the 31st", at(2027, 1, 31, 9, 0), Repeat{Kind: Monthly, MonthDay: 31}, []time.Time{at(2027, 2, 28, 9, 0), at(2027, 3, 31, 9, 0)}},
		{"hourly", at(2026, 11, 28, 23, 15), Repeat{Kind: Hourly}, []time.Time{at(2026, 11, 29, 0, 15), at(2026, 11, 29, 1, 15)}},
	}
	for _, c := range cases {
		due := c.due
		clock := due.Format("15:04")
		for i, want := range c.want {
			next, err := Next(due, clock, c.r, ist)
			if err != nil || !next.Equal(want) {
				t.Fatalf("%s #%d: got %v, %v; want %v", c.name, i, next, err, want)
			}
			due = next
		}
	}
	if _, err := Next(at(2026, 11, 28, 18, 0), "18:00", Repeat{Kind: Once}, ist); !errors.Is(err, ErrNoMore) {
		t.Fatalf("once: %v", err)
	}
	until := Repeat{Kind: Daily, Until: at(2026, 11, 29, 0, 0)}
	if next, err := Next(at(2026, 11, 28, 18, 0), "18:00", until, ist); err != nil || !next.Equal(at(2026, 11, 29, 18, 0)) {
		t.Fatalf("until: %v %v", next, err)
	}
	if _, err := Next(at(2026, 11, 29, 18, 0), "18:00", until, ist); !errors.Is(err, ErrNoMore) {
		t.Fatalf("past until: %v", err)
	}
}

func TestADailyJobKeepsItsWallTimeAcrossDaylightSaving(t *testing.T) {
	ny := zoneOf(t, "America/New_York")
	due := time.Date(2026, 3, 7, 2, 30, 0, 0, ny)
	next, err := Next(due, "02:30", Repeat{Kind: Daily}, ny)
	if err != nil || next.In(ny).Format("2006-01-02 15:04 -07:00") != "2026-03-08 03:30 -04:00" {
		t.Fatalf("over the gap: %v, %v", next.In(ny), err)
	}
	next, err = Next(next, "02:30", Repeat{Kind: Daily}, ny)
	if err != nil || next.In(ny).Format("2006-01-02 15:04 -07:00") != "2026-03-09 02:30 -04:00" {
		t.Fatalf("after the gap: %v, %v", next.In(ny), err)
	}
}

func TestDescribeReadsLikeAPerson(t *testing.T) {
	ist := zoneOf(t, "Asia/Kolkata")
	now := time.Date(2026, 11, 28, 1, 0, 0, 0, ist)
	at := time.Date(2026, 11, 28, 18, 0, 0, 0, ist)
	for r, want := range map[*Repeat]string{
		{Kind: Once}:  "Sat 28 Nov, 18:00",
		{Kind: Daily}: "Sat 28 Nov, 18:00, then every day",
		{Kind: Weekly, Every: 2, Days: []time.Weekday{time.Monday, time.Thursday}}: "Sat 28 Nov, 18:00, then every 2 weeks on Mon, Thu",
		{Kind: Weekdays, Until: time.Date(2026, 12, 31, 0, 0, 0, 0, ist)}:          "Sat 28 Nov, 18:00, then every weekday until 31 Dec",
	} {
		if got := Describe(at, *r, ist, now); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
