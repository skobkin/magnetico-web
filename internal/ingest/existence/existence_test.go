package existence

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/ingest/cache"
)

func h(n int) [20]byte {
	var x [20]byte
	binary.BigEndian.PutUint64(x[:], uint64(n))
	return x
}

// fakeFinder is a Finder over a mutable set, recording every call.
type fakeFinder struct {
	mu       sync.Mutex
	stored   map[[20]byte]struct{}
	calls    [][][20]byte
	err      error
	gate     chan struct{} // if non-nil, lookups block until it is closed
	entered  chan struct{} // receives one value per lookup that started
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

func newFake(stored ...int) *fakeFinder {
	f := &fakeFinder{stored: map[[20]byte]struct{}{}}
	for _, n := range stored {
		f.stored[h(n)] = struct{}{}
	}
	return f
}

func (f *fakeFinder) ExistingInfoHashes(ctx context.Context, in [][]byte) (map[[20]byte]struct{}, error) {
	cur := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.maxSeen.Load()
		if cur <= m || f.maxSeen.CompareAndSwap(m, cur) {
			break
		}
	}
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var batch [][20]byte
	for _, b := range in {
		batch = append(batch, [20]byte(b))
	}
	f.calls = append(f.calls, batch)
	if f.err != nil {
		return nil, f.err
	}
	out := map[[20]byte]struct{}{}
	for _, b := range batch {
		if _, ok := f.stored[b]; ok {
			out[b] = struct{}{}
		}
	}
	return out, nil
}

func (f *fakeFinder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newService(f Finder, clk *clock) (*Service, *cache.LRU) {
	c := cache.New(cache.Options{Now: clk.Now})
	return New(f, c, Options{}), c
}

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

func TestClassificationAndDeduplication(t *testing.T) {
	t.Parallel()
	f := newFake(1, 3)
	s, _ := newService(f, newClock())

	r := s.Check(context.Background(), [][20]byte{h(1), h(2), h(3), h(2), h(1), h(4)})
	if len(r.Exists) != 2 || r.Exists[0] != h(1) || r.Exists[1] != h(3) {
		t.Errorf("exists = %v", r.Exists)
	}
	if len(r.Missing) != 2 || r.Missing[0] != h(2) || r.Missing[1] != h(4) {
		t.Errorf("missing = %v", r.Missing)
	}
	if len(r.Unknown) != 0 {
		t.Errorf("unknown = %v", r.Unknown)
	}
	if f.callCount() != 1 || len(f.calls[0]) != 4 {
		t.Errorf("want one query over the 4 distinct hashes, got %v", f.calls)
	}
}

func TestEmptyInput(t *testing.T) {
	t.Parallel()
	f := newFake()
	s, _ := newService(f, newClock())
	r := s.Check(context.Background(), nil)
	if len(r.Exists)+len(r.Missing)+len(r.Unknown) != 0 || f.callCount() != 0 {
		t.Errorf("empty input: %+v, calls=%d", r, f.callCount())
	}
}

func TestPositiveCacheAvoidsDatabase(t *testing.T) {
	t.Parallel()
	f := newFake(1)
	clk := newClock()
	s, _ := newService(f, clk)
	s.Check(context.Background(), [][20]byte{h(1)})
	clk.Advance(48 * time.Hour) // positive entries never expire
	r := s.Check(context.Background(), [][20]byte{h(1)})
	if len(r.Exists) != 1 || f.callCount() != 1 {
		t.Errorf("exists=%v calls=%d", r.Exists, f.callCount())
	}
	if st := s.Stats(); st.CacheHits != 1 || st.DBLookups != 1 {
		t.Errorf("stats: %+v", st)
	}
}

func TestNegativeCacheExpires(t *testing.T) {
	t.Parallel()
	f := newFake()
	clk := newClock()
	s, _ := newService(f, clk)
	ctx := context.Background()

	s.Check(ctx, [][20]byte{h(9)})
	clk.Advance(19 * time.Second)
	if r := s.Check(ctx, [][20]byte{h(9)}); len(r.Missing) != 1 || f.callCount() != 1 {
		t.Fatalf("negative entry should still be served from the cache: calls=%d", f.callCount())
	}

	// The torrent is stored meanwhile; the stale negative entry lives until
	// its TTL, then the database is asked again.
	f.mu.Lock()
	f.stored[h(9)] = struct{}{}
	f.mu.Unlock()
	if r := s.Check(ctx, [][20]byte{h(9)}); len(r.Missing) != 1 {
		t.Fatalf("within the TTL the cached negative answer is served: %+v", r)
	}
	clk.Advance(2 * time.Second)
	r := s.Check(ctx, [][20]byte{h(9)})
	if len(r.Exists) != 1 || f.callCount() != 2 {
		t.Errorf("after the TTL: %+v calls=%d", r, f.callCount())
	}
}

func TestPromotionAfterAddTorrent(t *testing.T) {
	t.Parallel()
	f := newFake()
	clk := newClock()
	s, _ := newService(f, clk)
	ctx := context.Background()

	s.Check(ctx, [][20]byte{h(5)}) // cached as missing
	s.MarkExisting(h(5))           // consumer inserted it
	r := s.Check(ctx, [][20]byte{h(5)})
	if len(r.Exists) != 1 || f.callCount() != 1 {
		t.Errorf("promotion not visible: %+v calls=%d", r, f.callCount())
	}
	clk.Advance(time.Hour)
	if r := s.Check(ctx, [][20]byte{h(5)}); len(r.Exists) != 1 {
		t.Error("promoted entry expired")
	}
}

func TestDatabaseErrorIsUnknownAndNeverCached(t *testing.T) {
	t.Parallel()
	f := newFake(1)
	f.err = errors.New("boom")
	s, c := newService(f, newClock())
	ctx := context.Background()

	r := s.Check(ctx, [][20]byte{h(1), h(2)})
	if len(r.Unknown) != 2 || len(r.Missing) != 0 || len(r.Exists) != 0 {
		t.Fatalf("an error must yield Unknown only: %+v", r)
	}
	if c.Len() != 0 {
		t.Errorf("an error was cached (%d entries)", c.Len())
	}
	if st := s.Stats(); st.DBErrors != 1 || st.UnknownResult != 2 {
		t.Errorf("stats: %+v", st)
	}

	// Recovery: the next request asks the database again and gets answers.
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	r = s.Check(ctx, [][20]byte{h(1), h(2)})
	if len(r.Exists) != 1 || len(r.Missing) != 1 || len(r.Unknown) != 0 {
		t.Errorf("after recovery: %+v", r)
	}
	// The in-flight table is empty after failures.
	s.mu.Lock()
	n := len(s.inflight)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("%d in-flight entries leaked", n)
	}
}

func TestMixedCacheAndDatabaseErrors(t *testing.T) {
	t.Parallel()
	f := newFake(1)
	s, _ := newService(f, newClock())
	ctx := context.Background()
	s.Check(ctx, [][20]byte{h(1)}) // cached positive

	f.mu.Lock()
	f.err = errors.New("down")
	f.mu.Unlock()
	r := s.Check(ctx, [][20]byte{h(1), h(2)})
	if len(r.Exists) != 1 || r.Exists[0] != h(1) {
		t.Errorf("cached answer must survive a database outage: %+v", r)
	}
	if len(r.Unknown) != 1 || r.Unknown[0] != h(2) {
		t.Errorf("uncached hash must be Unknown: %+v", r)
	}
}

// Concurrent requests for the same missing hashes collapse into one lookup.
func TestConcurrentIdenticalMissesCollapse(t *testing.T) {
	t.Parallel()
	f := newFake(1)
	f.gate = make(chan struct{})
	f.entered = make(chan struct{}, 64)
	s, _ := newService(f, newClock())

	const n = 40
	results := make([]Result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = s.Check(context.Background(), [][20]byte{h(1), h(2)})
		}()
	}
	<-f.entered // the leader is inside the lookup, everyone else must join it
	deadline := time.Now().Add(5 * time.Second)
	// Every follower joins both hashes of the leader's lookup, so all of them have
	// joined once Joined reaches 2*(n-1); a smaller bound lets late starters begin
	// after the gate opens and query the database a second time.
	for s.Stats().Requests < n || s.Stats().Joined < 2*(n-1) {
		if time.Now().After(deadline) {
			t.Fatalf("requests did not join: %+v", s.Stats())
		}
		time.Sleep(time.Millisecond)
	}
	close(f.gate)
	wg.Wait()

	if f.callCount() != 1 {
		t.Fatalf("database queried %d times, want 1", f.callCount())
	}
	for i, r := range results {
		if len(r.Exists) != 1 || r.Exists[0] != h(1) || len(r.Missing) != 1 || r.Missing[0] != h(2) {
			t.Fatalf("result %d: %+v", i, r)
		}
	}
	if st := s.Stats(); st.DBLookups != 1 || st.Joined < 2*(n-1) {
		t.Errorf("stats: %+v", st)
	}
}

// Overlapping sets: every hash is resolved by exactly one database lookup
// while it is in flight; only the disjoint remainders are queried separately.
func TestOverlappingBatchesShareLookups(t *testing.T) {
	t.Parallel()
	f := newFake(2)
	f.gate = make(chan struct{})
	f.entered = make(chan struct{}, 4)
	s, _ := newService(f, newClock())

	var r1, r2 Result
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r1 = s.Check(context.Background(), [][20]byte{h(1), h(2), h(3)}) }()
	<-f.entered
	go func() { defer wg.Done(); r2 = s.Check(context.Background(), [][20]byte{h(2), h(3), h(4)}) }()
	<-f.entered // the second request's own lookup (only h(4))
	close(f.gate)
	wg.Wait()

	seen := map[[20]byte]int{}
	for _, c := range f.calls {
		for _, x := range c {
			seen[x]++
		}
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("hash %x queried %d times", n[:8], c)
		}
	}
	if len(seen) != 4 {
		t.Errorf("queried %d distinct hashes, want 4", len(seen))
	}
	if len(r1.Exists) != 1 || len(r1.Missing) != 2 || len(r2.Exists) != 1 || len(r2.Missing) != 2 {
		t.Errorf("r1=%+v r2=%+v", r1, r2)
	}
}

// A joined request is told Unknown (not Missing) when the lookup it joined
// fails.
func TestJoinedRequestsSeeTheLeadersError(t *testing.T) {
	t.Parallel()
	f := newFake(1)
	f.gate = make(chan struct{})
	f.entered = make(chan struct{}, 4)
	f.err = errors.New("boom")
	s, _ := newService(f, newClock())

	var r1, r2 Result
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r1 = s.Check(context.Background(), [][20]byte{h(1)}) }()
	<-f.entered
	go func() { defer wg.Done(); r2 = s.Check(context.Background(), [][20]byte{h(1)}) }()
	for s.Stats().Joined < 1 {
		time.Sleep(time.Millisecond)
	}
	close(f.gate)
	wg.Wait()
	if len(r1.Unknown) != 1 || len(r2.Unknown) != 1 || len(r1.Missing)+len(r2.Missing) != 0 {
		t.Errorf("r1=%+v r2=%+v", r1, r2)
	}
}

func TestCancelledContextYieldsUnknown(t *testing.T) {
	t.Parallel()
	f := newFake(1)
	f.gate = make(chan struct{}) // never opened
	s, c := newService(f, newClock())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	r := s.Check(ctx, [][20]byte{h(1)})
	if len(r.Unknown) != 1 || len(r.Missing) != 0 {
		t.Errorf("cancelled lookup: %+v", r)
	}
	if c.Len() != 0 {
		t.Error("cancelled lookup populated the cache")
	}
	// A joiner whose own context ends does not hang either.
	s.mu.Lock()
	n := len(s.inflight)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("%d in-flight entries leaked after cancellation", n)
	}
}

func TestConcurrencyCap(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.gate = make(chan struct{})
	f.entered = make(chan struct{}, 200)
	c := cache.New(cache.Options{})
	s := New(f, c, Options{MaxConcurrentLookups: 4})

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Check(context.Background(), [][20]byte{h(1000 + i)}) // distinct: no joining
		}()
	}
	for i := 0; i < 4; i++ {
		<-f.entered
	}
	time.Sleep(50 * time.Millisecond) // let the others queue on the semaphore
	if got := f.inFlight.Load(); got != 4 {
		t.Errorf("in-flight lookups = %d, want the cap 4", got)
	}
	close(f.gate)
	wg.Wait()
	if m := f.maxSeen.Load(); m > 4 {
		t.Errorf("observed %d concurrent lookups, cap is 4", m)
	}
	if f.callCount() != 30 {
		t.Errorf("lookups = %d", f.callCount())
	}
}

func TestDisabledCacheStillCollapsesAndNeverStores(t *testing.T) {
	t.Parallel()
	f := newFake(1)
	s := New(f, &cache.Disabled{}, Options{})
	ctx := context.Background()
	s.Check(ctx, [][20]byte{h(1)})
	s.Check(ctx, [][20]byte{h(1)})
	if f.callCount() != 2 {
		t.Errorf("with the cache disabled every request must hit the database: %d", f.callCount())
	}
}

func TestStressRace(t *testing.T) {
	t.Parallel()
	f := newFake(1, 3, 5, 7)
	s, _ := newService(f, newClock())
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				r := s.Check(context.Background(), [][20]byte{h(i % 10), h((i + g) % 10), h(3)})
				for _, x := range r.Missing {
					if _, ok := f.stored[x]; ok {
						t.Errorf("stored hash reported missing: %x", x[:8])
						return
					}
				}
				if i%50 == 0 {
					s.MarkExisting(h(11))
				}
			}
		}()
	}
	wg.Wait()
}
