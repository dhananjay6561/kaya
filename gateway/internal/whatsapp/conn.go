package whatsapp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/logging"
)

// ReasonNotPaired is the reason reported while no session is stored.
const ReasonNotPaired = "not paired: run make pair"

// Status is the connection state, using the states of the connection.state contract.
type Status struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type sessionLoader interface {
	Load(ctx context.Context) (*store.Device, error)
}

type client interface {
	AddEventHandler(handler whatsmeow.EventHandler) uint32
	ConnectContext(ctx context.Context) error
	Disconnect()
}

// Conn keeps Kaya connected to WhatsApp with the stored session.
type Conn struct {
	sessions  sessionLoader
	logger    *slog.Logger
	newClient func(*store.Device) client

	minBackoff time.Duration
	maxBackoff time.Duration

	mu     sync.RWMutex
	status Status
}

// NewConn returns a connection that uses the session in sessions.
func NewConn(sessions *Sessions, logger *slog.Logger) *Conn {
	waLogger := NewLogger(logger)
	return &Conn{
		sessions: sessions,
		logger:   logger,
		newClient: func(device *store.Device) client {
			return whatsmeow.NewClient(device, waLogger)
		},
		minBackoff: time.Second,
		maxBackoff: time.Minute,
		status:     Status{State: contracts.StateConnecting},
	}
}

// Status reports the current connection state.
func (c *Conn) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

func (c *Conn) set(state, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = Status{State: state, Reason: reason}
}

// Run connects with the stored session and stays connected until ctx is done. Without a session it
// waits, since pairing happens in a separate `gateway pair` run that restarts the service. Until the
// first connection succeeds it retries with backoff; after that whatsmeow reconnects by itself.
func (c *Conn) Run(ctx context.Context) error {
	backoff := c.minBackoff
	for {
		cli, err := c.connect(ctx)
		if err == nil {
			<-ctx.Done()
			if cli != nil {
				cli.Disconnect()
				c.set(contracts.StateDisconnected, "shutting down")
			}
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		c.set(contracts.StateDisconnected, err.Error())
		c.logger.Warn("whatsapp connection failed, retrying", "retry_in", backoff.String(), logging.Err(err))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, c.maxBackoff)
	}
}

func (c *Conn) connect(ctx context.Context) (client, error) {
	device, err := c.sessions.Load(ctx)
	if err != nil {
		return nil, err
	}
	if device == nil {
		c.set(contracts.StateLoggedOut, ReasonNotPaired)
		c.logger.Warn("whatsapp is not paired; run make pair")
		return nil, nil
	}
	cli := c.newClient(device)
	cli.AddEventHandler(c.handle)
	c.set(contracts.StateConnecting, "")
	if err := cli.ConnectContext(ctx); err != nil {
		return nil, fmt.Errorf("connect to whatsapp: %w", err)
	}
	return cli, nil
}

func (c *Conn) handle(evt any) {
	switch e := evt.(type) {
	case *events.Connected:
		c.set(contracts.StateConnected, "")
		c.logger.Info("whatsapp connected")
	case *events.Disconnected:
		c.set(contracts.StateDisconnected, "connection lost, reconnecting")
		c.logger.Warn("whatsapp disconnected, reconnecting")
	case *events.LoggedOut:
		reason := "logged out: " + e.Reason.String()
		c.set(contracts.StateLoggedOut, reason)
		c.logger.Error("whatsapp logged out; pair again with make pair", "reason", e.Reason.String())
	case *events.StreamReplaced:
		c.set(contracts.StateDisconnected, "replaced by another connection using this session")
		c.logger.Error("whatsapp session opened elsewhere; this connection was closed")
	case *events.TemporaryBan:
		c.set(contracts.StateDisconnected, "temporarily banned: "+e.String())
		c.logger.Error("whatsapp temporarily banned this device", "ban", e.String())
	case *events.ClientOutdated:
		c.set(contracts.StateDisconnected, "whatsmeow is outdated")
		c.logger.Error("whatsapp rejected this client version; update whatsmeow")
	case *events.ConnectFailure:
		c.logger.Warn("whatsapp connect failure", "reason", e.Reason.String())
	}
}
