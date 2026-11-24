package channel

import (
	"context"
	"errors"
	"slices"
	"testing"
)

var bg = context.Background()

func TestStubRecordsWhatItSendsOnceStarted(t *testing.T) {
	s := NewStub()
	_ = s.Start(bg)
	sent, err := s.Send(bg, OutgoingMessage{IdempotencyKey: "k1", Text: "hello DJ"}, SendOptions{})
	if err != nil || sent.ID == "" || sent.SentAt.IsZero() {
		t.Fatalf("got %+v %v", sent, err)
	}
	if !slices.Equal(s.Sent(), []OutgoingMessage{{IdempotencyKey: "k1", Text: "hello DJ"}}) {
		t.Fatalf("got %+v", s.Sent())
	}
}

func TestStubSendsARepeatedKeyOnce(t *testing.T) {
	s := NewStub()
	_ = s.Start(bg)
	first, _ := s.Send(bg, OutgoingMessage{IdempotencyKey: "same", Text: "hi"}, SendOptions{})
	again, _ := s.Send(bg, OutgoingMessage{IdempotencyKey: "same", Text: "hi"}, SendOptions{})
	if again != first || len(s.Sent()) != 1 {
		t.Fatalf("got %+v and %+v, %d sent", first, again, len(s.Sent()))
	}
	if _, err := s.Send(bg, OutgoingMessage{IdempotencyKey: "same", Text: "bye"}, SendOptions{}); !errors.As(err, new(*KeyReusedError)) {
		t.Fatalf("a reused key with other text must be refused, got %v", err)
	}
	if _, err := s.Send(bg, OutgoingMessage{IdempotencyKey: "same", Text: "bye"}, SendOptions{TextMayChange: true}); err != nil {
		t.Fatalf("unless the text may change: %v", err)
	}
}

func TestStubRefusesToSendOrReceiveWhenStopped(t *testing.T) {
	s := NewStub()
	if _, err := s.Send(bg, OutgoingMessage{IdempotencyKey: "k", Text: "x"}, SendOptions{}); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("got %v", err)
	}
	_ = s.Start(bg)
	s.Stop()
	if _, err := s.Send(bg, OutgoingMessage{IdempotencyKey: "k", Text: "x"}, SendOptions{}); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := s.Receive(bg, "x", ReceiveOptions{}); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("got %v", err)
	}
}

func TestStubHealthCheck(t *testing.T) {
	s := NewStub()
	if err := s.Check(bg); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("got %v", err)
	}
	_ = s.Start(bg)
	if err := s.Check(bg); err != nil {
		t.Fatal(err)
	}
}

func TestStubRefusesAnEmptyMessageOrKey(t *testing.T) {
	s := NewStub()
	_ = s.Start(bg)
	for _, m := range []OutgoingMessage{{IdempotencyKey: "k", Text: "  "}, {IdempotencyKey: " ", Text: "x"}} {
		if _, err := s.Send(bg, m, SendOptions{}); !errors.Is(err, ErrEmpty) {
			t.Fatalf("%+v: got %v", m, err)
		}
	}
}

func TestStubDeliversToEveryHandlerUntilItUnsubscribes(t *testing.T) {
	s := NewStub()
	_ = s.Start(bg)
	var seen []string
	off := s.OnMessage(func(_ context.Context, m IncomingMessage) (Handled, error) {
		seen = append(seen, "a:"+m.Text)
		return Handled{}, nil
	})
	s.OnMessage(func(_ context.Context, m IncomingMessage) (Handled, error) {
		seen = append(seen, "b:"+m.Text)
		return Handled{AnswerID: "3EB0B"}, nil
	})
	_, handled, _ := s.Receive(bg, "one", ReceiveOptions{})
	off()
	_, _, _ = s.Receive(bg, "two", ReceiveOptions{})
	if !slices.Equal(seen, []string{"a:one", "b:one", "b:two"}) || handled.AnswerID != "3EB0B" {
		t.Fatalf("seen %v, handled %+v", seen, handled)
	}
}

func TestStubKeepsDeliveringWhenOneHandlerFailsThenReportsEveryFailure(t *testing.T) {
	s := NewStub()
	_ = s.Start(bg)
	var seen []string
	first, third := errors.New("first broke"), errors.New("third broke")
	s.OnMessage(func(context.Context, IncomingMessage) (Handled, error) { return Handled{}, first })
	s.OnMessage(func(_ context.Context, m IncomingMessage) (Handled, error) {
		seen = append(seen, m.Text)
		return Handled{}, nil
	})
	s.OnMessage(func(context.Context, IncomingMessage) (Handled, error) { return Handled{}, third })
	_, _, err := s.Receive(bg, "x", ReceiveOptions{})
	if !errors.Is(err, first) || !errors.Is(err, third) || !slices.Equal(seen, []string{"x"}) {
		t.Fatalf("err %v, seen %v", err, seen)
	}
}

func TestStubFollowsWhatWasSent(t *testing.T) {
	s := NewStub()
	_ = s.Start(bg)
	if got, err := s.Follow(bg, "nothing"); got != nil || err != nil {
		t.Fatalf("got %v %v", got, err)
	}
	sent, _ := s.Send(bg, OutgoingMessage{IdempotencyKey: "k", Text: "x"}, SendOptions{})
	if got, _ := s.Follow(bg, "k"); got == nil || *got != sent {
		t.Fatalf("got %v", got)
	}
}
