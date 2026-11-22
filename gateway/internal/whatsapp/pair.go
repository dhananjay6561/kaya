package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/dhananjay6561/kaya/gateway/internal/ingest"
)

// ErrLinkedNotConnected means the phone linked and the session is stored, but pairing stopped
// before the first connection finished; the gateway finishes setup when it starts.
var ErrLinkedNotConnected = errors.New("linked, but stopped before the first connection finished; " +
	"start the gateway to finish setup")

// ErrAlreadyPaired means a session is stored, so pairing would create a second linked device.
var ErrAlreadyPaired = errors.New("already paired: to link again, remove Kaya under Linked devices on your phone, " +
	"start the gateway once so it notices, then pair again")

var phoneNumber = regexp.MustCompile(`^[1-9][0-9]{7,14}$`)

const (
	pairClientName   = "Chrome (Linux)"
	connectAfterPair = 30 * time.Second
)

// PairOptions chooses how to pair.
type PairOptions struct {
	// Phone, when set, pairs with an 8-character code entered on that phone instead of a QR code.
	// It is the full international number, digits only (for example 919876543210).
	Phone string
	Out   io.Writer
}

// NormalizePhone strips spaces, dashes and a leading + and checks what remains is an international number.
func NormalizePhone(raw string) (string, error) {
	var digits []rune
	for i, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			digits = append(digits, r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || (r == '+' && i == 0):
		default:
			return "", errors.New("phone number may only contain digits, spaces, dashes and a leading +")
		}
	}
	phone := string(digits)
	if !phoneNumber.MatchString(phone) {
		return "", errors.New("phone number must be the full international number, e.g. 919876543210")
	}
	return phone, nil
}

// Pair links a new device to WhatsApp and stores its session. It prints a QR code (or a pairing code
// when opts.Phone is set) to opts.Out and returns once the phone has linked and the first connection is up.
// History announcements that arrive meanwhile are queued through sink.
func Pair(ctx context.Context, sessions *Sessions, sink Announcements, logger *slog.Logger, opts PairOptions) error {
	if opts.Phone != "" {
		phone, err := NormalizePhone(opts.Phone)
		if err != nil {
			return err
		}
		opts.Phone = phone
	}
	existing, err := sessions.Load(ctx)
	if err != nil {
		return err
	}
	if existing != nil {
		return ErrAlreadyPaired
	}

	cli := newPairClient(sessions.New(), NewLogger(logger), holdMessages(ctx, sink))
	connected := make(chan struct{}, 1)
	cli.AddEventHandler(func(evt any) {
		if _, ok := evt.(*events.Connected); ok {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})
	items, err := cli.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("start pairing: %w", err)
	}
	if err := cli.ConnectContext(ctx); err != nil {
		return fmt.Errorf("connect to whatsapp: %w", err)
	}
	defer cli.Disconnect()

	p := pairing{
		out:       opts.Out,
		phone:     opts.Phone,
		connected: connected,
		timeout:   connectAfterPair,
		pairPhone: func(ctx context.Context, phone string) (string, error) {
			return cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, pairClientName)
		},
	}
	return p.follow(ctx, items)
}

type pairClient interface {
	AddEventHandler(handler whatsmeow.EventHandler) uint32
	GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	ConnectContext(ctx context.Context) error
	Disconnect()
	PairPhone(ctx context.Context, phone string, showPushNotification bool, clientType whatsmeow.PairClientType,
		clientDisplayName string) (string, error)
}

var newPairClient = func(device *store.Device, log waLog.Logger, hold whatsmeow.EventHandlerWithSuccessStatus) pairClient {
	cli := whatsmeow.NewClient(device, log)
	cli.SynchronousAck = true
	cli.EnableDecryptedEventBuffer = true
	cli.ManualHistorySyncDownload = true
	cli.DisableManualHistorySyncReceipt = true
	cli.AddEventHandlerWithSuccessStatus(hold)
	return cli
}

// Announcements stores what WhatsApp sends while pairing that must not be lost. The ingester
// implements it.
type Announcements interface {
	Handle(ctx context.Context, own ingest.Own, evt any) bool
}

// holdMessages is the pairing run's handler for incoming events. Ordinary messages are not
// acknowledged: the run cannot store them, so they stay with WhatsApp until the gateway connects.
// History announcements are stored right away through sink, because WhatsApp starts sending them
// the moment the phone links and whatsmeow confirms them before any handler runs; waiting for the
// gateway could lose them. Everything else passes, including the events pairing itself waits for.
func holdMessages(ctx context.Context, sink Announcements) whatsmeow.EventHandlerWithSuccessStatus {
	return func(evt any) bool {
		switch e := evt.(type) {
		case *events.Message:
			if sink != nil && e.Info.IsFromMe && e.Message.GetProtocolMessage().GetHistorySyncNotification() != nil {
				return sink.Handle(ctx, ingest.Own{}, e)
			}
			return false
		case *events.UndecryptableMessage:
			return false
		}
		return true
	}
}

type pairing struct {
	out       io.Writer
	phone     string
	connected <-chan struct{}
	timeout   time.Duration
	pairPhone func(ctx context.Context, phone string) (string, error)
}

func (p pairing) follow(ctx context.Context, items <-chan whatsmeow.QRChannelItem) error {
	codeShown := false
	for {
		var item whatsmeow.QRChannelItem
		var open bool
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item, open = <-items:
		}
		if !open {
			return errors.New("pairing ended before the phone linked; run it again")
		}
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			if err := p.show(ctx, item, codeShown); err != nil {
				return err
			}
			codeShown = true
		case whatsmeow.QRChannelSuccess.Event:
			_, _ = fmt.Fprintln(p.out, "Linked. Finishing setup...")
			select {
			case <-p.connected:
				_, _ = fmt.Fprintf(p.out, "Done. %s now shows under Linked devices on your phone.\n", DeviceName)
				return nil
			case <-time.After(p.timeout):
				return ErrLinkedNotConnected
			case <-ctx.Done():
				return ErrLinkedNotConnected
			}
		case whatsmeow.QRChannelTimeout.Event:
			return errors.New("pairing timed out before the phone linked; run it again")
		case whatsmeow.QRChannelEventError:
			return fmt.Errorf("pairing failed: %w", item.Error)
		case whatsmeow.QRChannelEventPasskeyRequest:
			return errors.New("WhatsApp asked for a passkey, which the gateway cannot provide; pair with a phone number instead")
		case whatsmeow.QRChannelScannedWithoutMultidevice.Event:
			_, _ = fmt.Fprintln(p.out, "Your phone has to support linked devices. Update WhatsApp and scan again.")
		default:
			return fmt.Errorf("pairing failed: %s", item.Event)
		}
	}
}

func (p pairing) show(ctx context.Context, item whatsmeow.QRChannelItem, codeShown bool) error {
	if p.phone == "" {
		_, _ = fmt.Fprintln(p.out, "\nOn your phone: WhatsApp > Settings > Linked devices > Link a device, then scan:")
		if err := renderQR(p.out, item.Code); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(p.out, "This code changes in %s.\n", item.Timeout.Round(time.Second))
		return nil
	}
	if codeShown {
		return nil
	}
	code, err := p.pairPhone(ctx, p.phone)
	if err != nil {
		return fmt.Errorf("request pairing code: %w", err)
	}
	_, _ = fmt.Fprintf(p.out, "\nOn your phone: tap the notification, or go to WhatsApp > Settings > Linked devices > "+
		"Link a device > Link with phone number instead, and enter:\n\n    %s\n\n", code)
	return nil
}
