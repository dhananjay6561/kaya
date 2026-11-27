package actions

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/dhananjay6561/kaya/core/internal/llm"
)

type cloud struct{ *llm.Stub }

func (cloud) Locality() llm.Locality { return llm.Cloud }

type maskPhone struct{}

func (maskPhone) Mask(text, _ string) string {
	return strings.ReplaceAll(text, "+918888888888", "[PHONE_1]")
}

type failing struct {
	*llm.Stub
	tier llm.Tier
}

func (f failing) Locality() llm.Locality { return llm.Local }
func (f failing) Complete(ctx context.Context, r llm.Request) (llm.Completion, error) {
	if r.Tier == f.tier {
		return llm.Completion{}, &llm.Error{Provider: "stub", Code: llm.RateLimited}
	}
	return f.Stub.Complete(ctx, r)
}

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestTheJudgeAsksTheRoutineModelWithWhatWasAskedMasked(t *testing.T) {
	stub := llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "Yes." }})
	judge := NewJudge(cloud{stub}, func() Masker { return maskPhone{} }, quietLog)
	got, err := judge(bg, "Send to Rahul (contact, +918888888888):\nrunning late", "haan bhej de yaar")
	if err != nil || got != Yes {
		t.Fatalf("got %q, %v", got, err)
	}
	r := stub.Requests()[0]
	user := r.Messages[1].Content
	if r.Tier != llm.Routine || r.Schema != nil || strings.Contains(user, "918888888888") || !strings.Contains(user, "[PHONE_1]") ||
		!strings.HasSuffix(user, "Their reply:\nhaan bhej de yaar") || !strings.Contains(r.Messages[0].Content, "when in doubt, it is other") {
		t.Fatalf("request %+v", r)
	}
}

func TestTheJudgeFallsBackToTheHeavyModelAndReadsAnythingElseAsOther(t *testing.T) {
	stub := llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "no" }})
	judge := NewJudge(failing{stub, llm.Routine}, nil, quietLog)
	if got, err := judge(bg, "Send to Rahul", "nah leave it"); err != nil || got != No || len(stub.Requests()) != 1 || stub.Requests()[0].Tier != llm.Heavy {
		t.Fatalf("got %q, %v", got, err)
	}
	both := NewJudge(failing{llm.NewStub(llm.StubOptions{}), llm.Routine}, nil, quietLog)
	if _, err := both(bg, "x", "y"); err != nil {
		t.Logf("the heavy stub echoed, which is not JSON: %v", err)
	}
	long := llm.NewStub(llm.StubOptions{})
	if got, err := NewJudge(long, nil, quietLog)(bg, "x", strings.Repeat("yes ", 200)); err != nil || got != Other || len(long.Requests()) != 0 {
		t.Fatalf("a long reply: %q, %v, %d calls", got, err, len(long.Requests()))
	}
	odd := llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "maybe" }})
	if got, err := NewJudge(odd, nil, quietLog)(bg, "x", "hmm"); err != nil || got != Other {
		t.Fatalf("an unknown meaning: %q, %v", got, err)
	}
	empty := llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return " … " }})
	if _, err := NewJudge(empty, nil, quietLog)(bg, "x", "hmm"); err == nil {
		t.Fatal("an empty answer was taken")
	}
	chatty := llm.NewStub(llm.StubOptions{Reply: func(llm.Request) string { return "Sure, they said yes" }})
	if got, err := NewJudge(chatty, nil, quietLog)(bg, "x", "hmm"); err != nil || got != Other {
		t.Fatalf("an answer that is not one of the words was %q, %v", got, err)
	}
}
