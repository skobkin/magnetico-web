package trackers

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// refreshTimeout bounds one reload.
const refreshTimeout = 10 * time.Second

// Snapshot is an immutable view of the ordered tracker list.
type Snapshot struct {
	URLs     []string
	Version  int64
	LoadedAt time.Time
}

// Cache keeps the last good snapshot in memory (§8.3.3). It is loaded at
// startup (Load, which fails when the list cannot be read), refreshed lazily
// when older than the TTL by a single background reload at a time, and never
// replaced by an empty list on error: a failed reload keeps serving the last
// good snapshot, logs the failure and counts it.
//
// Snapshot never blocks and never queries the database in the caller's
// goroutine, so the public /magnet route does no database work at all.
type Cache struct {
	src Source
	ttl time.Duration
	log *slog.Logger
	now func() time.Time

	snap atomic.Pointer[Snapshot]
	kick chan struct{} // capacity 1: at most one pending reload request

	mu          sync.Mutex
	nextRefresh time.Time
	version     int64

	Refreshes atomic.Int64
	Failures  atomic.Int64
}

// New returns a Cache. Call Load before serving and run Run as a background
// task.
func New(src Source, ttl time.Duration, log *slog.Logger) *Cache {
	return &Cache{src: src, ttl: ttl, log: log, now: time.Now, kick: make(chan struct{}, 1)}
}

// ErrNotLoaded is returned by Load when the first read fails.
var ErrNotLoaded = errors.New("trackers: the tracker list could not be loaded")

// Load reads the list once. It is a startup hook: an error fails startup.
func (c *Cache) Load(ctx context.Context) error {
	urls, err := c.src.URLs(ctx)
	if err != nil {
		return ErrNotLoaded
	}
	c.store(urls)
	return nil
}

func (c *Cache) store(urls []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.version++
	c.snap.Store(&Snapshot{URLs: append([]string(nil), urls...), Version: c.version, LoadedAt: c.now()})
	c.nextRefresh = c.now().Add(c.ttl)
}

// Snapshot returns the current list. When it is stale it asks the background
// task to reload (non-blocking, coalesced) and returns the current one.
func (c *Cache) Snapshot() *Snapshot {
	s := c.snap.Load()
	if s == nil {
		return &Snapshot{}
	}
	c.mu.Lock()
	stale := !c.now().Before(c.nextRefresh)
	c.mu.Unlock()
	if stale {
		c.requestRefresh()
	}
	return s
}

// URLs is Snapshot().URLs; the slice must not be modified.
func (c *Cache) URLs() []string { return c.Snapshot().URLs }

// Invalidate makes the next reload happen now (used after a change made in this
// process).
func (c *Cache) Invalidate() {
	c.mu.Lock()
	c.nextRefresh = time.Time{}
	c.mu.Unlock()
	c.requestRefresh()
}

func (c *Cache) requestRefresh() {
	select {
	case c.kick <- struct{}{}:
	default: // one is already pending: single flight
	}
}

// Run is the background task that performs the reloads, one at a time, until
// ctx is cancelled.
func (c *Cache) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
			c.refresh(ctx)
		}
	}
}

func (c *Cache) refresh(ctx context.Context) {
	c.mu.Lock()
	if c.now().Before(c.nextRefresh) { // refreshed meanwhile
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	urls, err := c.src.URLs(rctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		c.Failures.Add(1)
		c.log.Warn("tracker list refresh failed; keeping the last good snapshot", "error_type", errKind(err))
		c.mu.Lock()
		c.nextRefresh = c.now().Add(c.ttl) // retry at TTL cadence, not on every request
		c.mu.Unlock()
		return
	}
	c.Refreshes.Add(1)
	c.store(urls)
}

func errKind(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	return "error"
}
