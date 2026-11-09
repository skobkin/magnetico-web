package migration

// verify-decode (04B): a target-only full decode scan of every stored
// torrent. No source connection. Every target row is visited exactly once
// via bounded id keyset pagination; blobs are size-probed with
// octet_length() BEFORE any BYTEA is materialized (stored corruption may
// exceed the codec cap — oversized blobs produce a typed finding without
// being read). Valid blobs are packed into fetch chunks bounded by
// --max-inflight-bytes; a valid blob larger than the budget is admitted
// SOLO (the read-side analogue of the pipeline's solo slot) — never skipped
// for scheduling reasons.
//
// The pass covers all stored target rows, not all original source rows:
// its report states the 04A unresolved registry counts separately. The
// unavoidable historical blind spot — blob-absent file_count==1 rows whose
// original source file list cannot be proven — is counted (assumed-single)
// after the implied single file passes production validation, and stated in
// the report.
//
// Reads use bounded REPEATABLE READ READ ONLY transactions (per-database
// snapshot consistency); findings and checkpoints commit through separate
// connections. Two databases are never cross-database atomic: the operator
// keeps the target otherwise idle during verification.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
	"github.com/skobkin/magnetico-web/persistence"
)

// Verification issue stages and stable codes (decode pass).
const (
	IssueStageDecode   = "decode"
	IssueStageExtras   = "extras"
	IssueStageCoverage = "coverage"

	IssueCodeInvalidMetadata = "invalid_metadata"
	IssueCodeMissingBlob     = "missing_blob"
	IssueCodeDecodeFailure   = "decode_failure"
	IssueCodeFileCount       = "file_count_mismatch"
	IssueCodeNoncanonical    = "noncanonical_single_file_blob"
	IssueCodeBlobOverLimit   = "blob_over_limit"
	IssueCodeBlobNullData    = "blob_null_data"
	IssueCodeBlobVanished    = "blob_vanished"
	IssueCodeOrphanBlobs     = "orphan_target_blobs"
	IssueCodeCoverage        = "coverage_incomplete"
	IssueCodeTargetGrew      = "target_row_count_changed"
)

// VerifyDecodeInput configures one verify-decode pass.
type VerifyDecodeInput struct {
	TargetDSN string

	// Workers sets the blob-decoder parallelism: fetchAndDecodeChunk runs
	// this many goroutines decoding one chunk's blobs concurrently — real,
	// observable concurrency (pinned by
	// TestVerifyDecodeWorkersDecodeConcurrently).
	Workers          int
	BatchSize        int
	MaxInflightBytes int64

	// Limits overrides codec.DefaultLimits() (tests exercise oversize paths
	// with tiny fixtures); production leaves it nil.
	Limits *codec.Limits

	// Progress receives cumulative snapshots after every committed page.
	Progress func(VerifyProgress)
}

// VerifyDecode runs one full verify-decode pass: completed-run preflight,
// keyset scan of every target torrent with octet_length-first blob
// handling, the extras-phase orphan-blob check, exact coverage accounting
// and a durable pass/fail verdict (latest verification_runs row).
//
// Error taxonomy: typed preflight refusals (ErrRunNotComplete, corrupt
// state), *FatalError (exit 1), ErrInterrupted (exit 2), and
// ErrVerificationFindings after a COMPLETED scan with findings (exit 3) —
// the result is returned alongside in every completed case.
func VerifyDecode(ctx context.Context, in VerifyDecodeInput) (*VerificationResult, error) {
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
	settings, err := json.Marshal(map[string]any{
		"workers":            in.Workers,
		"batch_size":         in.BatchSize,
		"max_inflight_bytes": in.MaxInflightBytes,
	})
	if err != nil {
		return nil, err
	}

	// Adopt-or-new: a compatible interrupted pass resumes from its own
	// checkpoints; a fresh invocation never reuses a completed result.
	// Compatibility: same data revision (verification never mutates it) and
	// a non-complete phase. No source fingerprint applies (no source).
	run, resumed, err := state.StartVerification(runCtx, pool, state.CreateVerificationInput{
		RunID:             prog.RunID,
		Kind:              state.VerificationKindDecode,
		StartDataRevision: prog.DataRevision,
		ExpectedTargetIDs: &expectedTargets,
		SettingsRaw:       settings,
	}, func(c *state.VerificationRun) bool {
		return c.StartDataRevision == prog.DataRevision && c.Phase != state.VerificationPhaseComplete
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
		Kind:              state.VerificationKindDecode,
		VerificationID:    run.VerificationID,
		Seq:               run.Seq,
		Resumed:           resumed,
		StartDataRevision: run.StartDataRevision,
		ExpectedTargets:   run.ExpectedTargetIDs,
	}
	checkpoint := int64(0)
	if run.TargetCheckpoint != nil {
		checkpoint = *run.TargetCheckpoint
	}
	// expectedTarget mirrors the STORED checkpoint for the commit CAS: nil
	// until the first page commits (never conflate "no checkpoint" with 0).
	expectedTarget := run.TargetCheckpoint
	m := newDecodeMirrors(run)
	report := func() {
		if in.Progress != nil {
			in.Progress(VerifyProgress{
				Kind: result.Kind, Phase: m.phase, Resumed: resumed,
				Checkpoint:      checkpoint,
				ExpectedTargets: derefInt64(run.ExpectedTargetIDs),
				Observed:        m.observed(), Verified: m.verified, Mismatched: m.mismatch,
				Unverifiable: m.unverifiable, AssumedSingle: m.assumed, Issues: m.issues,
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
				return nil, ErrInterrupted
			}
			return nil, verificationFatal("extras", checkpoint, err)
		}
		result.OrphanTargetBlobs = orphans
		report()
	}

	if run.Phase == state.VerificationPhaseScanning {
		// Scanning loop: contiguous page-by-page frontier. The checkpoint
		// only ever moves at committed page boundaries, so an interrupted
		// pass resumes exactly where the last page committed.
		for {
			if runCtx.Err() != nil {
				return nil, interruptScan(runCtx, pool, run)
			}
			page, err := readDecodePage(runCtx, pool, checkpoint, in.BatchSize, in.MaxInflightBytes)
			if err != nil {
				if cause := fatalCause(runCtx); cause != nil {
					return nil, cause
				}
				if runCtx.Err() != nil {
					return nil, interruptScan(runCtx, pool, run)
				}
				return nil, verificationFatal("scan", checkpoint, err)
			}
			if len(page.rows) == 0 {
				break // scanned through the last torrent
			}

			pageRes, err := processDecodePage(runCtx, pool, page, limits, in.Workers, in.MaxInflightBytes)
			if err != nil {
				// Infrastructure failure (blob-fetch transaction, decode
				// scheduling): never committed as findings — stop fatally.
				if cause := fatalCause(runCtx); cause != nil {
					return nil, cause
				}
				if runCtx.Err() != nil {
					return nil, interruptScan(runCtx, pool, run)
				}
				return nil, verificationFatal("scan", checkpoint, err)
			}
			// Never commit a page whose processing was cut short.
			if runCtx.Err() != nil {
				return nil, interruptScan(runCtx, pool, run)
			}

			lastID := page.rows[len(page.rows)-1].id
			committed, err := state.CommitVerificationPage(runCtx, pool, state.VerificationPageCommit{
				VerificationID:      run.VerificationID,
				Issues:              pageRes.issues,
				VerifiedDelta:       pageRes.verified,
				MismatchDelta:       pageRes.mismatch,
				UnverifiableDelta:   pageRes.unverifiable,
				AssumedSingleDelta:  pageRes.assumed,
				SourceCheckpoint:    nil,
				TargetCheckpoint:    expectedTarget,
				NewSourceCheckpoint: nil,
				NewTargetCheckpoint: &lastID,
			})
			if err != nil {
				if cause := fatalCause(runCtx); cause != nil {
					return nil, cause
				}
				if runCtx.Err() != nil {
					return nil, interruptScan(runCtx, pool, run)
				}
				return nil, verificationFatal("scan", checkpoint, err)
			}
			if !committed {
				// Under the exclusive lock a lost CAS means our own commit
				// landed but the acknowledgement was lost (ambiguous COMMIT).
				fresh, gerr := state.GetVerification(runCtx, pool, run.VerificationID)
				if gerr != nil {
					return nil, verificationFatal("scan", checkpoint, gerr)
				}
				if !pageCommitted(fresh, nil, &lastID) {
					return nil, verificationFatal("scan", checkpoint, fmt.Errorf(
						"verification checkpoint moved underneath the scan (run %s)", run.VerificationID))
				}
				m.resync(fresh)
			} else {
				m.apply(pageRes)
			}
			checkpoint = lastID
			expectedTarget = &lastID
			result.ObservedTargets = m.observed()
			report()
		}

		// Extras phase: orphan target blobs (count + ID bounds, no content
		// reads), committed atomically with the phase advance — a resumed
		// pass (phase extras) knows the extras scan ran durably.
		orphans, oMin, oMax, err := decodeExtrasScan(runCtx, pool)
		if err != nil {
			if cause := fatalCause(runCtx); cause != nil {
				return nil, cause
			}
			if runCtx.Err() != nil {
				return nil, interruptScan(runCtx, pool, run)
			}
			return nil, verificationFatal("extras", checkpoint, err)
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
		// The extras commit keeps both checkpoints exactly as they are
		// (including NULL after a zero-row scan).
		committed, err := state.CommitVerificationPage(runCtx, pool, state.VerificationPageCommit{
			VerificationID:      run.VerificationID,
			Issues:              extraIssues,
			SourceCheckpoint:    nil,
			TargetCheckpoint:    expectedTarget,
			NewSourceCheckpoint: nil,
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
			return nil, verificationFatal("extras", checkpoint, err)
		}
		if !committed {
			fresh, gerr := state.GetVerification(runCtx, pool, run.VerificationID)
			if gerr != nil {
				return nil, verificationFatal("extras", checkpoint, gerr)
			}
			if !pageCommitted(fresh, nil, expectedTarget) {
				return nil, verificationFatal("extras", checkpoint, fmt.Errorf(
					"verification checkpoint moved underneath the extras scan (run %s)", run.VerificationID))
			}
			m.resync(fresh)
		} else {
			m.apply(decodePageResult{issues: extraIssues})
			m.phase = state.VerificationPhaseExtras
			result.OrphanTargetBlobs = orphans
		}
		report()
	}

	// Finalize: exact coverage accounting against a fresh row count, then
	// the durable verdict.
	var nowCount int64
	if err := pool.QueryRow(runCtx,
		`SELECT COUNT(*) FROM magneticod.torrents`).Scan(&nowCount); err != nil {
		if cause := fatalCause(runCtx); cause != nil {
			return nil, cause
		}
		if runCtx.Err() != nil {
			return nil, interruptScan(runCtx, pool, run)
		}
		return nil, verificationFatal("coverage", checkpoint, err)
	}
	observed := m.observed()
	var coverageIssue *state.VerificationIssue
	if run.ExpectedTargetIDs == nil || observed != *run.ExpectedTargetIDs {
		coverageIssue = &state.VerificationIssue{
			Code: IssueCodeCoverage, Stage: IssueStageCoverage, Severity: state.IssueCoverage,
			Details: map[string]any{
				"expected": derefInt64(run.ExpectedTargetIDs), "observed": observed,
			},
		}
	} else if nowCount != *run.ExpectedTargetIDs {
		// Rows appeared or disappeared mid-scan: the operator-immutability
		// assumption is broken — fail closed, never claim full coverage.
		coverageIssue = &state.VerificationIssue{
			Code: IssueCodeTargetGrew, Stage: IssueStageCoverage, Severity: state.IssueCoverage,
			Details: map[string]any{
				"at_start": *run.ExpectedTargetIDs, "at_end": nowCount,
			},
		}
	}
	var finalizeIssues []state.VerificationIssue
	if coverageIssue != nil {
		finalizeIssues = append(finalizeIssues, *coverageIssue)
	}
	finished, pass, counts, err := finishVerificationOutcome(runCtx, pool, run, nil, &observed, finalizeIssues)
	if err != nil {
		if cause := fatalCause(runCtx); cause != nil {
			return nil, cause
		}
		if runCtx.Err() != nil {
			return nil, interruptScan(runCtx, pool, run)
		}
		return nil, verificationFatal("coverage", checkpoint, err)
	}

	result.VerificationID = finished.VerificationID
	result.Seq = finished.Seq
	result.Pass = pass
	result.ObservedTargets = observed
	result.Verified = finished.VerifiedCount
	result.Mismatched = finished.MismatchCount
	result.Unverifiable = finished.UnverifiableCount
	result.AssumedSingle = finished.AssumedSingleCount
	result.IssuesBySeverity = counts

	// Report-only: the 04A registry counts, stated without conflation. The
	// verdict is already durable — like the post-finalize severity recount,
	// the query runs detached from the run's cancellation, because a cancel
	// here must neither fail the pass fatally nor misreport a resumable
	// interruption on a run that can never resume (Codex review iteration 5,
	// finding 2).
	cctx, ccancel := context.WithTimeout(context.WithoutCancel(context.Background()), dbOpTimeout)
	result.UnresolvedRecordErrors, result.UnresolvedIntegrity, err = state.CountErrors(cctx, pool, prog.RunID)
	ccancel()
	if err != nil {
		return nil, verificationFatal("coverage", checkpoint, err)
	}

	report()
	if !pass {
		return result, fmt.Errorf("%w (%d mismatch, %d coverage, %d unverifiable issues; run seq %d)",
			ErrVerificationFindings,
			counts[state.IssueMismatch], counts[state.IssueCoverage], counts[state.IssueUnverifiable],
			finished.Seq)
	}
	return result, nil
}

// interruptScan classifies a context cancellation: a fatal cause (lock
// loss) propagates as-is; a plain interrupt marks the pass interrupted
// (resumable) and returns ErrInterrupted.
func interruptScan(runCtx context.Context, pool *pgxpool.Pool, run *state.VerificationRun) error {
	if fatal := fatalCause(runCtx); fatal != nil {
		return fatal
	}
	ictx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), dbOpTimeout)
	defer cancel()
	if err := state.MarkInterrupted(ictx, pool, run.VerificationID); err != nil {
		return fmt.Errorf("%w (marking the pass interrupted also failed: %s)",
			ErrInterrupted, sanitizeDetail(err.Error()))
	}
	return ErrInterrupted
}

// decodeMirrors are the runner-side cumulative counters. They start from
// the stored row (resume-cumulative) and resync from the authoritative row
// after an ambiguous-commit reconciliation. sourceObserved carries the
// content pass's durable classified-source count (the decode pass leaves
// the column NULL and this field 0).
type decodeMirrors struct {
	verified, mismatch, unverifiable, assumed, issues int64
	sourceObserved                                    int64
	phase                                             string
}

func newDecodeMirrors(run *state.VerificationRun) *decodeMirrors {
	return &decodeMirrors{
		verified:       run.VerifiedCount,
		mismatch:       run.MismatchCount,
		unverifiable:   run.UnverifiableCount,
		assumed:        run.AssumedSingleCount,
		issues:         run.IssueCount,
		sourceObserved: derefInt64(run.ObservedSourceIDs),
		phase:          run.Phase,
	}
}

func (m *decodeMirrors) apply(p decodePageResult) {
	m.verified += p.verified
	m.mismatch += p.mismatch
	m.unverifiable += p.unverifiable
	m.assumed += p.assumed
	m.issues += int64(len(p.issues))
}

// applyContent applies one content-merge unit: counter deltas plus the
// source rows classified in the unit.
func (m *decodeMirrors) applyContent(p contentUnitResult) {
	m.apply(decodePageResult{
		issues: p.issues, verified: p.verified, mismatch: p.mismatch,
		unverifiable: p.unverifiable, assumed: p.assumed,
	})
	m.sourceObserved += p.sourceObserved
}

func (m *decodeMirrors) observed() int64 {
	return m.verified + m.mismatch + m.unverifiable + m.assumed
}

func (m *decodeMirrors) resync(run *state.VerificationRun) {
	m.verified = run.VerifiedCount
	m.mismatch = run.MismatchCount
	m.unverifiable = run.UnverifiableCount
	m.assumed = run.AssumedSingleCount
	m.issues = run.IssueCount
	m.sourceObserved = derefInt64(run.ObservedSourceIDs)
	m.phase = run.Phase
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefOrZero(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func i64ptr(v int64) *int64 { return &v }

// decodeRow is one target torrent of a scan page.
type decodeRow struct {
	id           int64
	infoHash     []byte
	name         string
	totalSize    int64
	discoveredOn int64
	fileCount    int64
	// hasBlobRow: a torrent_file_blobs row exists; blobLen nil with
	// hasBlobRow means NULL data (an anomaly).
	hasBlobRow bool
	blobLen    *int64
}

// retainedDecodeRowOverhead covers one retained decodeRow: the struct, the
// info-hash slice (header + 20-byte hash), the name header and its share of
// the page's slice bookkeeping — the target-side mirror of the source
// reader's retainedTorrentOverhead (a retention budget charges structure,
// not just string bytes).
const retainedDecodeRowOverhead = 128

type decodePage struct {
	rows []decodeRow

	// retainedBytes is the retention WEIGHT the page kept: infohash and
	// name bytes plus the structural overhead of every retained row. An
	// occupied page never admits a row that would push it past the page
	// budget; an empty page always admits its first row whatever it weighs
	// (readDecodePage — the target-side mirror of the source window).
	retainedBytes int64
}

// decodePageResult aggregates one page's verdicts.
type decodePageResult struct {
	issues                           []state.VerificationIssue
	verified, mismatch, unverifiable int64
	assumed                          int64
}

// readDecodePage reads one keyset metadata page in a bounded
// REPEATABLE READ READ ONLY transaction: the torrents row left-joined with
// its blob's octet_length — no BYTEA is materialized here.
//
// Row count alone does not bound the page's retention: names are unbounded
// text in the target schema, so the page enforces the same structural byte
// budget as the source reader (windowMaxBytes; 0 disables it). The weight
// charged per row is retainedDecodeRowOverhead + infohash + name bytes. An
// OCCUPIED page stops BEFORE the first row that would push it past the
// budget — that row and everything after it arrive in the next keyset page,
// so coverage and checkpoint resumability are unchanged (the page just got
// smaller under backpressure). An EMPTY page admits its first row whatever
// it weighs: a single oversized row is bounded by itself alone, never
// skipped for scheduling reasons.
func readDecodePage(ctx context.Context, pool *pgxpool.Pool, afterID int64, limit int, windowMaxBytes int64) (*decodePage, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `SELECT t.id, t.info_hash, t.name, t.total_size, t.discovered_on,
            t.file_count, b.torrent_id, octet_length(b.data)
        FROM magneticod.torrents t
        LEFT JOIN magneticod.torrent_file_blobs b ON b.torrent_id = t.id
        WHERE t.id > $1
        ORDER BY t.id
        LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	page := &decodePage{}
	for rows.Next() {
		var r decodeRow
		var blobID *int64
		if err := rows.Scan(&r.id, &r.infoHash, &r.name, &r.totalSize, &r.discoveredOn,
			&r.fileCount, &blobID, &r.blobLen); err != nil {
			return nil, err
		}
		weight := retainedDecodeRowOverhead + int64(len(r.infoHash)) + int64(len(r.name))
		if windowMaxBytes > 0 && len(page.rows) > 0 && page.retainedBytes+weight > windowMaxBytes {
			// Occupied page at the budget: stop before this row. The next
			// keyset page re-reads it — nothing is lost, the checkpoint
			// never skips it.
			break
		}
		r.hasBlobRow = blobID != nil
		page.rows = append(page.rows, r)
		page.retainedBytes += weight
	}
	return page, rows.Err()
}

// blobEntry pairs a page row index with its blob's probed length.
type blobEntry struct {
	idx    int
	length int64
}

// Row outcomes.
const (
	outcomeVerified = iota
	outcomeAssumed
	outcomeMismatch
)

type rowVerdict struct {
	outcome int
	issues  []state.VerificationIssue
}

type indexedVerdict struct {
	idx     int
	verdict rowVerdict
}

// processDecodePage classifies every row of the page (no-blob rows and
// anomalies inline), packs the fetchable blobs into budget-bounded chunks
// (solo admission for valid blobs over the budget), fetches chunk contents
// sequentially (one bounded snapshot transaction per chunk) and decodes in
// parallel across workers. Concurrent raw bytes never exceed the budget
// plus at most one codec-bounded oversized blob. An infrastructure failure
// (transaction, query) aborts the whole page as an error — never a finding.
func processDecodePage(ctx context.Context, pool *pgxpool.Pool, page *decodePage, limits codec.Limits, workers int, maxInflight int64) (decodePageResult, error) {
	res := decodePageResult{issues: []state.VerificationIssue{}}
	verdicts := make([]rowVerdict, len(page.rows))
	var entries []blobEntry
	for i := range page.rows {
		r := &page.rows[i]
		switch {
		case r.hasBlobRow && r.blobLen == nil:
			verdicts[i] = rowVerdict{outcome: outcomeMismatch, issues: []state.VerificationIssue{{
				SourceID: i64ptr(r.id), InfoHash: r.infoHash,
				Code: IssueCodeBlobNullData, Stage: IssueStageDecode, Severity: state.IssueMismatch,
				Details: map[string]any{"reason": "blob data is NULL"},
			}}}
		case r.hasBlobRow && *r.blobLen > int64(limits.MaxStoredBytes):
			// Typed finding WITHOUT fetching the content (directive: never
			// blindly materialize a possibly-corrupted oversized BYTEA).
			verdicts[i] = rowVerdict{outcome: outcomeMismatch, issues: []state.VerificationIssue{{
				SourceID: i64ptr(r.id), InfoHash: r.infoHash,
				Code: IssueCodeBlobOverLimit, Stage: IssueStageDecode, Severity: state.IssueMismatch,
				Details: map[string]any{
					"reason":  "stored blob exceeds the codec limit",
					"length":  *r.blobLen,
					"allowed": limits.MaxStoredBytes,
				},
			}}}
		case r.hasBlobRow:
			entries = append(entries, blobEntry{idx: i, length: *r.blobLen})
		default:
			verdicts[i] = verifyNoBlobRow(r, limits)
		}
	}

	chunks := packChunks(entries, maxInflight)
	for _, chunk := range chunks {
		ivs, err := fetchAndDecodeChunk(ctx, pool, page, chunk, limits, workers)
		if err != nil {
			return decodePageResult{}, err
		}
		for _, iv := range ivs {
			verdicts[iv.idx] = iv.verdict
		}
	}

	for _, v := range verdicts {
		switch v.outcome {
		case outcomeVerified:
			res.verified++
		case outcomeAssumed:
			res.assumed++
		default:
			res.mismatch++
		}
		res.issues = append(res.issues, v.issues...)
	}
	return res, nil
}

// verifyNoBlobRow checks a blob-absent row: file_count==1 reconstructs the
// implied typical single file (name, total_size) and must pass production
// validation to count as an assumed single (the historical blind spot);
// any other file_count is an integrity failure.
func verifyNoBlobRow(r *decodeRow, limits codec.Limits) rowVerdict {
	if r.fileCount != 1 {
		return rowVerdict{outcome: outcomeMismatch, issues: []state.VerificationIssue{{
			SourceID: i64ptr(r.id), InfoHash: r.infoHash,
			Code: IssueCodeMissingBlob, Stage: IssueStageDecode, Severity: state.IssueMismatch,
			Details: map[string]any{"reason": "no blob stored", "file_count": r.fileCount},
		}}}
	}
	meta := decodeMeta(r)
	files := []persistence.File{{Path: r.name, Size: r.totalSize}}
	if err := persistence.ValidateIngest(meta, files, limits); err != nil {
		return rowVerdict{outcome: outcomeMismatch, issues: []state.VerificationIssue{{
			SourceID: i64ptr(r.id), InfoHash: r.infoHash,
			Code: IssueCodeInvalidMetadata, Stage: IssueStageDecode, Severity: state.IssueMismatch,
			Details: map[string]any{"reason": sanitizeDetail(err.Error()), "assumed_single": true},
		}}}
	}
	return rowVerdict{outcome: outcomeAssumed}
}

// decodeMeta builds the persistence.Torrent for validation.
func decodeMeta(r *decodeRow) persistence.Torrent {
	return persistence.Torrent{
		ID:           int32(r.id),
		InfoHash:     r.infoHash,
		Name:         r.name,
		TotalSize:    r.totalSize,
		DiscoveredOn: r.discoveredOn,
	}
}

// packChunks greedily packs blob entries into chunks whose cumulative
// probed length stays within the budget; an entry whose own length exceeds
// the budget (but not the codec cap) forms a solo chunk — never skipped for
// scheduling reasons.
func packChunks(entries []blobEntry, maxInflight int64) [][]blobEntry {
	var chunks [][]blobEntry
	var chunk []blobEntry
	var chunkBytes int64
	flush := func() {
		if len(chunk) == 0 {
			return
		}
		chunks = append(chunks, chunk)
		chunk = nil
		chunkBytes = 0
	}
	for _, e := range entries {
		switch {
		case e.length > maxInflight:
			// Valid, codec-bounded, but over the scheduling budget: admit
			// solo (the read-side analogue of the pipeline's solo slot).
			flush()
			chunks = append(chunks, []blobEntry{e})
		case chunkBytes+e.length > maxInflight:
			flush()
			chunk = []blobEntry{e}
			chunkBytes = e.length
		default:
			chunk = append(chunk, e)
			chunkBytes += e.length
		}
	}
	flush()
	return chunks
}

// decodeWorkHook is a test-only seam: when non-nil, every blob-decode
// worker invokes it immediately before decoding one blob, making the
// worker pool's actual concurrency deterministically observable (the
// advertised --workers semantics are pinned by
// TestVerifyDecodeWorkersDecodeConcurrently). Production keeps it nil —
// the per-row cost is one atomic load.
var decodeWorkHook atomic.Pointer[func()]

// fetchAndDecodeChunk fetches one chunk's blob contents (one snapshot
// transaction, bounded by construction) and decodes its rows in parallel.
// Infrastructure failures return an error — the caller aborts the page
// fatally instead of recording findings.
func fetchAndDecodeChunk(ctx context.Context, pool *pgxpool.Pool, page *decodePage, chunk []blobEntry, limits codec.Limits, workers int) ([]indexedVerdict, error) {
	ids := make([]int64, len(chunk))
	for i, e := range chunk {
		ids[i] = page.rows[e.idx].id
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `SELECT torrent_id, data FROM magneticod.torrent_file_blobs
        WHERE torrent_id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	blobs := make(map[int64][]byte, len(chunk))
	for rows.Next() {
		var id int64
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		blobs[id] = data
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]indexedVerdict, len(chunk))
	jobs := make(chan int)
	var wg sync.WaitGroup
	if workers <= 0 {
		workers = 1
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				e := chunk[i]
				r := &page.rows[e.idx]
				blob, ok := blobs[r.id]
				if !ok {
					// The blob row vanished between the length probe and the
					// content fetch: the operator-immutability assumption is
					// broken — fail the row, never guess.
					out[i] = indexedVerdict{idx: e.idx, verdict: rowVerdict{
						outcome: outcomeMismatch,
						issues: []state.VerificationIssue{{
							SourceID: i64ptr(r.id), InfoHash: r.infoHash,
							Code: IssueCodeBlobVanished, Stage: IssueStageDecode, Severity: state.IssueMismatch,
							Details: map[string]any{"reason": "blob row missing at content fetch"},
						}},
					}}
					continue
				}
				if hook := decodeWorkHook.Load(); hook != nil {
					(*hook)()
				}
				out[i] = indexedVerdict{idx: e.idx, verdict: verifyBlobRow(r, blob, limits)}
			}
		}()
	}
	for i := range chunk {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out, nil
}

// verifyBlobRow decodes one fetched blob and applies the full check set:
// a typed finding from the codec taxonomy, file-count equality, production
// metadata/file-list validation, and the noncanonical-single representation
// check. Exactly one issue per failing row.
func verifyBlobRow(r *decodeRow, blob []byte, limits codec.Limits) rowVerdict {
	files, err := codec.Decode(blob, limits)
	if err != nil {
		code, details := decodeErrorIssueCode(err)
		return rowVerdict{outcome: outcomeMismatch, issues: []state.VerificationIssue{{
			SourceID: i64ptr(r.id), InfoHash: r.infoHash,
			Code: code, Stage: IssueStageDecode, Severity: state.IssueMismatch,
			Details: details,
		}}}
	}
	if int64(len(files)) != r.fileCount {
		return rowVerdict{outcome: outcomeMismatch, issues: []state.VerificationIssue{{
			SourceID: i64ptr(r.id), InfoHash: r.infoHash,
			Code: IssueCodeFileCount, Stage: IssueStageDecode, Severity: state.IssueMismatch,
			Details: map[string]any{"file_count": r.fileCount, "decoded_files": len(files)},
		}}}
	}
	meta := decodeMeta(r)
	pfiles := make([]persistence.File, len(files))
	copy(pfiles, files)
	if err := persistence.ValidateIngest(meta, pfiles, limits); err != nil {
		return rowVerdict{outcome: outcomeMismatch, issues: []state.VerificationIssue{{
			SourceID: i64ptr(r.id), InfoHash: r.infoHash,
			Code: IssueCodeInvalidMetadata, Stage: IssueStageDecode, Severity: state.IssueMismatch,
			Details: map[string]any{"reason": sanitizeDetail(err.Error())},
		}}}
	}
	// A single typical file stored WITH a blob instead of the required
	// no-blob representation is a noncanonical representation finding.
	if r.fileCount == 1 && persistence.NoBlobEligible(meta, pfiles) {
		return rowVerdict{outcome: outcomeMismatch, issues: []state.VerificationIssue{{
			SourceID: i64ptr(r.id), InfoHash: r.infoHash,
			Code: IssueCodeNoncanonical, Stage: IssueStageDecode, Severity: state.IssueMismatch,
			Details: map[string]any{"reason": "typical single file stored with a blob"},
		}}}
	}
	return rowVerdict{outcome: outcomeVerified}
}

// decodeExtrasScan counts orphan target blobs (blobs without a torrent
// row) with their ID bounds — a bounded summary, no content reads.
func decodeExtrasScan(ctx context.Context, pool *pgxpool.Pool) (count int64, minID, maxID *int64, err error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	err = tx.QueryRow(ctx, `SELECT COUNT(*), MIN(b.torrent_id), MAX(b.torrent_id)
        FROM magneticod.torrent_file_blobs b
        LEFT JOIN magneticod.torrents t ON t.id = b.torrent_id
        WHERE t.id IS NULL`).Scan(&count, &minID, &maxID)
	if err != nil {
		return 0, nil, nil, err
	}
	return count, minID, maxID, nil
}
