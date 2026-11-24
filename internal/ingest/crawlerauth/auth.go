// Package crawlerauth authenticates ingest requests against the crawler
// registry (docs/specs/crawler-ingest-delivery-v1.md §5). The presented
// credential is hashed at once and is the only source of a crawler's identity.
//
// A positive cache (fixed 60 s lifetime measured from the start of the lookup
// that produced the entry, never extended) keeps healthy crawlers off the
// database; every uncached credential, valid or not, costs at most one lookup
// through bounded coordination state, so a flood of invalid credentials can
// neither exhaust database connections nor memory.
package crawlerauth

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skobkin/magnetico-web/internal/ingest/crawlers"
)

// The documented bounds (docs/specs/crawler-ingest-delivery-v1.md §5).
const (
	CacheTTL          = 60 * time.Second
	MaxCacheEntries   = 4096
	MaxActiveLookups  = 8
	MaxPendingDigests = 64
	MaxWaiters        = 1024

	// DefaultSlotWait is how long a digest waits for one of the active lookup
	// slots before its waiters get "busy".
	DefaultSlotWait = 250 * time.Millisecond
	// DefaultLookupDeadline bounds every database lookup.
	DefaultLookupDeadline = 2 * time.Second

	minTokenBody = 43 // 32 bytes in unpadded base64url
	maxTokenBody = 128
)

// ErrUnauthorized is returned for a missing, malformed, unknown or disabled
// credential: one error value for all of them, so a response cannot tell them
// apart.
var ErrUnauthorized = errors.New("unauthorized")

// ErrUnavailable means authentication could not be decided: a database error,
// a lookup timeout (Busy=false) or saturated coordination state (Busy=true).
// Callers answer 503 and the crawler retries; it is never a 401.
type ErrUnavailable struct{ Busy bool }

func (e ErrUnavailable) Error() string {
	if e.Busy {
		return "crawler authentication is busy"
	}
	return "crawler authentication is unavailable"
}

// Identity is the authenticated crawler.
type Identity struct {
	CrawlerID string
	Name      string
}

// Lookup resolves a credential digest (*crawlers.Store).
type Lookup interface {
	LookupDigest(ctx context.Context, digest [32]byte) (crawlers.Record, error)
}

// Clock supplies time; production uses time.Now (with its monotonic reading).
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Observer receives the authentication events that statistics count. All
// methods are called without any authenticator lock held.
type Observer interface {
	AuthHit()
	AuthMiss()
	AuthDBLookup()
	AuthStorageFailure()
	AuthBusy()
}

// Options configures New. All fields are optional.
type Options struct {
	Clock    Clock
	Logger   *log.Logger
	Observer Observer
}

// Stats are cumulative authenticator counters.
type Stats struct {
	Hits, Misses, DBLookups, StorageFailures, Busy uint64
}

type entry struct {
	id        Identity
	expiresAt time.Time
}

type call struct {
	done chan struct{}
	id   Identity
	err  error
}

// Authenticator is safe for concurrent use.
type Authenticator struct {
	lookup Lookup
	clock  Clock
	log    *log.Logger
	obs    Observer

	slotWait       time.Duration
	lookupDeadline time.Duration

	slots chan struct{}

	mu      sync.Mutex
	cache   map[[32]byte]entry
	calls   map[[32]byte]*call
	waiters int

	hits, misses, dbLookups, storageFailures, busy atomic.Uint64
}

// New returns an Authenticator over lookup.
func New(lookup Lookup, opt Options) *Authenticator {
	a := &Authenticator{
		lookup: lookup, clock: opt.Clock, log: opt.Logger, obs: opt.Observer,
		slotWait: DefaultSlotWait, lookupDeadline: DefaultLookupDeadline,
		slots: make(chan struct{}, MaxActiveLookups),
		cache: map[[32]byte]entry{}, calls: map[[32]byte]*call{},
	}
	if a.clock == nil {
		a.clock = realClock{}
	}
	if a.log == nil {
		a.log = log.Default()
	}
	return a
}

// Stats returns a snapshot of the counters.
func (a *Authenticator) Stats() Stats {
	return Stats{Hits: a.hits.Load(), Misses: a.misses.Load(), DBLookups: a.dbLookups.Load(),
		StorageFailures: a.storageFailures.Load(), Busy: a.busy.Load()}
}

// wellFormed rejects anything that cannot be a credential minted by
// crawlers.Store without touching the database.
func wellFormed(bearer string) bool {
	body, ok := strings.CutPrefix(bearer, crawlers.TokenPrefix)
	if !ok || len(body) < minTokenBody || len(body) > maxTokenBody {
		return false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		isAlnum := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !isAlnum && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// Authenticate resolves the bearer credential to a crawler. It returns
// ErrUnauthorized (together with the crawler's identity when the credential
// belongs to a disabled crawler; otherwise a zero Identity), ErrUnavailable, or the context's error when the caller's
// context ends while waiting.
func (a *Authenticator) Authenticate(ctx context.Context, bearer string) (Identity, error) {
	if !wellFormed(bearer) {
		return Identity{}, ErrUnauthorized
	}
	digest := crawlers.Digest(bearer)

	a.mu.Lock()
	if e, ok := a.cache[digest]; ok {
		if a.clock.Now().Before(e.expiresAt) {
			a.mu.Unlock()
			a.hits.Add(1)
			if a.obs != nil {
				a.obs.AuthHit()
			}
			return e.id, nil
		}
		delete(a.cache, digest)
	}
	a.misses.Add(1)
	// Reserve a waiter place first: nothing is allocated for a request that
	// cannot be accommodated.
	if a.waiters >= MaxWaiters {
		a.mu.Unlock()
		return Identity{}, a.refuseBusy()
	}
	c, joined := a.calls[digest]
	if !joined {
		if len(a.calls) >= MaxPendingDigests {
			a.mu.Unlock()
			return Identity{}, a.refuseBusy()
		}
		c = &call{done: make(chan struct{})}
		a.calls[digest] = c
	}
	a.waiters++
	a.mu.Unlock()
	if a.obs != nil {
		a.obs.AuthMiss()
	}
	defer func() {
		a.mu.Lock()
		a.waiters--
		a.mu.Unlock()
	}()

	if !joined {
		//nolint:gosec // G118: the lookup deliberately outlives and is independent of any single waiter's request context
		go a.run(digest, c)
	}
	select {
	case <-c.done:
		return c.id, c.err
	case <-ctx.Done():
		return Identity{}, ctx.Err()
	}
}

func (a *Authenticator) refuseBusy() error {
	a.busy.Add(1)
	if a.obs != nil {
		a.obs.AuthBusy()
	}
	return ErrUnavailable{Busy: true}
}

// run performs the lookup for one digest on its own deadline, independent of
// any single waiter, and releases every reservation on every path.
func (a *Authenticator) run(digest [32]byte, c *call) {
	var (
		id  Identity
		err error
	)
	defer func() {
		if r := recover(); r != nil {
			a.log.Printf("ingest: crawler lookup panicked (details withheld)")
			id, err = Identity{}, ErrUnavailable{}
			a.countFailure()
		}
		a.mu.Lock()
		delete(a.calls, digest)
		a.mu.Unlock()
		c.id, c.err = id, err
		close(c.done)
	}()

	timer := time.NewTimer(a.slotWait)
	select {
	case a.slots <- struct{}{}:
		timer.Stop()
	case <-timer.C:
		err = a.refuseBusy()
		return
	}
	defer func() { <-a.slots }()

	// The entry's life starts here, before the query is sent: a delayed
	// response can only shorten it.
	startedAt := a.clock.Now()
	a.dbLookups.Add(1)
	if a.obs != nil {
		a.obs.AuthDBLookup()
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.lookupDeadline)
	rec, lerr := a.lookup.LookupDigest(ctx, digest)
	cancel()

	switch {
	case lerr == nil && rec.Enabled:
		id = Identity{CrawlerID: rec.ID, Name: rec.Name}
		exp := startedAt.Add(CacheTTL)
		if a.clock.Now().Before(exp) {
			a.store(digest, entry{id: id, expiresAt: exp})
		}
	case lerr == nil:
		// A disabled crawler: refused like an unknown credential, but its
		// identity travels with the refusal so statistics can count the request
		// for that crawler. It is never cached and never reaches the response.
		id = Identity{CrawlerID: rec.ID, Name: rec.Name}
		err = ErrUnauthorized
	case errors.Is(lerr, crawlers.ErrNotFound):
		err = ErrUnauthorized
	default:
		err = ErrUnavailable{}
		a.countFailure()
	}
}

func (a *Authenticator) countFailure() {
	a.storageFailures.Add(1)
	if a.obs != nil {
		a.obs.AuthStorageFailure()
	}
}

// store inserts an entry, evicting the entry that expires first when full.
func (a *Authenticator) store(d [32]byte, e entry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.cache[d]; !ok && len(a.cache) >= MaxCacheEntries {
		var oldest [32]byte
		var oldestAt time.Time
		first := true
		for k, v := range a.cache {
			if first || v.expiresAt.Before(oldestAt) {
				oldest, oldestAt, first = k, v.expiresAt, false
			}
		}
		delete(a.cache, oldest)
	}
	a.cache[d] = e
}

// stateSizes reports the coordination state (tests): call records, reserved
// waiters and lookup slots in use.
func (a *Authenticator) stateSizes() (calls, waiters, active int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls), a.waiters, len(a.slots)
}

func (a *Authenticator) cacheLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.cache)
}

type ctxKey struct{}

// WithIdentity returns a context carrying the authenticated crawler. It is the
// only way a crawler's identity travels through the ingest service.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the identity stored by WithIdentity.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}
