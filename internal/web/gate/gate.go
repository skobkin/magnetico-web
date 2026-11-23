// Package gate is a weighted admission gate with a bounded FIFO queue and a
// bounded wait (docs/wip/07E-web-search-details-plan.md §5.3). It bounds
// concurrent searches (weight 1 each) and concurrent file-list decodes
// (weight in bytes of estimated working set). It has no HTTP or SQL.
//
// A request heavier than the capacity is refused (ErrTooHeavy), never clamped:
// clamping would admit work that the capacity was meant to exclude.
package gate

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

var (
	// ErrBusy: the queue is full or the wait expired.
	ErrBusy = errors.New("gate: busy")
	// ErrTooHeavy: the requested weight exceeds the gate's capacity.
	ErrTooHeavy = errors.New("gate: weight exceeds capacity")
)

// Gate admits work up to a total weight. Waiters are served strictly in
// arrival order, so a heavy request cannot be starved by light ones.
type Gate struct {
	capacity   int64
	maxWaiters int
	maxWait    time.Duration

	mu    sync.Mutex
	used  int64
	queue *list.List // of *waiter
}

type waiter struct {
	weight  int64
	ready   chan struct{}
	granted bool
}

// New returns a Gate. capacity and maxWait must be positive; maxWaiters may
// be zero (never queue: refuse immediately when full).
func New(capacity int64, maxWaiters int, maxWait time.Duration) (*Gate, error) {
	if capacity <= 0 || maxWaiters < 0 || maxWait <= 0 {
		return nil, errors.New("gate: capacity and max wait must be positive, max waiters non-negative")
	}
	return &Gate{capacity: capacity, maxWaiters: maxWaiters, maxWait: maxWait, queue: list.New()}, nil
}

// Capacity returns the total weight.
func (g *Gate) Capacity() int64 { return g.capacity }

// InUse returns the currently admitted weight.
func (g *Gate) InUse() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used
}

// Waiting returns the number of queued requests.
func (g *Gate) Waiting() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.queue.Len()
}

// Acquire admits weight (≥ 1). It returns a release function (idempotent) on
// success; ErrTooHeavy if weight > capacity; ErrBusy if the queue is full or
// maxWait elapsed; or the context's error. A permit granted at the same
// instant the context ends is released before returning.
func (g *Gate) Acquire(ctx context.Context, weight int64) (release func(), err error) {
	if weight < 1 {
		weight = 1
	}
	if weight > g.capacity {
		return nil, ErrTooHeavy
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	g.mu.Lock()
	if g.queue.Len() == 0 && g.used+weight <= g.capacity {
		g.used += weight
		g.mu.Unlock()
		return g.releaser(weight), nil
	}
	if g.queue.Len() >= g.maxWaiters {
		g.mu.Unlock()
		return nil, ErrBusy
	}
	w := &waiter{weight: weight, ready: make(chan struct{})}
	el := g.queue.PushBack(w)
	g.mu.Unlock()

	timer := time.NewTimer(g.maxWait)
	defer timer.Stop()
	var waitErr error
	select {
	case <-w.ready:
		if err := ctx.Err(); err != nil {
			g.releaser(weight)()
			return nil, err
		}
		return g.releaser(weight), nil
	case <-ctx.Done():
		waitErr = ctx.Err()
	case <-timer.C:
		waitErr = ErrBusy
	}

	g.mu.Lock()
	if w.granted { // lost the race: the grant already happened
		g.mu.Unlock()
		g.releaser(weight)()
		return nil, waitErr
	}
	g.queue.Remove(el)
	g.grantLocked() // the removed waiter may have been blocking the head
	g.mu.Unlock()
	return nil, waitErr
}

func (g *Gate) releaser(weight int64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.used -= weight
			g.grantLocked()
			g.mu.Unlock()
		})
	}
}

// grantLocked admits queued waiters in order while the head fits.
func (g *Gate) grantLocked() {
	for el := g.queue.Front(); el != nil; el = g.queue.Front() {
		w := el.Value.(*waiter)
		if g.used+w.weight > g.capacity {
			return
		}
		g.used += w.weight
		w.granted = true
		g.queue.Remove(el)
		close(w.ready)
	}
}
