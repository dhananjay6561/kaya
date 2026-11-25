package masking

import (
	"encoding/json"
	"maps"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func masked(text string) string { return NewSession().Mask(text, "") }

func same(t *testing.T, got, want, why string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %q, want %q", why, got, want)
	}
}

func TestHidesIndianAndInternationalPhoneNumbersInTheWaysPeopleWriteThem(t *testing.T) {
	for _, phone := range []string{"9876543210", "+91 98765 43210", "+919876543210", "098765-43210", "91-98765-43210", "+1 415 555 2671", "+44 20 7946 0958"} {
		same(t, masked("call me on "+phone+" tomorrow"), "call me on [PHONE_1] tomorrow", phone)
	}
}

func TestHidesEmailsAndUPIIDs(t *testing.T) {
	same(t, masked("mail rahul.k@gmail.com"), "mail [EMAIL_1]", "email")
	same(t, masked("pay on rahul99@okaxis please"), "pay on [UPI_1] please", "upi")
}

func TestHidesOTPsOnlyWhereTheMessageIsAboutOne(t *testing.T) {
	same(t, masked("Your OTP is 482913. Do not share it."), "Your OTP is [OTP_1]. Do not share it.", "otp")
	same(t, masked("482913 is your verification code"), "[OTP_1] is your verification code", "verification")
	same(t, masked("X pe dm passcode is 6561"), "X pe dm passcode is [OTP_1]", "passcode")
	same(t, masked("meet at 4521 Main St"), "meet at 4521 Main St", "no code word, no OTP")
}

func TestHidesCardNumbersThatPassTheLuhnCheckAndOnlyThose(t *testing.T) {
	same(t, masked("card 4111 1111 1111 1111 exp 12/29"), "card [CARD_1] exp 12/29", "spaced")
	same(t, masked("card 4111-1111-1111-1111"), "card [CARD_1]", "dashed")
	same(t, masked("ref 1234 5678 9012 3456"), "ref 1234 5678 9012 3456", "not luhn")
}

func TestHidesBankDetailsAndGovernmentIDs(t *testing.T) {
	same(t, masked("A/c no: 50100234567891, IFSC HDFC0001234"), "A/c no: [ACCOUNT_1], IFSC [IFSC_1]", "bank")
	same(t, masked("account number 123456789012"), "account number [ACCOUNT_1]", "account")
	same(t, masked("aadhaar 2345 6789 0123"), "aadhaar [AADHAAR_1]", "aadhaar")
	same(t, masked("PAN ABCDE1234F"), "PAN [PAN_1]", "pan")
}

func TestHidesAddresses(t *testing.T) {
	same(t, masked("Flat No. 302, Sunshine Apartments"), "[ADDRESS_1], Sunshine Apartments", "flat")
	same(t, masked("H.No 14/2"), "[ADDRESS_1]", "house")
	same(t, masked("send it here:\n12 MG Road, Sector 15, Gurugram 122001\nthanks"), "send it here:\n[ADDRESS_1]\nthanks", "line")
	same(t, masked("pincode 110001"), "[ADDRESS_1]", "pincode")
}

func TestLeavesEverydayNumbersAlone(t *testing.T) {
	for _, text := range []string{
		"it costs ₹4,500 for 2 people", "see you at 10:30 on 25/11/2026", "the meeting is in 2026",
		"order #4839201 shipped", "flight 6E 2341 at gate 12", "my score was 98.5%", "call at 5 pm, 3 of us",
		"Salary of 150000 is credited; the office is near Cyber Hub.", "In your code, set the port to 5432 and timeout 30000",
		"Rs 45000 and the promo code SAVE10", "the door 2 times", "shop 24x7", "it costs 1,23,456 rupees", "₹ 98765 total",
		"the OTP screen ships in 2026", "the OTP service costs Rs 4500 a month", "tracking id 123456789012 via Bluedart",
	} {
		same(t, masked(text), text, text)
	}
	same(t, masked("your PIN is 482913"), "your PIN is [OTP_1]", "pin")
}

func TestGivesTheSameValueTheSamePlaceholderAcrossTextsAndPutsThemBack(t *testing.T) {
	s := NewSession()
	same(t, s.Mask("Rahul: 9876543210, rahul@x.com", ""), "Rahul: [PHONE_1], [EMAIL_1]", "one")
	same(t, s.Mask("again +91 98765 43210 and 9123456780", ""), "again [PHONE_1] and [PHONE_2]", "two")
	same(t, s.Restore("Call [PHONE_1] or [PHONE_2], mail [EMAIL_1]"), "Call 9876543210 or 9123456780, mail rahul@x.com", "restore")
	same(t, s.Restore("unknown [PHONE_9] stays"), "unknown [PHONE_9] stays", "unknown")
	if got := s.Counts(); !maps.Equal(got, map[string]int{Phone: 2, Email: 1}) {
		t.Fatalf("counts %v", got)
	}
}

func TestRestoresAnAddressLineThatHadAnotherDetailInsideIt(t *testing.T) {
	s := NewSession()
	text := "12 MG Road, Gurugram 122001, call 9876543210"
	hidden := s.Mask(text, "")
	if regexp.MustCompile(`9876543210|122001|MG Road`).MatchString(hidden) {
		t.Fatalf("leaked: %q", hidden)
	}
	same(t, s.Restore(hidden), text, "round trip")
}

func TestNeverConfusesAPlaceholderSomeoneTypedWithOneItIssued(t *testing.T) {
	s := NewSession()
	old := s.Mask("I couldn't find [PHONE_1] anywhere", "")
	real := s.Mask("Priya is on 9123456780", "")
	same(t, real, "Priya is on [PHONE_1]", "real")
	if strings.Contains(old, "[PHONE_1]") {
		t.Fatalf("the typed placeholder was not escaped: %q", old)
	}
	same(t, s.Restore(old), "I couldn't find [PHONE_1] anywhere", "typed")
	same(t, s.Restore(real), "Priya is on 9123456780", "issued")
}

func TestGivesBackExactlyTheOriginalTextAfterARoundTrip(t *testing.T) {
	for _, text := range []string{
		"https://x.com/someone/status/1859340012345678901 and https://wa.me/919876543210",
		"OTP 482913, card 4111 1111 1111 1111, flat 302, Sector 15 Gurugram 122001",
		"nothing private here at all",
		"😀 call 9876543210 😀",
	} {
		s := NewSession()
		same(t, s.Restore(s.Mask(text, "")), text, text)
	}
}

func hides(t *testing.T, text string, secrets ...string) {
	t.Helper()
	out := masked(text)
	for _, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Errorf("%s leaked from %q -> %q", secret, text, out)
		}
	}
}

func TestFindsANumberRightAfterAnotherNumber(t *testing.T) {
	hides(t, "Room 302 9876543210", "9876543210")
	hides(t, "ref 12345 9876543210", "9876543210")
	hides(t, "numbers: 98765 43210 91234 56780", "98765 43210", "91234 56780")
}

func TestGivesDifferentPANsAndIFSCsDifferentPlaceholders(t *testing.T) {
	s := NewSession()
	in := "Rahul PAN ABCDE1234F, Priya PAN PQRST1234Z, HDFC0001234 vs ICIC0001234"
	out := s.Mask(in, "")
	same(t, out, "Rahul PAN [PAN_1], Priya PAN [PAN_2], [IFSC_1] vs [IFSC_2]", "mask")
	same(t, s.Restore(out), in, "restore")
}

func TestJudgesEachRepeatOfAnAccountNumberByItsOwnContext(t *testing.T) {
	hides(t, "Sent from 100123456789; account no is 100123456789", "account no is 100123456789")
}

func TestHidesUPIIDsAtTheEndOfASentence(t *testing.T) {
	hides(t, "pay me at rahul@ybl.", "rahul@ybl")
	hides(t, "rahul@okhdfcbank.", "rahul@okhdfcbank")
	hides(t, "9876543210@ybl.", "9876543210")
}

func TestHidesOTPsHoweverTheyAreWritten(t *testing.T) {
	hides(t, "OTP:482913", "482913")
	hides(t, "otp482913", "482913")
	hides(t, "OTP: 4 8 2 9 1 3", "4 8 2 9 1 3")
	hides(t, "OTP is 482-913", "482-913")
	hides(t, "Use 482913 to log in, it's your login code", "482913")
	hides(t, "code hai 482913", "482913")
	same(t, NewSession().Mask("Here it is: 482913", "what was that OTP?"), "Here it is: [OTP_1]", "context")
}

func TestHidesLandlinesOddSpellingsOtherScriptsAndDottedCards(t *testing.T) {
	hides(t, "office 011-23456789", "23456789")
	hides(t, "office 011 2345 6789", "2345 6789")
	hides(t, "+91-9876-543-210", "9876-543-210")
	hides(t, "+91 9876 543 210", "9876 543 210")
	hides(t, "(+91) 98765-43210", "98765-43210")
	hides(t, "नंबर ९८७६५४३२१० है", "९८७६५४३२१०")
	hides(t, "full width ９８７６５４３２１０", "９８７６５４３２１０")
	hides(t, "4111.1111.1111.1111", "4111.1111.1111.1111")
	hides(t, "pan abcde1234f", "abcde1234f")
}

func TestHidesAddressesWrittenWithoutAPINCode(t *testing.T) {
	hides(t, "C-12, Lajpat Nagar, New Delhi", "C-12", "Lajpat")
	hides(t, "B-204, Sunshine Apartments, Sector 15, Gurugram", "B-204", "Sunshine")
	hides(t, "12 MG Road, Sector 15, Gurugram", "MG Road")
	hides(t, "send here:\n12 MG Road, Gurugram 122001, call 9876543210", "MG Road", "122001", "9876543210")
}

func TestHidesBankAccountsNamedByTheBankOrWithSpaces(t *testing.T) {
	hides(t, "HDFC 50100234567891", "50100234567891")
	hides(t, "transfer to 50100234567891 at HDFC", "50100234567891")
	hides(t, "a/c 5010 0234 5678 91", "5010 0234 5678 91")
}

func TestHidesVehicleAndPassportNumbers(t *testing.T) {
	hides(t, "car DL 3C AB 1234 parked", "DL 3C AB 1234")
	hides(t, "passport K1234567", "K1234567")
}

func TestRestoresPlaceholdersTheModelReformattedAndOnlyOnesItIssued(t *testing.T) {
	s := NewSession()
	s.Mask("Rahul 9876543210", "")
	for _, form := range []string{"[PHONE_1]", "[Phone_1]", "PHONE_1", "[PHONE 1]", "[ PHONE_1 ]", `[PHONE\_1]`} {
		same(t, s.Restore("call "+form+" now"), "call 9876543210 now", form)
	}
	same(t, s.Restore("call [PHONE_2] now"), "call [PHONE_2] now", "not issued")
}

func TestReadsCaseOnlyAsJavaScriptsIFlagDid(t *testing.T) {
	same(t, masked("Flat ſ12 and FLAT 12"), "Flat ſ12 and [ADDRESS_1]", "ſ is not s")
	s := NewSession()
	s.Mask("12 MG Road, Gurugram 122001", "")
	same(t, s.Restore("[ADDREſS_1] [address_1]"), "[ADDREſS_1] 12 MG Road, Gurugram 122001", "restore")
}

func TestStaysFastOnLongText(t *testing.T) {
	if raceDetector {
		t.Skip("timing means nothing under the race detector")
	}
	started := time.Now()
	masked(strings.Repeat("9876543210 rahul@x.com OTP 123456 ", 2000))
	if took := time.Since(started); took > time.Second {
		t.Fatalf("took %v", took)
	}
}

func TestStaysFastOnHostileInput(t *testing.T) {
	if raceDetector {
		t.Skip("timing means nothing under the race detector")
	}
	started := time.Now()
	for _, text := range []string{
		"flat" + strings.Repeat(" ", 5000) + "x",
		"Flat" + strings.Repeat("\n", 5000),
		"house no" + strings.Repeat("\t", 5000) + "#",
		strings.Repeat("1", 20000),
		strings.Repeat("1 ", 10000),
		strings.Repeat("12 road ", 5000),
		strings.Repeat("a", 30000) + "@" + strings.Repeat("b", 30000),
		strings.Repeat(", 122001", 3000),
		strings.Repeat("😀", 20000) + " 122001 road",
	} {
		masked(text)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("took %v", took)
	}
}

type goldenCase struct {
	Text     string         `json:"text"`
	Context  string         `json:"context"`
	Want     string         `json:"want"`
	Restored *string        `json:"restored"`
	Counts   map[string]int `json:"counts"`
}

func TestMasksLikeTheTypeScriptCore(t *testing.T) {
	raw, err := os.ReadFile("testdata/masking.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Cases    []goldenCase `json:"cases"`
		Sequence []struct {
			Text string `json:"text"`
			Want string `json:"want"`
		} `json:"sequence"`
		Restores []struct {
			Text string `json:"text"`
			Want string `json:"want"`
		} `json:"restores"`
		Counts   map[string]int `json:"counts"`
		Sessions []struct {
			Texts    []string       `json:"texts"`
			Wants    []string       `json:"wants"`
			Probe    string         `json:"probe"`
			Restored string         `json:"restored"`
			Counts   map[string]int `json:"counts"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Cases) < 2000 {
		t.Fatalf("only %d cases", len(g.Cases))
	}
	failures := 0
	for _, c := range g.Cases {
		s := NewSession()
		got := s.Mask(c.Text, c.Context)
		restored := c.Text
		if c.Restored != nil {
			restored = *c.Restored
		}
		if got != c.Want || s.Restore(got) != restored || !maps.Equal(s.Counts(), c.Counts) {
			failures++
			if failures <= 20 {
				t.Errorf("Mask(%q, %q)\n got  %q\n want %q", c.Text, c.Context, got, c.Want)
			}
		}
	}
	if failures > 0 {
		t.Errorf("%d of %d cases differ", failures, len(g.Cases))
	}
	s := NewSession()
	for _, c := range g.Sequence {
		same(t, s.Mask(c.Text, ""), c.Want, c.Text)
	}
	for _, c := range g.Restores {
		same(t, s.Restore(c.Text), c.Want, c.Text)
	}
	if !maps.Equal(s.Counts(), g.Counts) {
		t.Errorf("counts %v, want %v", s.Counts(), g.Counts)
	}
	if len(g.Sessions) < 100 {
		t.Fatalf("only %d sessions", len(g.Sessions))
	}
	for _, c := range g.Sessions {
		s := NewSession()
		for i, text := range c.Texts {
			same(t, s.Mask(text, ""), c.Wants[i], text)
		}
		same(t, s.Restore(c.Probe), c.Restored, c.Probe)
		if !maps.Equal(s.Counts(), c.Counts) {
			t.Errorf("counts %v, want %v", s.Counts(), c.Counts)
		}
	}
}
