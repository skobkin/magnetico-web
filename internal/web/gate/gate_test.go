package gate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustGate(t *testing.T, c int64, w int, wait time.Duration) *Gate {
	t.Helper()
	g, err := New(c, w, wait)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestNewValidates(t *testing.T) {
	for _, c := range []struct {
		c int64
		w int
		d time.Duration
	}{{0, 1, time.Second}, {1, -1, time.Second}, {1, 1, 0}} {
		if _, err := New(c.c, c.w, c.d); err == nil {
			t.Errorf("New(%v) accepted", c)
		}
	}
}

func TestAcquireReleaseAndIdempotentRelease(t *testing.T) {
	g := mustGate(t, 2, 0, time.Second)
	r1, err := g.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := g.Acquire(context.Background(), 1)
	if g.InUse() != 2 {
		t.Fatalf("InUse = %d", g.InUse())
	}
	if _, err := g.Acquire(context.Background(), 1); !errors.Is(err, ErrBusy) {
		t.Fatalf("full gate with no queue: %v", err)
	}
	r1()
	r1() // idempotent
	if g.InUse() != 1 {
		t.Fatalf("InUse after double release = %d", g.InUse())
	}
	r2()
	if g.InUse() != 0 {
		t.Fatal("not drained")
	}
}

func TestTooHeavyIsRefusedNotClamped(t *testing.T) {
	g := mustGate(t, 10, 1, time.Second)
	if _, err := g.Acquire(context.Background(), 11); !errors.Is(err, ErrTooHeavy) {
		t.Fatalf("got %v", err)
	}
	if r, err := g.Acquire(context.Background(), 10); err != nil {
		t.Fatalf("weight == capacity must be admissible: %v", err)
	} else {
		r()
	}
}

func TestWaitExpiresWithBusy(t *testing.T) {
	g := mustGate(t, 1, 4, 50*time.Millisecond)
	r, _ := g.Acquire(context.Background(), 1)
	defer r()
	start := time.Now()
	_, err := g.Acquire(context.Background(), 1)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d < 40*time.Millisecond || d > 2*time.Second {
		t.Fatalf("waited %v", d)
	}
	if g.Waiting() != 0 {
		t.Fatal("expired waiter left in the queue")
	}
}

func TestQueueFullFailsFast(t *testing.T) {
	g := mustGate(t, 1, 1, 5*time.Second)
	r, _ := g.Acquire(context.Background(), 1)
	defer r()
	done := make(chan error, 1)
	go func() { _, err := g.Acquire(context.Background(), 1); done <- err }()
	for g.Waiting() != 1 {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	if _, err := g.Acquire(context.Background(), 1); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("queue-full refusal waited")
	}
	r()
	if err := <-done; err != nil {
		t.Fatalf("queued waiter: %v", err)
	}
}

func TestContextCancelWhileWaiting(t *testing.T) {
	g := mustGate(t, 1, 4, 5*time.Second)
	r, _ := g.Acquire(context.Background(), 1)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := g.Acquire(ctx, 1); errc <- err }()
	for g.Waiting() != 1 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if g.Waiting() != 0 {
		t.Fatal("cancelled waiter left in the queue")
	}
	r()
	if g.InUse() != 0 {
		t.Fatal("weight leaked")
	}
	if _, err := g.Acquire(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("already-cancelled context admitted: %v", err)
	}
}

func TestFIFOAndHeavyWaiterNotStarved(t *testing.T) {
	g := mustGate(t, 10, 8, 5*time.Second)
	r1, _ := g.Acquire(context.Background(), 6)
	heavy := make(chan func(), 1)
	light := make(chan func(), 1)
	go func() { r, _ := g.Acquire(context.Background(), 8); heavy <- r }() // cannot fit next to 6
	for g.Waiting() != 1 {
		time.Sleep(time.Millisecond)
	}
	go func() { r, _ := g.Acquire(context.Background(), 3); light <- r }() // would fit (6+3), must not overtake
	for g.Waiting() != 2 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if g.InUse() != 6 || len(light) != 0 {
		t.Fatalf("a light request overtook the heavy head: in use %d", g.InUse())
	}
	r1()
	rh := <-heavy
	if g.InUse() < 8 {
		t.Fatalf("heavy not admitted first: %d", g.InUse())
	}
	rh()
	(<-light)()
	if g.InUse() != 0 {
		t.Fatal("weight leaked")
	}
}

func TestNeverOverCommitsUnderRace(t *testing.T) {
	const capacity = 5
	g := mustGate(t, capacity, 1000, 5*time.Second)
	var cur, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := int64(i%3 + 1)
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%7+1)*20*time.Millisecond)
			defer cancel()
			r, err := g.Acquire(ctx, w)
			if err != nil {
				return
			}
			n := cur.Add(w)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			cur.Add(-w)
			r()
		}(i)
	}
	wg.Wait()
	if peak.Load() > capacity {
		t.Fatalf("peak %d exceeded capacity %d", peak.Load(), capacity)
	}
	if g.InUse() != 0 || g.Waiting() != 0 {
		t.Fatalf("leak: in use %d, waiting %d", g.InUse(), g.Waiting())
	}
}
