// Package cache is the ingest service's bounded, process-local existence
// cache (docs/specs/crawler-ingest-delivery-v1.md §7).
//
// It is one LRU over 20-byte infohashes with split expiry:
//
//   - positive entries ("stored in PostgreSQL") never expire; they are only
//     evicted by LRU pressure or lost on restart. This relies on the
//     documented assumption that the torrent store is append-only in normal
//     operation (no deletion mechanism is planned);
//   - negative entries ("not stored") expire after NegativeTTL (default
//     20 s), because new torrents arrive continuously.
//
// Database errors must never be cached as negative: callers only Put what a
// successful lookup established. A confirmed AddTorrent promotes the entry to
// positive via PutExists.
//
// The list is array-backed (indexes, not pointers) and the map is
// pre-sized, so memory is predictable and verified by a heap test against
// EntryBytes. ExistenceCache is the seam that lets another algorithm replace
// this one without protocol changes.
package cache

import (
	"sync"
	"time"
)

const (
	// DefaultMaxBytes is the default cache budget (8 MiB).
	DefaultMaxBytes = 8 << 20
	// DefaultNegativeTTL is the default lifetime of a negative entry.
	DefaultNegativeTTL = 20 * time.Second
	// EntryBytes is the documented per-entry memory estimate used to derive
	// the entry count from the byte budget: a 40-byte list node plus the
	// map slot (24 bytes of key+value plus control metadata and load-factor
	// slack), rounded up. cache_test.go verifies, with real heap
	// measurements, that the actual cost stays below it.
	EntryBytes = 96
)

// State is a cached answer.
type State uint8

const (
	// Exists: the torrent is stored.
	Exists State = iota + 1
	// Missing: the torrent was not stored when last checked (expires).
	Missing
)

// ExistenceCache is the replaceable cache seam.
type ExistenceCache interface {
	// Get returns the cached state and true, or false for a miss. An
	// expired negative entry is a miss and is removed. A hit refreshes the
	// entry's LRU position.
	Get(hash [20]byte) (State, bool)
	// PutExists records a confirmed stored torrent (no expiry). It
	// overrides a negative entry.
	PutExists(hash [20]byte)
	// PutMissing records a confirmed absence with the negative TTL. It never
	// downgrades a positive entry.
	PutMissing(hash [20]byte)
	// Len is the current number of entries (expired negatives that have not
	// been touched yet included).
	Len() int
	// Stats returns the counters.
	Stats() Stats
}

// Stats are the cache counters.
type Stats struct {
	Hits            uint64 // Get found a live entry
	Misses          uint64 // Get found nothing (expired entries count here too)
	NegativeExpired uint64 // negative entries dropped because their TTL passed
	Evictions       uint64 // entries dropped by LRU pressure
	Promotions      uint64 // negative entries upgraded to positive
	Entries         int
	Capacity        int
}

// Options configures New. Zero values select the defaults.
type Options struct {
	// MaxBytes is the memory budget; the capacity is MaxBytes/EntryBytes
	// entries (at least 1).
	MaxBytes int64
	// NegativeTTL is the lifetime of negative entries.
	NegativeTTL time.Duration
	// Now is the clock (injected in tests).
	Now func() time.Time
}

const nilIdx int32 = -1

type node struct {
	key     [20]byte
	state   State
	expires int64 // unix nanoseconds; 0 = never
	prev    int32
	next    int32
}

// LRU is the cache implementation. It is safe for concurrent use.
type LRU struct {
	mu       sync.Mutex
	nodes    []node
	index    map[[20]byte]int32
	free     []int32 // indexes of unused nodes below len(nodes)
	head     int32   // most recently used
	tail     int32   // least recently used
	capacity int
	ttl      time.Duration
	now      func() time.Time

	hits, misses, expired, evictions, promotions uint64
}

// New builds an LRU.
func New(o Options) *LRU {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.NegativeTTL <= 0 {
		o.NegativeTTL = DefaultNegativeTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	capacity := int(o.MaxBytes / EntryBytes)
	if capacity < 1 {
		capacity = 1
	}
	return &LRU{
		nodes:    make([]node, 0, capacity),
		index:    make(map[[20]byte]int32, capacity),
		head:     nilIdx,
		tail:     nilIdx,
		capacity: capacity,
		ttl:      o.NegativeTTL,
		now:      o.Now,
	}
}

func (c *LRU) unlink(i int32) {
	n := &c.nodes[i]
	if n.prev != nilIdx {
		c.nodes[n.prev].next = n.next
	} else {
		c.head = n.next
	}
	if n.next != nilIdx {
		c.nodes[n.next].prev = n.prev
	} else {
		c.tail = n.prev
	}
	n.prev, n.next = nilIdx, nilIdx
}

func (c *LRU) pushFront(i int32) {
	n := &c.nodes[i]
	n.prev = nilIdx
	n.next = c.head
	if c.head != nilIdx {
		c.nodes[c.head].prev = i
	}
	c.head = i
	if c.tail == nilIdx {
		c.tail = i
	}
}

func (c *LRU) remove(i int32) {
	c.unlink(i)
	delete(c.index, c.nodes[i].key)
	c.nodes[i] = node{prev: nilIdx, next: nilIdx}
	c.free = append(c.free, i)
}

// Get implements ExistenceCache.
func (c *LRU) Get(hash [20]byte) (State, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i, ok := c.index[hash]
	if !ok {
		c.misses++
		return 0, false
	}
	n := &c.nodes[i]
	if n.expires != 0 && c.now().UnixNano() >= n.expires {
		c.remove(i)
		c.expired++
		c.misses++
		return 0, false
	}
	c.hits++
	if c.head != i {
		c.unlink(i)
		c.pushFront(i)
	}
	return n.state, true
}

// put inserts or updates. expires = 0 means never.
func (c *LRU) put(hash [20]byte, state State, expires int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i, ok := c.index[hash]; ok {
		n := &c.nodes[i]
		if state == Missing && n.state == Exists {
			// Never downgrade a positive entry; just refresh it.
			if c.head != i {
				c.unlink(i)
				c.pushFront(i)
			}
			return
		}
		if state == Exists && n.state == Missing {
			c.promotions++
		}
		n.state, n.expires = state, expires
		if c.head != i {
			c.unlink(i)
			c.pushFront(i)
		}
		return
	}

	var i int32
	switch {
	case len(c.free) > 0:
		i = c.free[len(c.free)-1]
		c.free = c.free[:len(c.free)-1]
	case len(c.nodes) < c.capacity:
		c.nodes = append(c.nodes, node{})
		i = int32(len(c.nodes) - 1)
	default:
		i = c.tail // evict the least recently used entry
		c.unlink(i)
		delete(c.index, c.nodes[i].key)
		c.evictions++
	}
	c.nodes[i] = node{key: hash, state: state, expires: expires, prev: nilIdx, next: nilIdx}
	c.index[hash] = i
	c.pushFront(i)
}

// PutExists implements ExistenceCache.
func (c *LRU) PutExists(hash [20]byte) { c.put(hash, Exists, 0) }

// PutMissing implements ExistenceCache.
func (c *LRU) PutMissing(hash [20]byte) {
	c.put(hash, Missing, c.now().Add(c.ttl).UnixNano())
}

// Len implements ExistenceCache.
func (c *LRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.index)
}

// Stats implements ExistenceCache.
func (c *LRU) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Hits: c.hits, Misses: c.misses, NegativeExpired: c.expired,
		Evictions: c.evictions, Promotions: c.promotions,
		Entries: len(c.index), Capacity: c.capacity,
	}
}

// Disabled is the cache used when caching is switched off
// (MAGNETICO_INGEST_CACHE_DISABLED): it stores nothing and never hits.
type Disabled struct {
	mu     sync.Mutex
	misses uint64
}

// Get implements ExistenceCache.
func (d *Disabled) Get([20]byte) (State, bool) {
	d.mu.Lock()
	d.misses++
	d.mu.Unlock()
	return 0, false
}

// PutExists implements ExistenceCache.
func (*Disabled) PutExists([20]byte) {}

// PutMissing implements ExistenceCache.
func (*Disabled) PutMissing([20]byte) {}

// Len implements ExistenceCache.
func (*Disabled) Len() int { return 0 }

// Stats implements ExistenceCache.
func (d *Disabled) Stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Stats{Misses: d.misses}
}

var (
	_ ExistenceCache = (*LRU)(nil)
	_ ExistenceCache = (*Disabled)(nil)
)
