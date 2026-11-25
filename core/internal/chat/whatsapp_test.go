package chat

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func same(t *testing.T, in, want string) {
	t.Helper()
	if got := ToWhatsApp(in); got != want {
		t.Errorf("ToWhatsApp(%q) = %q, want %q", in, got, want)
	}
}

func TestToWhatsAppTurnsMarkdownIntoWhatWhatsAppShows(t *testing.T) {
	same(t, "This is **important** and __this__ too", "This is *important* and _this_ too")
	same(t, "## Plan\nstep", "*Plan*\nstep")
	same(t, "see [the docs](https://example.com/a)", "see the docs (https://example.com/a)")
	same(t, "* one\n  + two", "- one\n  - two")
}

func TestToWhatsAppLeavesCodeBlocksAloneAndTrims(t *testing.T) {
	code := "```\n**not bold**\n# not a heading\n```"
	same(t, "  **a**\n"+code+"\n**b**  ", "*a*\n"+code+"\n*b*")
}

func TestToWhatsAppLeavesCodeAndWordsWithUnderscoresOrStarsAlone(t *testing.T) {
	for _, text := range []string{
		"`__init__` and `f(**a, **b)` stay",
		"my__var__name.py",
		"a ** b and c ** d",
		"lone ** here",
		"```\n**not bold**\n# not a heading\n```",
		"x**y**z",
	} {
		same(t, text, text)
	}
}

func TestToWhatsAppFlattensNestedEmphasisWhatsAppCannotShow(t *testing.T) {
	same(t, "***bold italic*** here", "*_bold italic_* here")
	same(t, "## **Plan**", "*Plan*")
	same(t, "## Title ##", "*Title*")
}

func TestToWhatsAppDoesNotTakeAHashInATitleOrABareHashForAHeading(t *testing.T) {
	same(t, "### Learning C#", "*Learning C#*")
	same(t, "#\n\nnot heading", "#\n\nnot heading")
}

func TestToWhatsAppKeepsBracketsThatBelongToALink(t *testing.T) {
	same(t, "[wiki](https://en.wikipedia.org/wiki/Foo_(bar))", "wiki (https://en.wikipedia.org/wiki/Foo_(bar))")
	same(t, "see [docs](https://a.b/c))", "see docs (https://a.b/c))")
}

func TestToWhatsAppTreatsEveryJavaScriptLineBreakAsOne(t *testing.T) {
	same(t, "# Title\r\nbody", "*Title*\nbody")
	same(t, "# Title * item", "*Title* - item")
	same(t, "**a b**", "**a b**")
}

func TestToWhatsAppStaysFastOnLongRunsOfSpacesAndMarkers(t *testing.T) {
	started := time.Now()
	ToWhatsApp(strings.Repeat(" ", 30_000) + "x" + strings.Repeat(" ", 30_000))
	ToWhatsApp(strings.Repeat("**a", 3_000))
	ToWhatsApp(strings.Repeat("__a", 3_000))
	ToWhatsApp(strings.Repeat("**x ", 20_000))
	ToWhatsApp("# " + strings.Repeat("a ", 30_000))
	ToWhatsApp(strings.Repeat("[a](https://x", 5_000))
	ToWhatsApp(strings.Repeat("[", 30_000))
	if took := time.Since(started); took > 500*time.Millisecond && !raceDetector {
		t.Fatalf("took %v", took)
	}
}

func TestToWhatsAppDropsTheSpacesMarkdownUsesForLineBreaks(t *testing.T) {
	same(t, "- one  \n- two\t\nend", "- one\n- two\nend")
}

func TestToWhatsAppKeepsWhatsAppsOwnFormatting(t *testing.T) {
	same(t, "*bold* _italic_ ~gone~", "*bold* _italic_ ~gone~")
}

func TestClipToClipsAtWholeCharacters(t *testing.T) {
	if got := ClipTo("abc", 3); got != "abc" {
		t.Fatal(got)
	}
	if got := ClipTo("ab😀😀", 3); got != "ab😀…" {
		t.Fatal(got)
	}
}

func TestToWhatsAppMatchesTheTypeScriptCore(t *testing.T) {
	raw, err := os.ReadFile("testdata/whatsapp.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Cases []struct {
			Text string `json:"text"`
			Want string `json:"want"`
		} `json:"cases"`
		Clip []struct {
			Text   string `json:"text"`
			Length int    `json:"length"`
			Want   string `json:"want"`
		} `json:"clip"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Cases) < 500 {
		t.Fatalf("only %d cases", len(g.Cases))
	}
	for _, c := range g.Cases {
		same(t, c.Text, c.Want)
	}
	for _, c := range g.Clip {
		if got := ClipTo(c.Text, c.Length); got != c.Want {
			t.Errorf("ClipTo(%q, %d) = %q, want %q", c.Text, c.Length, got, c.Want)
		}
	}
}
