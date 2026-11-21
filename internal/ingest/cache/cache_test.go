package cache

import (
	"encoding/binary"
	"runtime"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

func key(n int) [20]byte {
	var k [20]byte
	binary.BigEndian.PutUint64(k[:], uint64(n)*0x9E3779B97F4A7C15)
	binary.BigEndian.PutUint64(k[8:], uint64(n))
	return k
}

// capFor returns a MaxBytes that yields exactly n entries.
func capFor(n int) int64 { return int64(n) * EntryBytes }

func TestCapacityFromBudget(t *testing.T) {
	t.Parallel()
	if c := New(Options{}).Stats().Capacity; c != DefaultMaxBytes/EntryBytes {
		t.Errorf("default capacity = %d", c)
	}
	if c := New(Options{MaxBytes: capFor(1000)}).Stats().Capacity; c != 1000 {
		t.Errorf("capacity = %d", c)
	}
	if c := New(Options{MaxBytes: 1}).Stats().Capacity; c != 1 {
		t.Errorf("tiny budget capacity = %d, want 1", c)
	}
}

func TestPositiveAndNegativeEntries(t *testing.T) {
	t.Parallel()
	c := New(Options{MaxBytes: capFor(10)})
	if _, ok := c.Get(key(1)); ok {
		t.Fatal("empty cache hit")
	}
	c.PutExists(key(1))
	c.PutMissing(key(2))
	if st, ok := c.Get(key(1)); !ok || st != Exists {
		t.Errorf("key1: %v %v", st, ok)
	}
	if st, ok := c.Get(key(2)); !ok || st != Missing {
		t.Errorf("key2: %v %v", st, ok)
	}
	s := c.Stats()
	if s.Hits != 2 || s.Misses != 1 || s.Entries != 2 {
		t.Errorf("stats: %+v", s)
	}
}

func TestNegativeTTLExpiry(t *testing.T) {
	t.Parallel()
	clk := newClock()
	c := New(Options{MaxBytes: capFor(10), Now: clk.Now})
	c.PutMissing(key(1))

	clk.Advance(DefaultNegativeTTL - time.Nanosecond)
	if st, ok := c.Get(key(1)); !ok || st != Missing {
		t.Fatal("negative entry must be live just before the TTL")
	}
	// Refreshing by Get does not extend the TTL.
	clk.Advance(time.Nanosecond)
	if _, ok := c.Get(key(1)); ok {
		t.Fatal("negative entry must expire exactly at the TTL")
	}
	s := c.Stats()
	if s.NegativeExpired != 1 || s.Entries != 0 {
		t.Errorf("expired entry must be removed: %+v", s)
	}
	// Its slot is reusable.
	c.PutMissing(key(1))
	if c.Len() != 1 {
		t.Errorf("len = %d", c.Len())
	}
	// A new PutMissing restarts the TTL.
	clk.Advance(DefaultNegativeTTL - time.Second)
	c.PutMissing(key(1))
	clk.Advance(DefaultNegativeTTL - time.Second)
	if _, ok := c.Get(key(1)); !ok {
		t.Error("re-put must restart the TTL")
	}
}

func TestCustomTTL(t *testing.T) {
	t.Parallel()
	clk := newClock()
	c := New(Options{MaxBytes: capFor(4), NegativeTTL: time.Hour, Now: clk.Now})
	c.PutMissing(key(1))
	clk.Advance(59 * time.Minute)
	if _, ok := c.Get(key(1)); !ok {
		t.Error("expired too early")
	}
	clk.Advance(2 * time.Minute)
	if _, ok := c.Get(key(1)); ok {
		t.Error("did not expire")
	}
}

func TestPositiveEntriesNeverExpire(t *testing.T) {
	t.Parallel()
	clk := newClock()
	c := New(Options{MaxBytes: capFor(10), Now: clk.Now})
	c.PutExists(key(1))
	clk.Advance(365 * 24 * time.Hour)
	if st, ok := c.Get(key(1)); !ok || st != Exists {
		t.Error("positive entry expired")
	}
}

func TestPromotionAndNoDowngrade(t *testing.T) {
	t.Parallel()
	clk := newClock()
	c := New(Options{MaxBytes: capFor(10), Now: clk.Now})

	// Confirmed AddTorrent promotes a negative entry; the TTL no longer applies.
	c.PutMissing(key(1))
	c.PutExists(key(1))
	clk.Advance(time.Hour)
	if st, ok := c.Get(key(1)); !ok || st != Exists {
		t.Fatalf("promotion lost: %v %v", st, ok)
	}
	if c.Stats().Promotions != 1 || c.Len() != 1 {
		t.Errorf("stats: %+v", c.Stats())
	}

	// A late "missing" answer (a lookup that started before the insert) must
	// not downgrade a positive entry.
	c.PutMissing(key(1))
	clk.Advance(time.Hour)
	if st, ok := c.Get(key(1)); !ok || st != Exists {
		t.Fatal("positive entry was downgraded by PutMissing")
	}
}

func TestLRUEvictionOrder(t *testing.T) {
	t.Parallel()
	c := New(Options{MaxBytes: capFor(3)})
	c.PutExists(key(1))
	c.PutExists(key(2))
	c.PutExists(key(3))
	// Touch 1: the order becomes (old→new) 2, 3, 1.
	if _, ok := c.Get(key(1)); !ok {
		t.Fatal("miss")
	}
	c.PutExists(key(4)) // evicts 2
	if _, ok := c.Get(key(2)); ok {
		t.Error("key 2 should have been evicted first")
	}
	for _, k := range []int{1, 3, 4} {
		if _, ok := c.Get(key(k)); !ok {
			t.Errorf("key %d evicted unexpectedly", k)
		}
	}
	c.PutExists(key(5)) // order was 1,3,4 after the Gets above → evicts 1
	if _, ok := c.Get(key(1)); ok {
		t.Error("key 1 should have been evicted second")
	}
	s := c.Stats()
	if s.Evictions != 2 || s.Entries != 3 || s.Capacity != 3 {
		t.Errorf("stats: %+v", s)
	}
}

func TestPutRefreshesRecency(t *testing.T) {
	t.Parallel()
	c := New(Options{MaxBytes: capFor(2)})
	c.PutExists(key(1))
	c.PutExists(key(2))
	c.PutExists(key(1)) // re-put refreshes: 2 is now the LRU entry
	c.PutExists(key(3))
	if _, ok := c.Get(key(2)); ok {
		t.Error("key 2 should have been evicted")
	}
	if _, ok := c.Get(key(1)); !ok {
		t.Error("key 1 should have survived")
	}
}

func TestSingleEntryCache(t *testing.T) {
	t.Parallel()
	c := New(Options{MaxBytes: 1})
	c.PutExists(key(1))
	c.PutExists(key(2))
	if _, ok := c.Get(key(1)); ok {
		t.Error("old entry kept")
	}
	if _, ok := c.Get(key(2)); !ok {
		t.Error("new entry missing")
	}
}

// Expired entries free slots that are reused before anything is evicted.
func TestExpiredSlotsAreReusedBeforeEviction(t *testing.T) {
	t.Parallel()
	clk := newClock()
	c := New(Options{MaxBytes: capFor(3), Now: clk.Now})
	c.PutMissing(key(1))
	c.PutMissing(key(2))
	c.PutExists(key(3))
	clk.Advance(time.Minute)
	c.Get(key(1)) // drops the expired entry
	c.PutExists(key(4))
	if c.Stats().Evictions != 0 {
		t.Errorf("evicted although a slot was free: %+v", c.Stats())
	}
	if _, ok := c.Get(key(3)); !ok {
		t.Error("live entry lost")
	}
}

func TestDisabledCache(t *testing.T) {
	t.Parallel()
	var c ExistenceCache = &Disabled{}
	c.PutExists(key(1))
	c.PutMissing(key(2))
	if _, ok := c.Get(key(1)); ok {
		t.Error("disabled cache returned a hit")
	}
	if c.Len() != 0 || c.Stats().Misses != 1 {
		t.Errorf("stats: %+v", c.Stats())
	}
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	clk := newClock()
	c := New(Options{MaxBytes: capFor(500), Now: clk.Now})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				k := key((g*7919 + i) % 2000)
				switch i % 4 {
				case 0:
					c.PutExists(k)
				case 1:
					c.PutMissing(k)
				default:
					c.Get(k)
				}
				if i%1000 == 0 {
					clk.Advance(5 * time.Second)
				}
			}
		}()
	}
	wg.Wait()
	s := c.Stats()
	if s.Entries > s.Capacity || s.Entries != c.Len() {
		t.Errorf("stats: %+v len=%d", s, c.Len())
	}
	// Internal consistency: the list covers exactly the indexed entries.
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for i := c.head; i != nilIdx; i = c.nodes[i].next {
		n++
		if n > len(c.nodes) {
			t.Fatal("cycle in the LRU list")
		}
	}
	if n != len(c.index) {
		t.Errorf("list has %d nodes, index %d", n, len(c.index))
	}
}

// TestHeapBudget verifies the documented per-entry estimate against real
// heap usage: a full default-sized cache stays within its 8 MiB budget.
func TestHeapBudget(t *testing.T) {
	// Not parallel: heap measurements must not be disturbed by other tests.
	measure := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}

	before := measure()
	c := New(Options{MaxBytes: DefaultMaxBytes})
	capacity := c.Stats().Capacity
	// Fill with distinct entries, then churn through twice the capacity so
	// that eviction and slot reuse are included.
	for i := 0; i < capacity; i++ {
		if i%2 == 0 {
			c.PutExists(key(i))
		} else {
			c.PutMissing(key(i))
		}
	}
	for i := capacity; i < 2*capacity; i++ {
		c.PutExists(key(i))
	}
	after := measure()
	runtime.KeepAlive(c)

	used := int64(after) - int64(before)
	t.Logf("capacity=%d entries, heap used=%d bytes (%.1f B/entry), budget=%d, estimate=%d B/entry",
		capacity, used, float64(used)/float64(capacity), DefaultMaxBytes, EntryBytes)
	if c.Len() != capacity {
		t.Fatalf("len = %d, want %d", c.Len(), capacity)
	}
	if used > DefaultMaxBytes {
		t.Errorf("heap use %d exceeds the %d-byte budget", used, DefaultMaxBytes)
	}
	if per := float64(used) / float64(capacity); per > EntryBytes {
		t.Errorf("measured %.1f B/entry exceeds the documented estimate %d", per, EntryBytes)
	}
}
