package agent

import (
	"context"
	"slices"
	"sync"
)

type notesKey struct{}

type notes struct {
	mu    sync.Mutex
	lines []string
}

// AddNote gives the answer a line written by the tool itself, such as the exact time a reminder
// was set for, which is shown to DJ as it is rather than in the model's words.
func AddNote(ctx context.Context, line string) {
	n, ok := ctx.Value(notesKey{}).(*notes)
	if !ok {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if !slices.Contains(n.lines, line) {
		n.lines = append(n.lines, line)
	}
}

func withNotes(ctx context.Context) (context.Context, *notes) {
	n := &notes{}
	return context.WithValue(ctx, notesKey{}, n), n
}

func (n *notes) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.lines)
}
