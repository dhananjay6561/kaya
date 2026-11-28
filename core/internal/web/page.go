package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// Page is a web page's readable text.
type Page struct {
	URL   string
	Title string
	Text  string
}

var (
	// ErrBlocked is a link to an address that is not on the public internet, such as this machine
	// or the home network.
	ErrBlocked = errors.New("that address is not on the public internet")
	// ErrNotAPage is a link to something other than a web page or plain text.
	ErrNotAPage = errors.New("the link is not a web page")
	// ErrNoText is a page with no readable text, such as one built by scripts.
	ErrNoText = errors.New("the page has no readable text")
)

// PageError is a page the site would not give, with its HTTP status.
type PageError struct{ Status int }

func (e *PageError) Error() string { return fmt.Sprintf("the site answered %d", e.Status) }

// FetchOptions configure a Fetcher. Zero values take the defaults.
type FetchOptions struct {
	Timeout  time.Duration
	MaxBytes int64
	// AllowPrivate lets tests fetch from this machine; it is never set in Kaya itself.
	AllowPrivate bool
}

const (
	maxPageText  = 60_000
	articleFloor = 800
	userAgent    = "Mozilla/5.0 (compatible; Kaya/1.0; personal assistant)"
)

// Fetcher reads public web pages.
type Fetcher struct {
	o      FetchOptions
	client *http.Client
}

// NewFetcher returns a Fetcher that only ever connects to public addresses, checked on every
// connection, redirects included, so a link can never reach this machine or the home network.
func NewFetcher(o FetchOptions) *Fetcher {
	if o.Timeout == 0 {
		o.Timeout = 12 * time.Second
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 3 << 20
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		if o.AllowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return ErrBlocked
		}
		if ip, err := netip.ParseAddr(host); err != nil || !public(ip) {
			return ErrBlocked
		}
		return nil
	}}
	transport := &http.Transport{
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  8 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		ForceAttemptHTTP2:      true,
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if !webAddress(r.URL) {
			return ErrBlocked
		}
		return nil
	}}
	return &Fetcher{o: o, client: client}
}

// Fetch reads one page and returns its main text: the article when there is one, otherwise all
// the visible text, so menus and price lists are kept too.
func (f *Fetcher) Fetch(ctx context.Context, raw string) (Page, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !webAddress(u) {
		return Page{}, ErrBlocked
	}
	ctx, cancel := context.WithTimeout(ctx, f.o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.1")
	req.Header.Set("Accept-Language", "en-IN,en;q=0.9,hi;q=0.8")
	resp, err := f.client.Do(req)
	if err != nil {
		if errors.Is(err, ErrBlocked) {
			return Page{}, ErrBlocked
		}
		return Page{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Page{}, &PageError{Status: resp.StatusCode}
	}
	kind, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if kind != "" && kind != "text/html" && kind != "application/xhtml+xml" && kind != "text/plain" {
		return Page{}, ErrNotAPage
	}
	body, err := charset.NewReader(io.LimitReader(resp.Body, f.o.MaxBytes), resp.Header.Get("Content-Type"))
	if err != nil {
		return Page{}, err
	}
	content, err := io.ReadAll(body)
	if err != nil && len(content) == 0 {
		return Page{}, err
	}
	page := Page{URL: resp.Request.URL.String()}
	if kind == "text/plain" {
		page.Text = tidyText(string(content))
	} else {
		page.Title, page.Text = readable(content, resp.Request.URL)
	}
	if strings.TrimSpace(page.Text) == "" {
		return Page{}, ErrNoText
	}
	if r := []rune(page.Text); len(r) > maxPageText {
		page.Text = string(r[:maxPageText])
	}
	return page, nil
}

func readable(raw []byte, u *url.URL) (string, string) {
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return "", ""
	}
	title := tidy(textOf(find(doc, "title")))
	parser := readability.NewParser()
	if article, err := parser.ParseDocument(doc, u); err == nil {
		var b strings.Builder
		if article.RenderText(&b) == nil {
			if text := tidyText(b.String()); len([]rune(text)) >= articleFloor {
				if t := tidy(article.Title()); t != "" {
					title = t
				}
				return title, text
			}
		}
	}
	return title, tidyText(visibleText(doc))
}

var skipped = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true, "template": true, "iframe": true,
	"head": true, "nav": true, "footer": true, "form": true, "button": true, "select": true,
}

var blocks = map[string]bool{
	"p": true, "div": true, "li": true, "tr": true, "br": true, "section": true, "article": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "td": true, "th": true,
	"dd": true, "dt": true, "header": true, "main": true, "aside": true, "ul": true, "ol": true, "table": true,
}

// visibleText is every text a reader would see, a line per block.
func visibleText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipped[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blocks[n.Data] {
			b.WriteString("\n")
		} else if n.Type == html.ElementNode {
			b.WriteString(" ")
		}
	}
	walk(n)
	return b.String()
}

func find(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := find(c, tag); found != nil {
			return found
		}
	}
	return nil
}

func textOf(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

// tidyText puts each line on single spaces and drops empty lines and lines repeated in a row.
func tidyText(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = tidy(line)
		if line != "" && (len(out) == 0 || out[len(out)-1] != line) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func webAddress(u *url.URL) bool {
	return u != nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// public says whether an address is on the public internet: not this machine, the home network,
// a shared or reserved range, or a mapping that could lead to one.
func public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
