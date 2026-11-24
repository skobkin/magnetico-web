// Package existence answers "which of these infohashes are already stored?"
// for the ingest service (docs/specs/crawler-ingest-delivery-v1.md §5.1, §7).
//
// Flow per request: deduplicate → cache → join in-flight lookups for hashes
// another request is already resolving → one batched query for the rest
// (concurrency-capped). The answer has three classes:
//
//   - Exists:  confirmed stored in PostgreSQL;
//   - Missing: confirmed absent;
//   - Unknown: the lookup failed or was abandoned. An error is NEVER
//     reported as Missing and never cached.
//
// Pending ingest-queue rows are deliberately not consulted.
package existence

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/skobkin/magnetico-web/internal/ingest/cache"
)

// DefaultMaxConcurrentLookups caps simultaneous database lookups (§5.1).
const DefaultMaxConcurrentLookups = 16

// Finder is the persistence seam: persistence.Store satisfies it.
type Finder interface {
	ExistingInfoHashes(ctx context.Context, infoHashes [][]byte) (map[[20]byte]struct{}, error)
}

// Result classifies the distinct input hashes, each class in order of first
// occurrence in the input.
type Result struct {
	Exists  [][20]byte
	Missing [][20]byte
	Unknown [][20]byte

	// Cache outcome of the distinct hashes of this call: answered from a
	// positive entry, from a negative entry, or not answered by the cache.
	CacheHitsPositive, CacheHitsNegative, CacheMisses int
}

// Stats are the service counters.
type Stats struct {
	Requests      uint64 // Check calls
	Hashes        uint64 // distinct hashes classified
	CacheHits     uint64 // answered from the cache
	DBLookups     uint64 // ExistingInfoHashes calls
	DBHashes      uint64 // hashes sent to the database
	DBErrors      uint64 // failed lookups
	Joined        uint64 // hashes resolved by joining another request's lookup
	UnknownResult uint64 // hashes answered Unknown
}

// Options configures New.
type Options struct {
	// MaxConcurrentLookups defaults to DefaultMaxConcurrentLookups.
	MaxConcurrentLookups int
}

type call struct {
	done   chan struct{}
	exists bool
	err    error
}

// Service is safe for concurrent use.
type Service struct {
	finder Finder
	cache  cache.ExistenceCache
	sem    chan struct{}

	mu       sync.Mutex
	inflight map[[20]byte]*call

	requests, hashes, cacheHits, dbLookups, dbHashes, dbErrors, joined, unknown atomic.Uint64
}

// New builds a Service. c may be a cache.Disabled to switch caching off.
func New(f Finder, c cache.ExistenceCache, o Options) *Service {
	if o.MaxConcurrentLookups <= 0 {
		o.MaxConcurrentLookups = DefaultMaxConcurrentLookups
	}
	return &Service{
		finder:   f,
		cache:    c,
		sem:      make(chan struct{}, o.MaxConcurrentLookups),
		inflight: make(map[[20]byte]*call),
	}
}

// MarkExisting promotes a hash to a positive cache entry; the consumer calls
// it after AddTorrent succeeded (created or already present).
func (s *Service) MarkExisting(h [20]byte) { s.cache.PutExists(h) }

// Stats returns the counters.
func (s *Service) Stats() Stats {
	return Stats{
		Requests: s.requests.Load(), Hashes: s.hashes.Load(), CacheHits: s.cacheHits.Load(),
		DBLookups: s.dbLookups.Load(), DBHashes: s.dbHashes.Load(), DBErrors: s.dbErrors.Load(),
		Joined: s.joined.Load(), UnknownResult: s.unknown.Load(),
	}
}

// Check classifies hashes. It never returns an error: failures surface as
// Unknown entries so the caller can answer per hash.
func (s *Service) Check(ctx context.Context, hashes [][20]byte) Result {
	s.requests.Add(1)

	// 1. Deduplicate, keeping first-occurrence order.
	seen := make(map[[20]byte]struct{}, len(hashes))
	distinct := make([][20]byte, 0, len(hashes))
	for _, h := range hashes {
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		distinct = append(distinct, h)
	}
	s.hashes.Add(uint64(len(distinct)))

	state := make(map[[20]byte]byte, len(distinct)) // 'e' exists, 'm' missing, 'u' unknown

	// 2. Cache.
	var misses [][20]byte
	var hitsPos, hitsNeg int
	for _, h := range distinct {
		if st, ok := s.cache.Get(h); ok {
			s.cacheHits.Add(1)
			if st == cache.Exists {
				state[h] = 'e'
				hitsPos++
			} else {
				state[h] = 'm'
				hitsNeg++
			}
			continue
		}
		misses = append(misses, h)
	}

	// 3. Join in-flight lookups, own the rest.
	type waiter struct {
		h [20]byte
		c *call
	}
	var waits []waiter
	var owned [][20]byte
	var ownedCalls []*call
	if len(misses) > 0 {
		s.mu.Lock()
		for _, h := range misses {
			if c, ok := s.inflight[h]; ok {
				waits = append(waits, waiter{h, c})
				continue
			}
			c := &call{done: make(chan struct{})}
			s.inflight[h] = c
			owned = append(owned, h)
			ownedCalls = append(ownedCalls, c)
		}
		s.mu.Unlock()
		s.joined.Add(uint64(len(waits)))
	}

	// 4. One batched lookup for the hashes we own.
	if len(owned) > 0 {
		s.lookup(ctx, owned, ownedCalls)
		for i, h := range owned {
			c := ownedCalls[i]
			switch {
			case c.err != nil:
				state[h] = 'u'
			case c.exists:
				state[h] = 'e'
			default:
				state[h] = 'm'
			}
		}
	}

	// 5. Collect joined lookups.
	for _, w := range waits {
		select {
		case <-w.c.done:
			switch {
			case w.c.err != nil:
				state[w.h] = 'u'
			case w.c.exists:
				state[w.h] = 'e'
			default:
				state[w.h] = 'm'
			}
		case <-ctx.Done():
			state[w.h] = 'u'
		}
	}

	r := Result{CacheHitsPositive: hitsPos, CacheHitsNegative: hitsNeg, CacheMisses: len(misses)}
	for _, h := range distinct {
		switch state[h] {
		case 'e':
			r.Exists = append(r.Exists, h)
		case 'm':
			r.Missing = append(r.Missing, h)
		default:
			r.Unknown = append(r.Unknown, h)
		}
	}
	s.unknown.Add(uint64(len(r.Unknown)))
	return r
}

// lookup resolves the owned calls with one query, fills the cache with
// confirmed answers only, and always releases every call.
func (s *Service) lookup(ctx context.Context, owned [][20]byte, calls []*call) {
	var found map[[20]byte]struct{}
	var err error

	select {
	case s.sem <- struct{}{}:
		s.dbLookups.Add(1)
		s.dbHashes.Add(uint64(len(owned)))
		args := make([][]byte, len(owned))
		for i := range owned {
			args[i] = owned[i][:]
		}
		found, err = s.finder.ExistingInfoHashes(ctx, args)
		<-s.sem
		if err != nil {
			s.dbErrors.Add(1)
		}
	case <-ctx.Done():
		err = ctx.Err()
	}

	// Complete the calls: cache first, so that anyone who finds the in-flight
	// entry gone finds the cache entry instead.
	for i, h := range owned {
		c := calls[i]
		if err != nil {
			c.err = err // never cached: an error is not an answer
		} else if _, ok := found[h]; ok {
			c.exists = true
			s.cache.PutExists(h)
		} else {
			s.cache.PutMissing(h)
		}
	}
	s.mu.Lock()
	for i, h := range owned {
		delete(s.inflight, h)
		close(calls[i].done)
	}
	s.mu.Unlock()
}
