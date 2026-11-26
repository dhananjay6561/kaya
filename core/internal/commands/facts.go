package commands

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/dhananjay6561/kaya/core/internal/facts"
	"github.com/dhananjay6561/kaya/core/internal/text"
)

// FactBook keeps the facts about DJ that /facts, /remember and /forget work on.
type FactBook interface {
	List(ctx context.Context) ([]facts.Fact, error)
	Remember(ctx context.Context, body, waID string) (facts.Fact, bool, error)
	Forget(ctx context.Context, ids []int64) ([]facts.Fact, error)
}

const (
	noFactBook  = "Facts are not set up here."
	forgetUsage = "Send /forget with the number of a fact from /facts, like /forget 12."
	maxListed   = 60_000
	maxLine     = 300
)

func listFacts(ctx context.Context, book FactBook) (string, error) {
	if book == nil {
		return noFactBook, nil
	}
	all, err := book.List(ctx)
	if err != nil {
		return "", err
	}
	if len(all) == 0 {
		return "I don't know any facts about you yet. Tell me one with /remember, or just mention things as we talk and I'll pick them up.", nil
	}
	footer := "\n\nRemove one with /forget and its number."
	var b strings.Builder
	section := ""
	for i, f := range all {
		heading := "*I picked up*"
		if f.Source == facts.Told {
			heading = "*You told me*"
		}
		if heading != section {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(heading)
			section = heading
		}
		line := "\n" + fmt.Sprintf("%d. %s", f.ID, text.Clip(f.Text, maxLine))
		if b.Len()+len(line)+len(footer)+40 > maxListed {
			fmt.Fprintf(&b, "\n…and %d more.", len(all)-i)
			break
		}
		b.WriteString(line)
	}
	b.WriteString(footer)
	if len(all) >= facts.MaxFacts {
		fmt.Fprintf(&b, " I keep at most %d; a new one replaces the learned fact I have not needed longest.", facts.MaxFacts)
	}
	return b.String(), nil
}

func remember(ctx context.Context, book FactBook, body, waID string) (string, error) {
	if book == nil {
		return noFactBook, nil
	}
	if strings.TrimSpace(body) == "" {
		return "Send /remember with the fact after it, like /remember I'm vegetarian.", nil
	}
	f, created, err := book.Remember(ctx, body, waID)
	switch {
	case errors.Is(err, facts.ErrEmpty):
		return "Send /remember with the fact after it, like /remember I'm vegetarian.", nil
	case errors.Is(err, facts.ErrTooLong):
		return fmt.Sprintf("That is too long for one fact. Keep it under %d characters.", facts.MaxLength), nil
	case errors.Is(err, facts.ErrSecret):
		return "I don't keep passwords, PINs, codes, or card, account or ID numbers, even when asked.", nil
	case errors.Is(err, facts.ErrFull):
		return fmt.Sprintf("You told me %d facts already, the most I keep. Remove some with /forget first.", facts.MaxFacts), nil
	case err != nil:
		return "", err
	case !created:
		return fmt.Sprintf("I already know that (fact %d).", f.ID), nil
	}
	return fmt.Sprintf("Got it, fact %d: %s", f.ID, f.Text), nil
}

func forget(ctx context.Context, book FactBook, args string) (string, error) {
	if book == nil {
		return noFactBook, nil
	}
	fields := strings.FieldsFunc(norm.NFKC.String(args), func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(fields) == 0 || len(fields) > 20 {
		return forgetUsage, nil
	}
	var ids []int64
	for _, field := range fields {
		id, err := strconv.ParseInt(strings.TrimRight(strings.TrimPrefix(field, "#"), ".!?"), 10, 64)
		if err != nil || id <= 0 {
			return forgetUsage, nil
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	gone, err := book.Forget(ctx, ids)
	if err != nil {
		return "", err
	}
	if len(gone) == 0 {
		return "No fact has that number now. Send /facts to see them.", nil
	}
	lines := make([]string, len(gone))
	for i, f := range gone {
		lines[i] = fmt.Sprintf("%d. %s", f.ID, f.Text)
	}
	out := "Forgot:\n" + strings.Join(lines, "\n")
	if missing := len(ids) - len(gone); missing > 0 {
		out += fmt.Sprintf("\n(%d of those numbers matched no fact.)", missing)
	}
	return out, nil
}
