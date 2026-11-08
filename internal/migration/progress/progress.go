// Package progress renders migration progress for the operator: a rolling
// single-line renderer on a TTY, periodic structured log lines otherwise.
//
// It is display-only by construction: Update never blocks (a non-blocking
// store plus a wake channel), and all rendering happens on this package's
// own goroutine, so a stalled output sink can never stall the import
// pipeline. Rate and ETA are computed from this session's delta over the
// cumulative, DB-backed counters, so a resumed run reports an honest
// current throughput instead of a burst inflated by the previous session's
// totals — while the counters themselves stay cumulative and resume-honest.
package progress

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Snapshot is one cumulative reporting point. It mirrors the runner's
// Progress (kept a separate type so neither package depends on the other;
// main maps between them).
type Snapshot struct {
	// Title identifies non-import passes in the periodic log line (e.g.
	// "verify-decode"). Empty keeps the legacy `migrate torrents` line
	// byte-identical. The pass's own counters are mapped onto the fields
	// below by the caller in main (verified→Successes, mismatched→Errors,
	// …); the runner's final summary line carries the authoritative numbers
	// with the pass's own vocabulary.
	Title string

	Phase         string
	Checkpoint    int64
	Scanned       int64
	Successes     int64
	Errors        int64
	Blobs         int64
	RunErrors     int64
	ExpectedTotal int64

	// IndexStep/IndexTotal/IndexName carry the secondary-index rebuild
	// progress while the phase is building_indexes (0/"" otherwise):
	// IndexStep of IndexTotal indexes ensured, IndexName the most recent.
	IndexStep  int
	IndexTotal int
	IndexName  string
}

// Refresh cadence: a TTY redraws several times a second; a non-TTY run
// logs periodically (plan cadence: every 30 s). Ordinary progress is
// rate-limited to the interval on both sinks — Update fires once per
// committed batch and is only a wake-up, never a render order — while
// phase changes, index milestones, the first snapshot and the final state
// render immediately.
const (
	DefaultTTYInterval = 500 * time.Millisecond
	DefaultLogInterval = 30 * time.Second
)

// IsTTY reports whether f is attached to a terminal (a character device).
// Pipes and regular files are non-TTY, so redirected or piped runs fall
// back to periodic log lines; a redirect to some other character device is
// misdetected as a TTY, which only changes the rendering style, never the
// content or the pipeline's behavior.
func IsTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Renderer renders snapshots until stopped. Update is safe to call from any
// goroutine; Run must run on its own goroutine.
type Renderer struct {
	out      io.Writer // TTY line sink
	tty      bool
	logf     func(string, ...any) // non-TTY line sink
	interval time.Duration

	latest  atomic.Value // Snapshot
	wake    chan struct{}
	stop    sync.Once
	stopped chan struct{}
	done    chan struct{}
}

// New builds a renderer. A nil out falls back to os.Stderr (TTY sink); a
// nil logf falls back to log.Printf (non-TTY sink).
func New(tty bool, out io.Writer, logf func(string, ...any)) *Renderer {
	if out == nil {
		out = os.Stderr
	}
	if logf == nil {
		logf = log.Printf
	}
	r := &Renderer{
		out:      out,
		tty:      tty,
		logf:     logf,
		interval: DefaultLogInterval,
		wake:     make(chan struct{}, 1),
		stopped:  make(chan struct{}),
		done:     make(chan struct{}),
	}
	if tty {
		r.interval = DefaultTTYInterval
	}
	return r
}

// Update stores the newest snapshot and nudges the renderer. It never
// blocks: reporting must never stall the import pipeline, so a renderer
// that has fallen behind simply skips intermediate snapshots.
func (r *Renderer) Update(s Snapshot) {
	r.latest.Store(s)
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run renders until Stop. It blocks; start it with `go r.Run()`.
func (r *Renderer) Run() {
	defer close(r.done)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	var (
		started     time.Time
		baseScanned int64
		lastRender  time.Time
		lastKey     snapshotKey
	)
	for {
		select {
		case <-r.stopped:
			if s, ok := r.latest.Load().(Snapshot); ok {
				r.render(s, started, baseScanned, true)
			}
			return
		case <-r.wake:
		case <-ticker.C:
		}
		s, ok := r.latest.Load().(Snapshot)
		if !ok {
			continue
		}
		if started.IsZero() {
			started = time.Now()
			baseScanned = s.Scanned
		}
		// Ordinary progress is rate-limited to the refresh interval: Update
		// fires once per committed batch, and rendering per batch would spam
		// the non-TTY log once per batch and redraw the TTY line unboundedly.
		// Phase changes and index milestones are rare and operator-relevant,
		// so their key changes render immediately — as do the very first
		// snapshot and the final state.
		key := snapshotKey{s.Phase, s.IndexStep, s.IndexTotal, s.IndexName}
		if !lastRender.IsZero() && key == lastKey && time.Since(lastRender) < r.interval {
			continue
		}
		r.render(s, started, baseScanned, false)
		lastRender, lastKey = time.Now(), key
	}
}

// snapshotKey is the identity of a snapshot's operator-relevant state:
// counter movement between equal keys is rate-limited, a key change (a
// phase or an index milestone) renders immediately.
type snapshotKey struct {
	phase             string
	idxStep, idxTotal int
	idxName           string
}

// Stop ends the rendering goroutine and waits for it to emit the final
// line. Idempotent.
func (r *Renderer) Stop() {
	r.stop.Do(func() { close(r.stopped) })
	<-r.done
}

func (r *Renderer) render(s Snapshot, started time.Time, baseScanned int64, final bool) {
	elapsed, rate, eta, hasETA := sessionProgress(baseScanned, s, started)
	if r.tty {
		fmt.Fprint(r.out, ttyLine(s, elapsed, rate, hasETA, eta, final))
		return
	}
	r.logf("%s", logLine(s, elapsed, rate, hasETA, eta))
}

// sessionProgress computes elapsed time, this session's throughput and the
// ETA from the first snapshot of the session (baseScanned) to s. started is
// zero before the first render.
func sessionProgress(baseScanned int64, s Snapshot, started time.Time) (elapsed time.Duration, rate float64, eta time.Duration, hasETA bool) {
	if started.IsZero() {
		return 0, 0, 0, false
	}
	elapsed = time.Since(started)
	if elapsed <= 0 {
		return elapsed, 0, 0, false
	}
	// Rate from the SESSION delta: on a resumed run the cumulative Scanned
	// already includes the previous session's work, which would inflate the
	// rate and shrink the ETA dishonestly.
	rate = float64(s.Scanned-baseScanned) / elapsed.Seconds()
	remaining := s.ExpectedTotal - s.Scanned
	if rate > 0 && remaining > 0 {
		eta = time.Duration(float64(remaining) / rate * float64(time.Second))
		hasETA = true
	}
	return elapsed, rate, eta, hasETA
}

func phaseLabel(s Snapshot) string {
	if s.IndexTotal > 0 {
		return fmt.Sprintf("%s %d/%d: %s", s.Phase, s.IndexStep, s.IndexTotal, s.IndexName)
	}
	return s.Phase
}

func ttyLine(s Snapshot, elapsed time.Duration, rate float64, hasETA bool, eta time.Duration, final bool) string {
	b := &strings.Builder{}
	b.WriteString("\r\x1b[2K[")
	b.WriteString(phaseLabel(s))
	b.WriteString("]")
	if s.ExpectedTotal > 0 {
		fmt.Fprintf(b, " scanned %d/%d (%.1f%%)", s.Scanned, s.ExpectedTotal,
			100*float64(s.Scanned)/float64(s.ExpectedTotal))
	} else {
		fmt.Fprintf(b, " scanned %d", s.Scanned)
	}
	fmt.Fprintf(b, " ok %d err %d blobs %d", s.Successes, s.Errors, s.Blobs)
	if s.RunErrors > 0 {
		fmt.Fprintf(b, " integrity %d", s.RunErrors)
	}
	if rate > 0 {
		fmt.Fprintf(b, " %.1f/s", rate)
	}
	fmt.Fprintf(b, " up %s", dur(elapsed))
	if hasETA {
		fmt.Fprintf(b, " eta %s", dur(eta))
	}
	if final {
		b.WriteString("\n")
	}
	return b.String()
}

func logLine(s Snapshot, elapsed time.Duration, rate float64, hasETA bool, eta time.Duration) string {
	b := &strings.Builder{}
	if s.Title != "" {
		fmt.Fprintf(b, "%s: phase=%s", s.Title, phaseLabel(s))
	} else {
		fmt.Fprintf(b, "migrate torrents: phase=%s", phaseLabel(s))
	}
	if s.ExpectedTotal > 0 {
		fmt.Fprintf(b, " scanned=%d/%d", s.Scanned, s.ExpectedTotal)
	} else {
		fmt.Fprintf(b, " scanned=%d", s.Scanned)
	}
	fmt.Fprintf(b, " ok=%d err=%d blobs=%d", s.Successes, s.Errors, s.Blobs)
	if s.RunErrors > 0 {
		fmt.Fprintf(b, " integrity=%d", s.RunErrors)
	}
	if rate > 0 {
		fmt.Fprintf(b, " rate=%.1f/s", rate)
	}
	fmt.Fprintf(b, " elapsed=%s", dur(elapsed))
	if hasETA {
		fmt.Fprintf(b, " eta=%s", dur(eta))
	}
	return b.String()
}

// dur renders a duration compactly for a progress line (no sub-second
// noise): 45s, 2m10s, 1h07m.
func dur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d / time.Hour)
	m := int(d % time.Hour / time.Minute)
	sec := int(d % time.Minute / time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}
