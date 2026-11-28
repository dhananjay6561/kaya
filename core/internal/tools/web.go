package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/agent"
	"github.com/dhananjay6561/kaya/core/internal/web"
)

// WebSearcher searches the web.
type WebSearcher interface {
	Search(ctx context.Context, query, within string, limit int) ([]web.Result, error)
}

const webResults = 6

// WebSearch searches the public web. private says whether a query holds a private detail, such as
// a phone number or an address, which must never leave for a search engine.
func WebSearch(s WebSearcher, private func(string) bool, zone *time.Location) agent.Tool {
	return agent.Tool{
		Name: "web_search",
		Description: "Search the web for anything recent or that changes (news, prices, scores, releases, schedules) " +
			"or facts you are unsure of, instead of answering from memory. Write the query like a person types into a " +
			"search engine. Never put the owner's private details in it: phone numbers, addresses, emails, codes, or what " +
			"their chats say.",
		Guide: "How to answer: like a friend who just looked it up, casual and short, naming where you saw it when that " +
			"helps (\"Magicpin pe Truth Bowl ka dal makhani ₹99 dikh raha hai\"), your own pick if they want one, and a " +
			"practical tip when it matters (\"checkout pe delivery fee dekh lena\"). Use only what the results say: never " +
			"name a place, shop, product, price, number, date or result that is not written in them, even if it sounds " +
			"likely. If they are only listing pages with no names or prices, say so plainly and tell them what the page " +
			"has. If read_page is offered and the specifics they want are missing, open the most promising result first. " +
			"Right after a fact you took from a result, write its number in square brackets, such as [2], never round " +
			"ones: they never see it, it picks the link put under your answer. Never write a link, a web address or a " +
			"list of sources. Results can be old or wrong: say so when they disagree or look out of date.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "minLength": 2, "maxLength": 200, "description": "What to search for."},
				"recent": {
					"type": "string", "enum": ["day", "week", "month", "year"],
					"description": "Only pages from the last day, week, month or year. Leave it out unless freshness matters."
				}
			},
			"required": ["query"],
			"additionalProperties": false
		}`),
		Untrusted: true,
		Public:    true,
		MaxCalls:  2,
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Query  string `json:"query"`
				Recent string `json:"recent"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			query := strings.TrimSpace(args.Query)
			if private(query) {
				return "", agent.Problemf("the query holds a private detail, such as a phone number, an email, an address or a code. Search without it.")
			}
			found, err := s.Search(ctx, query, args.Recent, webResults)
			switch {
			case errors.Is(err, web.ErrNoEngines):
				return "", agent.Problemf("no search engine answered just now. Tell them web search is not working at the moment.")
			case errors.Is(err, web.ErrUnavailable):
				return "", agent.Problemf("web search is down just now. Tell them web search is not working at the moment.")
			case err != nil:
				return "", err
			case len(found) == 0:
				return "Nothing found for " + quote(query) + ".", nil
			}
			var b strings.Builder
			for i, r := range found {
				agent.Witness(ctx, r.URL, r.Title+"\n"+r.Snippet+"\n"+site(r.URL))
				n := agent.Cite(ctx, r.URL)
				if n == 0 {
					n = i + 1
				}
				fmt.Fprintf(&b, "[%d] %s (%s", n, r.Title, site(r.URL))
				if !r.Published.IsZero() {
					b.WriteString(", " + r.Published.In(zone).Format("2 Jan 2006"))
				}
				b.WriteString(")\n")
				if r.Snippet != "" {
					b.WriteString(r.Snippet + "\n")
				}
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

// site is a link's host without "www.".
func site(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
