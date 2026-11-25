package stats

import (
	"container/list"
	"context"
	"sync"
	"time"
)

type cacheEntry[K comparable, V any] struct {
	key K
	v   V
	at  time.Time
}

type flight[V any] struct {
	done chan struct{}
	v    V
	err  error
}

// cache is a bounded, TTL'd, least-recently-used cache with coalesced loads and
// last-known-good on failure. It stores no per-user data: keys are chart
// queries and cursors, values are database aggregates.
type cache[K comparable, V any] struct {
	ttl time.Duration
	max int
	now func() time.Time
	// loadTimeout bounds one load: it runs detached from any single caller.
	loadTimeout time.Duration

	mu       sync.Mutex
	items    map[K]*list.Element
	order    *list.List
	inflight map[K]*flight[V]
}

func newCache[K comparable, V any](ttl time.Duration, max int, now func() time.Time) *cache[K, V] {
	return &cache[K, V]{ttl: ttl, max: max, now: now, loadTimeout: 8 * time.Second,
		items: map[K]*list.Element{}, order: list.New(), inflight: map[K]*flight[V]{}}
}

// get returns the value for key. A fresh entry is returned as is; otherwise
// exactly one load runs for all concurrent callers. If the load fails and an
// older value exists, that value is returned with stale=true and no error.
func (c *cache[K, V]) get(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (v V, fetchedAt time.Time, stale bool, err error) {
	c.mu.Lock()
	var old *cacheEntry[K, V]
	if el, ok := c.items[key]; ok {
		e := el.Value.(*cacheEntry[K, V])
		if c.now().Sub(e.at) < c.ttl {
			c.order.MoveToFront(el)
			c.mu.Unlock()
			return e.v, e.at, false, nil
		}
		old = e
	}
	f, running := c.inflight[key]
	if !running {
		f = &flight[V]{done: make(chan struct{})}
		c.inflight[key] = f
		go c.run(key, f, load)
	}
	c.mu.Unlock()

	select {
	case <-f.done:
	case <-ctx.Done():
		return v, time.Time{}, false, ctx.Err()
	}
	if f.err != nil {
		if old != nil {
			return old.v, old.at, true, nil
		}
		return v, time.Time{}, false, f.err
	}
	return f.v, c.now(), false, nil
}

func (c *cache[K, V]) run(key K, f *flight[V], load func(ctx context.Context) (V, error)) {
	lctx, cancel := context.WithTimeout(context.Background(), c.loadTimeout)
	defer cancel()
	func() {
		defer func() {
			if r := recover(); r != nil {
				f.err = errPanic
			}
		}()
		f.v, f.err = load(lctx)
	}()
	c.mu.Lock()
	delete(c.inflight, key)
	if f.err == nil {
		c.storeLocked(key, f.v)
	}
	c.mu.Unlock()
	close(f.done)
}

func (c *cache[K, V]) storeLocked(key K, v V) {
	if el, ok := c.items[key]; ok {
		e := el.Value.(*cacheEntry[K, V])
		e.v, e.at = v, c.now()
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&cacheEntry[K, V]{key: key, v: v, at: c.now()})
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*cacheEntry[K, V]).key)
	}
}

func (c *cache[K, V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
