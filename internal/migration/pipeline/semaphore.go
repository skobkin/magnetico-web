package pipeline

// Byte-weight semaphore bounding the dispatched-but-uncommitted in-flight
// record weight. A record heavier than the whole capacity would deadlock
// waiting for itself, so such records take the dedicated solo slot instead:
// exactly one oversized record is admitted at a time, independent of the
// byte counter (plan directive ④). Total in-flight weight is therefore
// bounded by capacity + one codec-bounded record, structurally.

import (
	"context"
	"sync"
)

type weights struct {
	mu       sync.Mutex
	cond     *sync.Cond
	held     int64
	capacity int64

	// solo admits at most one oversized record at a time.
	solo chan struct{}
}

func newWeights(capacity int64) *weights {
	if capacity < 1 {
		capacity = 1
	}
	w := &weights{capacity: capacity, solo: make(chan struct{}, 1)}
	w.cond = sync.NewCond(&w.mu)
	return w
}

// acquire reserves n bytes of in-flight budget. Oversized requests
// (n >= capacity) bypass the counter and take the solo slot, so they can
// never wait on their own weight. The returned release func must be called
// exactly once, after the record's outcome is settled into a committed
// batch (or dropped at shutdown).
func (w *weights) acquire(ctx context.Context, n int64) (solo bool, release func(), err error) {
	if n < 0 {
		n = 0
	}
	if n >= w.capacity {
		select {
		case w.solo <- struct{}{}:
			return true, func() { <-w.solo }, nil
		case <-ctx.Done():
			return false, nil, ctx.Err()
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}

	// Wake all waiters when the context is cancelled: sync.Cond has no
	// context-aware variant, so a watcher broadcasts on ctx.Done.
	if done := ctx.Done(); done != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-done:
				w.mu.Lock()
				w.cond.Broadcast()
				w.mu.Unlock()
			case <-stop:
			}
		}()
	}

	for w.held+n > w.capacity {
		if err := ctx.Err(); err != nil {
			return false, nil, err
		}
		w.cond.Wait()
	}
	w.held += n
	return false, func() {
		w.mu.Lock()
		w.held -= n
		w.cond.Signal()
		w.mu.Unlock()
	}, nil
}
