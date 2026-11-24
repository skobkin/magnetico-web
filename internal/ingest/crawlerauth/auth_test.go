package crawlerauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/ingest/crawlers"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// tok builds a well-formed credential (mgc_ + 43 base64url characters).
func tok(i int) string { return fmt.Sprintf("mgc_%043d", i) }

type fakeLookup struct {
	mu        sync.Mutex
	recs      map[[32]byte]crawlers.Record
	err       error
	calls     atomic.Int64
	active    atomic.Int64
	maxActive atomic.Int64
	// hook runs inside LookupDigest (it may block, panic or move the clock).
	hook func(ctx context.Context) error
}

func newLookup() *fakeLookup { return &fakeLookup{recs: map[[32]byte]crawlers.Record{}} }

func (l *fakeLookup) set(token string, r crawlers.Record) {
	l.mu.Lock()
	l.recs[crawlers.Digest(token)] = r
	l.mu.Unlock()
}
func (l *fakeLookup) del(token string) {
	l.mu.Lock()
	delete(l.recs, crawlers.Digest(token))
	l.mu.Unlock()
}
func (l *fakeLookup) fail(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
}

func (l *fakeLookup) LookupDigest(ctx context.Context, d [32]byte) (crawlers.Record, error) {
	l.calls.Add(1)
	n := l.active.Add(1)
	defer l.active.Add(-1)
	for {
		m := l.maxActive.Load()
		if n <= m || l.maxActive.CompareAndSwap(m, n) {
			break
		}
	}
	if l.hook != nil {
		if err := l.hook(ctx); err != nil {
			return crawlers.Record{}, err
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return crawlers.Record{}, l.err
	}
	r, ok := l.recs[d]
	if !ok {
		return crawlers.Record{}, crawlers.ErrNotFound
	}
	return r, nil
}

func rec(id, name string) crawlers.Record { return crawlers.Record{ID: id, Name: name, Enabled: true} }

func newAuth(l Lookup, c Clock) *Authenticator {
	return New(l, Options{Clock: c, Logger: log.New(&bytes.Buffer{}, "", 0)})
}

func TestMissingMalformedUnknownAndDisabledAreUnauthorizedAndMalformedNeverHitTheDatabase(t *testing.T) {
	l := newLookup()
	l.set(tok(1), crawlers.Record{ID: "u1", Name: "off", Enabled: false})
	a := newAuth(l, newClock())
	for name, bearer := range map[string]string{
		"missing": "", "wrong prefix": "xyz_" + strings.Repeat("a", 43), "too short": "mgc_abc",
		"bad alphabet": "mgc_" + strings.Repeat("!", 43), "huge": "mgc_" + strings.Repeat("a", 4096),
	} {
		if _, err := a.Authenticate(context.Background(), bearer); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: %v, want ErrUnauthorized", name, err)
		}
	}
	if n := l.calls.Load(); n != 0 {
		t.Fatalf("malformed credentials caused %d lookups", n)
	}
	// Unknown and disabled give the very same error value (no oracle).
	_, e1 := a.Authenticate(context.Background(), tok(2))
	_, e2 := a.Authenticate(context.Background(), tok(1))
	if !errors.Is(e1, ErrUnauthorized) || e1 != e2 {
		t.Fatalf("unknown=%v disabled=%v: must be the identical ErrUnauthorized", e1, e2)
	}
}

func TestCacheHitAvoidsDatabase(t *testing.T) {
	l := newLookup()
	l.set(tok(1), rec("id-1", "a"))
	a := newAuth(l, newClock())
	for i := 0; i < 100; i++ {
		id, err := a.Authenticate(context.Background(), tok(1))
		if err != nil || id.CrawlerID != "id-1" || id.Name != "a" {
			t.Fatalf("Authenticate = %+v, %v", id, err)
		}
	}
	if n := l.calls.Load(); n != 1 {
		t.Fatalf("%d lookups for 100 requests, want 1", n)
	}
	st := a.Stats()
	if st.Hits != 99 || st.Misses != 1 || st.DBLookups != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestCacheEntryExpiresAfterExactlySixtySecondsAndIsRevalidated(t *testing.T) {
	l := newLookup()
	l.set(tok(1), rec("id-1", "a"))
	c := newClock()
	a := newAuth(l, c)
	mustAuth(t, a, tok(1))
	// Use does not extend the entry: touch it every 10 s.
	for i := 0; i < 5; i++ {
		c.Advance(10 * time.Second)
		mustAuth(t, a, tok(1))
	}
	c.Advance(9*time.Second + 999*time.Millisecond) // 59.999 s after the fetch
	mustAuth(t, a, tok(1))
	if l.calls.Load() != 1 {
		t.Fatalf("lookups = %d before the TTL, want 1", l.calls.Load())
	}
	c.Advance(time.Millisecond) // exactly 60 s
	mustAuth(t, a, tok(1))
	if l.calls.Load() != 2 {
		t.Fatalf("lookups = %d at exactly the TTL, want 2 (revalidated)", l.calls.Load())
	}
}

func mustAuth(t *testing.T, a *Authenticator, token string) Identity {
	t.Helper()
	id, err := a.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	return id
}

func TestDatabaseFailureNeverExtendsAnExpiredEntry(t *testing.T) {
	l := newLookup()
	l.set(tok(1), rec("id-1", "a"))
	c := newClock()
	a := newAuth(l, c)
	mustAuth(t, a, tok(1))
	c.Advance(61 * time.Second)
	l.fail(errors.New("connection refused"))
	_, err := a.Authenticate(context.Background(), tok(1))
	var un ErrUnavailable
	if !errors.As(err, &un) || un.Busy {
		t.Fatalf("expired entry + database failure: %v, want ErrUnavailable (not 401, not served from cache)", err)
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatal("a storage failure must never look like a 401")
	}
}

func TestStorageFailureIsUnavailableNeverUnauthorized(t *testing.T) {
	l := newLookup()
	l.fail(errors.New("boom"))
	a := newAuth(l, newClock())
	_, err := a.Authenticate(context.Background(), tok(1))
	var un ErrUnavailable
	if !errors.As(err, &un) || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
	if a.Stats().StorageFailures != 1 {
		t.Fatalf("stats = %+v", a.Stats())
	}
}

func TestRevokedDisabledAndRotatedCredentialsWorkAtMostUntilTTL(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(l *fakeLookup)
	}{
		{"disabled", func(l *fakeLookup) { l.set(tok(1), crawlers.Record{ID: "id-1", Name: "a", Enabled: false}) }},
		{"deleted", func(l *fakeLookup) { l.del(tok(1)) }},
		{"rotated", func(l *fakeLookup) { l.del(tok(1)); l.set(tok(2), rec("id-1", "a")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLookup()
			l.set(tok(1), rec("id-1", "a"))
			c := newClock()
			a := newAuth(l, c)
			mustAuth(t, a, tok(1))
			tc.revoke(l)
			c.Advance(30 * time.Second)
			mustAuth(t, a, tok(1)) // still authenticates inside the TTL (documented)
			c.Advance(30 * time.Second)
			if _, err := a.Authenticate(context.Background(), tok(1)); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("after the TTL: %v, want ErrUnauthorized", err)
			}
		})
	}
	// A new credential works at once: its digest is not cached.
	l := newLookup()
	a := newAuth(l, newClock())
	if _, err := a.Authenticate(context.Background(), tok(9)); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	l.set(tok(9), rec("id-9", "n"))
	if id := mustAuth(t, a, tok(9)); id.CrawlerID != "id-9" {
		t.Fatalf("new credential: %+v", id)
	}
}

func TestOnlyEnabledCrawlersAreCached(t *testing.T) {
	l := newLookup()
	l.set(tok(1), crawlers.Record{ID: "id-1", Name: "a", Enabled: false})
	a := newAuth(l, newClock())
	for i := 0; i < 3; i++ {
		_, _ = a.Authenticate(context.Background(), tok(1))
	}
	if l.calls.Load() != 3 {
		t.Fatalf("a disabled/unknown credential was cached (%d lookups)", l.calls.Load())
	}
}

func TestCoalescesConcurrentLookupsOfOneDigest(t *testing.T) {
	l := newLookup()
	l.set(tok(1), rec("id-1", "a"))
	release := make(chan struct{})
	l.hook = func(ctx context.Context) error { <-release; return nil }
	a := newAuth(l, newClock())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, err := a.Authenticate(context.Background(), tok(1)); err != nil || id.CrawlerID != "id-1" {
				t.Errorf("%+v %v", id, err)
			}
		}()
	}
	waitFor(t, func() bool { _, w, _ := a.stateSizes(); return w == 50 })
	close(release)
	wg.Wait()
	if n := l.calls.Load(); n != 1 {
		t.Fatalf("%d lookups for 50 concurrent requests of one digest, want 1", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in 5 s")
		}
		time.Sleep(time.Millisecond)
	}
}

func isBusy(err error) bool {
	var un ErrUnavailable
	return errors.As(err, &un) && un.Busy
}

func TestInvalidTokenFloodHoldsAtMostEightLookups(t *testing.T) {
	l := newLookup()
	release := make(chan struct{})
	l.hook = func(ctx context.Context) error { <-release; return nil }
	a := newAuth(l, newClock())
	a.slotWait = 20 * time.Millisecond
	var wg sync.WaitGroup
	var busy, unauthorized atomic.Int64
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := a.Authenticate(context.Background(), tok(1000+i))
			switch {
			case isBusy(err):
				busy.Add(1)
			case errors.Is(err, ErrUnauthorized):
				unauthorized.Add(1)
			default:
				t.Errorf("unexpected %v", err)
			}
		}(i)
	}
	waitFor(t, func() bool { return busy.Load() >= 150 })
	if m := l.maxActive.Load(); m > MaxActiveLookups {
		t.Fatalf("%d concurrent lookups, bound is %d", m, MaxActiveLookups)
	}
	close(release)
	wg.Wait()
	if m := l.maxActive.Load(); m > MaxActiveLookups {
		t.Fatalf("%d concurrent lookups after release", m)
	}
	if busy.Load()+unauthorized.Load() != 200 {
		t.Fatalf("busy=%d unauthorized=%d", busy.Load(), unauthorized.Load())
	}
}

func TestOutstandingLookupStateIsBounded(t *testing.T) {
	l := newLookup()
	release := make(chan struct{})
	l.hook = func(ctx context.Context) error { <-release; return nil }
	a := newAuth(l, newClock())
	a.slotWait = 5 * time.Second // keep digests waiting for slots so the maps fill
	const n = 10000
	var wg sync.WaitGroup
	var busy atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := a.Authenticate(context.Background(), tok(i)); isBusy(err) {
				busy.Add(1)
			}
		}(i)
	}
	waitFor(t, func() bool { return busy.Load() >= n-MaxPendingDigests })
	calls, waiters, active := a.stateSizes()
	if calls > MaxPendingDigests || waiters > MaxWaiters || active > MaxActiveLookups {
		t.Fatalf("calls=%d waiters=%d active=%d exceed the bounds %d/%d/%d", calls, waiters, active,
			MaxPendingDigests, MaxWaiters, MaxActiveLookups)
	}
	if calls != MaxPendingDigests {
		t.Fatalf("calls=%d, want exactly %d pending digests", calls, MaxPendingDigests)
	}
	close(release)
	wg.Wait()
	if calls, waiters, active = a.stateSizes(); calls != 0 || waiters != 0 || active != 0 {
		t.Fatalf("state not released: calls=%d waiters=%d active=%d", calls, waiters, active)
	}
}

func TestWaitersOfOneDigestAreBoundedAndTheRestGetBusy(t *testing.T) {
	l := newLookup()
	l.set(tok(1), rec("id-1", "a"))
	release := make(chan struct{})
	l.hook = func(ctx context.Context) error { <-release; return nil }
	a := newAuth(l, newClock())
	var wg sync.WaitGroup
	var busy, ok atomic.Int64
	for i := 0; i < MaxWaiters+500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Authenticate(context.Background(), tok(1)); isBusy(err) {
				busy.Add(1)
			} else if err == nil {
				ok.Add(1)
			}
		}()
	}
	waitFor(t, func() bool { return busy.Load() >= 500 })
	if _, w, _ := a.stateSizes(); w > MaxWaiters {
		t.Fatalf("%d waiters, bound %d", w, MaxWaiters)
	}
	close(release)
	wg.Wait()
	if ok.Load() != MaxWaiters || busy.Load() != 500 {
		t.Fatalf("ok=%d busy=%d, want %d and 500", ok.Load(), busy.Load(), MaxWaiters)
	}
}

func TestPendingDigestWaitsAtMostSlotWaitThenBusyAndLeavesNoState(t *testing.T) {
	l := newLookup()
	release := make(chan struct{})
	l.hook = func(ctx context.Context) error { <-release; return nil }
	a := newAuth(l, newClock())
	a.slotWait = 50 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < MaxActiveLookups; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, _ = a.Authenticate(context.Background(), tok(i)) }(i)
	}
	waitFor(t, func() bool { _, _, act := a.stateSizes(); return act == MaxActiveLookups })
	start := time.Now()
	_, err := a.Authenticate(context.Background(), tok(500))
	if !isBusy(err) {
		t.Fatalf("got %v, want busy", err)
	}
	if d := time.Since(start); d < 40*time.Millisecond || d > 2*time.Second {
		t.Fatalf("waited %v for a slot, want about 50 ms", d)
	}
	if calls, _, _ := a.stateSizes(); calls != MaxActiveLookups {
		t.Fatalf("the abandoned digest left a call record (%d)", calls)
	}
	close(release)
	wg.Wait()
}

func TestEveryLookupHasADeadline(t *testing.T) {
	l := newLookup()
	var sawDeadline atomic.Bool
	l.hook = func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); ok {
			sawDeadline.Store(true)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	a := newAuth(l, newClock())
	a.lookupDeadline = 80 * time.Millisecond
	start := time.Now()
	_, err := a.Authenticate(context.Background(), tok(1))
	var un ErrUnavailable
	if !errors.As(err, &un) || un.Busy {
		t.Fatalf("got %v, want ErrUnavailable (timeout)", err)
	}
	if !sawDeadline.Load() || time.Since(start) > 3*time.Second {
		t.Fatalf("deadline seen=%v elapsed=%v", sawDeadline.Load(), time.Since(start))
	}
	if DefaultLookupDeadline != 2*time.Second || DefaultSlotWait != 250*time.Millisecond {
		t.Fatal("the documented constants changed")
	}
}

func TestLookupSurvivesTheDepartureOfItsWaitersAndEndsWithinItsDeadline(t *testing.T) {
	l := newLookup()
	l.hook = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	a := newAuth(l, newClock())
	a.lookupDeadline = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := a.Authenticate(ctx, tok(1)); done <- err }()
	waitFor(t, func() bool { c, _, _ := a.stateSizes(); return c == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	waitFor(t, func() bool { c, w, act := a.stateSizes(); return c == 0 && w == 0 && act == 0 })
}

func TestEveryExitPathReleasesItsReservations(t *testing.T) {
	cases := map[string]func(l *fakeLookup, c *fakeClock){
		"success": func(l *fakeLookup, c *fakeClock) { l.set(tok(1), rec("id", "n")) },
		"unknown": func(l *fakeLookup, c *fakeClock) {},
		"error":   func(l *fakeLookup, c *fakeClock) { l.fail(errors.New("x")) },
		"panic": func(l *fakeLookup, c *fakeClock) {
			l.hook = func(ctx context.Context) error { panic("lookup exploded") }
		},
		"deadline": func(l *fakeLookup, c *fakeClock) {
			l.hook = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			l := newLookup()
			c := newClock()
			setup(l, c)
			a := newAuth(l, c)
			a.lookupDeadline = 50 * time.Millisecond
			_, _ = a.Authenticate(context.Background(), tok(1))
			if calls, waiters, active := a.stateSizes(); calls != 0 || waiters != 0 || active != 0 {
				t.Fatalf("leaked state: calls=%d waiters=%d active=%d", calls, waiters, active)
			}
		})
	}
	// The panic case answers ErrUnavailable and the authenticator keeps working.
	l := newLookup()
	l.hook = func(ctx context.Context) error { panic("boom") }
	a := newAuth(l, newClock())
	if _, err := a.Authenticate(context.Background(), tok(1)); err == nil || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("panic path: %v", err)
	}
	l.hook = nil
	l.set(tok(2), rec("id-2", "n"))
	mustAuth(t, a, tok(2))
}

func TestEntryLifeIsAnchoredAtTheStartOfTheLookup(t *testing.T) {
	l := newLookup()
	l.set(tok(1), rec("id-1", "a"))
	c := newClock()
	l.hook = func(ctx context.Context) error { c.Advance(1900 * time.Millisecond); return nil }
	a := newAuth(l, c)
	mustAuth(t, a, tok(1)) // fetched at T0, answered at T0+1.9 s
	l.hook = nil
	c.Advance(58*time.Second + 99*time.Millisecond) // T0+59.999 s
	mustAuth(t, a, tok(1))
	if l.calls.Load() != 1 {
		t.Fatal("entry expired too early")
	}
	c.Advance(time.Millisecond) // T0+60 s: gone although only 58.1 s after the answer
	mustAuth(t, a, tok(1))
	if l.calls.Load() != 2 {
		t.Fatalf("entry outlived startedAt+60s (lookups=%d)", l.calls.Load())
	}
}

func TestSlowLookupResponseDoesNotExtendTheRevocationWindow(t *testing.T) {
	l := newLookup()
	l.set(tok(1), rec("id-1", "a"))
	c := newClock()
	start := c.Now()
	// The database is read at the start and the crawler is revoked right after
	// the read; the cache may honour the credential for less than 60 s after it.
	a := newAuth(l, c)
	mustAuth(t, a, tok(1))
	l.del(tok(1)) // revoked at start+0
	c.Advance(59*time.Second + 999*time.Millisecond)
	mustAuth(t, a, tok(1)) // honoured from the cache for < 60 s
	c.Advance(time.Millisecond)
	if _, err := a.Authenticate(context.Background(), tok(1)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a revoked credential was accepted %v after the read: %v", c.Now().Sub(start), err)
	}
}

func TestResponseArrivingAfterItsExpiryIsNotCached(t *testing.T) {
	l := newLookup()
	l.set(tok(1), rec("id-1", "a"))
	c := newClock()
	l.hook = func(ctx context.Context) error { c.Advance(61 * time.Second); return nil }
	a := newAuth(l, c)
	mustAuth(t, a, tok(1)) // the waiter is still answered
	l.hook = nil
	mustAuth(t, a, tok(1))
	if l.calls.Load() != 2 {
		t.Fatalf("an already expired answer was cached (lookups=%d)", l.calls.Load())
	}
}

func TestJoinedWaiterIsAnsweredFromAReadNoOlderThanTheLookupDeadline(t *testing.T) {
	l := newLookup()
	l.hook = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	a := newAuth(l, newClock())
	a.lookupDeadline = 150 * time.Millisecond
	first := make(chan time.Duration, 1)
	start := time.Now()
	go func() { _, _ = a.Authenticate(context.Background(), tok(1)); first <- time.Since(start) }()
	waitFor(t, func() bool { c, _, _ := a.stateSizes(); return c == 1 })
	time.Sleep(60 * time.Millisecond)
	joinedAt := time.Now()
	_, err := a.Authenticate(context.Background(), tok(1))
	var un ErrUnavailable
	if !errors.As(err, &un) {
		t.Fatalf("joiner: %v", err)
	}
	if d := time.Since(joinedAt); d > 150*time.Millisecond+a.slotWait+time.Second {
		t.Fatalf("a joiner waited %v, beyond the lookup deadline", d)
	}
	<-first
}

func TestCacheIsBoundedAndEvictsOldest(t *testing.T) {
	l := newLookup()
	c := newClock()
	a := newAuth(l, c)
	for i := 0; i < MaxCacheEntries+50; i++ {
		l.set(tok(i), rec(fmt.Sprintf("id-%d", i), "n"))
		mustAuth(t, a, tok(i))
		c.Advance(time.Millisecond)
	}
	if n := a.cacheLen(); n != MaxCacheEntries {
		t.Fatalf("cache holds %d entries, want exactly %d", n, MaxCacheEntries)
	}
	before := l.calls.Load()
	mustAuth(t, a, tok(MaxCacheEntries+49)) // newest: cached
	if l.calls.Load() != before {
		t.Fatal("the newest entry was evicted")
	}
	mustAuth(t, a, tok(0)) // oldest: evicted, looked up again
	if l.calls.Load() != before+1 {
		t.Fatal("the oldest entry was not evicted")
	}
}

func TestNoPlaintextInCacheLogsOrErrors(t *testing.T) {
	var logs bytes.Buffer
	l := newLookup()
	secret := tok(777)
	l.set(secret, rec("id-7", "seven"))
	a := New(l, Options{Clock: newClock(), Logger: log.New(&logs, "", 0)})
	mustAuth(t, a, secret)
	l.del(secret)
	l.fail(errors.New("database says: " + "driver detail"))
	_, err := a.Authenticate(context.Background(), tok(778))
	for name, s := range map[string]string{
		"cache": fmt.Sprintf("%#v", reflect.ValueOf(&a.cache).Elem().Interface()),
		"logs":  logs.String(),
		"error": fmt.Sprint(err),
	} {
		if strings.Contains(s, secret) || strings.Contains(s, strings.TrimPrefix(secret, "mgc_")) {
			t.Errorf("%s contains the plaintext token", name)
		}
	}
	if strings.Contains(fmt.Sprint(err), "driver detail") {
		t.Errorf("driver text leaks into the error: %v", err)
	}
}

func TestConcurrentAuthentication(t *testing.T) {
	l := newLookup()
	for i := 0; i < 20; i++ {
		l.set(tok(i), rec(fmt.Sprintf("id-%d", i), fmt.Sprintf("n%d", i)))
	}
	c := newClock()
	a := newAuth(l, c)
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := (g + i) % 25
				id, err := a.Authenticate(context.Background(), tok(k))
				if k < 20 {
					if err != nil || id.CrawlerID != fmt.Sprintf("id-%d", k) {
						t.Errorf("k=%d: %+v %v", k, id, err)
					}
				} else if !errors.Is(err, ErrUnauthorized) {
					t.Errorf("k=%d: %v", k, err)
				}
				if i%50 == 0 {
					c.Advance(time.Second)
				}
			}
		}(g)
	}
	wg.Wait()
}
