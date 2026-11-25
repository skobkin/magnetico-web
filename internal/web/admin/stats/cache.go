package stats

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// cacheEntry is immutable from the moment it is published: a refresh replaces
// the list element's entry with a new one and never writes into the old one, so
// a reader that holds a copy can never observe a value and a fetch time that
// belong to different loads. (The value type must itself not be mutated after
// it has been returned; callers that adjust a cached value copy it first.)
type cacheEntry[K comparable, V any] struct {
	key K
	v   V
	at  time.Time
}

type flight[V any] struct {
	done chan struct{}
	v    V
	at   time.Time
	err  error
}

// Bounds on the work a burst of distinct keys can create. A series cache is
// keyed by attacker-influenced (chart, granularity, period) combinations, so
// coalescing per key is not enough: the loads running at once and the loads
// existing at all are limited too.
const (
	defaultMaxLoads   = 4  // loads running at once across every key
	defaultMaxPending = 32 // loads running or waiting for a slot across every key
	// defaultLoadTimeout bounds one load. A cold dashboard load takes milliseconds (p95 6 ms,
	// docs/wip/09-measurements); 8 s let a request wait that long for a table the statistics
	// outage had locked (Task 09A soak), and a chart needs two loads in sequence.
	defaultLoadTimeout = 3 * time.Second
)

// cache is a bounded, TTL'd, least-recently-used cache with coalesced loads and
// last-known-good on failure. It stores no per-user data: keys are chart
// queries and cursors, values are database aggregates.
type cache[K comparable, V any] struct {
	ttl time.Duration
	max int
	now func() time.Time
	// loadTimeout bounds one load, including its wait for a slot: it runs
	// detached from any single caller.
	loadTimeout time.Duration
	// maxLoads and maxPending are fixed after construction (tests lower them
	// before the first get).
	maxLoads, maxPending int

	mu       sync.Mutex
	items    map[K]*list.Element
	order    *list.List
	inflight map[K]*flight[V]
	slots    chan struct{} // created on first use, capacity maxLoads
}

func newCache[K comparable, V any](ttl time.Duration, max int, now func() time.Time) *cache[K, V] {
	return &cache[K, V]{ttl: ttl, max: max, now: now, loadTimeout: defaultLoadTimeout,
		maxLoads: defaultMaxLoads, maxPending: defaultMaxPending,
		items: map[K]*list.Element{}, order: list.New(), inflight: map[K]*flight[V]{}}
}

// get returns the value for key and the time it was fetched. A fresh entry is
// returned as is; otherwise exactly one load runs for all concurrent callers.
// If the load fails (or the bounds refuse it) and a value exists, the newest
// value is returned with stale=true and no error.
func (c *cache[K, V]) get(ctx context.Context, key K, load func(ctx context.Context) (V, error)) (v V, fetchedAt time.Time, stale bool, err error) {
	c.mu.Lock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*cacheEntry[K, V])
		if c.now().Sub(e.at) < c.ttl {
			c.order.MoveToFront(el)
			c.mu.Unlock()
			return e.v, e.at, false, nil
		}
	}
	f, running := c.inflight[key]
	if !running {
		if len(c.inflight) >= c.maxPending {
			old, at, ok := c.lastKnownGoodLocked(key)
			c.mu.Unlock()
			if ok {
				return old, at, true, nil
			}
			return v, time.Time{}, false, ErrBusy
		}
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
		c.mu.Lock()
		old, at, ok := c.lastKnownGoodLocked(key)
		c.mu.Unlock()
		if ok {
			return old, at, true, nil
		}
		return v, time.Time{}, false, f.err
	}
	return f.v, f.at, false, nil
}

// lastKnownGoodLocked returns the newest published value of key.
func (c *cache[K, V]) lastKnownGoodLocked(key K) (v V, at time.Time, ok bool) {
	el, found := c.items[key]
	if !found {
		return v, at, false
	}
	e := el.Value.(*cacheEntry[K, V])
	return e.v, e.at, true
}

func (c *cache[K, V]) slotChan() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.slots == nil {
		c.slots = make(chan struct{}, c.maxLoads)
	}
	return c.slots
}

func (c *cache[K, V]) run(key K, f *flight[V], load func(ctx context.Context) (V, error)) {
	lctx, cancel := context.WithTimeout(context.Background(), c.loadTimeout)
	defer cancel()
	slots := c.slotChan()
	select {
	case slots <- struct{}{}:
		func() {
			defer func() { <-slots }()
			defer func() {
				if r := recover(); r != nil {
					f.err = errPanic
				}
			}()
			f.v, f.err = load(lctx)
		}()
	case <-lctx.Done(): // never got a slot: the load does not run
		f.err = lctx.Err()
	}
	c.mu.Lock()
	delete(c.inflight, key)
	if f.err == nil {
		f.at = c.now()
		c.storeLocked(key, f.v, f.at)
	}
	c.mu.Unlock()
	close(f.done)
}

// storeLocked publishes a new entry; an existing one is replaced, not modified.
func (c *cache[K, V]) storeLocked(key K, v V, at time.Time) {
	e := &cacheEntry[K, V]{key: key, v: v, at: at}
	if el, ok := c.items[key]; ok {
		el.Value = e
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(e)
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

// pending is the number of loads that exist (running or waiting for a slot).
func (c *cache[K, V]) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inflight)
}
