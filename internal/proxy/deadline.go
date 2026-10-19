package proxy

import (
	"context"
	"sync"
	"time"
)

// routingDeadline enforces the Max Routing Time: how long one request may
// spend finding a Provider, across failovers and Recovery Waits. It only
// bounds routing: once a response is committed to the client (headers
// received and accepted), the deadline is disarmed so a long stream is never
// cut. A nil *routingDeadline means "no limit" and all methods are no-ops.
type routingDeadline struct {
	limit time.Duration
	timer *time.Timer
	done  chan struct{}

	mu        sync.Mutex
	expired   bool
	committed bool
	cancel    context.CancelFunc
}

func newRoutingDeadline(limit time.Duration) *routingDeadline {
	if limit <= 0 {
		return nil
	}
	d := &routingDeadline{limit: limit, done: make(chan struct{})}
	d.timer = time.AfterFunc(limit, d.fire)
	return d
}

func (d *routingDeadline) fire() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.committed || d.expired {
		return
	}
	d.expired = true
	close(d.done)
	if d.cancel != nil {
		d.cancel()
	}
}

// arm registers the cancel function of the current upstream attempt so an
// expiring deadline aborts it.
func (d *routingDeadline) arm(cancel context.CancelFunc) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cancel = cancel
	if d.expired {
		cancel()
	}
}

// commit stops the deadline because a response is about to be written. It
// returns false when the deadline already expired; the caller must then
// answer with no_provider_available instead.
func (d *routingDeadline) commit() bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.expired {
		return false
	}
	d.committed = true
	d.cancel = nil
	d.timer.Stop()
	return true
}

// expiredC is closed when the deadline expires; nil (blocks forever) without a limit.
func (d *routingDeadline) expiredC() <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.done
}

func (d *routingDeadline) isExpired() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.expired
}

// stop releases the timer when the request ends.
func (d *routingDeadline) stop() {
	if d == nil {
		return
	}
	d.timer.Stop()
}
