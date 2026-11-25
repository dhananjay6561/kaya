package search

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dhananjay6561/kaya/core/internal/masking"
)

var kolkata = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		panic(err)
	}
	return loc
}()

func at(hh, mm int) time.Time { return time.Date(2026, 11, 24, hh, mm, 0, 0, time.UTC) }

func line(id int64, sent time.Time, sender, text string) Line {
	return Line{MessageID: id, ChatID: 7, SentAt: sent, UpdatedAt: sent, Sender: sender, Text: text}
}

func ids(groups [][]Line) [][]int64 {
	var out [][]int64
	for _, g := range groups {
		var one []int64
		for _, l := range g {
			one = append(one, l.MessageID)
		}
		out = append(out, one)
	}
	return out
}

func TestGroupsMessagesSentCloseTogether(t *testing.T) {
	got := ids(Group([]Line{
		line(1, at(10, 0), "Rahul", "kal?"),
		line(2, at(10, 5), "Me", "haan"),
		line(3, at(10, 15), "Rahul", "ok"),
		line(4, at(10, 26), "Me", "late reply"),
	}, DefaultShape))
	want := [][]int64{{1, 2, 3}, {4}}
	if len(got) != 2 || len(got[0]) != 3 || got[1][0] != 4 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestStartsANewChunkWhenOneIsFull(t *testing.T) {
	var lines []Line
	for i := range 35 {
		lines = append(lines, line(int64(i), at(10, 0).Add(time.Duration(i)*time.Second), "Me", "hi"))
	}
	if got := Group(lines, DefaultShape); len(got) != 2 || len(got[0]) != 30 {
		t.Fatalf("got %d chunks, first of %d", len(got), len(got[0]))
	}
	long := strings.Repeat("word ", 400)
	if got := Group([]Line{line(1, at(10, 0), "Me", long), line(2, at(10, 1), "Me", long)}, DefaultShape); len(got) != 2 {
		t.Fatalf("two long messages share a chunk: %d", len(got))
	}
}

func TestRendersTheChatTheDayAndEachLineInTheOwnersZone(t *testing.T) {
	got := Render("College Gang", []Line{
		line(1, at(18, 25), "Rahul", "kal  milte\nhain?"),
		line(2, at(18, 35), "Me", "haan"),
	}, kolkata, DefaultShape, nil)
	want := "Chat: College Gang\n24 Nov 2026\n23:55 Rahul: kal milte hain?\n25 Nov 2026\n00:05 Me: haan"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestCutsALongMessageAtASpaceAndNeverInsideAPlaceholder(t *testing.T) {
	s := Shape{Gap: time.Minute, MaxChars: 20, MaxLines: 5}
	got := Render("c", []Line{line(1, at(10, 0), "Me", "one two three four five six seven")}, time.UTC, s, nil)
	if !strings.HasSuffix(got, "Me: one two three four…") {
		t.Fatalf("got %q", got)
	}
	if got := cut("call [PHONE_1] now please", 9); got != "call…" {
		t.Fatalf("cut inside a placeholder: %q", got)
	}
	if got := cut(strings.Repeat("क", 50), 10); utf8.RuneCountInString(got) != 11 {
		t.Fatalf("cut by bytes: %q", got)
	}
}

func TestMasksTheMessagesTogetherAndTheNamesApart(t *testing.T) {
	lines := []Line{
		line(1, at(10, 30), "Rahul", "code:"),
		line(2, at(10, 31), "+919876543210", "482913"),
		line(3, at(10, 32), "Rahul", "12 MG Road, Gurugram 122001"),
		line(4, at(10, 33), "Rahul", "mail rahul@x.com "+strings.Repeat("x", 1490)),
	}
	got := Render("+919123456780", lines, time.UTC, DefaultShape, masking.NewSession())
	for _, secret := range []string{"9876543210", "9123456780", "482913", "MG Road", "122001", "rahul@x.com"} {
		if strings.Contains(got, secret) {
			t.Errorf("%s leaked:\n%s", secret, got)
		}
	}
	if strings.Count(got, "\n") != 5 {
		t.Errorf("lines were lost or added:\n%s", got)
	}
	for _, kept := range []string{"Chat: [PHONE_1]", "10:31 [PHONE_2]: [OTP_1]", "10:32 Rahul: [ADDRESS_1]", "10:33 Rahul: mail [EMAIL_1]"} {
		if !strings.Contains(got, kept) {
			t.Errorf("missing %q in:\n%s", kept, got)
		}
	}
}

func TestHashesTheTextItWasMadeFrom(t *testing.T) {
	a := Render("c", []Line{line(1, at(10, 0), "Me", "hi")}, time.UTC, DefaultShape, nil)
	b := Render("c", []Line{line(1, at(10, 0), "Me", "hi!")}, time.UTC, DefaultShape, nil)
	again := Render("c", []Line{line(1, at(10, 0), "Me", "hi")}, time.UTC, DefaultShape, nil)
	if Hash(a) == Hash(b) || Hash(a) != Hash(again) || len(Hash(a)) != 64 {
		t.Fatal("hash does not follow the text")
	}
}
