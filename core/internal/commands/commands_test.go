package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/core/internal/facts"
	"github.com/dhananjay6561/kaya/core/internal/jobs"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/core/internal/tagging"
)

var (
	kolkata, _ = time.LoadLocation("Asia/Kolkata")
	quiet      = slog.New(slog.DiscardHandler)
	bg         = context.Background()
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
	status  func(context.Context) (string, error)
	chat    func(context.Context, channel.IncomingMessage) (ChatAnswer, error)
	fail    func(channel.OutgoingMessage) error
	now     func() time.Time
	facts   FactBook
	tasks   TaskBook
	summary func(context.Context, string) (string, error)
	alerts  AlertSwitch
	decide  func(context.Context, channel.IncomingMessage) (string, bool, error)
	replay  func(context.Context, channel.IncomingMessage) (string, bool, error)
	shown   func(context.Context, string, channel.SentMessage) error
	jobs    JobBook
}

// errAway is the rig's database outage; every other error is a handler's own.
var errAway = errors.New("the database is away")

func isAway(err error) bool { return errors.Is(err, errAway) }

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
	r.stop = Start(Options{Channel: r.ch, Logger: quiet, Pause: r.pause, Status: o.status, Chat: o.chat, Now: o.now, DatabaseAway: isAway, Facts: o.facts, Tasks: o.tasks, Zone: kolkata, Summary: o.summary, Alerts: o.alerts, Decide: o.decide, Replay: o.replay, Shown: o.shown, Jobs: o.jobs})
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
	away := fmt.Errorf("reading the status: %w", errAway)
	r := newRig(t, rigOptions{status: func(context.Context) (string, error) { return "", away }})
	out, err := r.say("/status", now)
	if !errors.Is(err, errAway) || len(out) != 0 {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestTellsDJWhenAnotherServiceIsUnreachable(t *testing.T) {
	unreachable := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	r := newRig(t, rigOptions{chat: func(context.Context, channel.IncomingMessage) (ChatAnswer, error) {
		return ChatAnswer{}, unreachable
	}})
	out, err := r.say("hello", now)
	if !errors.Is(err, unreachable) {
		t.Fatalf("got %v", err)
	}
	same(t, out, []string{"Something went wrong with that. Send /status to see how things stand."})
}

func TestNeedsToKnowWhenTheDatabaseIsAway(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("starting without DatabaseAway must fail")
		}
	}()
	Start(Options{Channel: channel.NewStub(), Logger: quiet, Pause: &pause.Memory{}})
}

func TestUsesTheKeysTheTypeScriptCoreUsed(t *testing.T) {
	r := newRig(t, rigOptions{chat: func(_ context.Context, m channel.IncomingMessage) (ChatAnswer, error) {
		switch m.Text {
		case "canned":
			return ChatAnswer{Text: "I had no answer to that."}, nil
		case "fails":
			return ChatAnswer{}, &llm.Error{Provider: "groq", Code: llm.Timeout}
		}
		return ChatAnswer{Text: "ok", Remember: true}, nil
	}, fail: func(m channel.OutgoingMessage) error {
		if m.Text == "ok" {
			return &channel.SendFailedError{RequestID: "request-id", Code: "no_answer"}
		}
		return nil
	}})
	for _, c := range []struct {
		text, id, key string
		o             channel.ReceiveOptions
	}{
		{"/status", "3EB0A", "command:3EB0A:builtin:status", now},
		{"/weather", "3EB0B", "command:3EB0B:unknown:weather", now},
		{"/pause now", "3EB0C", "command:3EB0C:extra_text:pause", now},
		{"canned", "3EB0D", "command:3EB0D:chat:canned", now},
		{"fails", "3EB0E", "command:3EB0E:failed", now},
		{"unsent", "3EB0F", "command:3EB0F:unanswered", now},
		{"late", "3EB0G", "command:3EB0G", channel.ReceiveOptions{SentAt: ago(11 * time.Minute)}},
	} {
		c.o.ID = c.id
		before := len(r.ch.sent())
		_, _, _ = r.ch.Receive(bg, c.text, c.o)
		sent := r.ch.sent()[before:]
		if len(sent) != 1 || sent[0].IdempotencyKey != c.key || sent[0].ReplyTo != c.id {
			t.Fatalf("%s: got %+v", c.text, sent)
		}
	}
}

func TestSendsTheTypeScriptCoresTexts(t *testing.T) {
	r := newRig(t, rigOptions{})
	help := "Commands:\n/help: this list\n/status: how Kaya is doing\n/pause: stop answering until /resume\n" +
		"/resume: start answering again\n/facts: what I know about you\n/remember <fact>: keep a fact about you\n" +
		"/forget <number>: drop a fact, by its number in /facts\n/tasks: open tasks and reminders found in your chats\n" +
		"/done <number>: close a task, by its number in /tasks\n/summary: what came in today; /summary <chat>: one chat's last day\n" +
		"/alerts: whether I message you when something needs you; /alerts on, /alerts off\n" +
		"/jobs: your reminders and scheduled messages; /cancel <number>: stop one\n" +
		"Anything else you send here goes to an AI model, " +
		"which answers with what it knows about you, your conversation and what a search of your other chats finds."
	same(t, r.answers("/help", now), []string{help})
	same(t, r.answers("/pause", now), []string{"Paused. Until you send /resume I answer only slash commands, like /status."})
	same(t, r.answers("/pause", now), []string{"Already paused. Send /resume to start again."})
	same(t, r.answers("/help", now), []string{help + "\nI am paused: only slash commands get an answer."})
	same(t, r.answers("/resume", now), []string{"Resumed. I am answering again."})
	same(t, r.answers("/resume", now), []string{"I was not paused."})
	same(t, r.answers("/nope", now), []string{"/nope is not a command I know. Send /help to see what I can do."})
	same(t, r.answers("/", now), []string{"Send /help to see what I can do."})
	same(t, r.answers("/pause 2h", now), []string{"/pause takes nothing after it, so nothing was done. Send just /pause."})

	for code, want := range map[string]string{
		llm.RateLimited:   "I have hit my limit with the AI service for now. Try again in a minute.",
		llm.Auth:          "The AI service refused my key, so I could not answer. Check GROQ_API_KEY.",
		llm.TooLong:       "That is too long for the AI service. Try a shorter message.",
		llm.Misconfigured: "The AI service does not know a model I am set to use. Check GROQ_MODEL_HEAVY and GROQ_MODEL_ROUTINE.",
		llm.Rejected:      "The AI service turned this message down. Try asking another way.",
		llm.BadToolCall:   "I fumbled one of my tools answering that. Try asking again.",
		llm.Timeout:       "The AI service is not answering right now. Try again in a bit.",
	} {
		if got := failureNotice(&llm.Error{Provider: "groq", Code: code}); got != want {
			t.Errorf("%s: got %q", code, got)
		}
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

// book is an in-memory FactBook.
type book struct {
	mu    sync.Mutex
	facts []facts.Fact
	next  int64
	err   error
	waIDs []string
}

func (b *book) List(context.Context) ([]facts.Fact, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.facts), b.err
}

func (b *book) Remember(_ context.Context, body, waID string) (facts.Fact, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return facts.Fact{}, false, b.err
	}
	switch body {
	case "too long":
		return facts.Fact{}, false, facts.ErrTooLong
	case "full":
		return facts.Fact{}, false, facts.ErrFull
	case "secret":
		return facts.Fact{}, false, facts.ErrSecret
	}
	for _, f := range b.facts {
		if strings.EqualFold(f.Text, body) {
			return f, false, nil
		}
	}
	b.next++
	f := facts.Fact{ID: b.next, Text: body, Source: facts.Told}
	b.facts, b.waIDs = append(b.facts, f), append(b.waIDs, waID)
	return f, true, nil
}

func (b *book) Forget(_ context.Context, ids []int64) ([]facts.Fact, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var gone, kept []facts.Fact
	for _, f := range b.facts {
		if slices.Contains(ids, f.ID) {
			gone = append(gone, f)
		} else {
			kept = append(kept, f)
		}
	}
	b.facts = kept
	return gone, b.err
}

func TestRememberFactsAndForgetThemByNumber(t *testing.T) {
	b := &book{}
	r := newRig(t, rigOptions{facts: b})
	same(t, r.answers("/facts", now), []string{"I don't know any facts about you yet. Tell me one with /remember, or just mention things as we talk and I'll pick them up."})
	same(t, r.answers("/remember", now), []string{"Send /remember with the fact after it, like /remember I'm vegetarian."})
	same(t, r.answers("/remember Works at Keploy", channel.ReceiveOptions{ID: "3EB0R1"}), []string{"Got it, fact 1: Works at Keploy"})
	if !slices.Equal(b.waIDs, []string{"3EB0R1"}) {
		t.Fatalf("wa ids %q", b.waIDs)
	}
	same(t, r.answers("/remember works at keploy", now), []string{"I already know that (fact 1)."})
	same(t, r.answers("/remember too long", now), []string{"That is too long for one fact. Keep it under 500 characters."})
	same(t, r.answers("/remember full", now), []string{"You told me 200 facts already, the most I keep. Remove some with /forget first."})
	same(t, r.answers("/remember secret", now), []string{"I don't keep passwords, PINs, codes, or card, account or ID numbers, even when asked."})
	b.facts = append(b.facts, facts.Fact{ID: 7, Text: "Has a cat", Source: facts.Learned})
	same(t, r.answers("/facts", now), []string{"*You told me*\n1. Works at Keploy\n\n*I picked up*\n7. Has a cat\n\nRemove one with /forget and its number."})
	same(t, r.answers("/forget", now), []string{"Send /forget with the number of a fact from /facts, like /forget 12."})
	same(t, r.answers("/forget the cat", now), []string{"Send /forget with the number of a fact from /facts, like /forget 12."})
	same(t, r.answers("/forget 99", now), []string{"No fact has that number now. Send /facts to see them."})
	same(t, r.answers("/forget #7, 7 99", now), []string{"Forgot:\n7. Has a cat\n(1 of those numbers matched no fact.)"})
	same(t, r.answers("/facts", now), []string{"*You told me*\n1. Works at Keploy\n\nRemove one with /forget and its number."})
	b.facts = append(b.facts, facts.Fact{ID: 8, Text: "Plays chess", Source: facts.Learned})
	same(t, r.answers("/forget １, 8.", now), []string{"Forgot:\n1. Works at Keploy\n8. Plays chess"})
}

func TestFactsListsALongListWithoutLosingTheEnd(t *testing.T) {
	b := &book{}
	for i := range facts.MaxFacts {
		source := facts.Told
		if i%2 == 1 {
			source = facts.Learned
		}
		b.facts = append(b.facts, facts.Fact{ID: int64(i + 1), Text: strings.Repeat("x", facts.MaxLength), Source: source})
	}
	r := newRig(t, rigOptions{facts: b})
	out := r.answers("/facts", now)[0]
	if utf8.RuneCountInString(out) > maxListed || !strings.Contains(out, "more.") ||
		!strings.HasSuffix(out, "a new one replaces the learned fact I have not needed longest.") {
		t.Fatalf("%d characters, ends %q", utf8.RuneCountInString(out), out[max(0, len(out)-200):])
	}
	if strings.Contains(out, strings.Repeat("x", maxLine+1)) {
		t.Fatal("a line was not cut")
	}
}

func TestFactCommandsWorkWhilePausedAndWaitOutADatabaseOutage(t *testing.T) {
	b := &book{}
	r := newRig(t, rigOptions{facts: b})
	_ = r.answers("/pause", now)
	same(t, r.answers("/remember Likes chai", now), []string{"Got it, fact 1: Likes chai"})
	b.err = errAway
	if out, err := r.say("/facts", now); !errors.Is(err, errAway) || len(out) != 0 {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestFactCommandsWithoutAFactBookSaySo(t *testing.T) {
	r := newRig(t, rigOptions{})
	for _, c := range []string{"/facts", "/remember x", "/forget 1"} {
		same(t, r.answers(c, now), []string{"Facts are not set up here."})
	}
}

// taskBook is an in-memory TaskBook.
type taskBook struct {
	mu    sync.Mutex
	tasks []tagging.Task
	err   error
}

func (b *taskBook) Open(_ context.Context, limit int) ([]tagging.Task, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []tagging.Task
	for _, t := range b.tasks {
		if !t.Done && len(out) < limit {
			out = append(out, t)
		}
	}
	return out, b.err
}

func (b *taskBook) CountOpen(context.Context) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, t := range b.tasks {
		if !t.Done {
			n++
		}
	}
	return n, b.err
}

func (b *taskBook) Done(_ context.Context, ids []int64) ([]tagging.Task, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var closed []tagging.Task
	for i, t := range b.tasks {
		if !t.Done && slices.Contains(ids, t.ID) {
			b.tasks[i].Done = true
			closed = append(closed, b.tasks[i])
		}
	}
	return closed, b.err
}

func TestListsOpenTasksAndClosesThemByNumber(t *testing.T) {
	sent := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	due := time.Date(2026, 11, 27, 11, 30, 0, 0, time.UTC)
	day := time.Date(2027, 1, 3, 0, 0, 0, 0, kolkata)
	b := &taskBook{tasks: []tagging.Task{
		{ID: 4, Kind: tagging.KindReminder, Title: "Call with Priya", Due: &due, DueHasTime: true, Chat: "Rahul", SentAt: sent},
		{ID: 5, Kind: tagging.KindReminder, Title: "Exam", Due: &day, Chat: "Office Team", SentAt: sent},
		{ID: 7, Kind: tagging.KindTask, Title: "Send Rahul the deck", Chat: "Rahul", SentAt: sent},
	}}
	r := newRig(t, rigOptions{tasks: b, now: func() time.Time { return sent }})
	same(t, r.answers("/tasks", now), []string{"*Open tasks*\n" +
		"4. Call with Priya, due Fri 27 Nov 17:00 (Rahul, 26 Nov)\n" +
		"5. Exam, due Sun 3 Jan 2027 (Office Team, 26 Nov)\n" +
		"7. Send Rahul the deck (Rahul, 26 Nov)\n\nClose one with /done and its number."})
	same(t, r.answers("/done", now), []string{"Send /done with the number of a task from /tasks, like /done 12."})
	same(t, r.answers("/done 99", now), []string{"No open task has that number now. Send /tasks to see them."})
	same(t, r.answers("/done 7, 4. 99", now), []string{"Done:\n4. Call with Priya, due Fri 27 Nov 17:00 (Rahul, 26 Nov)\n" +
		"7. Send Rahul the deck (Rahul, 26 Nov)\n(1 of those numbers matched no open task.)"})
	same(t, r.answers("/done 7", now), []string{"No open task has that number now. Send /tasks to see them."})
	b.tasks = nil
	same(t, r.answers("/tasks", now), []string{"No open tasks. I pick them up from your chats as messages come in."})
	for i := range 60 {
		b.tasks = append(b.tasks, tagging.Task{ID: int64(i + 1), Title: "t", Chat: "c", SentAt: sent})
	}
	if out := r.first("/tasks", now); !strings.Contains(out, "\n…and 10 more.\n\nClose one") {
		t.Fatalf("got %q", out)
	}
	b.err = errAway
	if out, err := r.say("/tasks", now); !errors.Is(err, errAway) || len(out) != 0 {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestTaskCommandsWithoutATaskBookSaySo(t *testing.T) {
	r := newRig(t, rigOptions{})
	for _, c := range []string{"/tasks", "/done 1"} {
		same(t, r.answers(c, now), []string{"Tasks are not set up here."})
	}
}

func TestSummaryOfTodayOrOfANamedChat(t *testing.T) {
	var asked []string
	r := newRig(t, rigOptions{summary: func(_ context.Context, chat string) (string, error) {
		asked = append(asked, chat)
		if chat == "" {
			return "today in short", nil
		}
		return "about " + chat, nil
	}})
	same(t, r.answers("/summary", now), []string{"today in short"})
	same(t, r.answers("/Summary  College Gang ", now), []string{"about College Gang"})
	if !slices.Equal(asked, []string{"", "College Gang"}) {
		t.Fatalf("asked %q", asked)
	}
	none := newRig(t, rigOptions{})
	same(t, none.answers("/summary", now), []string{"Summaries are not set up here."})
}

func TestASummaryTheModelCannotWriteGetsTheUsualNotice(t *testing.T) {
	r := newRig(t, rigOptions{summary: func(context.Context, string) (string, error) {
		return "", &llm.Error{Provider: "groq", Code: llm.RateLimited}
	}})
	out, err := r.say("/summary", now)
	if err == nil || len(out) != 1 || !strings.Contains(out[0], "limit") {
		t.Fatalf("got %q, %v", out, err)
	}
}

type alertSwitch struct {
	on      bool
	waiting int
}

func (a *alertSwitch) Enabled(context.Context) (bool, error) { return a.on, nil }
func (a *alertSwitch) Waiting(context.Context) (int, error)  { return a.waiting, nil }
func (a *alertSwitch) Set(_ context.Context, on bool) (bool, error) {
	changed := a.on != on
	a.on = on
	return changed, nil
}

func TestAlertsAreSwitchedOnAndOff(t *testing.T) {
	sw := &alertSwitch{on: true, waiting: 2}
	r := newRig(t, rigOptions{alerts: sw})
	same(t, r.answers("/alerts", now), []string{"Alerts are on: I message you here when something in your chats needs you, and before reminders. 2 waiting to go out with the next one. Send /alerts off to stop them."})
	same(t, r.answers("/alerts off", now), []string{"Alerts off. Any waiting were dropped; send /alerts on to start again."})
	same(t, r.answers("/Alerts OFF.", now), []string{"Alerts were already off."})
	same(t, r.answers("/alerts", now), []string{"Alerts are off. Send /alerts on and I will message you here when something in your chats needs you, and before reminders."})
	same(t, r.answers("/alerts on", now), []string{"Alerts on. I will message you here when something needs you."})
	same(t, r.answers("/alerts maybe", now), []string{alertsUsage})
	none := newRig(t, rigOptions{})
	same(t, none.answers("/alerts", now), []string{"Alerts are not set up here."})
}

func TestAYesOrNoForAWaitingActionNeverReachesTheModel(t *testing.T) {
	var asked []string
	r := newRig(t, rigOptions{
		chat: func(_ context.Context, m channel.IncomingMessage) (ChatAnswer, error) {
			asked = append(asked, m.Text)
			return ChatAnswer{Text: "model: " + m.Text, Remember: true}, nil
		},
		decide: func(_ context.Context, m channel.IncomingMessage) (string, bool, error) {
			if m.Text == "yes" {
				return "Sent to Rahul.", true, nil
			}
			return "", false, nil
		},
	})
	same(t, r.answers("yes", now), []string{"Sent to Rahul."})
	same(t, r.answers("what's up", now), []string{"model: what's up"})
	if !slices.Equal(asked, []string{"what's up"}) {
		t.Fatalf("the model was asked %q", asked)
	}
}

func TestAnActionIsMarkedAskedOnlyOnceItsAnswerIsSent(t *testing.T) {
	var shown []string
	r := newRig(t, rigOptions{
		chat: func(context.Context, channel.IncomingMessage) (ChatAnswer, error) {
			return ChatAnswer{Text: "*Send to Rahul: hi*", Remember: true, ActionID: "a1"}, nil
		},
		shown: func(_ context.Context, id string, sent channel.SentMessage) error {
			if sent.ID == "" {
				t.Error("marked asked without the sent message")
			}
			shown = append(shown, id)
			return nil
		},
	})
	same(t, r.answers("tell rahul hi", now), []string{"*Send to Rahul: hi*"})
	if !slices.Equal(shown, []string{"a1"}) {
		t.Fatalf("shown %v", shown)
	}
}

func TestALateYesThatAlreadyActedSaysWhatHappened(t *testing.T) {
	r := newRig(t, rigOptions{
		replay: func(_ context.Context, m channel.IncomingMessage) (string, bool, error) {
			return "Sent to Rahul.", m.Text == "yes", nil
		},
	})
	same(t, r.answers("yes", channel.ReceiveOptions{SentAt: ago(11 * time.Minute)}), []string{"Sent to Rahul."})
	if got := r.first("order the usual", channel.ReceiveOptions{SentAt: ago(11 * time.Minute)}); !strings.HasPrefix(got, "I was away when you sent this") {
		t.Fatalf("got %q", got)
	}
}

type jobBook struct{ list []jobs.Job }

func (b *jobBook) List(context.Context) ([]jobs.Job, error) { return b.list, nil }
func (b *jobBook) Cancel(_ context.Context, ids []int64) ([]jobs.Job, error) {
	var gone, kept []jobs.Job
	for _, j := range b.list {
		if slices.Contains(ids, j.ID) {
			gone = append(gone, j)
		} else {
			kept = append(kept, j)
		}
	}
	b.list = kept
	return gone, nil
}

func TestJobsListsRemindersAndCancelStopsThem(t *testing.T) {
	due := time.Date(2026, 11, 28, 18, 0, 0, 0, kolkata)
	book := &jobBook{list: []jobs.Job{
		{ID: 3, Text: "Call Shaurya", Repeat: jobs.Repeat{Kind: jobs.Once}, NextAt: due},
		{ID: 4, Text: "send me my open tasks", Ask: true, Repeat: jobs.Repeat{Kind: jobs.Weekdays}, NextAt: due.Add(15 * time.Hour)},
	}}
	r := newRig(t, rigOptions{jobs: book})
	same(t, r.answers("/jobs", now), []string{"*Reminders*\n3. Sat 28 Nov, 18:00: Call Shaurya\n4. Sun 29 Nov, 09:00, then every weekday: send me my open tasks (I do it)"})
	same(t, r.answers("/cancel 3", now), []string{"Cancelled: Call Shaurya."})
	same(t, r.answers("/cancel 3", now), []string{"No reminder with that number is set. Send /jobs to see them."})
	same(t, r.answers("/cancel soon", now), []string{cancelUsage})
	none := newRig(t, rigOptions{})
	same(t, none.answers("/jobs", now), []string{"Reminders are not set up here."})
}
