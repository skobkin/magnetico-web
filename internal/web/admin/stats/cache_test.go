package stats

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pair is a value with two fields that must always be observed together: a
// reader that sees one field of a refresh and the other of an older value has
// seen a torn entry.
type pair struct{ n, twice int }

// TestCacheEntriesAreImmutableAfterPublication (Task 09A): a failed load used to
// return the entry it had looked at before waiting, while a later successful
// load updated that very entry in place, so the value and its fetch time could
// be read while being written. Run under -race; every value must also be
// internally consistent and every time must belong to the value it came with.
func TestCacheEntriesAreImmutableAfterPublication(t *testing.T) {
	var now atomic.Int64
	clock := func() time.Time { return time.Unix(0, now.Load()) }
	c := newCache[string, pair](time.Millisecond, 4, clock)
	var seq atomic.Int64
	load := func(ctx context.Context) (pair, error) {
		n := seq.Add(1)
		if n%3 == 0 {
			return pair{}, errors.New("database down")
		}
		return pair{int(n), 2 * int(n)}, nil
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				now.Add(int64(2 * time.Millisecond)) // every call finds the entry expired
				v, at, _, err := c.get(bg, "k", load)
				if err != nil {
					continue // no last known good yet
				}
				if v.twice != 2*v.n {
					t.Errorf("torn value %+v", v)
					return
				}
				if at.IsZero() {
					t.Errorf("a served value has no fetch time")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestCacheReturnsTheFetchTimeOfTheValueItServes pins the pair (value, fetchedAt)
// for the last-known-good path after a newer success has replaced the entry.
func TestCacheReturnsTheFetchTimeOfTheValueItServes(t *testing.T) {
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := newCache[string, int](10*time.Second, 4, clk.now)
	_, at1, _, _ := c.get(bg, "k", func(context.Context) (int, error) { return 1, nil })
	clk.advance(11 * time.Second)
	v, at2, stale, err := c.get(bg, "k", func(context.Context) (int, error) { return 2, nil })
	if err != nil || v != 2 || stale || !at2.After(at1) {
		t.Fatalf("refresh: %d %v %v %v (first fetch %v)", v, at2, stale, err, at1)
	}
	clk.advance(11 * time.Second)
	v, at3, stale, err := c.get(bg, "k", func(context.Context) (int, error) { return 0, errors.New("down") })
	if err != nil || v != 2 || !stale || !at3.Equal(at2) {
		t.Fatalf("after a failed refresh the value 2 must come with ITS fetch time %v: got %d %v stale=%v err=%v", at2, v, at3, stale, err)
	}
}

// TestCacheBoundsConcurrentLoadsForDistinctKeys: coalescing bounds loads per
// key; the number of loads running at once across keys is bounded too.
func TestCacheBoundsConcurrentLoadsForDistinctKeys(t *testing.T) {
	c := newCache[int, int](time.Hour, 64, time.Now)
	c.maxLoads, c.maxPending = 3, 64
	var running, peak atomic.Int64
	release := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer running.Add(-1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return 1, nil
	}
	var wg sync.WaitGroup
	for k := 0; k < 20; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, _, err := c.get(bg, k, load); err != nil {
				t.Errorf("key %d: %v", k, err)
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for running.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // give an over-eager implementation the chance to exceed the bound
	if got := running.Load(); got != 3 {
		t.Fatalf("%d loads running at once, want exactly the bound 3", got)
	}
	close(release)
	wg.Wait()
	if peak.Load() > 3 {
		t.Fatalf("peak concurrent loads %d exceeded the bound 3", peak.Load())
	}
}

// TestCachePendingLoadsAreBoundedAndAnswerBusy: beyond the number of loads
// that may exist at all (running or waiting), a caller without a last known good
// gets ErrBusy at once and a caller with one gets it marked stale.
func TestCachePendingLoadsAreBoundedAndAnswerBusy(t *testing.T) {
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := newCache[int, int](time.Second, 64, clk.now)
	c.maxLoads, c.maxPending = 1, 2
	// key 100 has a value that will be expired.
	if _, _, _, err := c.get(bg, 100, func(context.Context) (int, error) { return 7, nil }); err != nil {
		t.Fatal(err)
	}
	clk.advance(2 * time.Second)

	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(1)
	blocking := func(ctx context.Context) (int, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return 1, nil
	}
	var wg sync.WaitGroup
	for k := 1; k <= 2; k++ { // fill the pending bound
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _, _ = c.get(bg, k, blocking) }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.pending() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.pending() != 2 {
		t.Fatalf("pending = %d, want 2", c.pending())
	}
	if _, _, _, err := c.get(bg, 3, blocking); !errors.Is(err, ErrBusy) {
		t.Fatalf("a new key beyond the bound: err = %v, want ErrBusy", err)
	}
	v, _, stale, err := c.get(bg, 100, blocking)
	if err != nil || v != 7 || !stale {
		t.Fatalf("an expired key beyond the bound must be served stale: %d %v %v", v, stale, err)
	}
	close(release)
	wg.Wait()
	if c.pending() != 0 {
		t.Fatalf("pending = %d after every load ended, want 0", c.pending())
	}
}

// TestCacheCancelledCallerDoesNotCancelTheSharedLoad keeps the existing
// contract: the load runs detached from any one caller and a second caller
// still receives the value.
func TestCacheCancelledCallerDoesNotCancelTheSharedLoad(t *testing.T) {
	c := newCache[string, int](time.Hour, 4, time.Now)
	release := make(chan struct{})
	started := make(chan struct{})
	load := func(ctx context.Context) (int, error) {
		close(started)
		select {
		case <-release:
			return 5, ctx.Err()
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(bg)
	errc := make(chan error, 1)
	go func() { _, _, _, err := c.get(ctx, "k", load); errc <- err }()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled caller got %v", err)
	}
	got := make(chan int, 1)
	go func() {
		v, _, _, _ := c.get(bg, "k", load)
		got <- v
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if v := <-got; v != 5 {
		t.Fatalf("the second caller got %d, want 5 from the same load", v)
	}
}

// TestCacheLoadDeadlineCoversTheWaitForASlot: a load waiting for a slot is
// bounded by the same deadline as a running one; it ends with an error, its
// pending place is released and it never runs afterwards.
func TestCacheLoadDeadlineCoversTheWaitForASlot(t *testing.T) {
	c := newCache[int, int](time.Hour, 8, time.Now)
	c.maxLoads, c.maxPending = 1, 8
	c.loadTimeout = 100 * time.Millisecond
	hold := make(chan struct{})
	go func() {
		_, _, _, _ = c.get(bg, 1, func(ctx context.Context) (int, error) { <-hold; return 1, nil })
	}()
	for c.pending() < 1 {
		time.Sleep(time.Millisecond)
	}
	var ran atomic.Bool
	_, _, _, err := c.get(bg, 2, func(context.Context) (int, error) { ran.Store(true); return 2, nil })
	if err == nil {
		t.Fatal("a load that never got a slot must fail when its deadline passes")
	}
	close(hold)
	time.Sleep(50 * time.Millisecond)
	if ran.Load() {
		t.Fatal("a load whose deadline passed while waiting for a slot ran afterwards")
	}
}
