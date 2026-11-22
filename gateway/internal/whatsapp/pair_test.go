package whatsapp

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/dhananjay6561/kaya/gateway/internal/logging"
	"github.com/dhananjay6561/kaya/gateway/internal/testdb"
)

func TestNormalizePhone(t *testing.T) {
	for raw, want := range map[string]string{
		"919876543210":      "919876543210",
		"+91 98765 43210":   "919876543210",
		"+1 (555) 010-0199": "15550100199",
	} {
		got, err := NormalizePhone(raw)
		if err != nil || got != want {
			t.Fatalf("%q: got %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "9876543", "09876543210", "91987654321098765", "91abc76543210", "919876543210;", "91+9876543210"} {
		if _, err := NormalizePhone(raw); err == nil {
			t.Fatalf("%q: want an error", raw)
		}
	}
}

func items(list ...whatsmeow.QRChannelItem) <-chan whatsmeow.QRChannelItem {
	ch := make(chan whatsmeow.QRChannelItem, len(list))
	for _, item := range list {
		ch <- item
	}
	close(ch)
	return ch
}

func code(text string) whatsmeow.QRChannelItem {
	return whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: text, Timeout: 20 * time.Second}
}

func TestFollowShowsQRCodesUntilLinked(t *testing.T) {
	var out bytes.Buffer
	connected := make(chan struct{}, 1)
	connected <- struct{}{}
	p := pairing{out: &out, connected: connected, timeout: time.Second}

	err := p.follow(context.Background(), items(code("first"), code("second"), whatsmeow.QRChannelSuccess))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "scan:"); n != 2 {
		t.Fatalf("want two QR codes, got %d", n)
	}
	if !strings.Contains(out.String(), "now shows under Linked devices") {
		t.Fatalf("missing the success line: %s", out.String())
	}
}

func TestFollowRequestsOnePairingCodeForAPhone(t *testing.T) {
	var out bytes.Buffer
	connected := make(chan struct{}, 1)
	connected <- struct{}{}
	calls := 0
	p := pairing{
		out: &out, phone: "919876543210", connected: connected, timeout: time.Second,
		pairPhone: func(_ context.Context, phone string) (string, error) {
			calls++
			if phone != "919876543210" {
				t.Fatalf("wrong phone %q", phone)
			}
			return "ABCD-EFGH", nil
		},
	}
	if err := p.follow(context.Background(), items(code("a"), code("b"), whatsmeow.QRChannelSuccess)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(out.String(), "ABCD-EFGH") || strings.Contains(out.String(), "scan:") {
		t.Fatalf("calls=%d out=%s", calls, out.String())
	}
}

func TestFollowFailures(t *testing.T) {
	never := make(chan struct{})
	for name, tc := range map[string]struct {
		items   <-chan whatsmeow.QRChannelItem
		pair    func(context.Context, string) (string, error)
		phone   string
		wantErr string
	}{
		"timeout":     {items: items(code("a"), whatsmeow.QRChannelTimeout), wantErr: "timed out"},
		"error":       {items: items(whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError, Error: errors.New("boom")}), wantErr: "boom"},
		"passkey":     {items: items(whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventPasskeyRequest}), wantErr: "passkey"},
		"outdated":    {items: items(whatsmeow.QRChannelClientOutdated), wantErr: "err-client-outdated"},
		"closed":      {items: items(code("a")), wantErr: "ended before"},
		"no connect":  {items: items(whatsmeow.QRChannelSuccess), wantErr: "linked, but stopped"},
		"code failed": {items: items(code("a")), phone: "919876543210", wantErr: "request pairing code", pair: func(context.Context, string) (string, error) { return "", errors.New("400") }},
	} {
		t.Run(name, func(t *testing.T) {
			p := pairing{out: &bytes.Buffer{}, phone: tc.phone, connected: never, timeout: 10 * time.Millisecond, pairPhone: tc.pair}
			err := p.follow(context.Background(), tc.items)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want an error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestFollowStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := pairing{out: &bytes.Buffer{}}
	if err := p.follow(ctx, make(chan whatsmeow.QRChannelItem)); !errors.Is(err, context.Canceled) {
		t.Fatalf("before the phone links, cancelling is a plain cancel; got %v", err)
	}
}

func TestCancellingAfterTheLinkSaysSo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := pairing{out: &bytes.Buffer{}, connected: make(chan struct{}), timeout: time.Hour}
	ch := make(chan whatsmeow.QRChannelItem, 1)
	ch <- whatsmeow.QRChannelSuccess
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if err := p.follow(ctx, ch); !errors.Is(err, ErrLinkedNotConnected) || errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestPairRefusesWhenASessionIsStored(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	sqlDB := db.SQL()
	defer func() { _ = sqlDB.Close() }()
	sessions := NewSessions(sqlDB, waLog.Noop)
	if _, err := sessions.Load(ctx); err != nil {
		t.Fatal(err)
	}
	savePaired(t, sessions, types.NewADJID("19995550100", 0, 7))
	original := newPairClient
	newPairClient = func(*store.Device, waLog.Logger) pairClient {
		t.Fatal("pairing must stop before it creates a client")
		return nil
	}
	t.Cleanup(func() { newPairClient = original })

	err := Pair(ctx, sessions, logging.New(&bytes.Buffer{}, "info"), PairOptions{Out: &bytes.Buffer{}})
	if !errors.Is(err, ErrAlreadyPaired) {
		t.Fatalf("want ErrAlreadyPaired, got %v", err)
	}
	if err := Pair(ctx, sessions, logging.New(&bytes.Buffer{}, "info"), PairOptions{Phone: "12", Out: &bytes.Buffer{}}); err == nil ||
		errors.Is(err, ErrAlreadyPaired) {
		t.Fatalf("a bad phone number must be refused before anything else, got %v", err)
	}
}
