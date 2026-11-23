package feed

import (
	"context"
	"sync"
	"time"
)

// pageCache is a small TTL cache of rendered pages with single-flight builds:
// concurrent requests for the same page share one build. The leader builds in
// its own goroutine with its own context (nothing outlives a request); when it
// fails, nothing is cached and each waiter retries as the new leader, a
// bounded number of times. It is consulted only AFTER the credential check.
type pageCache struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[cacheKey]*cacheEntry
}

type cacheKey struct {
	page    int
	version int64 // tracker snapshot version
	feed    string
}

type cacheEntry struct {
	done chan struct{}
	body []byte
	at   time.Time
	err  error
}

const (
	maxCacheEntries = 32
	waiterRetries   = 2
)

func newPageCache(ttl time.Duration, now func() time.Time) *pageCache {
	return &pageCache{ttl: ttl, now: now, m: map[cacheKey]*cacheEntry{}}
}

// get returns the page, building it at most once per TTL. hit reports a served
// cached or shared value.
func (c *pageCache) get(ctx context.Context, k cacheKey, build func(context.Context) ([]byte, error)) (body []byte, hit bool, err error) {
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		e := c.m[k]
		if e != nil {
			select {
			case <-e.done:
				if e.err == nil && c.now().Sub(e.at) < c.ttl {
					c.mu.Unlock()
					return e.body, true, nil
				}
				delete(c.m, k) // expired or failed
				e = nil
			default: // a build is running
			}
		}
		if e == nil {
			e = &cacheEntry{done: make(chan struct{})}
			c.evictLocked()
			c.m[k] = e
			c.mu.Unlock()
			b, berr := build(ctx)
			c.mu.Lock()
			e.body, e.err, e.at = b, berr, c.now()
			if berr != nil && c.m[k] == e {
				delete(c.m, k)
			}
			close(e.done)
			c.mu.Unlock()
			return b, false, berr
		}
		c.mu.Unlock()
		select {
		case <-e.done:
			if e.err == nil {
				return e.body, true, nil
			}
			if attempt >= waiterRetries {
				return nil, false, e.err
			}
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
}

// evictLocked drops expired entries and, if still full, the oldest finished one.
func (c *pageCache) evictLocked() {
	if len(c.m) < maxCacheEntries {
		return
	}
	now := c.now()
	var oldest cacheKey
	var oldestAt time.Time
	have := false
	for k, e := range c.m {
		select {
		case <-e.done:
			if now.Sub(e.at) >= c.ttl {
				delete(c.m, k)
				continue
			}
			if !have || e.at.Before(oldestAt) {
				oldest, oldestAt, have = k, e.at, true
			}
		default:
		}
	}
	if len(c.m) >= maxCacheEntries && have {
		delete(c.m, oldest)
	}
}
