package chat

import (
	"slices"
	"testing"
)

func TestCitationsAreHiddenAndOneLinkGoesUnderTheAnswer(t *testing.T) {
	sources := []string{"https://a.example/", "https://b.example/", "https://c.example/"}
	cases := []struct {
		name, text, want string
		links            []string
	}{
		{"the first cited source", "Magicpin pe ₹99 dikh raha hai [3]. Swiggy pe ₹120 [1].", "Magicpin pe ₹99 dikh raha hai. Swiggy pe ₹120.", []string{"https://c.example/", "https://a.example/"}},
		{"a list of numbers", "Abhi ₹39,999 se mil raha hai [2, 3].", "Abhi ₹39,999 se mil raha hai.", []string{"https://b.example/", "https://c.example/"}},
		{"gpt-oss's own brackets", "Base ₹82,900【3】, Air ₹94,990【1†source】.", "Base ₹82,900, Air ₹94,990.", []string{"https://c.example/", "https://a.example/"}},
		{"numbers that name no source", "Made up [7], real [2].", "Made up, real.", []string{"https://b.example/"}},
		{"round brackets at the end", "Pro Max ₹1,34,900 tak jaata hai. (2,\u202f3)\n\nAur kuch?", "Pro Max ₹1,34,900 tak jaata hai.\n\nAur kuch?", []string{"https://b.example/", "https://c.example/"}},
		{"round brackets closing a sentence", "Sab mila ke ₹150 (1).", "Sab mila ke ₹150.", []string{"https://a.example/"}},
		{"round brackets mid-sentence stay", "Step (2) mein checkout karo.", "Step (2) mein checkout karo.", nil},
		{"a round number that names no source stays", "Pack of (12)", "Pack of (12)", nil},
		{"nothing cited gets no link", "RCB jeeti.", "RCB jeeti.", nil},
	}
	for _, c := range cases {
		got, links := cite(c.text, sources, nil)
		if got != c.want || !slices.Equal(links, c.links) {
			t.Errorf("%s: got %q %q, want %q %q", c.name, got, links, c.want, c.links)
		}
	}
	if got, links := cite("Step [1] then [2].", nil, nil); got != "Step [1] then [2]." || links != nil {
		t.Errorf("without sources: %q %q", got, links)
	}
}

func TestAComparisonGetsALinkFromEachSite(t *testing.T) {
	sources := []string{"https://smartprix.example/a", "https://smartprix.example/b", "https://91mobiles.example/c", "https://gadgets.example/d"}
	_, links := cite("Smartprix pe ₹24,999 [1], review mein battery achhi bataayi [2][3], aur camera bhi theek [4].", sources, nil)
	if want := []string{"https://smartprix.example/a", "https://91mobiles.example/c"}; !slices.Equal(links, want) {
		t.Fatalf("links %q", links)
	}
}
