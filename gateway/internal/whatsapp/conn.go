package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/logging"
)

// Reasons reported in Status. They are fixed strings, so /health never carries error details.
const (
	ReasonNotPaired        = "not paired: run make pair"
	ReasonStoreUnavailable = "session store unavailable"
	ReasonSeveralSessions  = "more than one session is stored"
	ReasonUnreachable      = "cannot reach WhatsApp"
	ReasonReconnecting     = "connection lost, reconnecting"
	ReasonConnectFailed    = "WhatsApp refused the connection, retrying"
	ReasonTemporaryBan     = "temporarily banned by WhatsApp, retrying when the ban ends"
	ReasonLoggedOut        = "logged out from the phone: run make pair"
	ReasonReplaced         = "the session is in use elsewhere: stop the other gateway, then restart this one"
	ReasonOutdated         = "WhatsApp rejected this client version: update whatsmeow"
	ReasonShuttingDown     = "shutting down"
)

// maxReconnectErrors caps whatsmeow's reconnect delay, which grows by 2 s per failed attempt.
const maxReconnectErrors = 30

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

// ending is what a connection that whatsmeow will not bring back asks Run to do next.
type ending struct {
	generation uint64
	retryAfter time.Duration
	permanent  bool
}

// Conn keeps Kaya connected to WhatsApp with the stored session.
type Conn struct {
	sessions  sessionLoader
	logger    *slog.Logger
	newClient func(*store.Device) client

	minBackoff time.Duration
	maxBackoff time.Duration

	ended      chan ending
	generation atomic.Uint64

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
			cli := whatsmeow.NewClient(device, waLogger)
			cli.AutoReconnectHook = func(error) bool { return capReconnectErrors(cli) }
			return cli
		},
		minBackoff: time.Second,
		maxBackoff: time.Minute,
		ended:      make(chan ending, 1),
		status:     Status{State: contracts.StateConnecting},
	}
}

func capReconnectErrors(cli *whatsmeow.Client) bool {
	cli.AutoReconnectErrors = min(cli.AutoReconnectErrors, maxReconnectErrors)
	return true
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

// Run connects with the stored session and keeps it connected until ctx is done. Without a session
// it waits, since pairing happens in a separate `gateway pair` run that restarts the service.
// whatsmeow reconnects by itself after ordinary drops; Run starts a new connection, with backoff,
// when the first attempt fails or whatsmeow gives up (a refused connection or a temporary ban). It
// stops trying when the phone logged Kaya out, the session is in use elsewhere, or the client is
// too old.
func (c *Conn) Run(ctx context.Context) error {
	backoff := c.minBackoff
	for {
		cli, err := c.connect(ctx)
		if ctx.Err() != nil {
			c.disconnect(cli)
			return nil
		}
		var wait time.Duration
		switch {
		case err != nil:
			c.logger.Warn("whatsapp connection failed, retrying", "retry_in", backoff.String(), logging.Err(err))
			wait = backoff
		case cli == nil:
			<-ctx.Done()
			return nil
		default:
			end, ok := c.waitForEnd(ctx)
			if !ok {
				c.disconnect(cli)
				return nil
			}
			cli.Disconnect()
			if end.permanent {
				<-ctx.Done()
				return nil
			}
			wait = max(end.retryAfter, backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(backoff*2, c.maxBackoff)
	}
}

func (c *Conn) waitForEnd(ctx context.Context) (ending, bool) {
	for {
		select {
		case <-ctx.Done():
			return ending{}, false
		case end := <-c.ended:
			if end.generation == c.generation.Load() {
				return end, true
			}
		}
	}
}

func (c *Conn) disconnect(cli client) {
	if cli != nil {
		cli.Disconnect()
	}
	c.set(contracts.StateDisconnected, ReasonShuttingDown)
}

func (c *Conn) connect(ctx context.Context) (client, error) {
	device, err := c.sessions.Load(ctx)
	if err != nil {
		reason := ReasonStoreUnavailable
		if errors.Is(err, ErrSeveralSessions) {
			reason = ReasonSeveralSessions
		}
		c.set(contracts.StateDisconnected, reason)
		return nil, err
	}
	if device == nil {
		c.set(contracts.StateLoggedOut, ReasonNotPaired)
		c.logger.Warn("whatsapp is not paired; run make pair")
		return nil, nil
	}
	generation := c.generation.Add(1)
	cli := c.newClient(device)
	cli.AddEventHandler(func(evt any) {
		if generation == c.generation.Load() {
			c.handle(generation, evt)
		}
	})
	c.set(contracts.StateConnecting, "")
	if err := cli.ConnectContext(ctx); err != nil {
		c.set(contracts.StateDisconnected, ReasonUnreachable)
		return nil, fmt.Errorf("connect to whatsapp: %w", err)
	}
	return cli, nil
}

func (c *Conn) end(generation uint64, e ending) {
	e.generation = generation
	select {
	case c.ended <- e:
	default:
	}
}

func (c *Conn) handle(generation uint64, evt any) {
	switch e := evt.(type) {
	case *events.Connected:
		c.set(contracts.StateConnected, "")
		c.logger.Info("whatsapp connected")
	case *events.Disconnected:
		c.set(contracts.StateDisconnected, ReasonReconnecting)
		c.logger.Warn("whatsapp disconnected, reconnecting")
	case *events.LoggedOut:
		c.set(contracts.StateLoggedOut, ReasonLoggedOut)
		c.logger.Error("whatsapp logged out; pair again with make pair", "reason", e.Reason.String())
		c.end(generation, ending{permanent: true})
	case *events.StreamReplaced:
		c.set(contracts.StateDisconnected, ReasonReplaced)
		c.logger.Error("whatsapp session opened elsewhere; this connection stopped")
		c.end(generation, ending{permanent: true})
	case *events.ClientOutdated:
		c.set(contracts.StateDisconnected, ReasonOutdated)
		c.logger.Error("whatsapp rejected this client version; update whatsmeow")
		c.end(generation, ending{permanent: true})
	case *events.TemporaryBan:
		c.set(contracts.StateDisconnected, ReasonTemporaryBan)
		c.logger.Error("whatsapp temporarily banned this device", "code", int(e.Code), "expires_in", e.Expire.String())
		c.end(generation, ending{retryAfter: e.Expire})
	case *events.ConnectFailure:
		c.set(contracts.StateDisconnected, ReasonConnectFailed)
		c.logger.Warn("whatsapp refused the connection", "reason", e.Reason.String())
		c.end(generation, ending{})
	case *events.CATRefreshError:
		c.set(contracts.StateDisconnected, ReasonConnectFailed)
		c.logger.Warn("whatsapp token refresh failed", logging.Err(e.Error))
		c.end(generation, ending{})
	}
}
