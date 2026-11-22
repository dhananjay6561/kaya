package whatsapp

import (
	"context"
	"errors"
	"time"

	"github.com/dhananjay6561/kaya/gateway/internal/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/ingest"
	"github.com/dhananjay6561/kaya/gateway/internal/logging"
)

// maxUnpublished is how many state changes are kept while the database is away. Past that the
// oldest are dropped: core needs where the connection ended up, not every step on the way.
const maxUnpublished = 64

// Publisher stores connection.state events for core. Publishing the same event twice must be safe.
type Publisher interface {
	Publish(ctx context.Context, e contracts.Event) error
}

// SetPublisher gives the connection somewhere to report its state changes. Call it before Run.
func (c *Conn) SetPublisher(p Publisher) { c.publisher = p }

// set records a new state. A change is queued for core with the time it happened, so an event
// written late, after the database was away, still says when the connection really changed.
func (c *Conn) set(state, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.announced && c.status.State == state && c.status.Reason == reason {
		return
	}
	now := time.Now().UTC()
	c.status = Status{State: state, Reason: reason, Since: now}
	c.announced = true
	if c.publisher == nil {
		return
	}
	data := contracts.ConnectionState{State: state, Reason: reason}
	if c.device != nil {
		data.OwnJID = ingest.AddressOf(c.device.GetJID(), c.device.GetLID()).PN
	}
	event, err := contracts.NewEvent(contracts.EventConnectionState, data)
	if err != nil {
		c.logger.Error("could not build a connection state event", logging.Err(err))
		return
	}
	event.OccurredAt = contracts.At(now)
	if len(c.unpublished) == maxUnpublished {
		c.unpublished = c.unpublished[1:]
	}
	c.unpublished = append(c.unpublished, event)
	select {
	case c.publishAsk <- struct{}{}:
	default:
	}
}

// publishLoop writes queued state changes in order. While the database is away it keeps trying,
// and when stop closes it makes one last attempt, so the state the gateway shut down in is stored.
func (c *Conn) publishLoop(ctx context.Context, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	retry := c.publishRetry
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-stop:
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.publishOnStop)
			c.publishQueued(last)
			cancel()
			return
		case <-c.publishAsk:
		case <-timer.C:
		}
		if ctx.Err() != nil {
			continue
		}
		if c.publishQueued(ctx) {
			retry = c.publishRetry
			continue
		}
		timer.Reset(retry)
		retry = min(retry*2, c.publishRetryMax)
	}
}

// publishQueued reports whether the queue is empty afterwards.
func (c *Conn) publishQueued(ctx context.Context) bool {
	for {
		c.mu.RLock()
		if len(c.unpublished) == 0 {
			c.mu.RUnlock()
			return true
		}
		next := c.unpublished[0]
		c.mu.RUnlock()
		attempt, cancel := context.WithTimeout(ctx, c.publishTimeout)
		err := c.publisher.Publish(attempt, next)
		cancel()
		if errors.Is(err, contracts.ErrInvalid) {
			c.logger.Error("a connection state change does not fit the contract and was dropped", logging.Err(err))
			err = nil
		}
		if err != nil {
			if !c.publishFailing.Swap(true) && ctx.Err() == nil {
				c.logger.Warn("could not store a connection state change, trying again", logging.Err(err))
			}
			return false
		}
		if c.publishFailing.Swap(false) {
			c.logger.Info("connection state changes are being stored again")
		}
		c.mu.Lock()
		if len(c.unpublished) > 0 && c.unpublished[0].ID == next.ID {
			c.unpublished = c.unpublished[1:]
		}
		c.mu.Unlock()
	}
}
