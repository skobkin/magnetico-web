package crawler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/crawlqueue"
)

// --- the linger decision, with explicit wall and monotonic times -----------

func plan(count int, stop crawlqueue.StopReason, head int64, enqueued time.Time) crawlqueue.Plan {
	return crawlqueue.Plan{Count: count, Stop: stop, HeadID: head, LastID: head + int64(count) - 1, HeadEnqueuedAt: enqueued}
}

func TestSenderWaitsForLingerFromOldestRecord(t *testing.T) {
	wall := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	mono := time.Now()
	var l lingerState
	// The oldest record is 400 ms old: 600 ms of the 1 s linger are left.
	dispatch, wait := l.decide(plan(3, crawlqueue.StopNone, 1, wall.Add(-400*time.Millisecond)), time.Second, wall, mono)
	if dispatch || wait != 600*time.Millisecond {
		t.Fatalf("dispatch=%v wait=%v, want to wait 600ms", dispatch, wait)
	}
	// 600 ms later (monotonic) it is time, whatever the record count.
	dispatch, _ = l.decide(plan(5, crawlqueue.StopNone, 1, wall.Add(-400*time.Millisecond)), time.Second, wall.Add(600*time.Millisecond), mono.Add(600*time.Millisecond))
	if !dispatch {
		t.Fatal("not dispatched when the linger of the oldest record expired")
	}
	// A record already older than the linger (queued before a restart) goes at once.
	var l2 lingerState
	if dispatch, _ = l2.decide(plan(1, crawlqueue.StopNone, 9, wall.Add(-time.Hour)), time.Second, wall, mono); !dispatch {
		t.Fatal("an old record was made to wait")
	}
}

func TestSenderDispatchesAtOnceWhenTheNextRecordWouldNotFitOrTheBatchIsFull(t *testing.T) {
	wall, mono := time.Now(), time.Now()
	for _, stop := range []crawlqueue.StopReason{crawlqueue.StopBytes, crawlqueue.StopCount, crawlqueue.StopOversizeHead} {
		var l lingerState
		// A brand-new record: no linger applies once the batch cannot grow.
		if dispatch, wait := l.decide(plan(1, stop, 1, wall), time.Second, wall, mono); !dispatch || wait != 0 {
			t.Errorf("stop %v: dispatch=%v wait=%v, want an immediate dispatch", stop, dispatch, wait)
		}
	}
}

func TestSenderNeverSendsAnEmptyBatch(t *testing.T) {
	var l lingerState
	if dispatch, _ := l.decide(crawlqueue.Plan{}, time.Second, time.Now(), time.Now()); dispatch {
		t.Fatal("an empty plan was dispatched")
	}
}

func TestBackwardWallClockStepNeverExtendsTheLinger(t *testing.T) {
	wall := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	mono := time.Now()
	enq := wall // enqueued "now"
	var l lingerState
	if _, wait := l.decide(plan(1, crawlqueue.StopNone, 1, enq), time.Second, wall, mono); wait != time.Second {
		t.Fatalf("initial wait %v", wait)
	}
	// The wall clock jumps back an hour while waiting: the monotonic deadline stands.
	back := wall.Add(-time.Hour)
	dispatch, wait := l.decide(plan(1, crawlqueue.StopNone, 1, enq), time.Second, back, mono.Add(400*time.Millisecond))
	if dispatch || wait != 600*time.Millisecond {
		t.Fatalf("after a backward step: dispatch=%v wait=%v, want the remaining 600ms", dispatch, wait)
	}
	if dispatch, _ = l.decide(plan(1, crawlqueue.StopNone, 1, enq), time.Second, back, mono.Add(time.Second)); !dispatch {
		t.Fatal("the linger outlived SendLinger after a backward clock step")
	}
	// A restart with the clock behind EnqueuedAt: the same bound from the moment of observation.
	var l2 lingerState
	if _, wait := l2.decide(plan(1, crawlqueue.StopNone, 2, wall.Add(time.Hour)), time.Second, wall, mono); wait != time.Second {
		t.Fatalf("an EnqueuedAt an hour in the future waits %v, want exactly the linger", wait)
	}
}

func TestFutureEnqueuedAtIsTreatedAsNow(t *testing.T) {
	wall, mono := time.Now(), time.Now()
	var l lingerState
	_, wait := l.decide(plan(1, crawlqueue.StopNone, 1, wall.Add(24*time.Hour)), 500*time.Millisecond, wall, mono)
	if wait != 500*time.Millisecond {
		t.Fatalf("wait = %v, want 500ms", wait)
	}
}

func TestForwardWallClockStepDispatchesNoLaterThanTheLinger(t *testing.T) {
	wall, mono := time.Now(), time.Now()
	var l lingerState
	if _, wait := l.decide(plan(1, crawlqueue.StopNone, 1, wall), time.Second, wall, mono); wait != time.Second {
		t.Fatalf("wait %v", wait)
	}
	// A forward step of an hour: the monotonic deadline is unchanged (not extended, not shortened).
	dispatch, wait := l.decide(plan(1, crawlqueue.StopNone, 1, wall), time.Second, wall.Add(time.Hour), mono.Add(100*time.Millisecond))
	if dispatch || wait != 900*time.Millisecond {
		t.Fatalf("dispatch=%v wait=%v", dispatch, wait)
	}
	// A new head observed after the step is older than the linger by the wall clock: at once.
	var l2 lingerState
	if dispatch, _ = l2.decide(plan(1, crawlqueue.StopNone, 7, wall), time.Second, wall.Add(time.Hour), mono); !dispatch {
		t.Fatal("a record that is an hour old by the wall clock was made to wait")
	}
}

func TestLingerIsArmedOncePerHeadRecord(t *testing.T) {
	wall, mono := time.Now(), time.Now()
	var l lingerState
	_, w1 := l.decide(plan(1, crawlqueue.StopNone, 5, wall), time.Second, wall, mono)
	// More records arrive behind the same head: the deadline is not re-extended.
	_, w2 := l.decide(plan(40, crawlqueue.StopNone, 5, wall), time.Second, wall, mono.Add(300*time.Millisecond))
	if w1 != time.Second || w2 != 700*time.Millisecond {
		t.Fatalf("waits %v then %v", w1, w2)
	}
	// After the head was acknowledged, a new head arms a new linger.
	_, w3 := l.decide(plan(1, crawlqueue.StopNone, 46, wall), time.Second, wall, mono.Add(time.Second))
	if w3 != time.Second {
		t.Fatalf("new head waits %v, want a fresh linger", w3)
	}
}

// --- Run: real timers, a counting submitter ----------------------------------

type countingSubmitter struct {
	mu       sync.Mutex
	calls    []submitCall
	inFlight atomic.Int32
	maxIn    atomic.Int32
	results  []SubmitResult // consumed in order; then SubmitAccepted
	delay    time.Duration
}

type submitCall struct {
	at      time.Time
	records int
}

func (c *countingSubmitter) Submit(_ context.Context, payloads [][]byte) SubmitResult {
	n := c.inFlight.Add(1)
	defer c.inFlight.Add(-1)
	for {
		m := c.maxIn.Load()
		if n <= m || c.maxIn.CompareAndSwap(m, n) {
			break
		}
	}
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, submitCall{at: time.Now(), records: len(payloads)})
	if len(c.results) > 0 {
		r := c.results[0]
		c.results = c.results[1:]
		return r
	}
	return SubmitResult{Kind: SubmitAccepted, Status: 202}
}

func (c *countingSubmitter) snapshot() []submitCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]submitCall(nil), c.calls...)
}

type runRig struct {
	q      *crawlqueue.Queue
	sub    *countingSubmitter
	sender *Sender
	cancel context.CancelFunc
	done   chan struct{}
}

func startRun(t *testing.T, linger time.Duration, sub *countingSubmitter) *runRig {
	t.Helper()
	q, err := crawlqueue.Open(crawlqueue.Options{Dir: t.TempDir(), Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	s := NewSender(q, sub, &Counters{}, func(string, ...any) {})
	s.Linger = linger
	s.Sleep = func(ctx context.Context, d time.Duration) bool { // backoff: short, still cancellable
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Millisecond):
			return true
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &runRig{q: q, sub: sub, sender: s, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(r.done); s.Run(ctx, 2*time.Second) }()
	t.Cleanup(func() { cancel(); <-r.done })
	return r
}

func (r *runRig) add(t *testing.T, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		if ok, err := r.q.Append(bg, hashN(i), 1, goodPayload(t, i)); err != nil || !ok {
			t.Fatalf("append %d: %v %v", i, ok, err)
		}
	}
	r.sender.Wake()
}

func waitUntil(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWakeDoesNotSendImmediately(t *testing.T) {
	r := startRun(t, 400*time.Millisecond, &countingSubmitter{})
	start := time.Now()
	r.add(t, 1, 1)
	time.Sleep(150 * time.Millisecond)
	if n := len(r.sub.snapshot()); n != 0 {
		t.Fatalf("%d request(s) 150 ms after an append with a 400 ms linger", n)
	}
	waitUntil(t, "the lingering record to be sent", 3*time.Second, func() bool { return len(r.sub.snapshot()) == 1 })
	if d := r.sub.snapshot()[0].at.Sub(start); d < 350*time.Millisecond {
		t.Fatalf("sent after %v, before the linger expired", d)
	}
	if c := r.sub.snapshot()[0]; c.records != 1 {
		t.Fatalf("records = %d", c.records)
	}
}

func TestSenderDrainsABacklogBackToBackWithOneRequestInFlight(t *testing.T) {
	sub := &countingSubmitter{delay: 5 * time.Millisecond}
	r := startRun(t, 600*time.Millisecond, sub)
	start := time.Now()
	r.add(t, 1, 600) // 256 + 256 + 88
	waitUntil(t, "the two full batches", 5*time.Second, func() bool { return len(sub.snapshot()) >= 2 })
	calls := sub.snapshot()
	if calls[0].records != 256 || calls[1].records != 256 {
		t.Fatalf("batches %d and %d records, want 256 each (the hard cap)", calls[0].records, calls[1].records)
	}
	if d := calls[1].at.Sub(start); d > 400*time.Millisecond {
		t.Fatalf("full batches were not dispatched at once (second after %v)", d)
	}
	waitUntil(t, "the remainder", 5*time.Second, func() bool { return len(sub.snapshot()) == 3 })
	if sub.snapshot()[2].records != 88 || sub.snapshot()[2].at.Sub(start) < 500*time.Millisecond {
		t.Fatalf("the partial remainder must wait for its linger: %+v", sub.snapshot()[2])
	}
	if sub.maxIn.Load() != 1 {
		t.Fatalf("%d requests in flight at once, want 1", sub.maxIn.Load())
	}
	for _, c := range sub.snapshot() {
		if c.records == 0 || c.records > SendMaxRecords {
			t.Fatalf("batch of %d records", c.records)
		}
	}
}

func TestSenderSteadyStateIsAboutOneRequestPerSecond(t *testing.T) {
	const linger = 250 * time.Millisecond
	sub := &countingSubmitter{}
	r := startRun(t, linger, sub)
	start := time.Now()
	n := 0
	for time.Since(start) < 3*time.Second {
		n++
		r.add(t, n, n)
		time.Sleep(10 * time.Millisecond)
	}
	waitUntil(t, "the tail", 3*time.Second, func() bool {
		total := 0
		for _, c := range sub.snapshot() {
			total += c.records
		}
		return total == n
	})
	calls := len(sub.snapshot())
	perLinger := float64(calls) / (float64(3*time.Second) / float64(linger))
	if perLinger < 0.6 || perLinger > 1.4 {
		t.Fatalf("%d requests in 3 s with a %v linger = %.2f per linger interval, want ≈ 1", calls, linger, perLinger)
	}
	if mean := float64(n) / float64(calls); mean < 10 {
		t.Fatalf("mean %.1f records per request", mean)
	}
}

func TestShutdownCutsLingerAndAttemptsOneFinalDispatch(t *testing.T) {
	t.Run("delivered", func(t *testing.T) {
		sub := &countingSubmitter{}
		r := startRun(t, time.Hour, sub)
		r.add(t, 1, 3)
		time.Sleep(100 * time.Millisecond)
		if len(sub.snapshot()) != 0 {
			t.Fatal("sent during the linger")
		}
		start := time.Now()
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after shutdown")
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("shutdown took %v", time.Since(start))
		}
		if c := sub.snapshot(); len(c) != 1 || c[0].records != 3 {
			t.Fatalf("final dispatch: %+v", c)
		}
		if st := r.q.Stats(); st.QueueRecords != 0 {
			t.Fatalf("%d records left after a successful final dispatch", st.QueueRecords)
		}
	})
	t.Run("failed keeps the rows", func(t *testing.T) {
		sub := &countingSubmitter{results: []SubmitResult{{Kind: SubmitRetryable, Status: 503}}}
		r := startRun(t, time.Hour, sub)
		r.add(t, 1, 3)
		time.Sleep(50 * time.Millisecond)
		r.cancel()
		<-r.done
		if c := sub.snapshot(); len(c) != 1 {
			t.Fatalf("attempts: %+v", c)
		}
		if st := r.q.Stats(); st.QueueRecords != 3 {
			t.Fatalf("queue holds %d records, want all 3 kept", st.QueueRecords)
		}
	})
}

func TestRecoveryAfterIngestOutageKeepsBackoffAndDrainsInFullBatches(t *testing.T) {
	sub := &countingSubmitter{results: []SubmitResult{{Kind: SubmitRetryable, Status: 503}, {Kind: SubmitRetryable, Status: 503}}}
	r := startRun(t, 800*time.Millisecond, sub)
	r.add(t, 1, 520) // 256 + 256 + 8
	waitUntil(t, "the first two full batches after the outage", 8*time.Second, func() bool {
		ok := 0
		for _, c := range sub.snapshot() {
			if c.records == 256 {
				ok++
			}
		}
		return ok >= 4 // two failed attempts and two accepted ones, all full-size
	})
	for _, c := range sub.snapshot()[:4] {
		if c.records != 256 {
			t.Fatalf("a retry or recovery request had %d records, want full batches", c.records)
		}
	}
}
