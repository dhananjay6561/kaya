package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func site(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func page(contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(body))
	}
}

var local = NewFetcher(FetchOptions{AllowPrivate: true})

func TestAnArticleIsReadWithoutItsChrome(t *testing.T) {
	para := strings.Repeat("The 2026 final was won by Royal Challengers Bengaluru, who beat Gujarat Titans by 6 wickets. ", 12)
	u := site(t, page("text/html; charset=utf-8", `<html><head><title>IPL 2026 final report</title><script>var x=1</script></head><body>
		<nav>Home | Scores | Login</nav><article><h1>RCB win the final</h1><p>`+para+`</p><p>`+para+`</p></article>
		<footer>Cookie policy</footer></body></html>`))
	got, err := local.Fetch(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "Royal Challengers Bengaluru") || strings.Contains(got.Text, "Login") || strings.Contains(got.Text, "var x") || got.Title == "" {
		t.Fatalf("got %+v", got)
	}
}

func TestAMenuPageKeepsItsItemsAndPrices(t *testing.T) {
	u := site(t, page("text/html", `<html><head><title>Truth Bowl, HSR - magicpin</title></head><body><nav>Sign in</nav>
		<div class="menu"><div><span>Dal Makhani Bowl</span><span>₹99</span></div><div><span>Paneer Rice Bowl</span><span>₹149</span></div>
		<div>Delivery in 90-95 min</div><p>Truth Bowl serves North Indian rice bowls across HSR Layout, Koramangala and BTM, with
		vegetarian and non-vegetarian options, combos and desserts. Orders above 199 get free delivery.</p></div>
		<button>Add</button></body></html>`))
	got, err := local.Fetch(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Dal Makhani Bowl", "₹99", "Paneer Rice Bowl ₹149", "90-95 min"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("%q missing from %q", want, got.Text)
		}
	}
	if strings.Contains(got.Text, "Sign in") || got.Title != "Truth Bowl, HSR - magicpin" {
		t.Errorf("got %+v", got)
	}
}

func TestOtherCharsetsAndPlainTextAreRead(t *testing.T) {
	u := site(t, page("text/html; charset=iso-8859-1", "<html><body><p>Caf\xe9 menu</p><p>"+strings.Repeat("Espresso, cappuccino and filter coffee all day. ", 6)+"</p></body></html>"))
	if got, err := local.Fetch(context.Background(), u); err != nil || !strings.Contains(got.Text, "Café menu") {
		t.Fatalf("latin-1: %q %v", got.Text, err)
	}
	long := strings.Repeat("x", 200)
	u = site(t, page("text/plain", "line one\n\n\n"+long+"\n"+long))
	if got, err := local.Fetch(context.Background(), u); err != nil || got.Text != "line one\n"+long {
		t.Fatalf("plain: %q %v", got.Text, err)
	}
}

func TestPagesThatCannotBeReadSayWhy(t *testing.T) {
	cases := map[string]struct {
		h    http.HandlerFunc
		want func(error) bool
	}{
		"a pdf":      {page("application/pdf", "%PDF"), func(err error) bool { return errors.Is(err, ErrNotAPage) }},
		"no text":    {page("text/html", "<html><body><script>render()</script></body></html>"), func(err error) bool { return errors.Is(err, ErrNoText) }},
		"only menus": {page("text/html", "<html><body><div>Orders</div><div>Cart</div><div>Wishlist</div></body></html>"), func(err error) bool { return errors.Is(err, ErrNoText) }},
		"a refusal":  {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }, func(err error) bool { var p *PageError; return errors.As(err, &p) && p.Status == 403 }},
	}
	for name, c := range cases {
		if _, err := local.Fetch(context.Background(), site(t, c.h)); !c.want(err) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestLocalAndPrivateAddressesAreNeverFetched(t *testing.T) {
	reached := false
	u := site(t, func(w http.ResponseWriter, _ *http.Request) { reached = true; _, _ = w.Write([]byte("secret")) })
	strict := NewFetcher(FetchOptions{})
	for _, link := range []string{u, "http://localhost:1/", "http://[::1]:1/", "file:///etc/passwd", "http://someone@example.com/"} {
		if _, err := strict.Fetch(context.Background(), link); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: got %v", link, err)
		}
	}
	if reached {
		t.Fatal("a private address was reached")
	}
}

func TestARedirectToAPrivateAddressIsBlocked(t *testing.T) {
	inner := site(t, page("text/plain", "router admin "+strings.Repeat(".", 200)))
	outer := site(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, inner, http.StatusFound) })
	f := NewFetcher(FetchOptions{AllowPrivate: true})
	if got, err := f.Fetch(context.Background(), outer); err != nil || !strings.HasPrefix(got.Text, "router admin") {
		t.Fatalf("redirects are followed: %q %v", got.Text, err)
	}
	if _, err := NewFetcher(FetchOptions{}).Fetch(context.Background(), outer); !errors.Is(err, ErrBlocked) {
		t.Fatalf("got %v", err)
	}
}

func TestOnlyPublicAddressesArePublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700:4700::1111": true, "142.250.183.14": true,
		"127.0.0.1": false, "10.0.0.5": false, "172.18.0.3": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.100.1.1": false, "0.0.0.0": false, "::1": false, "fc00::1": false, "fe80::1": false, "::ffff:192.168.1.1": false,
		"64:ff9b::a00:1": false, "224.0.0.1": false, "198.18.0.1": false,
	} {
		if got := public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: public = %v", addr, got)
		}
	}
}
