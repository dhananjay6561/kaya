package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/pause"
)

var (
	quiet = slog.New(slog.DiscardHandler)
	bg    = context.Background()
)

func ago(d time.Duration) time.Time { return time.Now().Add(-d) }

type sendCall struct {
	ctx context.Context
	m   channel.OutgoingMessage
	o   channel.SendOptions
}

// watched is a stub channel that records every send and can make some fail.
type watched struct {
	*channel.Stub
	fail  func(m channel.OutgoingMessage) error
	mu    sync.Mutex
	calls []sendCall
}

func (w *watched) Send(ctx context.Context, m channel.OutgoingMessage, o channel.SendOptions) (channel.SentMessage, error) {
	w.mu.Lock()
	w.calls = append(w.calls, sendCall{ctx, m, o})
	w.mu.Unlock()
	if w.fail != nil {
		if err := w.fail(m); err != nil {
			return channel.SentMessage{}, err
		}
	}
	return w.Stub.Send(ctx, m, o)
}

func (w *watched) sent() []channel.OutgoingMessage { return w.Sent() }

type rig struct {
	t     *testing.T
	ch    *watched
	pause *pause.Memory
	stop  func()
}

type rigOptions struct {
	status func(context.Context) (string, error)
	chat   func(context.Context, channel.IncomingMessage) (ChatAnswer, error)
	fail   func(channel.OutgoingMessage) error
	now    func() time.Time
}

func newRig(t *testing.T, o rigOptions) *rig {
	t.Helper()
	r := &rig{t: t, ch: &watched{Stub: channel.NewStub(), fail: o.fail}, pause: &pause.Memory{}}
	if o.status == nil {
		o.status = func(context.Context) (string, error) { return "status text", nil }
	}
	if o.chat == nil {
		o.chat = func(_ context.Context, m channel.IncomingMessage) (ChatAnswer, error) {
			return ChatAnswer{Text: "You said: " + Clip(m.Text), Remember: true}, nil
		}
	}
	r.stop = Start(Options{Channel: r.ch, Logger: quiet, Pause: r.pause, Status: o.status, Chat: o.chat, Now: o.now})
	if err := r.ch.Start(bg); err != nil {
		t.Fatal(err)
	}
	return r
}

// say has DJ type text and returns what Kaya sent back, and how handling it ended.
func (r *rig) say(text string, o channel.ReceiveOptions) ([]string, error) {
	before := len(r.ch.sent())
	_, _, err := r.ch.Receive(bg, text, o)
	var out []string
	for _, m := range r.ch.sent()[before:] {
		out = append(out, m.Text)
	}
	return out, err
}

func (r *rig) answers(text string, o channel.ReceiveOptions) []string {
	r.t.Helper()
	out, err := r.say(text, o)
	if err != nil {
		r.t.Fatalf("%q: %v", text, err)
	}
	return out
}

func (r *rig) first(text string, o channel.ReceiveOptions) string {
	r.t.Helper()
	out := r.answers(text, o)
	if len(out) != 1 {
		r.t.Fatalf("%q: want one answer, got %q", text, out)
	}
	return out[0]
}

func (r *rig) paused() bool {
	r.t.Helper()
	st, _ := r.pause.State(bg)
	return st.Paused
}

func matches(t *testing.T, text, pattern string) {
	t.Helper()
	if !regexp.MustCompile(pattern).MatchString(text) {
		t.Fatalf("%q does not match %s", text, pattern)
	}
}

func same(t *testing.T, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) || len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func codeOf(err error) string {
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		return coded.ErrorCode()
	}
	return ""
}

var now = channel.ReceiveOptions{}

func TestAnswersWhatDJTypedForAsLongAsTheAnswerIsWorthHaving(t *testing.T) {
	r := newRig(t, rigOptions{})
	ctx, cancel := context.WithCancel(bg)
	m, _, err := r.ch.Receive(ctx, "remind me to call mum", channel.ReceiveOptions{SentAt: ago(4 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	want := channel.OutgoingMessage{IdempotencyKey: "command:" + m.ID + ":chat", ReplyTo: m.ID, Text: "You said: remind me to call mum"}
	if sent := r.ch.sent(); len(sent) != 1 || sent[0] != want {
		t.Fatalf("got %+v", sent)
	}
	call := r.ch.calls[0]
	if ttl := call.o.TTL; ttl <= 5*time.Minute || ttl > 6*time.Minute {
		t.Fatalf("the reply expires when the command turns 10 minutes old, got %v", ttl)
	}
	if !call.o.TextMayChange {
		t.Fatal("a retried answer may differ from the first")
	}
	cancel()
	if call.ctx.Err() == nil {
		t.Fatal("the wait must end when core stops")
	}
}

func TestDoesNotActOnACommandTakenInMoreThanTenMinutesLate(t *testing.T) {
	r := newRig(t, rigOptions{})
	same(t, r.answers("just in time", channel.ReceiveOptions{SentAt: ago(9 * time.Minute)}), []string{"You said: just in time"})

	late, _, err := r.ch.Receive(bg, "order the usual", channel.ReceiveOptions{SentAt: ago(11 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	want := channel.OutgoingMessage{
		IdempotencyKey: "command:" + late.ID,
		ReplyTo:        late.ID,
		Text:           "I was away when you sent this, so I did not act on it. Send it again if you still want it:\norder the usual",
	}
	if got := r.ch.sent()[1]; got != want {
		t.Fatalf("got %+v", got)
	}
	if ttl := r.ch.calls[1].o.TTL; ttl != time.Hour {
		t.Fatalf("got %v", ttl)
	}
}

func TestSaysNothingAboutACommandMoreThanADayOld(t *testing.T) {
	r := newRig(t, rigOptions{})
	same(t, r.answers("yesterday's note", channel.ReceiveOptions{SentAt: ago(25 * time.Hour)}), nil)
}

func TestTellsDJWhenTheAnswerCouldNotBeSentInTime(t *testing.T) {
	for _, code := range []string{"expired", "no_answer"} {
		r := newRig(t, rigOptions{fail: func(m channel.OutgoingMessage) error {
			if strings.HasPrefix(m.Text, "You said") {
				return &channel.SendFailedError{RequestID: "request-id", Code: code}
			}
			return nil
		}})
		out, err := r.say("turn the lights off", now)
		if codeOf(err) != code {
			t.Fatalf("the command fails with %s, got %v", code, err)
		}
		sent := r.ch.sent()
		if len(out) != 1 || !strings.HasSuffix(sent[0].IdempotencyKey, ":unanswered") {
			t.Fatalf("got %+v", sent)
		}
		matches(t, out[0], `^I could not answer this in time, so nothing was done\.`)
		matches(t, out[0], `turn the lights off$`)
	}
}

func TestSendsNothingMoreWhenTheAnswerMayHaveGoneOutAfterAll(t *testing.T) {
	r := newRig(t, rigOptions{fail: func(channel.OutgoingMessage) error {
		return &channel.SendFailedError{RequestID: "request-id", Code: "outcome_unknown"}
	}})
	out, err := r.say("hello", now)
	if codeOf(err) != "outcome_unknown" || len(out) != 0 {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestPassesOnANoticeThatCouldNotBeSent(t *testing.T) {
	away := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	r := newRig(t, rigOptions{fail: func(m channel.OutgoingMessage) error {
		if strings.HasPrefix(m.Text, "You said") {
			return &channel.SendFailedError{RequestID: "request-id", Code: "expired"}
		}
		return away
	}})
	if _, err := r.say("hello", now); !errors.Is(err, away) {
		t.Fatalf("the notice's failure decides whether the command runs again, got %v", err)
	}
}

func TestCutsALongMessageAtAWholeCharacter(t *testing.T) {
	if got := Clip("short"); got != "short" {
		t.Fatal(got)
	}
	long := Clip(strings.Repeat("x", 999) + "😀😀")
	if long != strings.Repeat("x", 999)+"😀…" || !utf8.ValidString(long) {
		t.Fatalf("got %q", long)
	}
	if !utf8.ValidString(Clip("broken \xed\xa0\xbd surrogate")) {
		t.Fatal("a broken character must not pass through")
	}
	if n := utf8.RuneCountInString(Clip(strings.Repeat("😀", 5000))); n != 1001 {
		t.Fatalf("got %d characters", n)
	}
}

func TestStopsAnsweringOnceSwitchedOff(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.stop()
	same(t, r.answers("anyone there?", now), nil)
}

func TestListsTheCommandsOnHelp(t *testing.T) {
	r := newRig(t, rigOptions{})
	help := r.first("/help", now)
	matches(t, help, `^Commands:`)
	for _, name := range []string{"/help", "/status", "/pause", "/resume"} {
		if !strings.Contains(help, name) {
			t.Fatalf("%s is not listed", name)
		}
	}
	if strings.Contains(help, "paused") {
		t.Fatal(help)
	}
}

func TestAnswersStatusWithTheStatus(t *testing.T) {
	r := newRig(t, rigOptions{})
	same(t, r.answers("/status", now), []string{"status text"})
}

func TestStopsEveryOtherAnswerBetweenPauseAndResume(t *testing.T) {
	r := newRig(t, rigOptions{})
	same(t, r.answers("before", now), []string{"You said: before"})

	matches(t, r.first("/pause", now), `^Paused\.`)
	if !r.paused() {
		t.Fatal("not paused")
	}
	same(t, r.answers("hello?", now), nil)
	same(t, r.answers("anyone", channel.ReceiveOptions{SentAt: ago(30 * time.Minute)}), nil)
	matches(t, r.first("/pause", now), `^Already paused\.`)
	matches(t, r.first("/help", now), `I am paused: only slash commands get an answer\.$`)
	same(t, r.answers("/status", now), []string{"status text"})
	matches(t, r.first("/nope", now), `^/nope is not a command I know\.`)

	matches(t, r.first("/resume", now), `^Resumed\.`)
	if r.paused() {
		t.Fatal("still paused")
	}
	same(t, r.answers("after", now), []string{"You said: after"})
	matches(t, r.first("/resume", now), `^I was not paused\.`)
}

func TestDoesNothingForABuiltinWithWordsAfterIt(t *testing.T) {
	r := newRig(t, rigOptions{})
	matches(t, r.first("/pause for 2h", now), `^/pause takes nothing after it`)
	if r.paused() {
		t.Fatal("paused")
	}
}

func TestDoesNotActOnASlashCommandTakenInLate(t *testing.T) {
	r := newRig(t, rigOptions{})
	matches(t, r.first("/pause", channel.ReceiveOptions{SentAt: ago(11 * time.Minute)}), `^I was away when you sent this, so I did not act on it\.`)
	if r.paused() {
		t.Fatal("paused")
	}
	same(t, r.answers("/resume", channel.ReceiveOptions{SentAt: ago(25 * time.Hour)}), nil)
}

func TestAnswersARetriedPauseTheSameWayOnce(t *testing.T) {
	r := newRig(t, rigOptions{})
	again := channel.ReceiveOptions{ID: "3EB0PAUSE"}
	matches(t, r.first("/pause", again), `^Paused\.`)
	same(t, r.answers("/pause", again), nil)
	if len(r.ch.sent()) != 1 || !r.paused() {
		t.Fatalf("got %+v", r.ch.sent())
	}
}

func TestDoesNotWorkOutARetriedStatusAgain(t *testing.T) {
	calls := 0
	r := newRig(t, rigOptions{status: func(context.Context) (string, error) {
		calls++
		return fmt.Sprintf("up %dm", calls), nil
	}})
	again := channel.ReceiveOptions{ID: "3EB0STATUS"}
	same(t, r.answers("/status", again), []string{"up 1m"})
	same(t, r.answers("/status", again), nil)
	if calls != 1 {
		t.Fatalf("an answer already queued is followed, not worked out again: %d calls", calls)
	}
}

func TestAnswersAnewWhenACommandWasEditedBeforeItsRetry(t *testing.T) {
	r := newRig(t, rigOptions{})
	again := channel.ReceiveOptions{ID: "3EB0EDITED"}
	same(t, r.answers("/status", again), []string{"status text"})
	matches(t, r.first("/pause", again), `^Paused\.`)
	if !r.paused() {
		t.Fatal("not paused")
	}
}

func TestDoesNotAskTheModelAgainForAnAnsweredRetry(t *testing.T) {
	calls := 0
	r := newRig(t, rigOptions{chat: func(context.Context, channel.IncomingMessage) (ChatAnswer, error) {
		calls++
		return ChatAnswer{Text: fmt.Sprintf("answer %d", calls), Remember: true}, nil
	}})
	again := channel.ReceiveOptions{ID: "3EB0CHAT"}
	same(t, r.answers("what is 2+2", again), []string{"answer 1"})
	_, handled, err := r.ch.Receive(bg, "what is 2+2", again)
	if err != nil || handled.AnswerID == "" || len(r.ch.sent()) != 1 {
		t.Fatalf("the retry reports the answer already sent: %+v %v", handled, err)
	}
	if calls != 1 {
		t.Fatalf("%d calls", calls)
	}
}

func TestReportsTheAnswerSoItIsRemembered(t *testing.T) {
	r := newRig(t, rigOptions{})
	_, handled, err := r.ch.Receive(bg, "hello", now)
	if err != nil || handled.AnswerID == "" {
		t.Fatalf("got %+v %v", handled, err)
	}
	_, handled, err = r.ch.Receive(bg, "/status", now)
	if err != nil || handled.AnswerID != "" {
		t.Fatalf("a slash answer is not part of the conversation: %+v %v", handled, err)
	}
}

func TestNeverRemembersACannedReplyEvenWhenRetried(t *testing.T) {
	calls := 0
	r := newRig(t, rigOptions{chat: func(context.Context, channel.IncomingMessage) (ChatAnswer, error) {
		calls++
		return ChatAnswer{Text: "I had no answer to that."}, nil
	}})
	again := channel.ReceiveOptions{ID: "3EB0CANNED"}
	_, handled, err := r.ch.Receive(bg, "???", again)
	if err != nil || handled.AnswerID != "" {
		t.Fatalf("got %+v %v", handled, err)
	}
	if sent := r.ch.sent(); len(sent) != 1 || !strings.HasSuffix(sent[0].IdempotencyKey, ":chat:canned") {
		t.Fatalf("got %+v", sent)
	}
	same(t, r.answers("???", again), nil)
	if calls != 1 {
		t.Fatalf("the retry follows the canned reply instead of asking again: %d calls", calls)
	}
}

func TestDoesNotAskTheModelWhenAMessageWaitedTooLongForItsTurn(t *testing.T) {
	calls := 0
	r := newRig(t, rigOptions{
		chat: func(context.Context, channel.IncomingMessage) (ChatAnswer, error) {
			calls++
			return ChatAnswer{Text: "late answer", Remember: true}, nil
		},
		now: func() time.Time { return time.Now().Add(10 * time.Minute) },
	})
	out, err := r.say("still there?", channel.ReceiveOptions{SentAt: ago(time.Minute)})
	if codeOf(err) != "expired" || calls != 0 {
		t.Fatalf("got %v after %d calls", err, calls)
	}
	matches(t, strings.Join(out, ""), `^I could not answer this in time`)
}

func TestPointsToStatusWhenASlashAnswerCouldNotGoOutInTime(t *testing.T) {
	r := newRig(t, rigOptions{fail: func(m channel.OutgoingMessage) error {
		if strings.HasPrefix(m.Text, "Paused") {
			return &channel.SendFailedError{RequestID: "request-id", Code: "expired"}
		}
		return nil
	}})
	out, err := r.say("/pause", now)
	if err == nil {
		t.Fatal("the command must fail")
	}
	same(t, out, []string{"I could not answer this in time. Send /status to see how things stand."})
}

func TestTellsDJWhenWorkingOutAnAnswerFails(t *testing.T) {
	r := newRig(t, rigOptions{status: func(context.Context) (string, error) {
		return "", errors.New("permission denied for table gateway_events")
	}})
	out, err := r.say("/status", now)
	if err == nil {
		t.Fatal("the command must fail")
	}
	same(t, out, []string{"Something went wrong with that. Send /status to see how things stand."})
}

func TestSaysNothingWhenTheDatabaseIsAway(t *testing.T) {
	away := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	r := newRig(t, rigOptions{status: func(context.Context) (string, error) { return "", away }})
	out, err := r.say("/status", now)
	if !errors.Is(err, away) || len(out) != 0 {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestSaysNothingWhenCoreIsStopping(t *testing.T) {
	r := newRig(t, rigOptions{chat: func(ctx context.Context, _ channel.IncomingMessage) (ChatAnswer, error) {
		<-ctx.Done()
		return ChatAnswer{}, ctx.Err()
	}})
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, _, err := r.ch.Receive(ctx, "hello", now); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if len(r.ch.calls) != 0 {
		t.Fatalf("got %+v", r.ch.calls)
	}
}

func TestPlainMessagesAreAnsweredByTheModel(t *testing.T) {
	var asked []string
	var askedCtx context.Context
	r := newRig(t, rigOptions{chat: func(ctx context.Context, m channel.IncomingMessage) (ChatAnswer, error) {
		asked, askedCtx = append(asked, m.Text), ctx
		return ChatAnswer{Text: "Four.", Remember: true}, nil
	}})
	ctx, cancel := context.WithCancel(bg)
	if _, _, err := r.ch.Receive(ctx, "what is 2+2", now); err != nil {
		t.Fatal(err)
	}
	same(t, asked, []string{"what is 2+2"})
	same(t, []string{r.ch.sent()[0].Text}, []string{"Four."})
	cancel()
	if askedCtx.Err() == nil {
		t.Fatal("the model's wait must end when core stops")
	}
}

func TestPlainMessagesNeverReachTheModelWhilePausedOrWhenLate(t *testing.T) {
	asked := 0
	r := newRig(t, rigOptions{chat: func(context.Context, channel.IncomingMessage) (ChatAnswer, error) {
		asked++
		return ChatAnswer{Text: "answer", Remember: true}, nil
	}})
	r.answers("/pause", now)
	same(t, r.answers("hello", now), nil)
	r.answers("/resume", now)
	matches(t, r.first("late one", channel.ReceiveOptions{SentAt: ago(11 * time.Minute)}), `^I was away`)
	if asked != 0 {
		t.Fatalf("asked %d times", asked)
	}
}

func TestPlainMessagesGetAPlainNoticeWhenTheModelFails(t *testing.T) {
	for _, c := range []struct{ code, notice string }{
		{llm.RateLimited, `^I have hit my limit with the AI service`},
		{llm.Auth, `refused my key`},
		{llm.Rejected, `turned this message down`},
		{llm.TooLong, `too long for the AI service`},
		{llm.Misconfigured, `Check GROQ_MODEL_HEAVY and GROQ_MODEL_ROUTINE`},
		{llm.Timeout, `is not answering right now`},
		{llm.Unavailable, `is not answering right now`},
		{llm.BadResponse, `is not answering right now`},
	} {
		r := newRig(t, rigOptions{chat: func(context.Context, channel.IncomingMessage) (ChatAnswer, error) {
			return ChatAnswer{}, &llm.Error{Provider: "groq", Code: c.code}
		}})
		out, err := r.say("hello", now)
		if codeOf(err) != c.code {
			t.Fatalf("%s: the command fails with its code, got %v", c.code, err)
		}
		if len(out) != 1 || !strings.HasSuffix(r.ch.sent()[0].IdempotencyKey, ":failed") {
			t.Fatalf("%s: got %+v", c.code, r.ch.sent())
		}
		matches(t, out[0], c.notice)
	}
}
