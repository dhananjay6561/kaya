// Package tools holds the tools Kaya's model can call.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/agent"
)

const clockLayout = "Mon 2 Jan 2006, 15:04"

// Clock tells the time in any time zone, now or at a given time, next to DJ's own, so the model
// never works out time zones or daylight saving in its head.
func Clock(zone *time.Location, now func() time.Time) agent.Tool {
	return agent.Tool{
		Name: "world_clock",
		Description: "The date and time in other time zones, now or at a given local time, with how far each is " +
			"from the owner's own zone; daylight saving is taken into account. Use it whenever a question needs the time " +
			"somewhere else or a time converted between places, instead of working it out yourself.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"zones": {
					"type": "array", "minItems": 1, "maxItems": 8,
					"items": {"type": "string"},
					"description": "IANA time zone names, such as America/New_York, Europe/London or Asia/Tokyo: for a city, the zone it is in."
				},
				"at": {
					"type": "string",
					"pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}$",
					"description": "A local time to convert instead of now, as YYYY-MM-DD HH:MM in the from zone."
				},
				"from": {
					"type": "string",
					"description": "The IANA zone the at time is in. Leave it out for the owner's own zone."
				}
			},
			"required": ["zones"],
			"additionalProperties": false
		}`),
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Zones []string `json:"zones"`
				At    string   `json:"at"`
				From  string   `json:"from"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			return clock(args.Zones, args.At, args.From, zone, now())
		},
	}
}

func loadZone(name string) (*time.Location, bool) {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" {
		return nil, false
	}
	loc, err := time.LoadLocation(name)
	return loc, err == nil
}

func clock(zones []string, at, from string, own *time.Location, now time.Time) (string, error) {
	instant := now
	var lines []string
	if at != "" {
		src := own
		if from != "" {
			var ok bool
			if src, ok = loadZone(from); !ok {
				return "", agent.Problemf("%q is not a time zone I know; use an IANA name such as Europe/Paris", from)
			}
		}
		wall, err := time.ParseInLocation("2006-01-02 15:04", at, src)
		if err != nil {
			return "", agent.Problemf("%q is not a date and time; write it as YYYY-MM-DD HH:MM", at)
		}
		if wall.Format("2006-01-02 15:04") != at {
			_, before := wall.Add(-24 * time.Hour).Zone()
			asUTC, _ := time.Parse("2006-01-02 15:04", at)
			wall = asUTC.Add(-time.Duration(before) * time.Second).In(src)
		}
		instant = wall
		line := "Converting " + at + " in " + src.String()
		if wall.Format("2006-01-02 15:04") != at {
			line += " (that time is skipped there when the clocks go forward, so it is taken as " + wall.Format("15:04") + ")"
		}
		lines = append(lines, line+".")
	}
	lines = append(lines, "The owner's zone, "+own.String()+": "+stamp(instant, own))
	for _, name := range zones {
		loc, ok := loadZone(name)
		if !ok {
			lines = append(lines, fmt.Sprintf("%q: not a time zone I know; use an IANA name such as Europe/Paris.", name))
			continue
		}
		lines = append(lines, loc.String()+": "+stamp(instant, loc)+", "+apart(instant, loc, own))
	}
	return strings.Join(lines, "\n"), nil
}

func stamp(t time.Time, loc *time.Location) string {
	local := t.In(loc)
	return local.Format(clockLayout) + " (UTC" + local.Format("-07:00") + ")"
}

func apart(t time.Time, loc, own *time.Location) string {
	_, theirs := t.In(loc).Zone()
	_, ours := t.In(own).Zone()
	diff := time.Duration(theirs-ours) * time.Second
	switch {
	case diff == 0:
		return "the same as the owner's"
	case diff > 0:
		return span(diff) + " ahead of the owner"
	default:
		return span(-diff) + " behind the owner"
	}
}

func span(d time.Duration) string {
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case m == 0:
		return fmt.Sprintf("%dh", h)
	case h == 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%dh%02dm", h, m)
	}
}
