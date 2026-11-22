package crawlqueue

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// A writer blocked in something that does not honour cancellation (a stand-in
// for an uninterruptible fsync) must not make CloseWithin wait forever, must
// not release the directory lock or close the handles, and must leave every
// committed record intact for the next open.
func TestCloseWithinAbandonsStalledWriter(t *testing.T) {
	var release func()
	var dir string
	q := open(t, func(o *Options) { release = holdWriter(o); dir = o.Dir })
	t.Cleanup(func() { release() })

	// Committed before the stall: needs the writer, so do it with the hook
	// released first, then arm a stall for the next operation.
	release()
	if ok, err := q.Append(bg, [20]byte{1}, 1, payload(1, 100)); err != nil || !ok {
		t.Fatalf("Append: %v %v", ok, err)
	}
	gate := make(chan struct{})
	var once sync.Once
	q.opts.hooks.beforeOp = func() { <-gate }
	t.Cleanup(func() { once.Do(func() { close(gate) }) })

	// An operation that will be stuck in the writer when we close.
	stuck := make(chan error, 1)
	go func() { _, err := q.Append(bg, [20]byte{2}, 1, payload(2, 100)); stuck <- err }()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	err := q.CloseWithin(200 * time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrCloseTimeout) {
		t.Fatalf("CloseWithin = %v, want ErrCloseTimeout", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("CloseWithin took %v; must be bounded by the grace period", elapsed)
	}
	if !q.Abandoned() {
		t.Fatal("Abandoned() = false")
	}
	// The lock must still be held and the handles open: another open of the
	// same directory is refused.
	if _, oerr := Open(Options{Dir: dir, Logf: func(string, ...any) {}}); !errors.Is(oerr, ErrLocked) {
		t.Fatalf("second Open = %v, want ErrLocked while the writer is abandoned", oerr)
	}
	// New work is refused.
	if _, aerr := q.Append(bg, [20]byte{3}, 1, payload(3, 10)); !errors.Is(aerr, ErrClosed) {
		t.Fatalf("Append after close = %v, want ErrClosed", aerr)
	}

	// Let the writer go; a later Close completes the release and the data is
	// recoverable: the first record is there, the stuck one was never acked.
	once.Do(func() { close(gate) })
	if err := <-stuck; err == nil {
		t.Log("the stalled append happened to commit before shutdown (allowed)")
	}
	if err := q.Close(); err != nil {
		t.Fatalf("Close after release: %v", err)
	}
	q2, err := Open(Options{Dir: dir, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer q2.Close()
	items, err := q2.Peek(bg, 10, 1<<20)
	if err != nil || len(items) < 1 || items[0].InfoHash != ([20]byte{1}) {
		t.Fatalf("committed record lost: %v %v", items, err)
	}
}

// An operation that does react to cancellation is interrupted in the second
// phase and CloseWithin then completes normally.
func TestCloseWithinInterruptsCancellableWork(t *testing.T) {
	var q *Queue
	q = open(t, func(o *Options) {
		o.hooks.beforeOp = func() { <-q.dbCtx.Done() }
	})
	go func() { _, _ = q.Append(bg, [20]byte{1}, 1, payload(1, 10)) }()
	time.Sleep(100 * time.Millisecond)
	if err := q.CloseWithin(300 * time.Millisecond); err != nil {
		t.Fatalf("CloseWithin = %v, want nil after interrupting the work", err)
	}
	if q.Abandoned() {
		t.Fatal("must not be abandoned")
	}
}

// A queue at its byte quota still shuts down promptly and keeps its rows.
func TestCloseWithinFullQueue(t *testing.T) {
	var dir string
	q := open(t, func(o *Options) { o.MaxBytes = 300; dir = o.Dir })
	for i := 0; i < 3; i++ {
		if _, err := q.Append(bg, [20]byte{byte(i + 1)}, 1, payload(i, 100)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Append(bg, [20]byte{9}, 1, payload(9, 100)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}
	start := time.Now()
	if err := q.CloseWithin(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("shutdown of a full queue took %v", time.Since(start))
	}
	q2, err := Open(Options{Dir: dir, MaxBytes: 300, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer q2.Close()
	if s := q2.Stats(); s.QueueRecords != 3 {
		t.Fatalf("records after shutdown = %d, want 3", s.QueueRecords)
	}
}
