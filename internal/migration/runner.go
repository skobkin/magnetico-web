// Package migration is the offline torrent migrator core (task 04A): it
// reads an original magnetico v0.12.0 PostgreSQL source read-only, encodes
// every torrent with the production codec, bulk-loads exact source IDs into
// a fresh 0.8.0 target with transactional checkpoints, an error registry,
// crash/resume and progress reporting. Index deferral and finalization live
// in the target layer; CLI wiring lives in opflags/main.
package migration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/pipeline"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
)

const (
	// SourceAdapterOriginal is the only source adapter 04A implements.
	SourceAdapterOriginal = "original-v0.12.0"
	// MigrationContractVersion is the legacy-torrent-migration contract
	// version this runner implements.
	MigrationContractVersion = 1

	// Run-scope integrity incident code for orphaned source file rows.
	CodeSourceOrphanFiles = "source_orphan_files"
	// Run-scope operational incident code for fatal failures.
	CodeFatalIncident = "fatal_incident"
)

// Defaults for the import tunables (CLI flags may override within range).
const (
	DefaultWorkers          = 4
	DefaultBatchSize        = 256
	DefaultMaxInflightBytes = 64 << 20
	MaxBatchSize            = 10000
	MaxWorkers              = 64

	// dbOpTimeout bounds one administrative database round trip (incident
	// recording, reconciliation, phase transitions).
	dbOpTimeout = 30 * time.Second
)

// ErrInterrupted reports a cancelled, incomplete migration: the settled
// prefix is committed and resumable. Exit-code 2 territory.
var ErrInterrupted = errors.New("migration interrupted before completion")

// ErrSourceIDRange reports source torrent IDs beyond the signed 32-bit
// range the 0.8.0 schema can hold — a clear fatal, never a truncation.
var ErrSourceIDRange = errors.New("source torrent IDs exceed the signed 32-bit ID range of the target schema")

// FatalError reports an operational failure that stopped the run. It
// records the attempted source-ID range and stage; its text is sanitized
// and never carries credentials.
type FatalError struct {
	Stage  string // preflight | connect | read | write | checkpoint | lock
	FromID int64  // checkpoint the attempted work started from
	ToID   int64  // attempted checkpoint (0 when unknown)
	Err    error
}

func (e *FatalError) Error() string {
	return fmt.Sprintf("migration failed at stage %q (source ids %d..%d): %s",
		e.Stage, e.FromID, e.ToID, sanitizeDetail(e.Err.Error()))
}

func (e *FatalError) Unwrap() error { return e.Err }

// ConfigError reports an invalid import tunable. Its text is static (a fixed
// message plus the offending number) and carries no secrets, so the CLI
// prints it verbatim.
type ConfigError struct{ msg string }

func (e *ConfigError) Error() string { return e.msg }

// Progress is a cumulative, resume-honest snapshot for reporting.
type Progress struct {
	Phase         state.Phase
	Checkpoint    int64
	Scanned       int64
	Successes     int64
	Errors        int64
	Blobs         int64
	RunErrors     int64
	ExpectedTotal int64

	// Secondary-index rebuild progress (deferred runs, building_indexes
	// phase only): IndexStep of IndexTotal indexes ensured, IndexName the
	// most recently ensured one. Zero/"" outside that phase.
	IndexStep  int
	IndexTotal int
	IndexName  string
}

// TorrentsInput configures one migrate-torrents run.
type TorrentsInput struct {
	SourceDSN string
	TargetDSN string

	DeferIndexes     bool
	Workers          int
	BatchSize        int
	MaxInflightBytes int64

	// Limits overrides codec.DefaultLimits() (used by tests to exercise
	// overflow paths with tiny fixtures); production leaves it nil.
	Limits *codec.Limits

	// Progress, when set, receives cumulative snapshots after every
	// committed batch, at start/end, on every phase transition and after
	// each secondary index is ensured during the rebuild.
	Progress func(Progress)

	// committer is the batch-commit dependency; nil uses the real target
	// writer. Tests inject doubles for fault injection.
	committer batchCommitter
}

// batchCommitter is the runner's commit seam (fault-injectable).
type batchCommitter interface {
	CommitBatch(ctx context.Context, b target.Batch) error
}

// ImportTorrents runs one full migrate-torrents pass: preflight, import
// pipeline with checkpointed batches, and finalization (deferred-index
// drop/rebuild, identity reseed, statistics, completion phase). Refusals are
// typed; the error taxonomy is: typed preflight refusals, *FatalError
// (exit 1), ErrInterrupted (exit 2).
func ImportTorrents(ctx context.Context, in TorrentsInput) error {
	if in.SourceDSN == "" {
		return target.ErrSourceRequired
	}
	if in.Workers <= 0 {
		in.Workers = DefaultWorkers
	}
	if in.Workers > MaxWorkers {
		return &ConfigError{msg: fmt.Sprintf("--workers must be 1..%d, got %d", MaxWorkers, in.Workers)}
	}
	if in.BatchSize <= 0 {
		in.BatchSize = DefaultBatchSize
	}
	if in.BatchSize > MaxBatchSize {
		return &ConfigError{msg: fmt.Sprintf("--batch-size must be 1..%d, got %d", MaxBatchSize, in.BatchSize)}
	}
	if in.MaxInflightBytes <= 0 {
		in.MaxInflightBytes = DefaultMaxInflightBytes
	}
	if in.MaxInflightBytes < 1<<20 {
		return &ConfigError{msg: fmt.Sprintf("--max-inflight-bytes must be at least 1 MiB, got %d", in.MaxInflightBytes)}
	}
	limits := codec.DefaultLimits()
	if in.Limits != nil {
		limits = *in.Limits
	}

	// Source: probe, fingerprint, identity, ID-range headroom check.
	src, err := original.Open(ctx, in.SourceDSN)
	if err != nil {
		return err
	}
	defer src.Close()

	fp, err := src.Fingerprint(ctx)
	if err != nil {
		return &FatalError{Stage: "preflight", Err: err}
	}
	srcID, err := src.Identity(ctx)
	if err != nil {
		return &FatalError{Stage: "preflight", Err: fmt.Errorf("read source identity: %w", err)}
	}
	if fp.MaxID > math.MaxInt32 {
		return fmt.Errorf("%w: max torrent id %d", ErrSourceIDRange, fp.MaxID)
	}

	// Target preflight: identity, lock, fresh-init or full resume
	// reconciliation. The lock is held for the runner's lifetime.
	pf, err := target.Preflight(ctx, target.Input{
		DSN:               in.TargetDSN,
		SourceIdentity:    srcID,
		SourceFingerprint: fp,
		DeferIndexes:      in.DeferIndexes,
		SourceAdapter:     SourceAdapterOriginal,
		ContractVersion:   MigrationContractVersion,
		CodecProfile:      profileName(codec.DefaultProfile()),
		Options: state.Options{
			Workers:          in.Workers,
			BatchSize:        in.BatchSize,
			MaxInflightBytes: in.MaxInflightBytes,
		},
	})
	if err != nil {
		return err
	}
	defer pf.Lock.Release(context.WithoutCancel(ctx))

	// The runner's own pool drives commits, incidents and phase transitions.
	pool, err := target.OpenPool(ctx, in.TargetDSN)
	if err != nil {
		return &FatalError{Stage: "connect", Err: err}
	}
	defer pool.Close()

	// Lock-connection liveness: loss is fatal (stop, never continue
	// unprotected). The monitor only pings while the lock connection is idle.
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)
	monitorCtx, stopMonitor := context.WithCancel(context.WithoutCancel(runCtx))
	defer stopMonitor()
	go pf.Lock.MonitorLockLoss(monitorCtx, func(err error) {
		cancelRun(fmt.Errorf("run lock connection lost: %w", err))
	})

	prog := *pf.Progress
	// Index-rebuild display state lives outside the DB row: the rebuild is
	// not checkpointed per index, only reconciled against the catalog.
	var idxStep, idxTotal int
	var idxName string
	report := func() {
		if in.Progress != nil {
			in.Progress(Progress{
				Phase:         prog.Phase,
				Checkpoint:    prog.LastProcessedID,
				Scanned:       prog.ScannedCount,
				Successes:     prog.SuccessCount,
				Errors:        prog.ErrorCount,
				Blobs:         prog.BlobCount,
				RunErrors:     prog.RunErrorCount,
				ExpectedTotal: prog.ExpectedTotal,
				IndexStep:     idxStep,
				IndexTotal:    idxTotal,
				IndexName:     idxName,
			})
		}
	}
	report()

	if prog.Phase.Complete() {
		return nil // finished run: idempotent no-op (final report in 04B/04C)
	}

	// Orphaned source file rows: one run-scope integrity incident (counts
	// and ID ranges, no paths), recorded once per run — resume-safe.
	inserted, err := ensureOrphanIncident(ctx, pool, pf.Progress.RunID, fp)
	if err != nil {
		return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "preflight",
			prog.LastProcessedID, 0, err)
	}
	if inserted {
		prog.RunErrorCount++
	}

	if prog.Phase == state.PhaseImporting {
		// Deferred runs: the authorized index-less state is created by an
		// atomic drop+mark on the lock connection before the first batch.
		// Interruption before the transaction leaves the strict state (the
		// resume performs the drop); interruption after leaves the
		// authorized index-less state.
		if in.DeferIndexes && !prog.IndexesDeferred {
			if err := target.DropSecondaryIndexes(runCtx, pf.Lock, pf.Progress.RunID); err != nil {
				if cause := fatalCause(runCtx); cause != nil {
					return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "lock",
						prog.LastProcessedID, 0, cause)
				}
				if runCtx.Err() != nil {
					return ErrInterrupted
				}
				return fatalWithIncident(ctx, pool, pf.Progress.RunID, "write",
					prog.LastProcessedID, 0, err)
			}
			prog.IndexesDeferred = true
		}

		// Import loop. Commit failures that reconcile as uncommitted replay
		// from the authoritative checkpoint with a fresh reader; committed
		// ambiguity continues transparently.
		rc := &runCommitter{
			inner:    in.committer,
			pool:     pool,
			runID:    pf.Progress.RunID,
			phase:    prog.Phase,
			expected: prog.LastProcessedID,
			// Counter mirrors resume CUMULATIVE: the target writer stores
			// per-batch deltas against the persisted totals, so the mirrors
			// must start from the stored totals or the Progress reports
			// under-count everything the previous session settled.
			scanned:   prog.ScannedCount,
			successes: prog.SuccessCount,
			errors:    prog.ErrorCount,
			blobs:     prog.BlobCount,
		}
		rc.onBatch = func() {
			prog.LastProcessedID = rc.expected
			prog.ScannedCount = rc.scanned
			prog.SuccessCount = rc.successes
			prog.ErrorCount = rc.errors
			prog.BlobCount = rc.blobs
			report()
		}
		if rc.inner == nil {
			rc.inner = target.NewBatchCommitter(pool)
		}

		pcfg := pipeline.Config{
			Workers:          in.Workers,
			BatchSize:        in.BatchSize,
			MaxInflightBytes: in.MaxInflightBytes,
			Profile:          codec.DefaultProfile(),
			Limits:           limits,
			VerifyDecode:     true,
		}

		// Reader retention bounds: per-torrent caps come from the production
		// codec limits (identical to what encode would enforce); the window
		// byte budget is the same --max-inflight-bytes the pipeline
		// semaphore enforces post-dispatch, applied earlier at read time.
		wlimits := original.WindowLimits{
			MaxFiles:       limits.MaxFiles,
			MaxPathBytes:   limits.MaxAggregatePathBytes,
			WindowMaxBytes: in.MaxInflightBytes,
		}
		const maxReplays = 3
		ws := newWindowSource(src, prog.LastProcessedID, in.BatchSize, wlimits)
		for attempt := 0; ; attempt++ {
			_, perr := pipeline.Run(runCtx, ws, pcfg, rc)
			prog.LastProcessedID = rc.expected
			prog.ScannedCount = rc.scanned
			prog.SuccessCount = rc.successes
			prog.ErrorCount = rc.errors
			prog.BlobCount = rc.blobs

			if perr == nil {
				break
			}
			if errors.Is(perr, pipeline.ErrInterrupted) {
				if cause := fatalCause(runCtx); cause != nil {
					return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "lock",
						rc.expected, 0, cause)
				}
				return ErrInterrupted
			}

			var ce *pipeline.CommitError
			if errors.As(perr, &ce) {
				if errors.Is(perr, target.ErrCheckpointConflict) {
					return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "checkpoint",
						rc.expected, ce.LastSourceID, perr)
				}
				// The batch did not land (reconciled inside Commit): replay from
				// the authoritative checkpoint unless the run was cancelled.
				if cause := fatalCause(runCtx); cause != nil {
					return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "lock",
						rc.expected, 0, cause)
				}
				if runCtx.Err() != nil {
					return ErrInterrupted
				}
				if attempt >= maxReplays {
					return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "write",
						rc.expected, ce.LastSourceID, fmt.Errorf("%w (after %d replays)", perr, attempt))
				}
				ws = newWindowSource(src, rc.expected, in.BatchSize, wlimits)
				continue
			}

			// Source read failure: fatal, the settled prefix stays committed.
			return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "read",
				rc.expected, rc.expected, fmt.Errorf("%w (settled through checkpoint %d)", perr, rc.expected))
		}

		// Observed orphans may be fewer than the fingerprint's global anti-join
		// count (windows only see orphans whose torrent_id falls inside the
		// scanned ID ranges) — but never more: an excess means the source grew
		// orphans mid-run, which the frozen fingerprint forbids.
		if ws.observedOrphans > fp.OrphanFiles {
			return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "read",
				rc.expected, rc.expected, fmt.Errorf(
					"read %d orphaned file rows but the fingerprint counted %d (source changed during import)",
					ws.observedOrphans, fp.OrphanFiles))
		}

		// Complete accounting: on a frozen source every torrent row must have
		// been scanned exactly once across the whole run (the committer's
		// mirrors are cumulative, including restored resume totals). A
		// shortfall means rows were silently skipped — e.g. a cursor bug —
		// and must stop the import before any completion phase can claim it;
		// an excess means the source grew mid-run, breaking the fingerprint.
		if rc.scanned != prog.ExpectedTotal {
			return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "read",
				rc.expected, rc.expected, fmt.Errorf(
					"scanned %d source records but the fingerprint counted %d torrents (incomplete or changed source)",
					rc.scanned, prog.ExpectedTotal))
		}

		// All source data processed: importing → data_loaded (compare-and-swap).
		prog.Phase = state.PhaseDataLoaded
		if err := target.TransitionPhase(ctx, pool, pf.Progress.RunID, state.AdvanceInput{
			RunID:          pf.Progress.RunID,
			ExpectedLastID: rc.expected,
			ExpectedPhase:  state.PhaseImporting,
			NewLastID:      rc.expected,
			NewPhase:       state.PhaseDataLoaded,
		}); err != nil {
			if cause := fatalCause(runCtx); cause != nil {
				return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "lock",
					rc.expected, 0, cause)
			}
			if runCtx.Err() != nil {
				return ErrInterrupted
			}
			return fatalWithIncident(ctx, pool, pf.Progress.RunID, "checkpoint",
				rc.expected, rc.expected, err)
		}
		report()
	}

	if prog.Phase == state.PhaseDataLoaded {
		// Explicit building_indexes transition BEFORE any index creation
		// (own transaction, committed before the first CREATE INDEX): a
		// crash after the first CREATE can never leave data_loaded, whose
		// preflight requires all three deferred indexes absent.
		if err := target.TransitionPhase(ctx, pool, pf.Progress.RunID, state.AdvanceInput{
			RunID:          pf.Progress.RunID,
			ExpectedLastID: prog.LastProcessedID,
			ExpectedPhase:  state.PhaseDataLoaded,
			NewLastID:      prog.LastProcessedID,
			NewPhase:       state.PhaseBuildingIndexes,
		}); err != nil {
			if cause := fatalCause(runCtx); cause != nil {
				return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "lock",
					prog.LastProcessedID, 0, cause)
			}
			if runCtx.Err() != nil {
				return ErrInterrupted
			}
			return fatalWithIncident(ctx, pool, pf.Progress.RunID, "checkpoint",
				prog.LastProcessedID, prog.LastProcessedID, err)
		}
		prog.Phase = state.PhaseBuildingIndexes
		report()
	}

	// building_indexes: rebuild missing deferred indexes (lock connection),
	// reseed the identity sequence, refresh statistics, commit the
	// completion phase. Partial index progress on resume is reconciled
	// against the catalog. Finalization runs on runCtx so a detected lock
	// loss or operator interrupt stops it, and the completion phase
	// transition itself executes on the lock-owning connection (proven
	// ownership at completion time — pool-side bookkeeping alone must never
	// finalize a run).
	// The index-rebuild callback runs on this goroutine (finalizeImport is
	// synchronous), so mutating the display state and reporting here is
	// race-free.
	onIndex := func(ensured, total int, name string) {
		idxStep, idxTotal, idxName = ensured, total, name
		report()
	}
	fin, err := finalizeImport(runCtx, pf.Lock, pool, pf.Progress.RunID, prog.LastProcessedID, prog.IndexesDeferred, onIndex)
	if err != nil {
		if cause := fatalCause(runCtx); cause != nil {
			return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "lock",
				prog.LastProcessedID, 0, cause)
		}
		if runCtx.Err() != nil {
			return ErrInterrupted
		}
		// The lock connection can die between monitor pings; the failing
		// lock-connection operation itself is the first detector (RunLock
		// fails closed). Report it as the lock-loss fatal it is.
		if lockErr := pf.Lock.Lost(); lockErr != nil {
			return fatalWithIncident(runCtx, pool, pf.Progress.RunID, "lock",
				prog.LastProcessedID, 0, lockErr)
		}
		stage := "write"
		if errors.Is(err, target.ErrCheckpointConflict) {
			stage = "checkpoint"
		}
		return fatalWithIncident(ctx, pool, pf.Progress.RunID, stage,
			prog.LastProcessedID, prog.LastProcessedID, err)
	}
	prog.Phase = fin
	report()
	return nil
}

// finalizeImport completes a data-loaded run. The caller has already
// transitioned into building_indexes; deferred=false runs keep their indexes
// maintained throughout and skip the rebuild. ctx must be the run context
// (not the parent): every step must stop once the lock is known lost or the
// run was interrupted, and the completion transition runs on the
// lock-owning connection so finalization cannot succeed unprotected.
// onIndex, when non-nil, reports each ensured secondary index for display.
func finalizeImport(ctx context.Context, lock *state.RunLock, pool *pgxpool.Pool, runID string, checkpoint int64, deferred bool, onIndex func(ensured, total int, name string)) (state.Phase, error) {
	if deferred {
		if err := target.BuildSecondaryIndexes(ctx, lock, onIndex); err != nil {
			return "", err
		}
	}
	if _, err := target.ReseedIdentity(ctx, pool); err != nil {
		return "", err
	}
	if err := target.AnalyzeTarget(ctx, pool); err != nil {
		return "", err
	}
	return target.CompleteImport(ctx, lock, runID, checkpoint)
}

// fatalCause distinguishes an operator interrupt (nil, or plain
// context.Canceled) from a fatal cancellation cause (lock loss).
func fatalCause(runCtx context.Context) error {
	cause := context.Cause(runCtx)
	if cause == nil || errors.Is(cause, context.Canceled) {
		return nil
	}
	return cause
}

// runCommitter adapts pipeline batches to the target writer, updates the
// cumulative mirrors and reconciles ambiguous commits.
type runCommitter struct {
	inner    batchCommitter
	pool     *pgxpool.Pool
	runID    string
	phase    state.Phase
	expected int64 // authoritative checkpoint mirror (starts at the stored one)

	scanned, successes, errors, blobs int64
	onBatch                           func()
}

func (rc *runCommitter) Commit(ctx context.Context, pb *pipeline.Batch) error {
	tb := target.Batch{
		RunID:          rc.runID,
		ExpectedLastID: rc.expected,
		ExpectedPhase:  rc.phase,
		LastSourceID:   pb.LastSourceID,
		Torrents:       make([]target.TorrentRow, 0, len(pb.Successes)),
		Errors:         make([]state.ErrorRecord, 0, len(pb.Failures)),
	}
	for _, s := range pb.Successes {
		tb.Torrents = append(tb.Torrents, target.TorrentRow{
			ID:           s.Record.SourceID,
			InfoHash:     s.Record.InfoHash,
			Name:         s.Record.Name,
			TotalSize:    s.Record.TotalSize,
			DiscoveredOn: s.Record.DiscoveredOn,
			FileCount:    int32(len(s.Record.Files)),
			Blob:         s.Blob,
		})
	}
	for _, f := range pb.Failures {
		tb.Errors = append(tb.Errors, f.ErrorRecord())
	}

	err := rc.inner.CommitBatch(ctx, tb)
	if err == nil {
		rc.apply(tb)
		return nil
	}
	if errors.Is(err, target.ErrCheckpointConflict) {
		return err // fatal: never retry past moved run state
	}
	// Ambiguous or transient failure: reconcile against the stored
	// checkpoint with an uncancelled context before anything else.
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), dbOpTimeout)
	defer rcancel()
	committed, rerr := target.ReconcileAmbiguousCommit(rctx, rc.pool, rc.runID, tb.ExpectedLastID, tb.LastSourceID)
	if rerr != nil {
		return fmt.Errorf("%w (reconciliation failed: %s)", err, sanitizeDetail(rerr.Error()))
	}
	if committed {
		rc.apply(tb)
		return nil
	}
	return err // uncommitted: replayable from the checkpoint
}

func (rc *runCommitter) apply(tb target.Batch) {
	rc.expected = tb.LastSourceID
	rc.scanned += int64(len(tb.Torrents) + len(tb.Errors))
	rc.successes += int64(len(tb.Torrents))
	rc.errors += int64(len(tb.Errors))
	for _, t := range tb.Torrents {
		if t.Blob != nil {
			rc.blobs++
		}
	}
	if rc.onBatch != nil {
		rc.onBatch()
	}
}

// profileName renders the stable stored codec-profile name.
func profileName(p codec.Profile) string {
	switch p {
	case codec.ProfileCompact:
		return "compact"
	case codec.ProfileBalanced:
		return "balanced"
	case codec.ProfileMaximum:
		return "maximum"
	default:
		return fmt.Sprintf("profile-%d", int(p))
	}
}
