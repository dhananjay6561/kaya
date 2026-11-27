package commands

import (
	"context"
	"fmt"
	"strings"
)

// AlertSwitch turns Kaya's alerts on and off.
type AlertSwitch interface {
	Enabled(ctx context.Context) (bool, error)
	Set(ctx context.Context, on bool) (bool, error)
	Waiting(ctx context.Context) (int, error)
}

const alertsUsage = "Send /alerts on or /alerts off, or just /alerts to see whether they are on."

func switchAlerts(ctx context.Context, sw AlertSwitch, args string) (string, error) {
	if sw == nil {
		return "Alerts are not set up here.", nil
	}
	switch strings.ToLower(strings.Trim(args, " .!")) {
	case "":
		on, err := sw.Enabled(ctx)
		if err != nil {
			return "", err
		}
		if !on {
			return "Alerts are off. Send /alerts on and I will message you here when something in your chats needs you, and before reminders.", nil
		}
		waiting, err := sw.Waiting(ctx)
		if err != nil {
			return "", err
		}
		answer := "Alerts are on: I message you here when something in your chats needs you, and before reminders."
		if waiting > 0 {
			answer += fmt.Sprintf(" %d waiting to go out with the next one.", waiting)
		}
		return answer + " Send /alerts off to stop them.", nil
	case "on":
		changed, err := sw.Set(ctx, true)
		if err != nil {
			return "", err
		}
		if !changed {
			return "Alerts were already on.", nil
		}
		return "Alerts on. I will message you here when something needs you.", nil
	case "off":
		changed, err := sw.Set(ctx, false)
		if err != nil {
			return "", err
		}
		if !changed {
			return "Alerts were already off.", nil
		}
		return "Alerts off. Any waiting were dropped; send /alerts on to start again.", nil
	default:
		return alertsUsage, nil
	}
}
