package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dhananjay6561/kaya/core/internal/agent"
	"github.com/dhananjay6561/kaya/core/internal/llm"
	"github.com/dhananjay6561/kaya/core/internal/web"
)

type fetcher struct {
	asked string
	page  web.Page
	err   error
}

func (f *fetcher) Fetch(_ context.Context, link string) (web.Page, error) {
	f.asked = link
	return f.page, f.err
}

type failing struct{ *llm.Stub }

func (failing) Complete(context.Context, llm.Request) (llm.Completion, error) {
	return llm.Completion{}, &llm.Error{Provider: "stub", Code: llm.RateLimited}
}

var menu = web.Page{
	URL:   "https://www.magicpin.in/hsr/truth-bowl",
	Title: "Truth Bowl, HSR - magicpin",
	Text:  "Dal Makhani Bowl ₹99\nPaneer Rice Bowl ₹149\nDelivery in 90-95 min",
}

func TestReadPageKeepsWhatAnswersTheQuestion(t *testing.T) {
	var asked llm.Request
	model := llm.NewStub(llm.StubOptions{Reply: func(r llm.Request) string { asked = r; return "Dal Makhani Bowl ₹99\nDelivery in 90-95 min" }})
	f := &fetcher{page: menu}
	ctx, cited := agent.CollectSources(context.Background())
	got, err := ReadPage(f, model, private).Run(ctx, []byte(`{"url":"https://magicpin.in/hsr/truth-bowl","looking_for":"dal makhani price"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "[1] Truth Bowl, HSR - magicpin (magicpin.in)\nDal Makhani Bowl ₹99\nDelivery in 90-95 min"; got != want {
		t.Fatalf("got %q", got)
	}
	if asked.Tier != llm.Routine || !strings.Contains(asked.Messages[1].Content, "Looking for: dal makhani price") || !strings.Contains(asked.Messages[1].Content, "Paneer Rice Bowl ₹149") {
		t.Fatalf("asked %+v", asked)
	}
	if links := cited(); len(links) != 1 || links[0] != "https://magicpin.in/hsr/truth-bowl" || f.asked != links[0] {
		t.Fatalf("cited %v, fetched %q", links, f.asked)
	}
}

func TestReadPageGivesTheStartWhenItCannotShorten(t *testing.T) {
	got, err := ReadPage(&fetcher{page: menu}, failing{llm.NewStub(llm.StubOptions{})}, private).
		Run(context.Background(), []byte(`{"url":"https://magicpin.in/x","looking_for":"menu"}`))
	if err != nil || !strings.Contains(got, "could not be shortened") || !strings.Contains(got, "Paneer Rice Bowl ₹149") {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestReadPageSaysWhyAPageCouldNotBeRead(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{web.ErrBlocked, "not on the public internet"},
		{web.ErrNotAPage, "not a web page"},
		{web.ErrNoText, "needs a browser"},
		{&web.PageError{Status: 403}, "answered 403"},
		{context.DeadlineExceeded, "too long"},
		{errors.New("connection reset"), "did not answer"},
	} {
		_, err := ReadPage(&fetcher{err: c.err}, llm.NewStub(llm.StubOptions{}), private).
			Run(context.Background(), []byte(`{"url":"https://a.example/","looking_for":"menu"}`))
		var problem *agent.Problem
		if !errors.As(err, &problem) || !strings.Contains(problem.Message, c.want) {
			t.Errorf("%v: got %v", c.err, err)
		}
	}
}

func TestReadPageNeverSendsAPrivateDetail(t *testing.T) {
	f := &fetcher{page: menu}
	_, err := ReadPage(f, llm.NewStub(llm.StubOptions{}), private).
		Run(context.Background(), []byte(`{"url":"https://a.example/","looking_for":"who owns 9876543210"}`))
	var problem *agent.Problem
	if !errors.As(err, &problem) || f.asked != "" {
		t.Fatalf("fetched %q, err %v", f.asked, err)
	}
}
