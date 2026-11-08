package migration

// verify-content (04B): the full source↔target equality gate. Both
// databases are scanned independently with ascending-ID keyset pagination
// (source via WindowSnapshot in bounded REPEATABLE READ READ ONLY
// transactions; target via the same metadata pages and octet_length-first
// blob fetches as verify-decode) and merged in one ascending-ID walk — no
// global ID map, O(1) memory beyond one bounded window/page.
//
// Matched IDs compare exact info_hash/name/total_size/discovered_on and the
// complete canonical (path,size) multiset including duplicates — byte-exact,
// no normalization, never recompressed bytes. The target side of a matched
// pair is the decoded blob, or the reconstructed no-blob single; a source
// file list that cannot fit production limits is drained and classified
// not-representable (unverifiable) — never claimed compared. Source-only IDs
// classify as unresolved-record (coverage) when the 04A registry still holds
// the ID, else missing-target (mismatch): an existing source error never
// excuses a missing row from the equality gate. Target-only IDs are extra
// rows (mismatch). Every ID on both sides is accounted for, including gaps,
// no-file torrents and dangling/NULL-owned source file rows (aggregate
// integrity finding at finalize).
//
// Fingerprint policy (directives ④⑤⑦): the fresh source fingerprint is
// compared with Fingerprint.Equivalent — never byte-equal JSON — so a
// PostgreSQL restart or a TCP↔socket switch neither aborts resume nor
// fabricates drift. Content-anchor drift vs the recorded IMPORT fingerprint
// is one source_fingerprint_drift issue with severity info: provenance,
// reported, never counted into pass/fail — only actual content mismatches,
// unresolved integrity problems and incomplete coverage fail the pass. The
// same Equivalent comparison gates resume against the run's own recorded
// fingerprint; a changed source between sessions aborts the old run (fresh
// invocation, history preserved).
//
// Verification findings never enter the 04A errors registry; the source is
// never written (the adapter's connections are session read-only).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/pgidentity"
	"github.com/skobkin/magnetico-web/internal/migration/pipeline"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
)

// Verification issue stages and stable codes (content pass; severities in
// parentheses). Stages/codes shared with the decode pass are reused.
const (
	IssueStageContent = "content"

	IssueCodeContentMismatch  = "content_mismatch"            // mismatch
	IssueCodeMissingTargetRow = "missing_target_row"          // mismatch
	IssueCodeExtraTargetRow   = "extra_target_row"            // mismatch
	IssueCodeUnresolvedRecord = "unresolved_record_error"     // coverage
	IssueCodeNotRepresentable = "source_not_representable"    // unverifiable
	IssueCodeSourceDrift      = "source_fingerprint_drift"    // info
	IssueCodeSourceIntegrity  = "source_integrity_unresolved" // coverage
	IssueCodeSourceGrew       = "source_row_count_changed"    // coverage
	// Aggregate finalize finding: unresolved record-scope registry entries
	// with no per-row unresolved_record_error issue — their source rows are
	// gone, so the merge never saw them.
	IssueCodeUnresolvedCoverage = "unresolved_record_coverage" // coverage
)

// registryPageSize bounds one keyset page of unresolved-registry IDs held
// in memory during the merge.
const registryPageSize = 512

// VerifyContentInput configures one verify-content pass.
type VerifyContentInput struct {
	SourceDSN string
	TargetDSN string

	Workers          int
	BatchSize        int
	MaxInflightBytes int64

	// Limits overrides codec.DefaultLimits() (tests exercise oversize paths
	// with tiny fixtures); production leaves it nil.
	Limits *codec.Limits

	// Progress receives cumulative snapshots after every committed unit.
	Progress func(VerifyProgress)
}

// VerifyContent runs one full source↔target equality pass: completed-run
// preflight, source identity/distinctness checks, the ascending-ID merge
// scan with per-unit durable commits, the extras-phase orphan check and
// exact coverage accounting with a durable verdict.
//
// Error taxonomy: typed preflight refusals (ErrRunNotComplete,
// target.ErrSameDatabase, corrupt state), *FatalError (exit 1),
// ErrInterrupted (exit 2), and ErrVerificationFindings after a COMPLETED
// scan with findings (exit 3) — the result is returned alongside in every
// completed case.
func VerifyContent(ctx context.Context, in VerifyContentInput) (*VerificationResult, error) {
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

	// Source: opened read-only (session default), fingerprinted, and
	// proven DISTINCT from the target before anything is compared (the same
	// fail-closed pgidentity policy as the import preflight).
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

	// The recorded import fingerprint: drift vs it is provenance (info),
	// never a pass blocker. Unreadable run state is corruption.
	var importFP original.Fingerprint
	if err := json.Unmarshal(prog.FingerprintRaw, &importFP); err != nil {
		return nil, verificationFatal("preflight", 0, fmt.Errorf("recorded source fingerprint: %w", err))
	}
	drift := !fresh.Equivalent(importFP)

	// Lock-connection liveness: loss is fatal, never continue unprotected.
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)
	monitorCtx, stopMonitor := context.WithCancel(context.WithoutCancel(runCtx))
	defer stopMonitor()
	go cr.Lock.MonitorLockLoss(monitorCtx, func(err error) {
		cancelRun(fmt.Errorf("run lock connection lost: %w", err))
	})

	var expectedTargets int64
	if err := pool.QueryRow(runCtx,
		`SELECT COUNT(*) FROM magneticod.torrents`).Scan(&expectedTargets); err != nil {
		return nil, verificationFatal("scan", 0, err)
	}
	identityRaw, err := json.Marshal(srcID)
	if err != nil {
		return nil, err
	}
	fingerprintRaw, err := json.Marshal(fresh)
	if err != nil {
		return nil, err
	}
	settings, err := json.Marshal(map[string]any{
		"workers":                  in.Workers,
		"batch_size":               in.BatchSize,
		"max_inflight_bytes":       in.MaxInflightBytes,
		"source_fingerprint_drift": drift,
	})
	if err != nil {
		return nil, err
	}

	// Adopt-or-new. Resume compatibility: same data revision, a non-complete
	// phase, and the run's own recorded source fingerprint Equivalent to the
	// fresh one — a source change between sessions aborts the old run
	// (directive ⑦: Equivalent, never byte-equal JSON).
	run, resumed, err := state.StartVerification(runCtx, pool, state.CreateVerificationInput{
		RunID:                prog.RunID,
		Kind:                 state.VerificationKindContent,
		StartDataRevision:    prog.DataRevision,
		SourceIdentityRaw:    identityRaw,
		SourceFingerprintRaw: fingerprintRaw,
		ExpectedSourceIDs:    &fresh.Torrents,
		ExpectedTargetIDs:    &expectedTargets,
		SettingsRaw:          settings,
	}, func(c *state.VerificationRun) bool {
		if c.StartDataRevision != prog.DataRevision ||
			c.Phase == state.VerificationPhaseComplete {
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

	result := &VerificationResult{
		Kind:              state.VerificationKindContent,
		VerificationID:    run.VerificationID,
		Seq:               run.Seq,
		Resumed:           resumed,
		StartDataRevision: run.StartDataRevision,
		ExpectedTargets:   run.ExpectedTargetIDs,
	}
	targetAfter := derefOrZero(run.TargetCheckpoint)
	srcStart := prog.SourceMinID - 1
	srcReadCursor := srcStart
	if run.SourceCheckpoint != nil {
		srcReadCursor = *run.SourceCheckpoint
	}
	// expectedSource/expectedTarget mirror the STORED checkpoints for the
	// commit CAS (nil until the first commit on each side — never conflate
	// "no checkpoint" with 0).
	expectedSource := run.SourceCheckpoint
	expectedTarget := run.TargetCheckpoint
	m := newDecodeMirrors(run)
	// sourceObserved mirror: durable cumulative value (resumed runs carry
	// it; fresh runs start at 0). newDecodeMirrors read the column already.
	var driftIssue *state.VerificationIssue
	if drift {
		driftIssue = &state.VerificationIssue{
			Code: IssueCodeSourceDrift, Stage: IssueStageContent, Severity: state.IssueInfo,
			Details: map[string]any{
				"reason":          "source content anchors changed since the recorded import fingerprint",
				"stored_torrents": importFP.Torrents, "fresh_torrents": fresh.Torrents,
				"stored_max_id": importFP.MaxID, "fresh_max_id": fresh.MaxID,
			},
		}
	}
	driftEmitted := false
	withDrift := func(issues []state.VerificationIssue) []state.VerificationIssue {
		if driftIssue != nil && !driftEmitted {
			issues = append(issues, *driftIssue)
			driftEmitted = true
		}
		return issues
	}
	report := func() {
		if in.Progress != nil {
			in.Progress(VerifyProgress{
				Kind: result.Kind, Phase: m.phase, Resumed: resumed,
				Checkpoint:      targetAfter,
				ExpectedTargets: derefInt64(run.ExpectedTargetIDs),
				Observed:        m.observed(), Verified: m.verified, Mismatched: m.mismatch,
				Unverifiable: m.unverifiable, AssumedSingle: m.assumed, Issues: m.issues,
				SourceObserved: m.sourceObserved,
			})
		}
	}
	report()

	// A run already past its scan (adopted with phase extras) committed its
	// orphan finding durably — restore the result field from the persisted
	// issue so the summary matches history (Codex review iteration 2,
	// finding 4).
	if run.Phase == state.VerificationPhaseExtras {
		orphans, err := state.OrphanBlobIssueCount(runCtx, pool, run.VerificationID)
		if err != nil {
			if cause := fatalCause(runCtx); cause != nil {
				return nil, cause
			}
			if runCtx.Err() != nil {
				return nil, interruptScan(runCtx, pool, run)
			}
			return nil, verificationFatal("extras", derefInt64(run.SourceCheckpoint), err)
		}
		result.OrphanTargetBlobs = orphans
		report()
	}

	// The unresolved-registry iterator: keyset pages of unresolved
	// record-scope IDs, walked alongside the merge. It starts at the same
	// cursor the merge resumes from — every registry entry at or below the
	// source checkpoint was already consumed (and committed with its
	// unresolved_record_error issue) by the pre-interrupt portion of the
	// scan, so a resumed pass must not see those entries again. Buffered
	// error: checked before every commit — an infrastructure failure never
	// commits.
	reg := &registryIter{pool: pool, runID: prog.RunID, afterID: srcReadCursor}

	// commitUnit commits one accumulated unit atomically: its issues,
	// counter deltas, both compare-and-swap checkpoints and the cumulative
	// coverage mirrors. newTarget/newSource are the post-commit checkpoint
	// values; passing the current expected pointer keeps that side
	// unchanged (nil-safe: a side never committed stays NULL). Every
	// merge/drain commit goes through here, including the bounded mid-page
	// and mid-window flushes — one commit shape, one reconciliation path.
	commitUnit := func(unit *contentUnitResult, newTarget, newSource *int64) error {
		unit.issues = withDrift(unit.issues)
		candSource := m.sourceObserved + unit.sourceObserved
		candTarget := m.observed() + unit.verified + unit.mismatch + unit.unverifiable + unit.assumed
		committed, err := state.CommitVerificationPage(runCtx, pool, state.VerificationPageCommit{
			VerificationID: run.VerificationID,
			Issues:         unit.issues,
			VerifiedDelta:  unit.verified, MismatchDelta: unit.mismatch,
			UnverifiableDelta:   unit.unverifiable,
			AssumedSingleDelta:  unit.assumed,
			SourceCheckpoint:    expectedSource,
			TargetCheckpoint:    expectedTarget,
			NewSourceCheckpoint: newSource, NewTargetCheckpoint: newTarget,
			NewObservedSourceIDs: &candSource,
			NewObservedTargetIDs: &candTarget,
		})
		if err != nil {
			return err
		}
		if !committed {
			// Under the exclusive lock a lost CAS means our own commit
			// landed but the acknowledgement was lost (ambiguous COMMIT).
			freshRow, gerr := state.GetVerification(runCtx, pool, run.VerificationID)
			if gerr != nil {
				return gerr
			}
			if !pageCommitted(freshRow, newSource, newTarget) {
				return fmt.Errorf(
					"verification checkpoint moved underneath the scan (run %s)", run.VerificationID)
			}
			m.resync(freshRow)
		} else {
			m.applyContent(*unit)
		}
		targetAfter = derefInt64(newTarget)
		expectedTarget = newTarget
		expectedSource = newSource
		result.ObservedTargets = m.observed()
		report()
		return nil
	}
	// scanFatal wraps a commitUnit error with the standard fatal/interrupt
	// handling: a lock-loss cause wins, a cancelled context interrupts
	// (resumable), everything else is infrastructure failure.
	scanFatal := func(stage string, err error) (*VerificationResult, error) {
		if cause := fatalCause(runCtx); cause != nil {
			return nil, cause
		}
		if runCtx.Err() != nil {
			return nil, interruptScan(runCtx, pool, run)
		}
		return nil, verificationFatal(stage, targetAfter, err)
	}

	if run.Phase == state.VerificationPhaseScanning {
		// Merge-scan state: one buffered source window (bounded by the
		// window byte budget), read-on-demand by the merge; buf rows are
		// classified exactly once and never re-read (the read cursor only
		// advances past absorbed windows, the COMMIT cursor only past
		// classified rows).
		var (
			buf         []original.TorrentRow
			bufFiles    map[int32][]original.FileRow
			bufOverflow map[int32]original.Overflow
			orphanSeen  int64
			sourceDone  bool
		)
		absorb := func(w *original.Window) {
			if bufFiles == nil {
				bufFiles = map[int32][]original.FileRow{}
				bufOverflow = map[int32]original.Overflow{}
			}
			buf = append(buf, w.Torrents...)
			for id, rows := range w.Files {
				bufFiles[id] = rows
			}
			for id, ov := range w.Overflow {
				bufOverflow[id] = ov
			}
			orphanSeen += w.OrphanFiles
			if len(w.Torrents) > 0 {
				srcReadCursor = int64(w.Torrents[len(w.Torrents)-1].ID)
			}
		}
		// pull absorbs one bounded window when the buffer is empty.
		pull := func() error {
			if sourceDone || len(buf) > 0 {
				return nil
			}
			w, err := src.WindowSnapshot(runCtx, srcReadCursor, in.BatchSize, original.WindowLimits{
				MaxFiles:       limits.MaxFiles,
				MaxPathBytes:   limits.MaxAggregatePathBytes,
				WindowMaxBytes: in.MaxInflightBytes,
			})
			if err != nil {
				return err
			}
			if len(w.Torrents) == 0 {
				sourceDone = true
				return nil
			}
			absorb(w)
			return nil
		}

		for {
			if runCtx.Err() != nil {
				return nil, interruptScan(runCtx, pool, run)
			}
			tp, err := readDecodePage(runCtx, pool, targetAfter, in.BatchSize)
			if err != nil {
				if cause := fatalCause(runCtx); cause != nil {
					return nil, cause
				}
				if runCtx.Err() != nil {
					return nil, interruptScan(runCtx, pool, run)
				}
				return nil, verificationFatal("scan", targetAfter, err)
			}
			if len(tp.rows) == 0 {
				break // scanned through the last target row
			}
			pageMax := tp.rows[len(tp.rows)-1].id

			// Decoded target content loads lazily, one budget-bounded chunk
			// at a time as the merge walks the page ascending — never
			// page-wide (the inflight budget bounds raw blob bytes, not the
			// decoded file lists, so a whole decoded page is unbounded).
			content := newPageContent(runCtx, pool, tp, limits, in.MaxInflightBytes)

			unit := &contentUnitResult{issues: []state.VerificationIssue{}}
			lastSrc := int64(0)
			haveSrc := false
			// flushUnit commits the unit accumulated so far. boundary is the
			// target checkpoint after the commit — the certification point a
			// resumed pass re-reads the target from (keyset pagination). It
			// must only ever advance past CLASSIFIED target rows:
			//   - after-tr flushes: the row just classified;
			//   - source-only prefix flushes: the last classified target row
			//     of this page, or the unchanged checkpoint when none yet —
			//     moving an unclassified target row forward would make the
			//     resume re-compare it against a source cursor that has
			//     already moved past its match;
			//   - the page end: the page max (every page row classified).
			flushUnit := func(boundary *int64) error {
				newSource := expectedSource
				if haveSrc {
					// Copy: commitUnit stores this pointer as the CAS
					// expectation for the NEXT flush, so it must never alias
					// the mutable lastSrc (later classifications in the same
					// page would corrupt the expected checkpoint).
					src := lastSrc
					newSource = &src
				}
				if err := commitUnit(unit, boundary, newSource); err != nil {
					return err
				}
				*unit = contentUnitResult{issues: []state.VerificationIssue{}}
				return nil
			}
			// Bounded units: a sparse target over a dense source would
			// otherwise pile the whole source-only prefix into one commit.
			flushAt := func(boundary *int64) error {
				if len(unit.issues) < in.BatchSize {
					return nil
				}
				return flushUnit(boundary)
			}
			lastTr := int64(0)
			haveTr := false
			for idx := range tp.rows {
				tr := &tp.rows[idx]
				// Source rows strictly below the target row: source-only
				// classification (pull windows on demand while the buffer
				// is empty and the source is not exhausted).
				for {
					if !sourceDone && len(buf) == 0 {
						if err := pull(); err != nil {
							if cause := fatalCause(runCtx); cause != nil {
								return nil, cause
							}
							if runCtx.Err() != nil {
								return nil, interruptScan(runCtx, pool, run)
							}
							return nil, verificationFatal("scan", targetAfter, err)
						}
						if sourceDone || len(buf) == 0 {
							break
						}
					}
					if len(buf) == 0 || int64(buf[0].ID) >= tr.id {
						break
					}
					if err := classifySourceOnly(runCtx, &buf[0], unit, reg); err != nil {
						if cause := fatalCause(runCtx); cause != nil {
							return nil, cause
						}
						if runCtx.Err() != nil {
							return nil, interruptScan(runCtx, pool, run)
						}
						return nil, verificationFatal("scan", targetAfter, err)
					}
					lastSrc = int64(buf[0].ID)
					haveSrc = true
					delete(bufFiles, buf[0].ID)
					delete(bufOverflow, buf[0].ID)
					buf = buf[1:]
					// Prefix flush: certify up to the last classified target
					// row (this page's rows live in the same atomic commit),
					// or hold the checkpoint when none classified yet.
					if haveTr {
						b := lastTr
						if err := flushAt(&b); err != nil {
							return scanFatal("scan", err)
						}
					} else if err := flushAt(expectedTarget); err != nil {
						return scanFatal("scan", err)
					}
				}

				// Matched pair or target-only row.
				if len(buf) > 0 && int64(buf[0].ID) == tr.id {
					srcRow := buf[0]
					files := bufFiles[srcRow.ID]
					ov, overflowing := bufOverflow[srcRow.ID]
					delete(bufFiles, srcRow.ID)
					delete(bufOverflow, srcRow.ID)
					buf = buf[1:]
					lastSrc = int64(srcRow.ID)
					haveSrc = true
					tc, err := content.get(idx)
					if err != nil {
						return scanFatal("scan", err)
					}
					classifyMatched(&srcRow, files, ov, overflowing, tr, tc, unit)
				} else {
					// Target-only row (the source has no such ID).
					unit.mismatch++
					unit.issues = append(unit.issues, state.VerificationIssue{
						SourceID: i64ptr(tr.id), InfoHash: tr.infoHash,
						Code: IssueCodeExtraTargetRow, Stage: IssueStageContent, Severity: state.IssueMismatch,
						Details: map[string]any{"reason": "target row has no source torrent"},
					})
				}
				lastTr = tr.id
				haveTr = true
				b := tr.id
				if err := flushAt(&b); err != nil {
					return scanFatal("scan", err)
				}
			}
			// Registry infrastructure failure: never commit past it.
			if reg.err != nil {
				if cause := fatalCause(runCtx); cause != nil {
					return nil, cause
				}
				if runCtx.Err() != nil {
					return nil, interruptScan(runCtx, pool, run)
				}
				return nil, verificationFatal("scan", targetAfter, reg.err)
			}

			if runCtx.Err() != nil {
				return nil, interruptScan(runCtx, pool, run)
			}

			// Close the page: the target checkpoint always moves to the
			// page boundary; the source side only when source rows were
			// classified since the last flush. A flush that already fired
			// at the last row makes this a harmless checkpoint no-op.
			if err := flushUnit(&pageMax); err != nil {
				return scanFatal("scan", err)
			}
		}

		// Drain phase: source IDs beyond the last target row. Windows are
		// absorbed one at a time and committed in bounded units; every row
		// classifies source-only.
		for !sourceDone {
			if runCtx.Err() != nil {
				return nil, interruptScan(runCtx, pool, run)
			}
			if len(buf) == 0 {
				if err := pull(); err != nil {
					if cause := fatalCause(runCtx); cause != nil {
						return nil, cause
					}
					if runCtx.Err() != nil {
						return nil, interruptScan(runCtx, pool, run)
					}
					return nil, verificationFatal("scan", targetAfter, err)
				}
				if sourceDone || len(buf) == 0 {
					break
				}
			}
			unit := &contentUnitResult{issues: []state.VerificationIssue{}}
			windowLast := int64(buf[len(buf)-1].ID)
			// flushDrain commits the unit accumulated so far. The target
			// checkpoint never moves during the drain; the source checkpoint
			// moves to the last classified row.
			flushDrain := func(boundary int64) error {
				if err := commitUnit(unit, expectedTarget, &boundary); err != nil {
					return err
				}
				*unit = contentUnitResult{issues: []state.VerificationIssue{}}
				return nil
			}
			for len(buf) > 0 {
				if err := classifySourceOnly(runCtx, &buf[0], unit, reg); err != nil {
					if cause := fatalCause(runCtx); cause != nil {
						return nil, cause
					}
					if runCtx.Err() != nil {
						return nil, interruptScan(runCtx, pool, run)
					}
					return nil, verificationFatal("scan", targetAfter, err)
				}
				rowID := int64(buf[0].ID)
				delete(bufFiles, buf[0].ID)
				delete(bufOverflow, buf[0].ID)
				buf = buf[1:]
				if len(unit.issues) >= in.BatchSize {
					if err := flushDrain(rowID); err != nil {
						return scanFatal("scan", err)
					}
				}
			}
			if reg.err != nil {
				if cause := fatalCause(runCtx); cause != nil {
					return nil, cause
				}
				if runCtx.Err() != nil {
					return nil, interruptScan(runCtx, pool, run)
				}
				return nil, verificationFatal("scan", targetAfter, reg.err)
			}
			// Close the window (a flush at the last row makes this a no-op).
			if err := flushDrain(windowLast); err != nil {
				return scanFatal("scan", err)
			}
			buf = nil
			bufFiles, bufOverflow = nil, nil
			result.SourceObserved = m.sourceObserved
			report()
		}

		// Extras phase: orphan target blobs (identical to the decode pass),
		// committed atomically with the phase advance.
		orphans, oMin, oMax, err := decodeExtrasScan(runCtx, pool)
		if err != nil {
			if cause := fatalCause(runCtx); cause != nil {
				return nil, cause
			}
			if runCtx.Err() != nil {
				return nil, interruptScan(runCtx, pool, run)
			}
			return nil, verificationFatal("extras", targetAfter, err)
		}
		extraIssues := []state.VerificationIssue{}
		if orphans > 0 {
			extraIssues = append(extraIssues, state.VerificationIssue{
				Code: IssueCodeOrphanBlobs, Stage: IssueStageExtras,
				Severity: state.IssueMismatch,
				Details: map[string]any{
					"count":  orphans,
					"min_id": derefOrZero(oMin), "max_id": derefOrZero(oMax),
				},
			})
		}
		extraIssues = withDrift(extraIssues)
		committed, err := state.CommitVerificationPage(runCtx, pool, state.VerificationPageCommit{
			VerificationID:      run.VerificationID,
			Issues:              extraIssues,
			SourceCheckpoint:    expectedSource,
			TargetCheckpoint:    expectedTarget,
			NewSourceCheckpoint: expectedSource,
			NewTargetCheckpoint: expectedTarget,
			NewPhase:            state.VerificationPhaseExtras,
		})
		if err != nil {
			// Cancellation while the extras commit waits (a lock, a stalled
			// peer) is a resumable interruption, exactly like every scanning
			// branch — not a fatal (Codex review iteration 4, finding 3).
			if cause := fatalCause(runCtx); cause != nil {
				return nil, cause
			}
			if runCtx.Err() != nil {
				return nil, interruptScan(runCtx, pool, run)
			}
			return nil, verificationFatal("extras", targetAfter, err)
		}
		if !committed {
			freshRow, gerr := state.GetVerification(runCtx, pool, run.VerificationID)
			if gerr != nil {
				return nil, verificationFatal("extras", targetAfter, gerr)
			}
			if !pageCommitted(freshRow, expectedSource, expectedTarget) {
				return nil, verificationFatal("extras", targetAfter, fmt.Errorf(
					"verification checkpoint moved underneath the extras scan (run %s)", run.VerificationID))
			}
			m.resync(freshRow)
		} else {
			m.apply(decodePageResult{issues: extraIssues})
			m.phase = state.VerificationPhaseExtras
			result.OrphanTargetBlobs = orphans
		}
		report()
	}

	// Finalize: exact coverage accounting on BOTH sides against fresh
	// counts, the aggregate source-integrity finding, then the durable
	// verdict.
	var nowTargets int64
	if err := pool.QueryRow(runCtx,
		`SELECT COUNT(*) FROM magneticod.torrents`).Scan(&nowTargets); err != nil {
		if cause := fatalCause(runCtx); cause != nil {
			return nil, cause
		}
		if runCtx.Err() != nil {
			return nil, interruptScan(runCtx, pool, run)
		}
		return nil, verificationFatal("coverage", targetAfter, err)
	}
	fpNow, err := src.Fingerprint(runCtx)
	if err != nil {
		if cause := fatalCause(runCtx); cause != nil {
			return nil, cause
		}
		if runCtx.Err() != nil {
			return nil, interruptScan(runCtx, pool, run)
		}
		return nil, verificationFatal("coverage", targetAfter, err)
	}
	regRecord, regIntegrity, err := state.CountErrors(runCtx, pool, prog.RunID)
	if err != nil {
		if cause := fatalCause(runCtx); cause != nil {
			return nil, cause
		}
		if runCtx.Err() != nil {
			return nil, interruptScan(runCtx, pool, run)
		}
		return nil, verificationFatal("coverage", targetAfter, err)
	}
	// Aggregate unresolved-record coverage: every unresolved registry entry
	// needs a per-row unresolved_record_error issue from the merge. An entry
	// whose source row vanished is never seen by the scan — without this
	// check the equality gate could pass over an unaccounted record.
	uncCount, uncMin, uncMax, err := state.UncoveredUnresolvedRecords(runCtx, pool, prog.RunID, run.VerificationID)
	if err != nil {
		if cause := fatalCause(runCtx); cause != nil {
			return nil, cause
		}
		if runCtx.Err() != nil {
			return nil, interruptScan(runCtx, pool, run)
		}
		return nil, verificationFatal("coverage", targetAfter, err)
	}

	observedTarget := m.observed()
	observedSource := m.sourceObserved
	var finalizeIssues []state.VerificationIssue
	if run.ExpectedTargetIDs == nil || observedTarget != *run.ExpectedTargetIDs {
		finalizeIssues = append(finalizeIssues, state.VerificationIssue{
			Code: IssueCodeCoverage, Stage: IssueStageCoverage, Severity: state.IssueCoverage,
			Details: map[string]any{
				"expected": derefInt64(run.ExpectedTargetIDs), "observed": observedTarget,
			},
		})
	} else if nowTargets != *run.ExpectedTargetIDs {
		finalizeIssues = append(finalizeIssues, state.VerificationIssue{
			Code: IssueCodeTargetGrew, Stage: IssueStageCoverage, Severity: state.IssueCoverage,
			Details: map[string]any{"at_start": *run.ExpectedTargetIDs, "at_end": nowTargets},
		})
	}
	if run.ExpectedSourceIDs == nil || observedSource != *run.ExpectedSourceIDs {
		finalizeIssues = append(finalizeIssues, state.VerificationIssue{
			Code: IssueCodeCoverage, Stage: IssueStageCoverage, Severity: state.IssueCoverage,
			Details: map[string]any{
				"side": "source", "expected": derefInt64(run.ExpectedSourceIDs),
				"observed": observedSource,
			},
		})
	} else if fpNow.Torrents != *run.ExpectedSourceIDs {
		finalizeIssues = append(finalizeIssues, state.VerificationIssue{
			Code: IssueCodeSourceGrew, Stage: IssueStageCoverage, Severity: state.IssueCoverage,
			Details: map[string]any{"at_start": *run.ExpectedSourceIDs, "at_end": fpNow.Torrents},
		})
	}
	if fpNow.OrphanFiles > 0 || fpNow.NullOwnerFiles > 0 || regIntegrity > 0 {
		// Run-scope source-integrity problems: an explicit coverage finding
		// (counts only — no names, no paths). Orphan file rows observed
		// during the scan fold into the fresh fingerprint counts at
		// finalize — the CURRENT source state is what the equality gate
		// reports.
		finalizeIssues = append(finalizeIssues, state.VerificationIssue{
			Code: IssueCodeSourceIntegrity, Stage: IssueStageCoverage, Severity: state.IssueCoverage,
			Details: map[string]any{
				"orphan_source_files": fpNow.OrphanFiles,
				"null_owner_files":    fpNow.NullOwnerFiles,
				"registry_integrity":  regIntegrity,
			},
		})
	}
	if uncCount > 0 {
		finalizeIssues = append(finalizeIssues, state.VerificationIssue{
			Code: IssueCodeUnresolvedCoverage, Stage: IssueStageCoverage, Severity: state.IssueCoverage,
			Details: map[string]any{
				"reason":    "unresolved record-scope registry entries with no classified source row",
				"uncovered": uncCount, "min_id": uncMin, "max_id": uncMax,
			},
		})
	}
	if driftIssue != nil && !driftEmitted {
		// The drift finding never made it into a committed unit (e.g. an
		// extras-phase resume): commit it atomically with the verdict.
		finalizeIssues = append(finalizeIssues, *driftIssue)
		driftEmitted = true
	}

	finished, pass, counts, err := finishVerificationOutcome(
		runCtx, pool, run, &observedSource, &observedTarget, finalizeIssues)
	if err != nil {
		if cause := fatalCause(runCtx); cause != nil {
			return nil, cause
		}
		if runCtx.Err() != nil {
			return nil, interruptScan(runCtx, pool, run)
		}
		return nil, verificationFatal("coverage", targetAfter, err)
	}

	result.VerificationID = finished.VerificationID
	result.Seq = finished.Seq
	result.Pass = pass
	result.ObservedTargets = observedTarget
	result.SourceObserved = derefInt64(finished.ObservedSourceIDs)
	result.Verified = finished.VerifiedCount
	result.Mismatched = finished.MismatchCount
	result.Unverifiable = finished.UnverifiableCount
	result.AssumedSingle = finished.AssumedSingleCount
	result.IssuesBySeverity = counts

	// Report-only: the 04A registry counts, stated without conflation.
	result.UnresolvedRecordErrors = regRecord
	result.UnresolvedIntegrity = regIntegrity

	report()
	if !pass {
		return result, fmt.Errorf("%w (%d mismatch, %d coverage, %d unverifiable issues; run seq %d)",
			ErrVerificationFindings,
			counts[state.IssueMismatch], counts[state.IssueCoverage], counts[state.IssueUnverifiable],
			finished.Seq)
	}
	return result, nil
}

// contentUnitResult aggregates one merged unit's verdicts: target-row
// counter deltas, the source rows classified, and the unit's issues.
type contentUnitResult struct {
	issues                           []state.VerificationIssue
	verified, mismatch, unverifiable int64
	assumed                          int64
	sourceObserved                   int64
}

// tpContent is one target row's fetched content state.
type tpContent struct {
	broken  *state.VerificationIssue // structural target anomaly — compare skipped
	files   []codec.File             // decoded content
	err     error                    // decode failure (typed issue)
	missing bool                     // blob vanished between probe and fetch
}

// pageContent lazily fetches and decodes one target page's blob rows in
// ascending row order. Rows that resolve without content (NULL data,
// over-limit blobs) are precomputed as broken findings; blob-backed rows
// are served one SEGMENT at a time and only the CURRENT segment's decoded
// lists stay live. A segment is bounded on BOTH sides: fetched within the
// inflight budget in stored bytes, and cut so the retained decoded file
// lists never exceed the same budget — the stored bound alone cannot cap
// decoded content, because wire-format-v1 payloads are compressed (a 1 KB
// stored blob can legitimately decompress to megabytes; Codex review
// iteration 1, finding 5, and iteration 2, finding 1). A row whose decoded
// list alone exceeds the budget is served solo, bounded by the codec's own
// MaxUncompressedBytes — the read-side analogue of the pipeline's solo
// slot. The merge accesses rows strictly ascending, which the segment
// window follows; a no-fetch row whose index falls between segments
// resolves to nil without disturbing the window.
type pageContent struct {
	ctx         context.Context
	pool        *pgxpool.Pool
	tp          *decodePage
	limits      codec.Limits
	maxInflight int64

	broken  map[int64]*tpContent // content-independent resolutions, by row id
	entries []blobEntry          // fetch+decode rows, ascending page index
	next    int                  // next entry position (first of the next segment)
	cur     map[int64]*tpContent // decoded contents of the loaded segment, by row id
	curLo   int                  // inclusive page-index bound of cur
	curHi   int                  // exclusive page-index bound of cur
}

// newPageContent probes the page's blob rows by octet_length (never
// materializing content) and records the fetch+decode order. No database
// I/O beyond the length probe happens here — content loads on demand.
func newPageContent(ctx context.Context, pool *pgxpool.Pool, tp *decodePage, limits codec.Limits, maxInflight int64) *pageContent {
	pc := &pageContent{
		ctx: ctx, pool: pool, tp: tp, limits: limits,
		maxInflight: maxInflight,
		broken:      map[int64]*tpContent{},
	}
	for i := range tp.rows {
		r := &tp.rows[i]
		if !r.hasBlobRow {
			continue // no-blob representation: nothing to fetch
		}
		switch {
		case r.blobLen == nil:
			pc.broken[r.id] = &tpContent{broken: &state.VerificationIssue{
				SourceID: i64ptr(r.id), InfoHash: r.infoHash,
				Code: IssueCodeBlobNullData, Stage: IssueStageContent, Severity: state.IssueMismatch,
				Details: map[string]any{"reason": "blob data is NULL"},
			}}
		case *r.blobLen > int64(limits.MaxStoredBytes):
			// Never materialize a possibly-corrupted oversized BYTEA.
			pc.broken[r.id] = &tpContent{broken: &state.VerificationIssue{
				SourceID: i64ptr(r.id), InfoHash: r.infoHash,
				Code: IssueCodeBlobOverLimit, Stage: IssueStageContent, Severity: state.IssueMismatch,
				Details: map[string]any{
					"reason":  "stored blob exceeds the codec limit",
					"length":  *r.blobLen,
					"allowed": limits.MaxStoredBytes,
				},
			}}
		default:
			pc.entries = append(pc.entries, blobEntry{idx: i, length: *r.blobLen})
		}
	}
	return pc
}

// get returns the decoded content for the page row at index idx (nil for
// no-blob rows). Each access past the loaded window loads the next serving
// segment, releasing the previous one's decoded lists for garbage
// collection.
func (pc *pageContent) get(idx int) (*tpContent, error) {
	if c, ok := pc.broken[pc.tp.rows[idx].id]; ok {
		return c, nil
	}
	if pc.cur != nil && idx >= pc.curLo && idx < pc.curHi {
		return pc.cur[pc.tp.rows[idx].id], nil
	}
	for pc.next < len(pc.entries) {
		e := pc.entries[pc.next]
		if e.idx < idx {
			// Stale entry (only reachable on non-ascending access).
			pc.next++
			continue
		}
		if e.idx > idx {
			return nil, nil // no fetch entry at or before idx: not blob-backed
		}
		if err := pc.loadSegment(); err != nil {
			return nil, err
		}
		return pc.cur[pc.tp.rows[idx].id], nil
	}
	pc.cur = nil
	return nil, nil
}

// loadSegment fetches and decodes the next serving segment, replacing the
// previous one's decoded lists. The segment is bounded in two steps:
//
// fetch — entries are taken while their cumulative STORED length fits the
// inflight budget; a row whose own length exceeds it starts a solo segment
// (never skipped for scheduling reasons);
//
// decode — the fetched rows are decoded in ascending order and the segment
// is closed BEFORE a row whose decoded list would push the retained DECODED
// bytes past the same budget, so a segment's decoded content stays within
// the budget and a single over-budget row is served solo, bounded by the
// codec's MaxUncompressedBytes. Rows fetched but left undecoded by this
// cut are re-fetched (within the same budget) by the next segment — wasted
// work only when compression makes decoded content far exceed stored
// bytes, never a correctness or retention problem.
//
// Decoding is sequential on purpose: the decoded-side cut needs each row's
// decoded size before admitting the next, and admitting rows concurrently
// would recreate the unbounded retention this segment design exists to
// prevent.
func (pc *pageContent) loadSegment() error {
	start := pc.next
	end := start + 1
	stored := pc.entries[start].length
	for end < len(pc.entries) {
		e := pc.entries[end]
		if e.length > pc.maxInflight || stored+e.length > pc.maxInflight {
			break
		}
		stored += e.length
		end++
	}
	ids := make([]int64, end-start)
	for i := start; i < end; i++ {
		ids[i-start] = pc.tp.rows[pc.entries[i].idx].id
	}
	tx, err := pc.pool.BeginTx(pc.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(pc.ctx)) }()
	rows, err := tx.Query(pc.ctx, `SELECT torrent_id, data FROM magneticod.torrent_file_blobs
        WHERE torrent_id = ANY($1::bigint[])`, ids)
	if err != nil {
		return err
	}
	blobs := make(map[int64][]byte, end-start)
	for rows.Next() {
		var id int64
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return err
		}
		blobs[id] = data
	}
	if err := rows.Err(); err != nil {
		return err
	}

	cur := make(map[int64]*tpContent, end-start)
	var decoded int64
	i := start
	for ; i < end; i++ {
		e := &pc.entries[i]
		c := pc.decodeEntry(e, blobs)
		d := decodedBytes(c)
		if len(cur) > 0 && decoded+d > pc.maxInflight {
			break // the segment ends here; this row opens the next one
		}
		cur[pc.tp.rows[e.idx].id] = c
		decoded += d
	}
	pc.next = i
	pc.cur = cur
	pc.curLo = pc.entries[start].idx
	pc.curHi = pc.entries[i-1].idx + 1
	return nil
}

// decodeEntry resolves one fetched blob row: a vanished blob row (broken
// operator-immutability assumption) fails the row, never guesses.
func (pc *pageContent) decodeEntry(e *blobEntry, blobs map[int64][]byte) *tpContent {
	r := &pc.tp.rows[e.idx]
	blob, ok := blobs[r.id]
	if !ok {
		return &tpContent{missing: true}
	}
	files, err := codec.Decode(blob, pc.limits)
	if err != nil {
		return &tpContent{err: err}
	}
	return &tpContent{files: files}
}

// decodedBytes estimates one decoded row's live memory: the []codec.File
// backing array at slice CAPACITY (24 bytes per entry: 16-byte string
// header + 8-byte size) plus each path's own string data. It is the
// retention currency of a serving segment — the inflight budget bounds
// raw (compressed) blob bytes, which do not cap decoded content. Charging
// capacity, not length, and the full entry size, keeps the estimate honest
// for append-grown slices of tiny paths (Codex review iteration 3,
// finding 2: 30,000 one-byte paths were charged 510 KB while their real
// arrays held ~740 KB).
func decodedBytes(c *tpContent) int64 {
	if c == nil || c.files == nil {
		return 0
	}
	n := int64(cap(c.files)) * 24
	for _, f := range c.files {
		n += int64(len(f.Path))
	}
	return n
}

// classifyMatched compares one matched (source, target) pair: structural
// target anomalies win, then fingerprint drift-class decode failures, then
// the exact metadata + canonical multiset compare. Exactly one issue per
// failing row; overflowed source records are unverifiable, never claimed
// compared.
func classifyMatched(srcRow *original.TorrentRow, files []original.FileRow, ov original.Overflow, overflowing bool, tr *decodeRow, tc *tpContent, unit *contentUnitResult) {
	// The source row of the pair is classified however the comparison goes.
	unit.sourceObserved++
	base := state.VerificationIssue{
		SourceID: i64ptr(tr.id), InfoHash: tr.infoHash, Stage: IssueStageContent,
	}
	// Source overflow: the file list was drained — never comparable.
	if overflowing {
		unit.unverifiable++
		issue := base
		issue.Code = IssueCodeNotRepresentable
		issue.Severity = state.IssueUnverifiable
		issue.Details = map[string]any{
			"reason": "source file list exceeds production limits", "observed": ov.Observed,
		}
		unit.issues = append(unit.issues, issue)
		return
	}
	// Structural target anomalies (never fetched / cannot decode).
	switch {
	case tc == nil:
		// No blob row: the target side is the reconstructed no-blob single.
		srcFiles := make([]codec.File, len(files))
		for i, f := range files {
			srcFiles[i] = codec.File{Path: f.Path, Size: f.Size}
		}
		fields := compareMetadata(srcRow, tr)
		if int64(len(srcFiles)) != tr.fileCount || len(srcFiles) != 1 ||
			srcFiles[0].Path != tr.name || srcFiles[0].Size != tr.totalSize {
			fields = append(fields, "files")
		}
		if len(fields) > 0 {
			unit.mismatch++
			issue := base
			issue.Code = IssueCodeContentMismatch
			issue.Severity = state.IssueMismatch
			issue.Details = map[string]any{
				"fields": fields, "source_files": len(srcFiles), "target_files": 1,
			}
			unit.issues = append(unit.issues, issue)
			return
		}
		unit.verified++
		return
	case tc.broken != nil:
		unit.mismatch++
		unit.issues = append(unit.issues, *tc.broken)
		return
	case tc.missing:
		unit.mismatch++
		issue := base
		issue.Code = IssueCodeBlobVanished
		issue.Severity = state.IssueMismatch
		issue.Details = map[string]any{"reason": "blob row missing at content fetch"}
		unit.issues = append(unit.issues, issue)
		return
	case tc.err != nil:
		unit.mismatch++
		code, details := decodeErrorIssueCode(tc.err)
		issue := base
		issue.Code = code
		issue.Severity = state.IssueMismatch
		issue.Details = details
		unit.issues = append(unit.issues, issue)
		return
	}

	// Full compare: exact metadata fields + complete canonical multiset.
	// The stored file_count is a real target column: the source's complete
	// file list (overflowed records never reach here) must match it exactly
	// like any other metadata field — a tampered count desyncs the row from
	// its blob even when the decoded list itself still compares equal.
	srcFiles := make([]codec.File, len(files))
	for i, f := range files {
		srcFiles[i] = codec.File{Path: f.Path, Size: f.Size}
	}
	fields := compareMetadata(srcRow, tr)
	if int64(len(srcFiles)) != tr.fileCount {
		fields = append(fields, "file_count")
	}
	if !pipeline.EqualCanonical(tc.files, srcFiles) {
		fields = append(fields, "files")
	}
	if len(fields) > 0 {
		unit.mismatch++
		issue := base
		issue.Code = IssueCodeContentMismatch
		issue.Severity = state.IssueMismatch
		issue.Details = map[string]any{
			"fields": fields, "source_files": len(srcFiles), "target_files": len(tc.files),
		}
		unit.issues = append(unit.issues, issue)
		return
	}
	unit.verified++
}

// compareMetadata returns the names of the metadata fields that differ
// (bounded detail — field names only, never values).
func compareMetadata(srcRow *original.TorrentRow, tr *decodeRow) []string {
	var fields []string
	if !bytes.Equal(srcRow.InfoHash, tr.infoHash) {
		fields = append(fields, "info_hash")
	}
	if srcRow.Name != tr.name {
		fields = append(fields, "name")
	}
	if srcRow.TotalSize != tr.totalSize {
		fields = append(fields, "total_size")
	}
	if srcRow.DiscoveredOn != tr.discoveredOn {
		fields = append(fields, "discovered_on")
	}
	return fields
}

// classifySourceOnly classifies a source row absent from the target:
// unresolved-record (coverage) when the 04A registry still holds the ID,
// missing-target (mismatch) otherwise. It never increments target-row
// counters — there is no target row: the mismatch severity lives on the
// issue alone, so the target-side observed coverage stays exact across
// commits and resumes.
func classifySourceOnly(ctx context.Context, srcRow *original.TorrentRow, unit *contentUnitResult, reg *registryIter) error {
	unit.sourceObserved++
	// Stale entries below this row (a vanished source row's entry, or one
	// whose row matched the target) must not block the classification of
	// the row at hand; see registryIter.skipBelow.
	if err := reg.skipBelow(ctx, int64(srcRow.ID)); err != nil {
		return err
	}
	id, ok, err := reg.peek(ctx)
	if err != nil {
		return err
	}
	issue := state.VerificationIssue{
		SourceID: i64ptr(int64(srcRow.ID)), InfoHash: srcRow.InfoHash, Stage: IssueStageContent,
	}
	if ok && id == int64(srcRow.ID) {
		if err := reg.advance(ctx); err != nil {
			return err
		}
		issue.Code = IssueCodeUnresolvedRecord
		issue.Severity = state.IssueCoverage
		issue.Details = map[string]any{"reason": "unresolved record error — no target row"}
	} else {
		issue.Code = IssueCodeMissingTargetRow
		issue.Severity = state.IssueMismatch
		issue.Details = map[string]any{"reason": "source torrent row absent from target"}
	}
	unit.issues = append(unit.issues, issue)
	return nil
}

// registryIter walks the unresolved record-scope registry in ascending ID
// keyset pages. peek returns the smallest unresolved ID greater than every
// advanced ID; errors are buffered in err and surface to the caller (the
// merge aborts before the next commit).
type registryIter struct {
	pool      *pgxpool.Pool
	runID     string
	afterID   int64
	page      []int64
	idx       int
	exhausted bool
	err       error
}

func (it *registryIter) peek(ctx context.Context) (int64, bool, error) {
	if it.err != nil {
		return 0, false, it.err
	}
	if it.idx >= len(it.page) {
		if it.exhausted {
			return 0, false, nil
		}
		page, err := state.UnresolvedRecordIDs(ctx, it.pool, it.runID, it.afterID, registryPageSize)
		if err != nil {
			it.err = err
			return 0, false, err
		}
		if len(page) == 0 {
			it.exhausted = true
			return 0, false, nil
		}
		it.page = page
		it.idx = 0
		it.afterID = page[len(page)-1]
	}
	return it.page[it.idx], true, nil
}

func (it *registryIter) advance(ctx context.Context) error {
	if it.err != nil {
		return it.err
	}
	it.idx++
	return nil
}

// skipBelow advances past registry entries strictly below id — entries
// whose source rows never reached classifySourceOnly: they were matched
// in the target (that classification never consults the registry) or the
// source rows vanished entirely. Such entries can never get a per-row
// unresolved_record_error issue; they stay uncovered and the finalize
// coverage check accounts for them, but they must not poison the
// classification of the row at hand (Codex review iteration 2, finding 3).
func (it *registryIter) skipBelow(ctx context.Context, id int64) error {
	for {
		cur, ok, err := it.peek(ctx)
		if err != nil || !ok || cur >= id {
			return err
		}
		if err := it.advance(ctx); err != nil {
			return err
		}
	}
}
