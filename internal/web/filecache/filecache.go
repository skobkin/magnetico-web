// Package filecache is the production file-index cache of the web service
// (docs/wip/07E-web-search-details-plan.md §6, C1 decisions in
// docs/wip/07E-c1-report.md §14): a byte-bounded LRU of filetree.Arena
// indexes with single-flight cold builds behind a weighted decode gate.
//
// The cold path is
//
//	FileIdentity → cache lookup → single-flight → FileHeader → PeekDecodedSize
//	→ gate → FileBlob → DecodeFileList → BuildArena → insert
//
// Rules the code enforces:
//   - an entry is served only if its identity (torrent id, file count, total
//     size, blob xmin) equals the freshly read identity and it is younger than
//     MaxAge; a request never receives an index of another blob version;
//   - no database connection is held while waiting on the gate or decoding
//     (every Source call is its own short statement), and admission precedes
//     the whole-blob read;
//   - neither the compressed blob nor the decoded []File is retained;
//   - errors and cancelled builds are never cached, with one exception: the
//     deterministic filetree.ErrIndexComplexity of an immutable blob version is
//     remembered briefly (bounded count, finite lifetime, keyed by the same
//     identity), so repeated requests do not repeat an expensive decode and
//     scan. Transient, cancelled, storage and corruption errors are not;
//   - a build's database phase is cancelled when every waiter has left before
//     decoding starts; once decoding starts it finishes (the codec is not
//     interruptible) and its result is cached for the next request;
//   - Close cancels database phases, waits for the build goroutines (bounded
//     by its context) and guarantees none touches the database afterwards.
package filecache

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/filetree"
	"github.com/skobkin/magnetico-web/internal/web/gate"
	"github.com/skobkin/magnetico-web/persistence"
)

// Decisions fixed at C1 (docs/wip/07E-c1-report.md §14).
const (
	// DecodeCapacity is the weighted decode gate capacity.
	DecodeCapacity int64 = 192 << 20
	// CacheBudget is the LRU accounting budget in bytes of Index.Bytes().
	CacheBudget int64 = 128 << 20
	// MaxAge bounds staleness from anything the identity cannot see.
	MaxAge = 10 * time.Minute
	// QueueWait and MaxWaiters bound the decode queue (plan §5.3).
	QueueWait  = 5 * time.Second
	MaxWaiters = 32
	// BuildTimeout bounds the database and queue phases of one build.
	BuildTimeout = 30 * time.Second
	// NegativeTTL and MaxNegative bound the remembered index-complexity
	// refusals (finite lifetime, small fixed count).
	NegativeTTL = 5 * time.Minute
	MaxNegative = 1024

	maxAttempts   = 3 // identity retries when the blob changes under a request
	entryOverhead = 256
)

// Errors in addition to persistence's and gate's.
var (
	// ErrClosed: the cache was closed (shutdown).
	ErrClosed = errors.New("filecache: closed")
	// ErrTooLargeToIndex: the admission weight exceeds the decode capacity.
	// With the default codec limits this cannot happen (MaxWeight is checked at
	// construction); it signals raised limits or a smaller gate.
	ErrTooLargeToIndex = errors.New("filecache: file list too large to index within the decode capacity")
	// ErrBuildPanic: a build goroutine panicked and was recovered.
	ErrBuildPanic = errors.New("filecache: build panicked")
)

// Source is the persistence surface of a cold build.
type Source interface {
	FileIdentity(ctx context.Context, id int32) (persistence.FileIdentity, error)
	FileHeader(ctx context.Context, id int32) (header []byte, storedLen int, err error)
	FileBlob(ctx context.Context, id int32, ver string) ([]byte, error)
	Limits() codec.Limits
}

var _ Source = (*persistence.Reader)(nil)

// Weight is the conservative admission estimate of one cold build (C1 W0 plus
// the index-overhead allowance). It is an admission estimate, not a proven
// bound on physical memory (docs/wip/07E-c1-report.md §5):
//
//	stored + declared + 24·files + P + 36·files
//
// P = min(files·MaxPathBytes, MaxAggregatePathBytes) is the assumed retained
// path volume; 36·files covers directory records, name references and the
// block table in the worst case of one directory per file.
func Weight(l codec.Limits, storedLen int, declared uint64, files int64) int64 {
	p := min(files*int64(l.MaxPathBytes), int64(l.MaxAggregatePathBytes))
	return int64(storedLen) + int64(declared) + (24+36)*files + p
}

// MaxWeight is the largest Weight any blob valid under l can have.
func MaxWeight(l codec.Limits) int64 {
	return Weight(l, l.MaxStoredBytes, uint64(l.MaxUncompressedBytes), int64(l.MaxFiles))
}

// Config configures a Cache. Zero values take the C1 defaults.
type Config struct {
	DecodeCapacity int64
	Budget         int64
	MaxAge         time.Duration
	QueueWait      time.Duration
	MaxWaiters     int
	BuildTimeout   time.Duration
	NegativeTTL    time.Duration
	MaxNegative    int
	Logger         *slog.Logger
	Now            func() time.Time // tests
}

// Result is what Get returns.
type Result struct {
	Torrent persistence.Torrent
	Index   filetree.Index
	// Hit: served from the cache without a decode. Single: the intentional
	// no-blob single-file torrent (not cached).
	Hit, Single bool
}

// Stats are cumulative counters plus the current occupancy.
type Stats struct {
	Hits, Misses, Coalesced int64 // Coalesced: misses that joined a running build
	Builds, BuildErrors     int64 // Builds counts started decodes
	Evictions, Expired      int64
	Replaced                int64 // entry replaced because its identity changed
	Oversize                int64 // built but too large to retain
	Retries                 int64 // identity retries after ErrBlobChanged
	Abandoned               int64 // builds cancelled because every waiter left (not errors)
	NegativeHits            int64 // requests answered from a remembered index-complexity refusal
	Negative                int   // remembered refusals currently held
	Entries                 int
	Bytes                   int64
	Flights                 int
}

type key struct {
	id    int32
	count int32
	size  int64
	ver   string
}

func keyOf(fi persistence.FileIdentity) key {
	return key{fi.ID, fi.FileCount, fi.TotalSize, fi.Version}
}

// negEntry remembers a deterministic complexity refusal of one blob version.
type negEntry struct {
	err     error
	expires time.Time
}

type entry struct {
	k       key
	idx     filetree.Index
	t       persistence.Torrent
	cost    int64
	created time.Time
	el      *list.Element
}

// Cache is safe for concurrent use.
type Cache struct {
	src  Source
	gate *gate.Gate
	cfg  Config
	log  *slog.Logger
	now  func() time.Time

	rootCtx    context.Context
	rootCancel context.CancelFunc
	wg         sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	entries map[int32]*entry
	lru     *list.List // front = most recently used; of *entry
	bytes   int64
	flights map[key]*flight
	neg     map[key]negEntry

	onDecode func() // test hook: called when a decode is committed

	hits, misses, coalesced, builds, buildErrs, evictions, expired,
	replaced, oversize, retries, abandoned, negHits atomic.Int64
}

// New returns a Cache. It fails if a blob valid under the source's limits
// could never be admitted (MaxWeight > DecodeCapacity): legal file lists must
// stay browsable, so this is a startup invariant, not a silent clamp.
func New(src Source, cfg Config) (*Cache, error) {
	if src == nil {
		return nil, errors.New("filecache: a source is required")
	}
	if cfg.DecodeCapacity <= 0 {
		cfg.DecodeCapacity = DecodeCapacity
	}
	if cfg.Budget <= 0 {
		cfg.Budget = CacheBudget
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = MaxAge
	}
	if cfg.QueueWait <= 0 {
		cfg.QueueWait = QueueWait
	}
	if cfg.MaxWaiters <= 0 {
		cfg.MaxWaiters = MaxWaiters
	}
	if cfg.BuildTimeout <= 0 {
		cfg.BuildTimeout = BuildTimeout
	}
	if cfg.NegativeTTL <= 0 {
		cfg.NegativeTTL = NegativeTTL
	}
	if cfg.MaxNegative <= 0 {
		cfg.MaxNegative = MaxNegative
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if w := MaxWeight(src.Limits()); w > cfg.DecodeCapacity {
		return nil, fmt.Errorf("filecache: the largest admissible weight (%d bytes) exceeds the decode capacity (%d bytes)",
			w, cfg.DecodeCapacity)
	}
	g, err := gate.New(cfg.DecodeCapacity, cfg.MaxWaiters, cfg.QueueWait)
	if err != nil {
		return nil, err
	}
	c := &Cache{src: src, gate: g, cfg: cfg, log: cfg.Logger, now: cfg.Now,
		entries: map[int32]*entry{}, lru: list.New(), flights: map[key]*flight{}, neg: map[key]negEntry{}}
	c.rootCtx, c.rootCancel = context.WithCancel(context.Background())
	return c, nil
}

// Gate exposes the decode gate (occupancy for tests and measurements).
func (c *Cache) Gate() *gate.Gate { return c.gate }

// Stats returns a snapshot.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Hits: c.hits.Load(), Misses: c.misses.Load(), Coalesced: c.coalesced.Load(),
		Builds: c.builds.Load(), BuildErrors: c.buildErrs.Load(),
		Evictions: c.evictions.Load(), Expired: c.expired.Load(), Replaced: c.replaced.Load(),
		Oversize: c.oversize.Load(), Retries: c.retries.Load(), Abandoned: c.abandoned.Load(), NegativeHits: c.negHits.Load(), Negative: len(c.neg),
		Entries: len(c.entries), Bytes: c.bytes, Flights: len(c.flights),
	}
}

// Close stops accepting requests, cancels the database phases of running
// builds and waits for the build goroutines until ctx ends. After it returns
// nil no build goroutine exists. On a timeout the remaining goroutines are
// CPU-only (their database phase is over or cancelled) and finish by
// themselves.
func (c *Cache) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	clear(c.neg)
	c.mu.Unlock()
	c.rootCancel()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("filecache: %w with builds still running", ctx.Err())
	}
}

// Get returns the file index of torrent id for the current stored version of
// its blob. Identity is revalidated on every call. A changed blob is retried a
// bounded number of times from the identity lookup; limit and corruption
// errors are never retried.
func (c *Cache) Get(ctx context.Context, id int32) (Result, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			c.retries.Add(1)
		}
		res, err := c.get(ctx, id)
		if err == nil {
			return res, nil
		}
		if !errors.Is(err, persistence.ErrBlobChanged) {
			return Result{}, err
		}
		lastErr = err
	}
	return Result{}, lastErr
}

func (c *Cache) get(ctx context.Context, id int32) (Result, error) {
	fi, err := c.src.FileIdentity(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if !fi.HasBlob {
		files, err := persistence.SingleFileList(fi.Torrent)
		if err != nil {
			return Result{}, err
		}
		idx, err := filetree.BuildArena(ctx, files)
		if err != nil {
			return Result{}, err
		}
		return Result{Torrent: fi.Torrent, Index: idx, Single: true}, nil
	}
	k := keyOf(fi)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Result{}, ErrClosed
	}
	if e := c.entries[id]; e != nil {
		switch {
		case e.k != k:
			c.replaced.Add(1)
			c.removeLocked(e) // stale: a new build will replace it
		case c.now().Sub(e.created) > c.cfg.MaxAge:
			c.expired.Add(1)
			c.removeLocked(e)
		default:
			c.lru.MoveToFront(e.el)
			c.hits.Add(1)
			res := Result{Torrent: fi.Torrent, Index: e.idx, Hit: true}
			c.mu.Unlock()
			return res, nil
		}
	}
	if n, ok := c.neg[k]; ok {
		if c.now().Before(n.expires) {
			c.negHits.Add(1)
			c.mu.Unlock()
			return Result{}, n.err
		}
		delete(c.neg, k)
	}
	c.misses.Add(1)
	f := c.flights[k]
	joined := f != nil
	if joined {
		c.coalesced.Add(1)
	} else {
		// The creating request is the first waiter from the start: the build
		// goroutine may reach beginDecode before this function returns.
		f = &flight{c: c, k: k, t: fi.Torrent, done: make(chan struct{}), waiters: 1}
		f.ctx, f.cancel = context.WithTimeout(c.rootCtx, c.cfg.BuildTimeout)
		c.flights[k] = f
		c.wg.Add(1)
		go f.run()
	}
	if joined {
		f.mu.Lock()
		f.waiters++
		f.mu.Unlock()
	}
	c.mu.Unlock()

	select {
	case <-f.done:
		f.leave(false)
		if f.err != nil {
			return Result{}, f.err
		}
		return Result{Torrent: fi.Torrent, Index: f.idx}, nil
	case <-ctx.Done():
		f.leave(true)
		return Result{}, ctx.Err()
	}
}

func (c *Cache) removeLocked(e *entry) {
	delete(c.entries, e.k.id)
	c.lru.Remove(e.el)
	c.bytes -= e.cost
	e.idx = nil // release the index even if the entry is referenced elsewhere
}

// insert retains idx unless it alone exceeds the budget. It evicts from the
// least recently used end until the budget holds.
func (c *Cache) insert(k key, t persistence.Torrent, idx filetree.Index) {
	cost := int64(idx.Bytes()) + entryOverhead
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if cost > c.cfg.Budget {
		c.oversize.Add(1)
		return
	}
	if old := c.entries[k.id]; old != nil {
		c.removeLocked(old)
	}
	e := &entry{k: k, idx: idx, t: t, cost: cost, created: c.now()}
	e.el = c.lru.PushFront(e)
	c.entries[k.id] = e
	c.bytes += cost
	for c.bytes > c.cfg.Budget {
		last := c.lru.Back()
		if last == nil || last.Value.(*entry) == e {
			break
		}
		c.removeLocked(last.Value.(*entry))
		c.evictions.Add(1)
	}
}

// rememberComplexity stores a refusal for this exact blob version. Entries of
// the same torrent with another identity are dropped; when the table is full
// expired entries go first, then the one closest to expiry.
func (c *Cache) rememberComplexity(k key, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	now := c.now()
	for ok := range c.neg {
		if ok.id == k.id && ok != k {
			delete(c.neg, ok)
		}
	}
	if _, exists := c.neg[k]; !exists && len(c.neg) >= c.cfg.MaxNegative {
		var oldest key
		var oldestAt time.Time
		first := true
		for ok, n := range c.neg {
			if !now.Before(n.expires) {
				delete(c.neg, ok)
				continue
			}
			if first || n.expires.Before(oldestAt) {
				oldest, oldestAt, first = ok, n.expires, false
			}
		}
		if len(c.neg) >= c.cfg.MaxNegative && !first {
			delete(c.neg, oldest)
		}
	}
	c.neg[k] = negEntry{err: err, expires: now.Add(c.cfg.NegativeTTL)}
}

// ---- single-flight -----------------------------------------------------------

type flight struct {
	c *Cache
	k key
	t persistence.Torrent

	ctx    context.Context // database and queue phases
	cancel context.CancelFunc

	mu      sync.Mutex
	waiters int
	started bool // the decode began: no longer cancellable by waiters leaving

	done chan struct{}
	idx  filetree.Index
	err  error
}

// leave removes a waiter. When the last waiter leaves before the decode
// started, the database and queue phases are cancelled.
//
// The flight is unregistered under the cache lock in the same step, so a new
// request can never join a flight that is already doomed.
func (f *flight) leave(abandoned bool) {
	c := f.c
	c.mu.Lock()
	f.mu.Lock()
	f.waiters--
	stop := abandoned && f.waiters == 0 && !f.started
	f.mu.Unlock()
	if stop && c.flights[f.k] == f {
		delete(c.flights, f.k)
	}
	c.mu.Unlock()
	if stop {
		f.cancel()
	}
}

// beginDecode atomically commits to decoding; it fails when every waiter has
// already left (then the work is unnecessary and is cancelled).
func (f *flight) beginDecode() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.waiters == 0 {
		return false
	}
	f.started = true
	return true
}

func (f *flight) run() {
	c := f.c
	defer c.wg.Done()
	defer func() {
		// Remove the flight before waking waiters so a new request after a
		// failure starts a fresh build.
		c.mu.Lock()
		if c.flights[f.k] == f {
			delete(c.flights, f.k)
		}
		c.mu.Unlock()
		f.cancel()
		close(f.done)
	}()
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("file index build panicked", "torrent_id", f.k.id, "panic_type", fmt.Sprintf("%T", r),
				"stack", string(debug.Stack()))
			f.idx, f.err = nil, ErrBuildPanic
			c.buildErrs.Add(1)
		}
	}()
	f.idx, f.err = f.build()
	switch {
	case errors.Is(f.err, context.Canceled):
		c.abandoned.Add(1) // every waiter left (or shutdown): not a failure
	case f.err != nil:
		c.buildErrs.Add(1)
		if errors.Is(f.err, filetree.ErrIndexComplexity) {
			c.rememberComplexity(f.k, f.err)
		}
	default:
		c.insert(f.k, f.t, f.idx)
	}
}

func (f *flight) build() (filetree.Index, error) {
	c := f.c
	l := c.src.Limits()
	if int64(f.k.count) > int64(l.MaxFiles) {
		return nil, fmt.Errorf("%w: %w", persistence.ErrCorruptBlob,
			&codec.LimitError{Limit: codec.LimitFiles, Actual: uint64(f.k.count), Allowed: uint64(l.MaxFiles)})
	}
	hdr, storedLen, err := c.src.FileHeader(f.ctx, int32(f.k.id))
	if err != nil {
		return nil, err
	}
	_, declared, err := codec.PeekDecodedSize(hdr, storedLen, l)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", persistence.ErrCorruptBlob, err)
	}
	w := Weight(l, storedLen, declared, int64(f.k.count))
	if w > c.gate.Capacity() {
		c.log.Error("file list too large to index", "torrent_id", f.k.id, "weight", w, "capacity", c.gate.Capacity())
		return nil, ErrTooLargeToIndex
	}
	release, err := c.gate.Acquire(f.ctx, w) // no connection is held here
	if err != nil {
		return nil, err
	}
	defer release() // success, error, cancellation and panic

	blob, err := c.src.FileBlob(f.ctx, f.k.id, f.k.ver)
	if err != nil {
		return nil, err
	}
	if !f.beginDecode() {
		return nil, context.Canceled // all waiters left: nobody wants this
	}
	c.builds.Add(1)
	if c.onDecode != nil {
		c.onDecode()
	}
	files, err := persistence.DecodeFileList(f.t, blob, l)
	blob = nil
	if err != nil {
		return nil, err
	}
	// BuildArena checks its context; use the root context so waiters leaving
	// cannot abort work that is already paid for, while shutdown still can.
	idx, err := filetree.BuildArena(c.rootCtx, files)
	files = nil
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return nil, ErrClosed
		case errors.Is(err, filetree.ErrIndexComplexity):
			// Valid data beyond the web index limits: not corruption.
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", persistence.ErrCorruptBlob, err)
	}
	return idx, nil
}
