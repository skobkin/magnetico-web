package migration

// retry-errors (04B): targeted, transactional repair of unresolved record
// errors. The work list is the 04A registry itself — keyset pages of
// unresolved record-scope IDs (re-queried per batch: resolving IDs shrinks
// the set), fetched set-oriented by ID (FetchByIDs, budget-split via its
// `remaining`), validated/encoded/decode-verified through the exported
// production Processor, and committed per ID in ONE transaction
// (target.CommitRetrySuccess/Invalid/Missing) that also advances the pass's
// revision-tracked cursor. No ON CONFLICT, no overwrites: unexpected target
// state is a fatal conflict. The import checkpoint (scanned_count,
// last_processed_id) is never touched.
//
// Source drift policy: the fresh source fingerprint must be
// Fingerprint.Equivalent to the recorded import fingerprint (a PostgreSQL
// restart or transport switch is tolerated exactly as in 04A resume);
// content-anchor drift refuses with ErrSourceDrift. --accept-source-repairs
// authorizes a deliberately repaired source, matched to the original by
// STABLE evidence only — database name and unchanged Min/Max ID bounds
// (never postmaster start time or transport fields, which cannot prove
// identity). The authorization is an operator assertion, NOT proof the
// source is the original database; unrelated rows may have changed. The
// old+new sanitized fingerprints are recorded on the pass; the stored import
// fingerprint is never mutated; a fresh full verify-content is required
// afterward (enforced mechanically: every attempt bumps data_revision,
// staling all previous verification passes).
//
// Resume discipline: a pass is resumable iff it is non-terminal, its
// current_data_revision equals progress.data_revision (kept equal by
// construction — the same transaction bumps both) and the fresh source
// fingerprint is Equivalent to the pass's own recorded fingerprint. A
// source change between interrupted attempts aborts the pass — including
// already-authorized repair passes, which then need a fresh invocation
// with new explicit authorization.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/pgidentity"
	"github.com/skobkin/magnetico-web/internal/migration/pipeline"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
)

// ErrSourceDrift reports source content-anchor drift vs the recorded
// import fingerprint (or vs the stable-evidence match under
// --accept-source-repairs): fatal, the operator decides.
var ErrSourceDrift = errors.New("source fingerprint differs from the recorded import fingerprint")

// ErrRetryRemaining reports a completed retry pass with still-unresolved
// record errors (exit-3 territory — the summary is printed, not a fatal).
var ErrRetryRemaining = errors.New("retry pass completed with unresolved record errors")

// maxCommitReplays bounds the replay of a provably-not-landed ambiguous
// attempt transaction.
const maxCommitReplays = 3

// RetryProgress is one cumulative retry-pass snapshot for reporting.
type RetryProgress struct {
	Phase             string // retrying | complete
	Resumed           bool
	Attempted         int64
	Fixed             int64
	StillInvalid      int64
	Missing           int64
	Cursor            int64 // last attempted source ID (0 until the first attempt)
	UnresolvedAtStart int64
	UnresolvedNow     int64
}

// RetryResult is the outcome of one retry-errors invocation.
type RetryResult struct {
	RetryID              string
	State                string // the terminal pass state
	Resumed              bool
	Attempted            int64
	Fixed                int64
	StillInvalid         int64
	Missing              int64
	UnresolvedAtStart    int64
	UnresolvedRemaining  int64
	AcceptedSourceRepair bool
	PhaseFlipped         bool
}

// RetryErrorsInput configures one retry pass.
type RetryErrorsInput struct {
	SourceDSN string
	TargetDSN string

	Workers          int
	BatchSize        int
	MaxInflightBytes int64

	// AcceptSourceRepairs authorizes retrying against a source whose
	// content anchors drifted from the recorded import fingerprint. See the
	// file comment for the exact semantics and their limits.
	AcceptSourceRepairs bool

	// Limits overrides codec.DefaultLimits() (tests exercise oversize paths
	// with tiny fixtures); production leaves it nil.
	Limits *codec.Limits

	// Progress receives cumulative snapshots after every settled attempt.
	Progress func(RetryProgress)
}

// RetryErrors runs one retry pass: completed-run preflight, source drift
// policy, registry-driven batches of production-identical repair attempts,
// ambiguous-COMMIT reconciliation, the phase flip when everything resolved,
// and the durable pass verdict.
//
// Error taxonomy: typed preflight refusals (ErrRunNotComplete,
// target.ErrSameDatabase, ErrSourceDrift, corrupt state), *FatalError
// (exit 1), ErrInterrupted (exit 2), and ErrRetryRemaining after a
// COMPLETED pass with still-invalid/missing records (exit 3) — the result
// is returned alongside in every completed case.
func RetryErrors(ctx context.Context, in RetryErrorsInput) (*RetryResult, error) {
	if in.SourceDSN == "" {
		return nil, target.ErrSourceRequired
	}
	workers, batchSize, maxInflight, err := normalizeVerifyConfig(in.Workers, in.BatchSize, in.MaxInflightBytes)
	if err != nil {
		return nil, err
	}
	in.Workers, in.BatchSize, in.MaxInflightBytes = workers, batchSize, maxInflight
	limits := codec.DefaultLimits()
	if in.Limits != nil {
		limits = *in.Limits
	}

	cr, err := target.CompletedRunPreflight(ctx, in.TargetDSN)
	if err != nil {
		return nil, err
	}
	defer cr.Lock.Release(context.WithoutCancel(ctx))
	defer cr.Pool.Close()
	pool := cr.Pool
	prog := cr.Progress

	// Source: opened read-only, fingerprinted, proven DISTINCT from the
	// target (the same fail-closed pgidentity policy as every other pass).
	src, err := original.Open(ctx, in.SourceDSN)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	fresh, err := src.Fingerprint(ctx)
	if err != nil {
		return nil, verificationFatal("preflight", 0, err)
	}
	srcID, err := src.Identity(ctx)
	if err != nil {
		return nil, verificationFatal("preflight", 0, fmt.Errorf("read source identity: %w", err))
	}
	same, ok := pgidentity.SameDatabase(srcID, cr.Identity)
	if !ok {
		return nil, fmt.Errorf("%w (source identity unreadable)", target.ErrTargetIdentityUnknown)
	}
	if same {
		return nil, target.ErrSameDatabase
	}

	// Drift policy vs the recorded import fingerprint.
	var importFP original.Fingerprint
	if err := json.Unmarshal(prog.FingerprintRaw, &importFP); err != nil {
		return nil, verificationFatal("preflight", 0, fmt.Errorf("recorded source fingerprint: %w", err))
	}
	var previousFP *original.Fingerprint
	if !fresh.Equivalent(importFP) {
		if !in.AcceptSourceRepairs {
			return nil, fmt.Errorf("%w (source torrents %d→%d, id range [%d,%d]→[%d,%d]; repair the source or pass --accept-source-repairs)",
				ErrSourceDrift, importFP.Torrents, fresh.Torrents,
				importFP.MinID, importFP.MaxID, fresh.MinID, fresh.MaxID)
		}
		// Stable-evidence match: database name and unchanged ID bounds —
		// never postmaster start time or transport fields. This is an
		// operator assertion, not proof of identity (documented limitation).
		if fresh.Database != importFP.Database || fresh.MinID != importFP.MinID || fresh.MaxID != importFP.MaxID {
			return nil, fmt.Errorf("%w (stable evidence mismatch: database %q→%q, id range [%d,%d]→[%d,%d])",
				ErrSourceDrift, importFP.Database, fresh.Database,
				importFP.MinID, importFP.MaxID, fresh.MinID, fresh.MaxID)
		}
		previousFP = &importFP
	}

	// Lock-connection liveness: loss is fatal, never continue unprotected.
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)
	monitorCtx, stopMonitor := context.WithCancel(context.WithoutCancel(runCtx))
	defer stopMonitor()
	go cr.Lock.MonitorLockLoss(monitorCtx, func(err error) {
		cancelRun(fmt.Errorf("run lock connection lost: %w", err))
	})

	unresolvedAtStart, err := state.CountUnresolvedRecords(runCtx, pool, prog.RunID)
	if err != nil {
		return nil, verificationFatal("preflight", 0, err)
	}
	fingerprintRaw, err := json.Marshal(fresh)
	if err != nil {
		return nil, err
	}
	var previousRaw json.RawMessage
	if previousFP != nil {
		b, merr := json.Marshal(previousFP)
		if merr != nil {
			return nil, merr
		}
		previousRaw = b
	}
	settings, err := json.Marshal(map[string]any{
		"workers":               in.Workers,
		"batch_size":            in.BatchSize,
		"max_inflight_bytes":    in.MaxInflightBytes,
		"accept_source_repairs": in.AcceptSourceRepairs,
	})
	if err != nil {
		return nil, err
	}

	// Adopt-or-new with the revision-tracked resume discipline.
	run, resumed, err := state.StartRetryRun(runCtx, pool, state.CreateRetryRunInput{
		RunID:                  prog.RunID,
		StartDataRevision:      prog.DataRevision,
		UnresolvedAtStart:      unresolvedAtStart,
		SourceFingerprintRaw:   fingerprintRaw,
		AcceptedSourceRepair:   previousFP != nil,
		PreviousFingerprintRaw: previousRaw,
		SettingsRaw:            settings,
	}, func(c *state.RetryRun) bool {
		if c.CurrentDataRevision != prog.DataRevision {
			return false
		}
		var storedFP original.Fingerprint
		if err := json.Unmarshal(c.SourceFingerprintRaw, &storedFP); err != nil {
			return false
		}
		return fresh.Equivalent(storedFP)
	})
	if err != nil {
		if cause := fatalCause(runCtx); cause != nil {
			return nil, cause
		}
		if runCtx.Err() != nil {
			return nil, ErrInterrupted
		}
		return nil, err
	}

	result := &RetryResult{
		RetryID:              run.RetryID,
		Resumed:              resumed,
		Attempted:            run.AttemptedCount,
		Fixed:                run.FixedCount,
		StillInvalid:         run.StillInvalidCount,
		Missing:              run.MissingCount,
		UnresolvedAtStart:    run.UnresolvedAtStart,
		AcceptedSourceRepair: run.AcceptedSourceRepair,
	}
	// Mirrors: the commit CAS inputs come from the run's own durable state
	// (resume-cumulative); they resync from the authoritative row after an
	// ambiguous-commit reconciliation.
	cursor := run.LastSourceID
	revision := run.CurrentDataRevision
	report := func() {
		if in.Progress != nil {
			cursorID := int64(0)
			if cursor != nil {
				cursorID = *cursor
			}
			in.Progress(RetryProgress{
				Phase: "retrying", Resumed: resumed,
				Attempted: result.Attempted, Fixed: result.Fixed,
				StillInvalid: result.StillInvalid, Missing: result.Missing,
				Cursor:            cursorID,
				UnresolvedAtStart: result.UnresolvedAtStart,
			})
		}
	}
	report()

	wlimits := original.WindowLimits{
		MaxFiles:       limits.MaxFiles,
		MaxPathBytes:   limits.MaxAggregatePathBytes,
		WindowMaxBytes: in.MaxInflightBytes,
	}
	processor := pipeline.NewProcessor(limits, codec.DefaultProfile(), in.Workers, true)
	// The registry page cursor starts below every possible source ID (the
	// pass cursor doubles as the keyset boundary once attempts begin).
	afterID := prog.SourceMinID - 1
	if cursor != nil {
		afterID = *cursor
	}

	// interruptRetry marks the pass interrupted (resumable) on a graceful
	// shutdown. Resume inherently reconciles ambiguous attempts: a landed
	// commit's cursor includes the ID, a rolled-back one is re-attempted.
	interruptRetry := func() error {
		ictx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), dbOpTimeout)
		defer cancel()
		if err := state.MarkRetryInterrupted(ictx, pool, run.RetryID); err != nil {
			return fmt.Errorf("%w (marking the pass interrupted also failed: %s)",
				ErrInterrupted, sanitizeDetail(err.Error()))
		}
		return ErrInterrupted
	}

	// settle attempts one ID exactly once: build the classification exactly
	// like the import adapter, run the production Processor, and commit the
	// outcome in its single transaction — with ambiguous-COMMIT
	// reconciliation (attempt row present ⇒ committed; provably not landed
	// ⇒ replay ≤ maxCommitReplays; anything else fatal).
	settle := func(sourceID int64, row *original.TorrentRow, files []original.FileRow, ov *original.Overflow) error {
		rec, readFailure := retryBuildRecord(row, files, ov, wlimits)

		commitKind := "fixed"
		commit := target.RetryAttemptCommit{
			RetryID: run.RetryID, RunID: prog.RunID,
			Record: target.RetryRecord{SourceID: sourceID},
		}
		switch {
		case row == nil:
			// The original record disappeared from the source.
			commitKind = "missing"
			commit.Stage, commit.Code = pipeline.StageRead, "missing_source"
			commit.Details = map[string]any{"reason": "source torrent row disappeared"}
		case readFailure != nil:
			commitKind = "invalid"
			commit.Record.InfoHash = readFailure.InfoHash
			commit.Stage, commit.Code, commit.Details = readFailure.Stage, readFailure.Code, readFailure.Details
		default:
			success, pfail := processor.Process(rec)
			if pfail != nil {
				commitKind = "invalid"
				commit.Record.InfoHash = pfail.InfoHash
				commit.Stage, commit.Code, commit.Details = pfail.Stage, pfail.Code, pfail.Details
			} else {
				commit.Record = target.RetryRecord{
					SourceID:     sourceID,
					InfoHash:     success.Record.InfoHash,
					Name:         success.Record.Name,
					TotalSize:    success.Record.TotalSize,
					DiscoveredOn: success.Record.DiscoveredOn,
					FileCount:    int32(len(success.Record.Files)),
					Blob:         success.Blob,
				}
			}
		}
		commitFn := func() error {
			commit.ExpectedLastSourceID = cursor
			switch commitKind {
			case "fixed":
				return target.CommitRetrySuccess(runCtx, pool, commit)
			case "invalid":
				return target.CommitRetryInvalid(runCtx, pool, commit)
			default:
				return target.CommitRetryMissing(runCtx, pool, commit)
			}
		}

		for attemptNo := 0; ; attemptNo++ {
			cerr := commitFn()
			if cerr == nil {
				break // settled
			}
			if cause := fatalCause(runCtx); cause != nil {
				return cause
			}
			if runCtx.Err() != nil {
				return interruptRetry()
			}
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(context.Background()), dbOpTimeout)
			rc, rerr := target.ReconcileRetryAttempt(rctx, pool, target.RetryReconcileInput{
				RetryID: run.RetryID, RunID: prog.RunID, SourceID: sourceID,
				ExpectedDataRevision: revision,
			})
			rcancel()
			if rerr != nil {
				return verificationFatal("retry", sourceID,
					fmt.Errorf("ambiguous attempt commit (%v): %w", sanitizeDetail(cerr.Error()), rerr))
			}
			switch {
			case rc.Committed:
				// Landed: resync the mirrors from the authoritative row.
				gctx, gcancel := context.WithTimeout(context.WithoutCancel(context.Background()), dbOpTimeout)
				freshRun, gerr := state.GetRetryRun(gctx, pool, run.RetryID)
				gcancel()
				if gerr != nil || freshRun == nil {
					return verificationFatal("retry", sourceID, fmt.Errorf("resync after ambiguous commit: %v", gerr))
				}
				cursor = freshRun.LastSourceID
				revision = freshRun.CurrentDataRevision
				result.Attempted = freshRun.AttemptedCount
				result.Fixed = freshRun.FixedCount
				result.StillInvalid = freshRun.StillInvalidCount
				result.Missing = freshRun.MissingCount
				return nil
			case rc.Replayable && attemptNo+1 < maxCommitReplays:
				continue
			default:
				return verificationFatal("retry", sourceID, fmt.Errorf(
					"attempt for source %d neither committed nor replayable: %w", sourceID, cerr))
			}
		}
		// Settled normally: advance the mirrors (every outcome bumps the
		// pass revision and cursor by construction of the commit).
		cursor = &sourceID
		revision++
		result.Attempted++
		switch commitKind {
		case "fixed":
			result.Fixed++
		case "invalid":
			result.StillInvalid++
		default:
			result.Missing++
		}
		return nil
	}

	// Batch loop: keyset pages of the unresolved registry (re-queried per
	// batch — resolving IDs shrinks the set; still-invalid IDs stay behind
	// the pass cursor and are never re-attempted in this pass).
	for {
		if runCtx.Err() != nil {
			if cause := fatalCause(runCtx); cause != nil {
				return nil, cause
			}
			return nil, interruptRetry()
		}
		ids, err := state.UnresolvedRecordIDs(runCtx, pool, prog.RunID, afterID, in.BatchSize)
		if err != nil {
			if cause := fatalCause(runCtx); cause != nil {
				return nil, cause
			}
			if runCtx.Err() != nil {
				return nil, interruptRetry()
			}
			return nil, verificationFatal("retry", derefInt64(cursor), err)
		}
		if len(ids) == 0 {
			break // attempted everything still unresolved within the pass
		}

		// Fetch the batch set-oriented (budget-split via `remaining`) and
		// settle each window immediately, ascending — the batch's source
		// rows never accumulate beyond one window (the window budget bounds
		// them; nothing piles the whole batch in memory). Registry IDs the
		// source stops returning (deleted between the registry query and the
		// fetch) settle as missing_source, interleaved below each fetched ID
		// so the pass cursor never moves backward: FetchByIDs returns
		// ascending prefixes with the ascending suffix in `remaining`, so an
		// unsettled registry ID below the current fetched ID has had its
		// chance in an earlier window and is genuinely absent.
		next := 0 // first ids index not yet settled
		todo := ids
		for len(todo) > 0 {
			w, remaining, err := src.FetchByIDs(runCtx, todo, wlimits)
			if err != nil {
				if cause := fatalCause(runCtx); cause != nil {
					return nil, cause
				}
				if runCtx.Err() != nil {
					return nil, interruptRetry()
				}
				return nil, verificationFatal("retry", derefInt64(cursor), err)
			}
			for i := range w.Torrents {
				t := &w.Torrents[i]
				id := int64(t.ID)
				for next < len(ids) && ids[next] < id {
					// Absent from the source: explicit missing_source — the
					// disappearance is diagnosed, never silently excused.
					if err := settle(ids[next], nil, nil, nil); err != nil {
						if errors.Is(err, ErrInterrupted) || fatalCause(runCtx) != nil {
							return nil, err
						}
						return nil, err
					}
					report()
					next++
				}
				var ov *original.Overflow
				if o, has := w.Overflow[t.ID]; has {
					ov = &o
				}
				if err := settle(id, t, w.Files[t.ID], ov); err != nil {
					if errors.Is(err, ErrInterrupted) || fatalCause(runCtx) != nil {
						return nil, err
					}
					return nil, err
				}
				afterID = id
				report()
				next++
			}
			todo = remaining
		}
		// Registry IDs after the last window (or every ID, when the source
		// returned nothing): missing_source, ascending.
		for next < len(ids) {
			if err := settle(ids[next], nil, nil, nil); err != nil {
				if errors.Is(err, ErrInterrupted) || fatalCause(runCtx) != nil {
					return nil, err
				}
				return nil, err
			}
			afterID = ids[next]
			report()
			next++
		}
	}

	// Close the pass and report the remaining unresolved count.
	remaining, err := state.CountUnresolvedRecords(runCtx, pool, prog.RunID)
	if err != nil {
		if cause := fatalCause(runCtx); cause != nil {
			return nil, cause
		}
		if runCtx.Err() != nil {
			return nil, interruptRetry()
		}
		return nil, verificationFatal("retry", derefInt64(cursor), err)
	}
	finished, err := state.FinishRetryRun(runCtx, pool, run.RetryID, remaining == 0)
	if err != nil {
		if cause := fatalCause(runCtx); cause != nil {
			return nil, cause
		}
		if runCtx.Err() != nil {
			return nil, interruptRetry()
		}
		return nil, verificationFatal("retry", derefInt64(cursor), err)
	}
	result.State = finished.State
	result.UnresolvedRemaining = remaining

	// The phase flip fires when everything resolved (the gate inside
	// CompleteRetryPhase enforces zero unresolved record AND integrity
	// errors); it never auto-clears orphan incidents.
	flipped, err := target.CompleteRetryPhase(runCtx, pool, prog.RunID)
	if err != nil {
		return nil, verificationFatal("retry", derefInt64(cursor), err)
	}
	result.PhaseFlipped = flipped

	report()
	if remaining > 0 {
		return result, fmt.Errorf("%w (%d still unresolved; pass %s)", ErrRetryRemaining, remaining, run.RetryID)
	}
	return result, nil
}

// retryBuildRecord mirrors the import adapter's conversion for one fetched
// row: read-stage failures (ID range, read overflow, no files) are
// pre-settled as failures; a buildable row returns the neutral Record.
// row == nil means the source record disappeared (missing_source).
func retryBuildRecord(row *original.TorrentRow, files []original.FileRow, ov *original.Overflow, wlimits original.WindowLimits) (*pipeline.Record, *pipeline.Failure) {
	if row == nil {
		return nil, nil
	}
	switch {
	case row.ID <= 0:
		return nil, &pipeline.Failure{
			SourceID: int64(row.ID), InfoHash: row.InfoHash,
			Stage: pipeline.StageRead, Code: pipeline.CodeSourceIntegrity,
			Details: map[string]any{"reason": "source ID out of the supported positive range"},
		}
	case ov != nil:
		code, limitName, allowed := pipeline.CodeFileCountExceeded, original.OverflowFileCount, int64(wlimits.MaxFiles)
		if ov.Reason == original.OverflowPathBytes {
			code, limitName, allowed = pipeline.CodeCodecLimit, original.OverflowPathBytes, int64(wlimits.MaxPathBytes)
		}
		return nil, &pipeline.Failure{
			SourceID: int64(row.ID), InfoHash: row.InfoHash,
			Stage: pipeline.StageRead, Code: code,
			Details: map[string]any{"limit": limitName, "actual": ov.Observed, "allowed": allowed},
		}
	case len(files) == 0:
		return nil, &pipeline.Failure{
			SourceID: int64(row.ID), InfoHash: row.InfoHash,
			Stage: pipeline.StageRead, Code: pipeline.CodeMissingFiles,
		}
	}
	rec := &pipeline.Record{
		SourceID:     row.ID,
		InfoHash:     row.InfoHash,
		Name:         row.Name,
		TotalSize:    row.TotalSize,
		DiscoveredOn: row.DiscoveredOn,
		Files:        make([]pipeline.FileEntry, len(files)),
	}
	for i, f := range files {
		rec.Files[i] = pipeline.FileEntry{Path: f.Path, Size: f.Size, SourceFileID: f.ID}
	}
	return rec, nil
}
