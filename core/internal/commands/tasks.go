package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/tagging"
	"github.com/dhananjay6561/kaya/core/internal/text"
)

// TaskBook keeps the tasks /tasks and /done work on.
type TaskBook interface {
	Open(ctx context.Context, limit int) ([]tagging.Task, error)
	CountOpen(ctx context.Context) (int, error)
	Done(ctx context.Context, ids []int64) ([]tagging.Task, error)
}

const (
	noTaskBook = "Tasks are not set up here."
	doneUsage  = "Send /done with the number of a task from /tasks, like /done 12."
	maxTasks   = 50
)

// describe is one task as a line: its number and title, when it is due, and where it came from.
func describe(t tagging.Task, zone *time.Location) string {
	line := fmt.Sprintf("%d. %s", t.ID, text.Clip(t.Title, 200))
	if t.Due != nil {
		layout := "Mon 2 Jan"
		if t.DueHasTime {
			layout += " 15:04"
		}
		line += ", due " + t.Due.In(zone).Format(layout)
	}
	return line + " (" + t.Chat + ", " + t.SentAt.In(zone).Format("2 Jan") + ")"
}

func listTasks(ctx context.Context, book TaskBook, zone *time.Location) (string, error) {
	if book == nil {
		return noTaskBook, nil
	}
	open, err := book.Open(ctx, maxTasks)
	if err != nil {
		return "", err
	}
	if len(open) == 0 {
		return "No open tasks. I pick them up from your chats as messages come in.", nil
	}
	lines := make([]string, len(open))
	for i, t := range open {
		lines[i] = describe(t, zone)
	}
	out := "*Open tasks*\n" + strings.Join(lines, "\n")
	if len(open) == maxTasks {
		total, err := book.CountOpen(ctx)
		if err != nil {
			return "", err
		}
		if total > maxTasks {
			out += fmt.Sprintf("\n…and %d more.", total-maxTasks)
		}
	}
	return out + "\n\nClose one with /done and its number.", nil
}

func done(ctx context.Context, book TaskBook, args string, zone *time.Location) (string, error) {
	if book == nil {
		return noTaskBook, nil
	}
	ids, ok := numbers(args)
	if !ok {
		return doneUsage, nil
	}
	closed, err := book.Done(ctx, ids)
	if err != nil {
		return "", err
	}
	if len(closed) == 0 {
		return "No open task has that number now. Send /tasks to see them.", nil
	}
	lines := make([]string, len(closed))
	for i, t := range closed {
		lines[i] = describe(t, zone)
	}
	out := "Done:\n" + strings.Join(lines, "\n")
	if missing := len(ids) - len(closed); missing > 0 {
		out += fmt.Sprintf("\n(%d of those numbers matched no open task.)", missing)
	}
	return out, nil
}
