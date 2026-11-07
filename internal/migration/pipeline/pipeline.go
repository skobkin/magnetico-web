package pipeline

// Bounded import pipeline: one record feed, Workers processors, and an
// ordered committer that advances a contiguous frontier over settled
// outcomes. Every scanned record yields exactly one Success or one Failure;
// BatchSize bounds ALL processed records per commit (successes and failures
// alike), so runs through invalid data cannot grow unbounded transactions.

// Ordering: outcomes may arrive out of order; the committer commits only
// the contiguous prefix of settled sequence numbers. Sequence gaps never
// occur (every dispatched record settles or is dropped at shutdown); source
// ID gaps are irrelevant — the checkpoint is the last committed record's
// source ID.

// Cancellation: on ctx cancellation the feed stops reading, queued records
// are dropped, in-flight records may still settle, and every contiguous
// ready prefix is committed (on an uncancelled settle context) before Run
// returns ErrInterrupted. A commit whose transaction was cancelled mid-flight
// has an ambiguous outcome: it is returned as a CommitError for the caller
// to reconcile against the authoritative checkpoint.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skobkin/magnetico-web/codec"
)

// ErrInterrupted reports a cancelled run whose settled prefix was committed;
// the migration resumes from the last committed checkpoint.
var ErrInterrupted = errors.New("migration pipeline interrupted")

// CommitError reports a failed batch commit. The writer's transaction was
// rolled back or its outcome is unknown (ambiguity); the caller must
// reconcile against the stored checkpoint before any retry.
type CommitError struct {
	LastSourceID int64 // checkpoint the batch attempted
	Processed    int   // records the batch carried (successes + failures)
	Err          error
}

func (e *CommitError) Error() string {
	return fmt.Sprintf("batch commit failed at checkpoint %d (%d records): %v",
		e.LastSourceID, e.Processed, e.Err)
}

func (e *CommitError) Unwrap() error { return e.Err }

// Source supplies records in strictly increasing SourceID order. Next
// returns one chunk of jobs; ok=false ends the input. A Job with a
// pre-settled Failure skips processing — read-stage failures such as
// missing files or file-count overflow.
type Source interface {
	Next(ctx context.Context) ([]Job, bool, error)
}

// Job is one dispatched record or pre-settled failure.
type Job struct {
	Record  *Record
	Failure *Failure
}

// Committer receives dispatch-ordered, contiguous batches.
type Committer interface {
	Commit(ctx context.Context, b *Batch) error
}

// Batch is one commit unit: at most BatchSize processed records, ordered by
// dispatch; LastSourceID becomes the new checkpoint on a successful commit.
type Batch struct {
	LastSourceID int64
	Successes    []Success
	Failures     []Failure
}

// Processed counts every record the batch carries — the hard cap applies to
// successes and failures alike.
func (b *Batch) Processed() int { return len(b.Successes) + len(b.Failures) }

// Blobs counts records that will write a blob row.
func (b *Batch) Blobs() int {
	n := 0
	for i := range b.Successes {
		if b.Successes[i].Blob != nil {
			n++
		}
	}
	return n
}

// Config bounds and tunes a Run.
type Config struct {
	Workers          int
	BatchSize        int
	MaxInflightBytes int64
	Profile          codec.Profile
	Limits           codec.Limits
	VerifyDecode     bool          // decode + canonical-content verification of every blob
	SettleTimeout    time.Duration // commits after cancellation; 0 → 30s
}

// Result accumulates what this run settled into committed batches.
type Result struct {
	Scanned      int64
	Successes    int64
	Errors       int64
	Blobs        int64
	LastSourceID int64
}

type job struct {
	seq     int64
	srcID   int64
	rec     *Record
	pre     *Failure
	solo    bool
	release func()
}

type outcome struct {
	seq     int64
	srcID   int64
	solo    bool
	success *Success
	failure *Failure
	release func()
}

const defaultSettleTimeout = 30 * time.Second

// Run drives one importing pass over src. On a fatal source or commit error
// it stops the run and returns the error; on cancellation it settles the
// contiguous ready prefix and returns (Result, ErrInterrupted).
func Run(ctx context.Context, src Source, cfg Config, commit Committer) (Result, error) {
	if cfg.Workers < 1 {
		return Result{}, fmt.Errorf("pipeline: workers must be >= 1, got %d", cfg.Workers)
	}
	if cfg.BatchSize < 1 {
		return Result{}, fmt.Errorf("pipeline: batch size must be >= 1, got %d", cfg.BatchSize)
	}
	if cfg.MaxInflightBytes < 1 {
		return Result{}, fmt.Errorf("pipeline: max-inflight-bytes must be >= 1, got %d", cfg.MaxInflightBytes)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	settleTimeout := cfg.SettleTimeout
	if settleTimeout <= 0 {
		settleTimeout = defaultSettleTimeout
	}

	sem := newWeights(cfg.MaxInflightBytes)
	proc := newProcessor(cfg.Limits, cfg.Profile, cfg.Workers, cfg.VerifyDecode)

	jobs := make(chan job, cfg.Workers*2)
	outcomes := make(chan outcome, cfg.Workers*2+8)

	// Feed: read windows, acquire in-flight budget, dispatch. Blocking on
	// acquire or on the jobs channel is the backpressure path.
	var dispatched atomic.Int64
	feedErr := make(chan error, 1)
	go func() {
		defer close(jobs)
		var seq int64
		for {
			js, ok, err := src.Next(ctx)
			if err != nil {
				feedErr <- err
				return
			}
			if !ok {
				feedErr <- nil
				return
			}
			for _, j := range js {
				var weight, srcID int64
				if j.Record != nil {
					weight = j.Record.Weight()
					srcID = int64(j.Record.SourceID)
				} else {
					srcID = j.Failure.SourceID
				}
				solo, release, aerr := sem.acquire(ctx, weight)
				if aerr != nil {
					feedErr <- aerr
					return
				}
				seq++
				// Increment BEFORE the handoff: the worker may settle and
				// deliver the outcome before this goroutine resumes, and the
				// committer's all-dispatched-settled check must never see a
				// stale (lower) count — that would stall a commit the feed's
				// own budget release depends on.
				dispatched.Add(1)
				select {
				case jobs <- job{
					seq: seq, srcID: srcID, rec: j.Record, pre: j.Failure,
					solo: solo, release: release,
				}:
				case <-ctx.Done():
					dispatched.Add(-1)
					release()
					feedErr <- ctx.Err()
					return
				}
			}
		}
	}()

	// Workers: process in parallel; once the run is cancelled, drop the
	// remaining queue (their budget is released; the frontier stops there).
	var wg sync.WaitGroup
	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					j.release()
					continue
				}
				o := outcome{seq: j.seq, srcID: j.srcID, solo: j.solo, release: j.release}
				if j.pre != nil {
					o.failure = j.pre
				} else {
					s, f := proc.process(j.rec)
					o.success, o.failure = s, f
				}
				select {
				case outcomes <- o:
				case <-ctx.Done():
					j.release()
					return
				}
			}
		}()
	}
	workersDone := make(chan struct{})
	go func() { wg.Wait(); close(outcomes); close(workersDone) }()

	var res Result
	var interrupted bool
	var committedThrough int64 // highest settled seq folded into a committed batch
	pending := map[int64]outcome{}

	// peekBatch returns the contiguous, dispatch-ordered outcomes starting
	// at committedThrough+1, capped at cfg.BatchSize processed records,
	// without removing them from pending. A batch that reaches an oversized
	// (solo) record ends there: the solo record rides alone in its own
	// transaction and its commit releases the solo slot for the feed.
	peekBatch := func() ([]outcome, bool) {
		batch := make([]outcome, 0, cfg.BatchSize)
		for seq := committedThrough + 1; len(batch) < cfg.BatchSize; seq++ {
			o, has := pending[seq]
			if !has {
				break
			}
			batch = append(batch, o)
			if o.solo {
				return batch, true
			}
		}
		return batch, false
	}

	commitBatch := func(cctx context.Context, batch []outcome) error {
		b := &Batch{LastSourceID: batch[len(batch)-1].srcID}
		for _, o := range batch {
			if o.success != nil {
				b.Successes = append(b.Successes, *o.success)
			} else {
				b.Failures = append(b.Failures, *o.failure)
			}
		}
		if err := commit.Commit(cctx, b); err != nil {
			return &CommitError{LastSourceID: b.LastSourceID, Processed: b.Processed(), Err: err}
		}
		res.Scanned += int64(len(batch))
		res.Successes += int64(len(b.Successes))
		res.Errors += int64(len(b.Failures))
		res.Blobs += int64(b.Blobs())
		res.LastSourceID = b.LastSourceID
		for _, o := range batch {
			delete(pending, o.seq)
			o.release()
		}
		committedThrough = batch[len(batch)-1].seq
		return nil
	}

	// commitReady drains the ready frontier until it stalls, honoring solo
	// boundaries: a prefix ending at a solo record commits the prefix
	// first, then the solo record alone. Draining matters: a commit can
	// unlock budget/solo slots whose records are ALREADY settled, and no
	// further outcome arrival would re-trigger the commit check.
	commitReady := func(cctx context.Context) error {
		for {
			batch, endsAtSolo := peekBatch()
			if len(batch) == 0 {
				return nil
			}
			if endsAtSolo && len(batch) > 1 {
				if err := commitBatch(cctx, batch[:len(batch)-1]); err != nil {
					return err
				}
				continue
			}
			if err := commitBatch(cctx, batch); err != nil {
				return err
			}
		}
	}

	finish := func() (Result, error) {
		// Settle every contiguous ready prefix before returning.
		for {
			sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
			err := commitReady(sctx)
			scancel()
			if err != nil {
				cancel()
				<-workersDone
				<-feedErr
				return res, err
			}
			if batch, _ := peekBatch(); len(batch) == 0 {
				break
			}
		}
		cancel()
		<-workersDone
		ferr := <-feedErr
		if ferr != nil && !errors.Is(ferr, context.Canceled) && !errors.Is(ferr, context.DeadlineExceeded) {
			// The settled prefix is committed; report the read failure.
			return res, fmt.Errorf("pipeline: source read failed after source id %d: %w", res.LastSourceID, ferr)
		}
		if interrupted || len(pending) > 0 {
			// Pending leftovers imply dropped records (a cancelled run).
			return res, ErrInterrupted
		}
		return res, nil
	}

	for {
		if !interrupted {
			select {
			case <-ctx.Done():
				interrupted = true
			default:
			}
		}
		if interrupted {
			return finish()
		}

		o, ok := <-outcomes
		if !ok {
			// Workers exited: input ended (or was cancelled). Settle the
			// remainder; interrupted-ness is decided in finish.
			return finish()
		}
		pending[o.seq] = o

		// Commit while the run is healthy when (a) a full batch is ready,
		// (b) an oversized (solo) record reaches the frontier, or (c) every
		// dispatched record has settled — the semaphore may admit fewer
		// than BatchSize records at once, and waiting for fill would then
		// deadlock against the dispatch backpressure (liveness over fill).
		batch, endsAtSolo := peekBatch()
		allSettled := committedThrough+int64(len(pending)) == dispatched.Load()
		if len(batch) == 0 || (len(batch) < cfg.BatchSize && !endsAtSolo && !allSettled) {
			continue
		}
		if err := commitReady(ctx); err != nil {
			cancel()
			<-workersDone
			<-feedErr
			return res, err
		}
	}
}
