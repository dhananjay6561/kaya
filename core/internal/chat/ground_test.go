package chat

import (
	"slices"
	"testing"
)

var evidence = []string{
	"Dal Makhani in Sector 6 - Zomato\nView all restaurants serving Dal Makhani in Sector 6.\nzomato.com",
	"Truth Bowl HSR - magicpin\nDal Makhani Bowl ₹99, delivery in 40 min\nmagicpin.in",
	"Apple iPhone 17 Price in India\nStarts at Rs. 82,900; Pro at ₹1,34,900 (10% off with cards)\nreliancedigital.in",
	"hsr layout sector 6 mein 150 ke andar dal makhani kahan se mangau?",
}

func TestMadeUpPlacesAndPricesAreFound(t *testing.T) {
	answer := "Kuch options:\n\n- *Shree Krishna Dhaba* – dal makhani ₹120‑₹130\n- **Truth Bowl** ka bowl ₹99, 40 min mein\n" +
		"- *The Food Factory* – thali 145 ke around\n\nBudget 150 ka hai na."
	got := ungrounded(answer, evidence)
	want := []string{"₹120", "₹130", "145", "Shree Krishna Dhaba", "The Food Factory"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWhatTheResultsSayIsGrounded(t *testing.T) {
	for _, answer := range []string{
		"iPhone 17 ₹82,900 se shuru, Pro Rs 134900 ka hai, cards pe 10% off.",
		"*Truth Bowl* pe dal makhani bowl ₹99 hai, 150 ke andar aa jayega.",
		"Pro Max ₹1.5 lakh tak jaata hai, 2 din mein delivery.",
		"*Zomato* pe *Sector 6* ke dal makhani places ki list hai.",
		"*Tip:* checkout pe fee dekh lena.",
	} {
		if got := ungrounded(answer, evidence); len(got) != 0 {
			t.Errorf("%q: flagged %q", answer, got)
		}
	}
}

func TestLinesWithMadeUpClaimsAreDropped(t *testing.T) {
	answer := "Kuch options:\n\n- *Shree Krishna Dhaba* – ₹120\n- *Truth Bowl* – ₹99\n\nCheckout pe fee dekh lena."
	got := dropClaims(answer, []string{"Shree Krishna Dhaba", "₹120"})
	if want := "Kuch options:\n\n- *Truth Bowl* – ₹99\n\nCheckout pe fee dekh lena."; got != want {
		t.Fatalf("got %q", got)
	}
}
