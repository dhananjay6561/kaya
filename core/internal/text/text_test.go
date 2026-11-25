package text

import (
	"strings"
	"testing"
)

func TestTrimsLikeJavaScript(t *testing.T) {
	if got := Trim("\ufeff hi \u3000\n"); got != "hi" {
		t.Fatalf("got %q", got)
	}
	if got := Trim("\u0085hi"); got != "\u0085hi" {
		t.Fatalf("JavaScript keeps U+0085, got %q", got)
	}
}

func TestClipsAtWholeCharacters(t *testing.T) {
	if got := Clip("short", 10); got != "short" {
		t.Fatalf("got %q", got)
	}
	if got := Clip(strings.Repeat("x", 9)+"😀😀", 10); got != strings.Repeat("x", 9)+"😀…" {
		t.Fatalf("got %q", got)
	}
	if got := Clip("broken \xff byte", 100); got != "broken � byte" {
		t.Fatalf("got %q", got)
	}
}

func TestKeepsJoinedEmojiWhole(t *testing.T) {
	family := "👨\u200d👩\u200d👧"
	if got := FirstGraphemes(strings.Repeat(family, 5), 3); got != strings.Repeat(family, 3) {
		t.Fatalf("got %q", got)
	}
}
