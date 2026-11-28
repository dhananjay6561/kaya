package chat

import (
	"slices"
	"testing"

	"github.com/dhananjay6561/kaya/core/internal/agent"
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

func TestPlainCapitalisedNamesAreChecked(t *testing.T) {
	answer := "Wahan Biryani House, Shree Krishna ya Truth Bowl jaise spots aate hain. Delivery fee dekh lena."
	if got := ungrounded(answer, evidence); !slices.Equal(got, []string{"Biryani House", "Shree Krishna"}) {
		t.Fatalf("got %q", got)
	}
}

func TestAnUncitedAnswerGetsTheLinkOfTheSiteItNames(t *testing.T) {
	shown := []agent.Shown{
		{Link: "https://www.magicbricks.com/sobha-daffodil-hsr-layout", Text: "Sobha Daffodil HSR Layout flats"},
		{Link: "https://www.zomato.com/bangalore/delivery-in-hsr/dish-dal-makhani", Text: "Dal Makhani in HSR Layout - Zomato"},
		{Link: "https://www.amazon.co.in/earbuds", Text: "Earbuds"},
	}
	sources := []string{shown[0].Link, shown[1].Link, shown[2].Link}
	if _, link := cite("HSR Layout mein Zomato pe dal makhani combo dekh lo.", sources, shown); len(link) != 1 || link[0] != shown[1].Link {
		t.Fatalf("link %q", link)
	}
	if _, link := cite("Amazon pe 2,999 ka mil raha hai.", sources, shown); len(link) != 1 || link[0] != shown[2].Link {
		t.Fatalf("amazon.co.in: %q", link)
	}
	if _, link := cite("HSR Layout mein dal makhani ke combos milte hain.", sources, shown); link != nil {
		t.Fatalf("an answer naming no site got %q", link)
	}
}
