package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

var bg = context.Background()

type phone struct {
	mu   sync.Mutex
	sent []channel.OutgoingMessage
	errs []error
}

func (p *phone) Send(_ context.Context, m channel.OutgoingMessage, _ channel.SendOptions) (channel.SentMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.errs) > 0 {
		err := p.errs[0]
		p.errs = p.errs[1:]
		return channel.SentMessage{}, err
	}
	p.sent = append(p.sent, m)
	return channel.SentMessage{ID: "3EB0JOB"}, nil
}

func (p *phone) texts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.sent))
	for i, m := range p.sent {
		out[i] = m.Text
	}
	return out
}

type rig struct {
	t      *testing.T
	db     *store.DB
	phone  *phone
	pause  *pause.Memory
	now    time.Time
	zone   *time.Location
	asked  []string
	failed error
}

func newRig(t *testing.T) *rig {
	t.Helper()
	core, _ := testdb.MigratedPair(t, "kaya_core")
	return &rig{t: t, db: core, phone: &phone{}, pause: &pause.Memory{}, now: time.Now().Truncate(time.Minute), zone: zoneOf(t, "Asia/Kolkata")}
}

func (r *rig) scheduler() *Scheduler {
	r.t.Helper()
	s, err := NewScheduler(Options{
		DB: r.db, Channel: r.phone, Pause: r.pause, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Zone: r.zone,
		Now: func() time.Time { return r.now },
		Ask: func(_ context.Context, text string) (string, error) {
			r.asked = append(r.asked, text)
			return "Here are your open tasks: deck for Rahul.", r.failed
		},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *rig) create(j Job) Job {
	r.t.Helper()
	if j.Clock == "" {
		j.Clock = j.NextAt.In(r.zone).Format("15:04")
	}
	made, _, err := NewStore(r.db).Create(bg, j, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return made
}

func (r *rig) step(s *Scheduler) time.Duration {
	r.t.Helper()
	wait, err := s.Step(bg)
	if err != nil {
		r.t.Fatal(err)
	}
	return wait
}

func TestAReminderFiresOnceAtItsTimeAndEnds(t *testing.T) {
	r := newRig(t)
	s := r.scheduler()
	r.create(Job{Text: "Call Shaurya", Repeat: Repeat{Kind: Once}, NextAt: r.now.Add(5 * time.Minute)})
	if wait := r.step(s); wait != 5*time.Minute || len(r.phone.texts()) != 0 {
		t.Fatalf("before its time: wait %v, sent %q", wait, r.phone.texts())
	}
	r.now = r.now.Add(5 * time.Minute)
	if wait := r.step(s); wait != time.Hour {
		t.Fatalf("with nothing left the scheduler sleeps %v", wait)
	}
	r.step(s)
	if got := r.phone.texts(); len(got) != 1 || got[0] != "*Reminder:* Call Shaurya" {
		t.Fatalf("sent %q", got)
	}
	if left, _ := NewStore(r.db).List(bg); len(left) != 0 {
		t.Fatalf("a one-off job is still active: %+v", left)
	}
}

func TestARepeatingJobMovesOnAndALateOneSaysSo(t *testing.T) {
	r := newRig(t)
	s := r.scheduler()
	due := r.now.Add(-10 * time.Minute)
	r.create(Job{Text: "Stand up and stretch", Repeat: Repeat{Kind: Daily}, NextAt: due})
	r.step(s)
	got := r.phone.texts()
	if len(got) != 1 || !strings.HasPrefix(got[0], "*Reminder:* Stand up and stretch\n(It was due at ") || !strings.HasSuffix(got[0], "I was offline then.)") {
		t.Fatalf("sent %q", got)
	}
	left, _ := NewStore(r.db).List(bg)
	if len(left) != 1 || !left[0].NextAt.Equal(due.AddDate(0, 0, 1)) {
		t.Fatalf("next %+v", left)
	}

	r.create(Job{Text: "Water the plants", Repeat: Repeat{Kind: Daily}, NextAt: r.now.Add(-30 * time.Hour)})
	r.step(s)
	if got := r.phone.texts(); len(got) != 1 {
		t.Fatalf("a day-old repeating job was sent: %q", got)
	}
	plants, _ := NewStore(r.db).List(bg)
	for _, j := range plants {
		if j.Text == "Water the plants" && !j.NextAt.After(r.now) {
			t.Fatalf("a missed repeating job did not move past now: %v", j.NextAt)
		}
	}
}

func TestAnAskJobIsAnsweredWhenItFires(t *testing.T) {
	r := newRig(t)
	s := r.scheduler()
	r.create(Job{Text: "send me my open tasks", Ask: true, Repeat: Repeat{Kind: Weekdays}, NextAt: r.now})
	r.step(s)
	if got := r.phone.texts(); len(got) != 1 || got[0] != "Here are your open tasks: deck for Rahul." || len(r.asked) != 1 {
		t.Fatalf("sent %q, asked %q", got, r.asked)
	}
	r.failed = errors.New("model away")
	r.create(Job{Text: "summarise my day", Ask: true, Repeat: Repeat{Kind: Once}, NextAt: r.now})
	r.step(s)
	if got := r.phone.texts(); len(got) != 2 || !strings.HasPrefix(got[1], "*Scheduled:* summarise my day\n(I couldn't prepare it") {
		t.Fatalf("sent %q", got)
	}
}

func TestNothingFiresWhilePausedAndAFailedSendIsRetriedUnderItsKey(t *testing.T) {
	r := newRig(t)
	s := r.scheduler()
	r.create(Job{Text: "Pay rent", Repeat: Repeat{Kind: Once}, NextAt: r.now})
	_, _ = r.pause.Pause(bg, "test")
	if wait := r.step(s); wait != 30*time.Second || len(r.phone.texts()) != 0 {
		t.Fatalf("paused: wait %v, sent %q", wait, r.phone.texts())
	}
	_, _ = r.pause.Resume(bg, "test")
	r.phone.errs = []error{errors.New("gateway away")}
	if _, err := s.Step(bg); err == nil {
		t.Fatal("a failed send was not reported")
	}
	r.step(s)
	r.step(s)
	r.phone.mu.Lock()
	sent := append([]channel.OutgoingMessage(nil), r.phone.sent...)
	r.phone.mu.Unlock()
	if len(sent) != 1 || !strings.HasPrefix(sent[0].IdempotencyKey, "job:") {
		t.Fatalf("sent %+v", sent)
	}
}

func TestTheSameJobIsNotMadeTwiceAndCanBeCancelled(t *testing.T) {
	r := newRig(t)
	st := NewStore(r.db)
	j := Job{Text: "Call Mom", Repeat: Repeat{Kind: Weekly, Days: []time.Weekday{time.Sunday}}, NextAt: r.now.Add(time.Hour), Clock: "18:00"}
	first, made, err := st.Create(bg, j, 0)
	if err != nil || !made {
		t.Fatalf("%v %v", made, err)
	}
	again, made, err := st.Create(bg, Job{Text: " call mom ", Repeat: j.Repeat, NextAt: j.NextAt, Clock: "18:00"}, 0)
	if err != nil || made || again.ID != first.ID {
		t.Fatalf("a repeat was made again: %v %+v %v", made, again, err)
	}
	if list, _ := st.List(bg); len(list) != 1 || list[0].Repeat.Days[0] != time.Sunday {
		t.Fatalf("list %+v", list)
	}
	if gone, err := st.Cancel(bg, []int64{first.ID, 999}); err != nil || len(gone) != 1 {
		t.Fatalf("cancelled %+v, %v", gone, err)
	}
	if list, _ := st.List(bg); len(list) != 0 {
		t.Fatalf("still active %+v", list)
	}
}

func TestFiredJobsJoinTheConversation(t *testing.T) {
	r := newRig(t)
	s := r.scheduler()
	r.create(Job{Text: "Call Shaurya", Repeat: Repeat{Kind: Once}, NextAt: r.now})
	r.step(s)
	got, err := Recent(bg, r.db, time.Now().Add(-time.Hour), 5)
	if err != nil || len(got) != 1 || got[0].Answer != "*Reminder:* Call Shaurya" || !got[0].Unasked || got[0].Question != FiredHeadsUp {
		t.Fatalf("got %+v, %v", got, err)
	}
}
