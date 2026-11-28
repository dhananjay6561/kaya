package chat

import "testing"

func TestCitationsAreHiddenAndOneLinkGoesUnderTheAnswer(t *testing.T) {
	sources := []string{"https://a.example/", "https://b.example/", "https://c.example/"}
	cases := []struct{ name, text, want, link string }{
		{"the first cited source", "Magicpin pe ₹99 dikh raha hai [3]. Swiggy pe ₹120 [1].", "Magicpin pe ₹99 dikh raha hai. Swiggy pe ₹120.", "https://c.example/"},
		{"a list of numbers", "Abhi ₹39,999 se mil raha hai [2, 3].", "Abhi ₹39,999 se mil raha hai.", "https://b.example/"},
		{"gpt-oss's own brackets", "Base ₹82,900【3】, Air ₹94,990【1†source】.", "Base ₹82,900, Air ₹94,990.", "https://c.example/"},
		{"numbers that name no source", "Made up [7], real [2].", "Made up, real.", "https://b.example/"},
		{"round brackets at the end", "Pro Max ₹1,34,900 tak jaata hai. (2,\u202f3)\n\nAur kuch?", "Pro Max ₹1,34,900 tak jaata hai.\n\nAur kuch?", "https://b.example/"},
		{"round brackets closing a sentence", "Sab mila ke ₹150 (1).", "Sab mila ke ₹150.", "https://a.example/"},
		{"round brackets mid-sentence stay", "Step (2) mein checkout karo.", "Step (2) mein checkout karo.", ""},
		{"a round number that names no source stays", "Pack of (12)", "Pack of (12)", ""},
		{"nothing cited gets no link", "RCB jeeti.", "RCB jeeti.", ""},
	}
	for _, c := range cases {
		got, link := cite(c.text, sources, nil)
		if got != c.want || link != c.link {
			t.Errorf("%s: got %q %q, want %q %q", c.name, got, link, c.want, c.link)
		}
	}
	if got, link := cite("Step [1] then [2].", nil, nil); got != "Step [1] then [2]." || link != "" {
		t.Errorf("without sources: %q %q", got, link)
	}
}
