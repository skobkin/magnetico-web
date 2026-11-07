package progress

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// absDuration is |d|.
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// The renderer is display-only plumbing: it must never block the pipeline
// (Update), it must render on both sinks, and its rate/ETA must come from
// this session's delta — a resumed run must not inflate throughput with the
// previous session's cumulative totals.

func TestNonTTYLogsUpdatesAndFinalLine(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var lines []string
	r := New(false, nil, func(f string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(f, args...))
	})
	r.interval = 10 * time.Millisecond
	go r.Run()

	r.Update(Snapshot{Phase: "importing", Scanned: 9000, ExpectedTotal: 10000, Successes: 8900, Errors: 100})
	r.Update(Snapshot{Phase: "importing", Scanned: 9500, ExpectedTotal: 10000, Successes: 9400, Errors: 100})

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(lines)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected periodic log lines, got %d", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(lines) < 2 {
		t.Fatalf("log lines = %d, want at least the periodic renders plus the final one", len(lines))
	}
	last := lines[len(lines)-1]
	for _, want := range []string{
		"phase=importing", "scanned=9500/10000", "ok=9400", "err=100", "blobs=0", "elapsed=",
	} {
		if !strings.Contains(last, want) {
			t.Fatalf("log line %q missing %q", last, want)
		}
	}
}

// A resumed run's first snapshot already carries the previous session's
// cumulative counters: the reported rate must come from the session delta
// only, and the ETA from the remaining work over that honest rate.
func TestSessionRateUsesSessionDelta(t *testing.T) {
	t.Parallel()

	// Resumed at 9000/10000; this session processed 500 in 2s.
	s := Snapshot{Phase: "importing", Scanned: 9500, ExpectedTotal: 10000}
	_, rate, eta, hasETA := sessionProgress(9000, s, time.Now().Add(-2*time.Second))
	if math.Abs(rate-250) > 0.01 {
		t.Fatalf("rate = %f, want 250 (session delta 500 over 2s, not 9500 cumulative)", rate)
	}
	if !hasETA || absDuration(eta-2*time.Second) > time.Millisecond {
		t.Fatalf("eta = (%v, %v), want 2s for the 500 remaining at 250/s", eta, hasETA)
	}

	// Complete or no-estimate cases: no remaining work, no rate, no elapsed.
	if _, _, _, has := sessionProgress(0, Snapshot{Scanned: 10, ExpectedTotal: 10}, time.Now()); has {
		t.Fatal("a fully scanned snapshot must not report an ETA")
	}
	if _, _, _, has := sessionProgress(0, Snapshot{Scanned: 5, ExpectedTotal: 10}, time.Time{}); has {
		t.Fatal("no snapshot has arrived yet: no rate or ETA may be reported")
	}
}

func TestTTYLineRendersPhaseIndexAndClears(t *testing.T) {
	t.Parallel()

	// syncBuffer: the renderer goroutine writes while this goroutine polls.
	out := &syncBuffer{}
	r := New(true, out, nil)
	go r.Run()
	r.Update(Snapshot{Phase: "building_indexes", Scanned: 1000, ExpectedTotal: 1000,
		Successes: 1000, IndexStep: 2, IndexTotal: 3, IndexName: "idx_torrents_discovered_on"})

	deadline := time.Now().Add(3 * time.Second)
	for out.String() == "" {
		if time.Now().After(deadline) {
			t.Fatal("TTY renderer produced no output")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.Stop()

	s := out.String()
	for _, want := range []string{
		"\r\x1b[2K[", "building_indexes 2/3: idx_torrents_discovered_on",
		"scanned 1000/1000 (100.0%)", "ok 1000 err 0", " up ", "\n",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("TTY output %q missing %q", s, want)
		}
	}
}

// A stalled output sink must never block the reporter: Update returns
// immediately, snapshots are dropped (not queued), and Stop unblocks once
// the sink recovers.
func TestUpdateNeverBlocksOnStalledSink(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	r := New(true, &blockingWriter{release: release}, nil)
	go r.Run()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			r.Update(Snapshot{Phase: "importing", Scanned: int64(i), ExpectedTotal: 100})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Update blocked on a stalled output sink")
	}

	close(release)
	r.Stop()
}

type blockingWriter struct {
	release chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	// Blocked until the test closes release (a stalled sink); after close,
	// every receive returns immediately.
	<-w.release
	return len(p), nil
}

// syncBuffer is a mutex-guarded bytes.Buffer: the renderer goroutine writes
// while the test goroutine polls.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestIsTTY(t *testing.T) {
	t.Parallel()

	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	defer pipeR.Close()
	defer pipeW.Close()
	if IsTTY(pipeW) {
		t.Fatal("a pipe must not be detected as a TTY")
	}

	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer devnull.Close()
	if !IsTTY(devnull) {
		t.Fatalf("%s is a character device and must be detected as a TTY", os.DevNull)
	}
}

func TestDur(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"}, {45 * time.Second, "45s"}, {130 * time.Second, "2m10s"},
		{4020 * time.Second, "1h07m"}, {-5 * time.Second, "0s"},
	} {
		if got := dur(tc.d); got != tc.want {
			t.Fatalf("dur(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
