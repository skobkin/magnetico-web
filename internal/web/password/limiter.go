package password

import (
	"context"
	"runtime"
	"sync/atomic"
	"time"
)

// DefaultWait is how long a caller queues for a hashing slot before ErrBusy.
const DefaultWait = 3 * time.Second

// Limiter bounds concurrent password hashing (spec §4.3.5): each Argon2
// computation holds tens of MiB, so unbounded concurrency would exhaust the
// container. A caller waits a bounded time, then gets ErrBusy and the busy
// counter is incremented (W3 maps it to 503 with Retry-After).
type Limiter struct {
	slots chan struct{}
	wait  time.Duration
	busy  atomic.Uint64
}

// NewLimiter returns a limiter with the given capacity (at least 1).
func NewLimiter(capacity int, wait time.Duration) *Limiter {
	if capacity < 1 {
		capacity = 1
	}
	return &Limiter{slots: make(chan struct{}, capacity), wait: wait}
}

// DefaultCapacity is max(1, min(GOMAXPROCS, 2)).
func DefaultCapacity() int { return max(1, min(runtime.GOMAXPROCS(0), 2)) }

// Capacity returns the number of concurrent slots.
func (l *Limiter) Capacity() int { return cap(l.slots) }

// BusyCount returns how many callers gave up with ErrBusy.
func (l *Limiter) BusyCount() uint64 { return l.busy.Load() }

// Do runs fn while holding a slot. It returns ErrBusy when no slot frees up
// within the wait, or the context error if ctx ends first.
func (l *Limiter) Do(ctx context.Context, fn func()) error {
	select {
	case l.slots <- struct{}{}:
	default:
		timer := time.NewTimer(l.wait)
		defer timer.Stop()
		select {
		case l.slots <- struct{}{}:
		case <-timer.C:
			l.busy.Add(1)
			return ErrBusy
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() { <-l.slots }()
	fn()
	return nil
}
