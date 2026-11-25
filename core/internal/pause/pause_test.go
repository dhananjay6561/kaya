package pause

import (
	"context"
	"errors"
	"testing"

	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

var bg = context.Background()

func flip(t *testing.T, fn func(context.Context, string) (bool, error), by string) bool {
	t.Helper()
	changed, err := fn(bg, by)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func state(t *testing.T, s Switch) State {
	t.Helper()
	st, err := s.State(bg)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func behavesLikeAPause(t *testing.T, make func(t *testing.T) Switch) {
	t.Run("starts running, pauses once and resumes once", func(t *testing.T) {
		s := make(t)
		if st := state(t, s); st.Paused || !st.Since.IsZero() {
			t.Fatalf("got %+v", st)
		}
		if !flip(t, s.Pause, "cmd-1") {
			t.Fatal("the first pause pauses")
		}
		paused := state(t, s)
		if !paused.Paused || paused.Since.IsZero() {
			t.Fatalf("got %+v", paused)
		}
		if flip(t, s.Pause, "cmd-2") {
			t.Fatal("a second pause finds Kaya already paused")
		}
		if again := state(t, s); !again.Since.Equal(paused.Since) {
			t.Fatalf("a second pause keeps the first time: %v then %v", paused.Since, again.Since)
		}
		if !flip(t, s.Resume, "cmd-3") {
			t.Fatal("resume resumes")
		}
		if st := state(t, s); st.Paused {
			t.Fatalf("got %+v", st)
		}
		if flip(t, s.Resume, "cmd-4") {
			t.Fatal("a second resume finds Kaya not paused")
		}
	})

	t.Run("gives a repeated command the answer it got the first time", func(t *testing.T) {
		s := make(t)
		for _, step := range []struct {
			fn   func(context.Context, string) (bool, error)
			by   string
			want bool
		}{
			{s.Pause, "cmd-a", true},
			{s.Pause, "cmd-a", true},
			{s.Resume, "cmd-b", true},
			{s.Resume, "cmd-b", true},
			{s.Pause, "cmd-a", true},
			{s.Resume, "cmd-c", true},
		} {
			if got := flip(t, step.fn, step.by); got != step.want {
				t.Fatalf("%s: got %v", step.by, got)
			}
		}
	})
}

func TestMemory(t *testing.T) {
	behavesLikeAPause(t, func(*testing.T) Switch { return &Memory{} })
}

func databases(t *testing.T) (core, admin *store.DB) {
	t.Helper()
	return testdb.MigratedPair(t, "kaya_core", store.WithAppName("kaya-core"))
}

func exec(t *testing.T, db *store.DB, sql string) {
	t.Helper()
	if _, err := db.Exec(bg, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestStored(t *testing.T) {
	core, admin := databases(t)
	behavesLikeAPause(t, func(t *testing.T) Switch {
		exec(t, admin, "UPDATE pause_state SET paused_at = NULL, changed_by = NULL")
		return NewStored(core)
	})

	t.Run("survives a restart: a new switch reads the same state", func(t *testing.T) {
		exec(t, admin, "UPDATE pause_state SET paused_at = NULL, changed_by = NULL")
		flip(t, NewStored(core).Pause, "cmd-x")
		if !state(t, NewStored(core)).Paused {
			t.Fatal("a new switch must read the pause")
		}
	})

	t.Run("refuses to guess when the row is gone", func(t *testing.T) {
		exec(t, admin, "DELETE FROM pause_state")
		t.Cleanup(func() { exec(t, admin, "INSERT INTO pause_state DEFAULT VALUES") })
		s := NewStored(core)
		if _, err := s.State(bg); !errors.Is(err, ErrMissing) {
			t.Fatalf("state: %v", err)
		}
		if _, err := s.Pause(bg, "cmd-y"); !errors.Is(err, ErrMissing) {
			t.Fatalf("pause: %v", err)
		}
		if _, err := s.Resume(bg, "cmd-z"); !errors.Is(err, ErrMissing) {
			t.Fatalf("resume: %v", err)
		}
	})
}
