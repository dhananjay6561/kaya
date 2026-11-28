package chat

import (
	"slices"
	"testing"
)

func TestCitationsBecomeLinksUnderTheAnswer(t *testing.T) {
	sources := []string{"https://a.example/", "https://b.example/", "https://c.example/"}
	cases := []struct {
		name, text, want string
		links            []string
	}{
		{
			"renumbered in the order they appear",
			"Base model is ₹82,900 [3]. Pro is ₹1,34,900 [1].",
			"Base model is ₹82,900 [1]. Pro is ₹1,34,900 [2].",
			[]string{"[1] https://c.example/", "[2] https://a.example/"},
		},
		{
			"one source cited twice and a list",
			"Price [2], offers [2, 3] [2][3].",
			"Price [1], offers [1][2] [1][2].",
			[]string{"[1] https://b.example/", "[2] https://c.example/"},
		},
		{
			"numbers that name no source are dropped",
			"Made up [7]. Real [1] [0].",
			"Made up. Real [1].",
			[]string{"[1] https://a.example/"},
		},
		{"nothing cited", "RCB won.", "RCB won.", nil},
	}
	for _, c := range cases {
		got, links := cite(c.text, sources)
		if got != c.want || !slices.Equal(links, c.links) {
			t.Errorf("%s: got %q %q, want %q %q", c.name, got, links, c.want, c.links)
		}
	}
	if got, links := cite("Step [1] then [2].", nil); got != "Step [1] then [2]." || links != nil {
		t.Errorf("without sources: %q %q", got, links)
	}
}
