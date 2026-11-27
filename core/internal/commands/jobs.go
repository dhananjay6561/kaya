package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/jobs"
)

// JobBook keeps reminders and scheduled jobs.
type JobBook interface {
	List(ctx context.Context) ([]jobs.Job, error)
	Cancel(ctx context.Context, ids []int64) ([]jobs.Job, error)
}

const cancelUsage = "Send /cancel with the number of a reminder from /jobs, like /cancel 3."

func listJobs(ctx context.Context, book JobBook, zone *time.Location, now time.Time) (string, error) {
	if book == nil {
		return "Reminders are not set up here.", nil
	}
	list, err := book.List(ctx)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "No reminders set. Ask me, like \"remind me at 6 to call Rahul\".", nil
	}
	lines := make([]string, len(list))
	for i, j := range list {
		kind := ""
		if j.Ask {
			kind = " (I do it)"
		}
		lines[i] = fmt.Sprintf("%d. %s: %s%s", j.ID, jobs.Describe(j.NextAt, j.Repeat, zone, now), j.Text, kind)
	}
	return "*Reminders*\n" + strings.Join(lines, "\n"), nil
}

func cancelJob(ctx context.Context, book JobBook, args string) (string, error) {
	if book == nil {
		return "Reminders are not set up here.", nil
	}
	var ids []int64
	for _, f := range strings.FieldsFunc(args, func(r rune) bool { return r == ',' || r == ' ' || r == '#' }) {
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil || id < 1 {
			return cancelUsage, nil
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return cancelUsage, nil
	}
	gone, err := book.Cancel(ctx, ids)
	if err != nil {
		return "", err
	}
	if len(gone) == 0 {
		return "No reminder with that number is set. Send /jobs to see them.", nil
	}
	names := make([]string, len(gone))
	for i, j := range gone {
		names[i] = j.Text
	}
	return "Cancelled: " + strings.Join(names, "; ") + ".", nil
}
