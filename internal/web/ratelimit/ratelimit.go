// Package ratelimit is the bounded, process-local rate limiter of the web
// service (docs/specs/web-v1.md §4.6, docs/wip/07D-web-auth-session-plan.md
// §7). It has no HTTP, SQL or goroutines and no external dependency.
//
// Each Limiter is a set of independent GCRA token buckets, one per Key, with a
// hard cap on the number of keys (least-recently-used eviction). A key that
// has fully refilled is indistinguishable from an unknown key, so eviction
// prefers such keys and an attacker needs to churn the whole table to make a
// busy key forget its history. The limiter never locks an account out: every
// bucket refills with time.
//
// The clock is injectable so tests use a fake clock; the production clock is
// the monotonic process clock (rate limits are per process, not a database
// concept).
package ratelimit

import (
	"container/list"
	"crypto/sha256"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Key identifies one bucket. It is fixed-size so memory per key is bounded
// regardless of what an attacker submits.
type Key [17]byte

// Key kinds (first byte).
const (
	kindUnknown = 0
	kindIPv4    = 4
	kindIPv6    = 6
	kindUser    = 'u'
)

// IPKey derives the bucket key of a client address: the IPv4 address itself,
// or the IPv6 /64 (a single subscriber commonly owns a whole /64). An
// IPv4-mapped IPv6 address is treated as IPv4, a zoned or invalid address
// shares the single "unknown" key. The address must come from the trusted
// proxy resolution (server.ClientIP), never from a raw header.
func IPKey(a netip.Addr) Key {
	var k Key
	if !a.IsValid() || a.Zone() != "" {
		k[0] = kindUnknown
		return k
	}
	a = a.Unmap()
	if a.Is4() {
		b := a.As4()
		k[0] = kindIPv4
		copy(k[1:], b[:])
		return k
	}
	b := a.As16()
	k[0] = kindIPv6
	copy(k[1:], b[:8])
	return k
}

// UserKey derives the bucket key of a login name: a truncated hash of the
// case-folded name, so the typed string (which may be a mistyped password) is
// never kept in memory. Folding makes "Alice" and "alice" share one bucket,
// which only makes the limit stricter; authentication itself stays
// case-sensitive.
func UserKey(username string) Key {
	sum := sha256.Sum256([]byte("magnetico/ratelimit/user/v1\x00" + strings.ToLower(username)))
	var k Key
	k[0] = kindUser
	copy(k[1:], sum[:16])
	return k
}

// Config configures a Limiter.
type Config struct {
	// Burst is the number of events a full bucket allows at once (≥ 1).
	Burst int
	// Interval is the time to refill one event (> 0).
	Interval time.Duration
	// MaxKeys bounds the number of tracked keys (≥ 1).
	MaxKeys int
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Limiter is a keyed set of token buckets. It is safe for concurrent use.
type Limiter struct {
	burst    time.Duration // Burst × Interval: the largest tolerated backlog
	interval time.Duration
	maxKeys  int
	now      func() time.Time

	mu  sync.Mutex
	m   map[Key]*list.Element
	lru *list.List // front = most recently used
}

type entry struct {
	key Key
	// tat is the GCRA "theoretical arrival time": the instant at which the
	// bucket is full again. tat ≤ now means full.
	tat time.Time
}

// New validates cfg and returns a Limiter.
func New(cfg Config) (*Limiter, error) {
	if cfg.Burst < 1 || cfg.Interval <= 0 || cfg.MaxKeys < 1 {
		return nil, errors.New("ratelimit: burst, interval and max keys must be positive")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		burst:    time.Duration(cfg.Burst) * cfg.Interval,
		interval: cfg.Interval,
		maxKeys:  cfg.MaxKeys,
		now:      now,
		m:        make(map[Key]*list.Element),
		lru:      list.New(),
	}, nil
}

// Reservation is the outcome of Reserve.
type Reservation struct {
	// OK reports whether the event may proceed.
	OK bool
	// RetryAfter is how long until the same key would be admitted (zero when OK).
	RetryAfter time.Duration

	l    *Limiter
	key  Key
	e    *entry // the bucket instance charged; a refund goes to it alone
	once sync.Once
}

// Cancel returns the unit taken by a granted reservation (for example after a
// successful login, so legitimate use is not throttled). It is idempotent and
// a no-op for a refused reservation.
func (r *Reservation) Cancel() {
	if r == nil || !r.OK || r.l == nil {
		return
	}
	r.once.Do(func() { r.l.refund(r.key, r.e) })
}

// evictionScan is how many least-recently-used entries are inspected for a
// fully refilled bucket before the very oldest is dropped.
const evictionScan = 16

// Reserve takes one unit from key's bucket if available. A refused call does
// not consume anything.
func (l *Limiter) Reserve(key Key) *Reservation {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	el, ok := l.m[key]
	var e *entry
	if ok {
		e = el.Value.(*entry)
		l.lru.MoveToFront(el)
	} else {
		l.evictLocked(now)
		e = &entry{key: key, tat: now}
		l.m[key] = l.lru.PushFront(e)
	}

	base := e.tat
	if base.Before(now) {
		base = now
	}
	newTAT := base.Add(l.interval)
	if over := newTAT.Sub(now) - l.burst; over > 0 {
		return &Reservation{RetryAfter: over}
	}
	e.tat = newTAT
	return &Reservation{OK: true, l: l, key: key, e: e}
}

// refund returns one unit to the bucket instance that was charged. If that
// bucket has since been evicted, the key may now name a different bucket
// (recreated and perhaps exhausted); crediting it would grant allowance that
// was never paid for, so the refund is dropped.
func (l *Limiter) refund(key Key, charged *entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.m[key]; ok && el.Value.(*entry) == charged {
		charged.tat = charged.tat.Add(-l.interval)
	}
}

// evictLocked makes room for one new key. Fully refilled buckets carry no
// information, so one of them is dropped in preference to a busy bucket.
func (l *Limiter) evictLocked(now time.Time) {
	if len(l.m) < l.maxKeys {
		return
	}
	victim := l.lru.Back()
	scanned := 0
	for el := l.lru.Back(); el != nil && scanned < evictionScan; el, scanned = el.Prev(), scanned+1 {
		if !el.Value.(*entry).tat.After(now) {
			victim = el
			break
		}
	}
	if victim != nil {
		delete(l.m, victim.Value.(*entry).key)
		l.lru.Remove(victim)
	}
}

// Len reports the number of tracked keys (for tests and metrics).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// Pair is the result of ReservePair.
type Pair struct {
	// OK reports whether both buckets admitted the event.
	OK bool
	// RetryAfter is the longest wait among the refusing buckets.
	RetryAfter time.Duration
	// A and B are the individual reservations (both refused-or-cancelled
	// when OK is false), so a caller can refund just one of them later.
	A, B *Reservation
}

// ReservePair takes one unit from both buckets atomically in effect: if
// either refuses, whatever was taken from the other is returned and the wait
// is the longest of the refusals. Neither bucket is charged for a refused
// attempt, so a refused attacker cannot drain a victim's allowance further.
func ReservePair(a *Limiter, ka Key, b *Limiter, kb Key) Pair {
	ra, rb := a.Reserve(ka), b.Reserve(kb)
	p := Pair{A: ra, B: rb}
	if ra.OK && rb.OK {
		p.OK = true
		return p
	}
	ra.Cancel()
	rb.Cancel()
	p.RetryAfter = max(ra.RetryAfter, rb.RetryAfter)
	return p
}

// RetryAfterSeconds renders a wait as a Retry-After header value: whole
// seconds, rounded up, at least 1.
func RetryAfterSeconds(d time.Duration) int {
	s := int((d + time.Second - 1) / time.Second)
	return max(s, 1)
}
