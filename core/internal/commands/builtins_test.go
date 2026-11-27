package commands

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func slash(kind, name string) *Slash { return &Slash{Kind: kind, Name: name} }

func sameSlash(t *testing.T, text string, want *Slash) {
	t.Helper()
	got := ParseSlash(text)
	if (got == nil) != (want == nil) || got != nil && *got != *want {
		t.Fatalf("%q: got %+v, want %+v", text, got, want)
	}
}

func TestKnowsTheFourBuiltinsWhateverTheCaseAndSpacing(t *testing.T) {
	sameSlash(t, "/help", slash(Builtin, "help"))
	sameSlash(t, "  /Pause \n", slash(Builtin, "pause"))
	sameSlash(t, "/STATUS", slash(Builtin, "status"))
	sameSlash(t, "/resume", slash(Builtin, "resume"))
}

func TestRefusesABuiltinWithWordsAfterIt(t *testing.T) {
	sameSlash(t, "/pause 2h", slash(ExtraText, "pause"))
	sameSlash(t, "/resume please", slash(ExtraText, "resume"))
	sameSlash(t, "/facts all", slash(ExtraText, "facts"))
}

func TestKeepsTheWordsAfterRememberAndForgetAsWritten(t *testing.T) {
	sameSlash(t, "/facts", slash(Builtin, "facts"))
	sameSlash(t, "/Remember  I'm  vegetarian\nand allergic to ＰＥＡＮＵＴＳ. ", &Slash{Kind: Builtin, Name: "remember", Args: "I'm  vegetarian\nand allergic to ＰＥＡＮＵＴＳ."})
	sameSlash(t, "\u200e／remember\u00a0मेरी बहन प्रिया है", &Slash{Kind: Builtin, Name: "remember", Args: "मेरी बहन प्रिया है"})
	sameSlash(t, "/forget 12, 13", &Slash{Kind: Builtin, Name: "forget", Args: "12, 13"})
	sameSlash(t, "/remember", slash(Builtin, "remember"))
	sameSlash(t, "/forget.", slash(Builtin, "forget"))
}

func TestTreatsAnyOtherSlashAsUnknownAndPlainTextAsNoCommand(t *testing.T) {
	sameSlash(t, "/weather", slash(Unknown, "weather"))
	sameSlash(t, "/", slash(Unknown, ""))
	sameSlash(t, "/ pause", slash(Unknown, ""))
	sameSlash(t, "/usr/bin is here", slash(Unknown, "usr/bin"))
	sameSlash(t, "pause", nil)
	sameSlash(t, "please /pause", nil)
	if got := ParseSlash("/" + strings.Repeat("😀", 100)); utf8.RuneCountInString(got.Name) != 30 {
		t.Fatalf("got %q", got.Name)
	}
	family := "👨\u200d👩\u200d👧"
	if got := ParseSlash("/" + strings.Repeat(family, 40)); got.Name != strings.Repeat(family, 30) {
		t.Fatalf("whole emoji only: got %q", got.Name)
	}
}

func TestSeesThroughWhatPhonesAndKeyboardsAdd(t *testing.T) {
	sameSlash(t, "\u200e/resume", slash(Builtin, "resume"))
	sameSlash(t, "/pause\u200b", slash(Builtin, "pause"))
	sameSlash(t, "/pause.", slash(Builtin, "pause"))
	sameSlash(t, "/Status!", slash(Builtin, "status"))
	sameSlash(t, "\uff0fpause", slash(Builtin, "pause"))
	sameSlash(t, "/pause\u00a0now", slash(ExtraText, "pause"))
	sameSlash(t, "/pause\u3000now", slash(ExtraText, "pause"))
}

func TestLetsOnlyTheFourBuiltinsPastTheCommandCap(t *testing.T) {
	for text, want := range map[string]bool{
		"/pause": true, " /Resume.": true, "/pause 2h": false, "/nope": false, "hello": false,
		"/facts": true, "/remember I like tea": false, "/forget 3": false, "/remember": true,
		"/tasks": true, "/done 3": false, "/tasks please": false,
		"/summary": false, "/summary office": false,
	} {
		if got := IsBuiltin(text); got != want {
			t.Errorf("%q: got %v", text, got)
		}
	}
}
