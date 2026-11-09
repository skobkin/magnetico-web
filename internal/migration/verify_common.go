package migration

// Shared 04B verification plumbing: the exit-code sentinels, the progress
// snapshot, page-commit reconciliation and the shared finish path for
// verify-decode and verify-content.
//
// Verification never writes to the 04A error registry: its findings live in
// magnetico_migration.verification_issues, its verdicts in
// verification_runs. Infrastructure failures are returned typed, never
// recorded as import incidents (the import registry stays import-domain).

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/migration/state"
)

// ErrVerificationFindings reports a verification scan that COMPLETED but
// found mismatches, coverage gaps or unverifiable records (exit-3
// territory — the summary is printed, not a fatal failure).
var ErrVerificationFindings = errors.New("verification completed with findings")

// Verification kind identifiers used in reporting.
const (
	VerifyKindDecode  = state.VerificationKindDecode
	VerifyKindContent = state.VerificationKindContent
)

// VerifyProgress is a cumulative, resume-honest verification snapshot for
// reporting. Observed counts target rows visited; Verified/Mismatched/
// Unverifiable/AssumedSingle classify them; Issues counts persisted issue
// rows.
type VerifyProgress struct {
	Kind       string
	Phase      string // scanning | extras | complete
	Resumed    bool
	Checkpoint int64 // last committed target-side page boundary (torrent ID)

	ExpectedTargets int64 // target rows recorded at run start
	Observed        int64 // target rows visited so far
	Verified        int64
	Mismatched      int64
	Unverifiable    int64
	AssumedSingle   int64
	Issues          int64

	// Content pass only (zero for decode).
	SourceObserved int64
}

// VerificationResult is the outcome of one verification invocation: the
// authoritative run row (latest seq), the severity split of its persisted
// issues, the 04A registry counts (report-only, never conflated) and the
// blind-spot bookkeeping the report must state.
type VerificationResult struct {
	Kind              string
	VerificationID    string
	Seq               int64
	Pass              bool
	Resumed           bool
	StartDataRevision int64

	ExpectedTargets *int64
	ObservedTargets int64
	// SourceObserved: source rows classified (content pass only; the
	// durable cumulative value from the run row).
	SourceObserved int64
	Verified       int64
	Mismatched     int64
	Unverifiable   int64
	AssumedSingle  int64

	IssuesBySeverity map[string]int64

	// Decode pass: the extras-phase orphan-blob finding (0 = none).
	OrphanTargetBlobs int64

	// 04A registry counts, report-only: a decode pass covers all stored
	// target rows, NOT all original source rows — the report states both
	// without conflating them.
	UnresolvedRecordErrors int64
	UnresolvedIntegrity    int64
}

// verifyIssueSeverityCounts recounts the persisted issue rows by severity —
// the authoritative pass/fail basis (dedupe during replays makes local
// mirrors approximate).
func verifyIssueSeverityCounts(ctx context.Context, pool *pgxpool.Pool, verificationID string) (map[string]int64, error) {
	rows, err := pool.Query(ctx, `SELECT severity, COUNT(*) FROM magnetico_migration.verification_issues
        WHERE verification_id = $1::uuid GROUP BY severity`, verificationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var sev string
		var n int64
		if err := rows.Scan(&sev, &n); err != nil {
			return nil, err
		}
		counts[sev] = n
	}
	return counts, rows.Err()
}

// verificationPass reports whether the severity split permits a pass: any
// mismatch, coverage or unverifiable issue fails it; info issues are
// recorded provenance only.
func verificationPass(counts map[string]int64) bool {
	return counts[state.IssueMismatch] == 0 &&
		counts[state.IssueCoverage] == 0 &&
		counts[state.IssueUnverifiable] == 0
}

// pageCommitted reconciles an ambiguous CommitVerificationPage result.
// Under the exclusive run lock a lost CAS means the commit actually landed
// (only this process updates verification state): the checkpoints moved to
// exactly the attempted values. Anything else is not our commit.
func pageCommitted(run *state.VerificationRun, newSource, newTarget *int64) bool {
	if run == nil {
		return false
	}
	return ptrEqual(run.SourceCheckpoint, newSource) && ptrEqual(run.TargetCheckpoint, newTarget)
}

func ptrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// decodeErrorIssueCode maps a codec.Decode error to a stable, bounded issue
// code from the codec taxonomy. Details stay secret-free (counts, lengths,
// limit names — never paths).
func decodeErrorIssueCode(err error) (code string, details map[string]any) {
	details = map[string]any{"reason": sanitizeDetail(err.Error())}
	var le *codec.LimitError
	switch {
	case errors.As(err, &le):
		details["limit"] = le.Limit
		details["actual"] = le.Actual
		details["allowed"] = le.Allowed
		return "codec_limit", details
	case errors.Is(err, codec.ErrUnsupportedVersion):
		return "wire_version", details
	case errors.Is(err, codec.ErrUnsupportedSerializer),
		errors.Is(err, codec.ErrUnsupportedCompressor):
		return "wire_ids", details
	case errors.Is(err, codec.ErrTruncated), errors.Is(err, codec.ErrTrailingBytes):
		return "framing", details
	case errors.Is(err, codec.ErrNonCanonicalOrder):
		return "noncanonical_order", details
	case errors.Is(err, codec.ErrCorruptCompressed):
		return "compressed_stream", details
	case errors.Is(err, codec.ErrOverflow), errors.Is(err, codec.ErrNegativeSize),
		errors.Is(err, codec.ErrInvalidCount):
		return "numeric", details
	case errors.Is(err, codec.ErrNonMaximalPrefix),
		errors.Is(err, codec.ErrPrefixOutOfRange),
		errors.Is(err, codec.ErrDuplicateDirName),
		errors.Is(err, codec.ErrEmptyPathComponent),
		errors.Is(err, codec.ErrPathComponentSeparator):
		return "structure", details
	default:
		return "decode_error", details
	}
}

// verificationFatal wraps an infrastructure failure with the checkpoint it
// stopped at. Verification failures are never recorded as import incidents.
func verificationFatal(stage string, checkpoint int64, err error) error {
	return &FatalError{Stage: stage, FromID: checkpoint, ToID: checkpoint, Err: err}
}

// finishVerificationOutcome closes a verification run. The pass decision
// needs the authoritative severity recount, so the recount happens first;
// finalize issues (coverage shortfalls, mid-scan growth, fingerprint
// provenance) commit atomically with the state flip — a crash between
// finding and verdict replays idempotently (NULL-source issues dedupe via
// the partial unique index). Any finalize issue of severity
// mismatch/coverage/unverifiable forces fail; info issues never do.
// Returns the finished run, the pass decision and the final severity
// counts (finalize issues included).
func finishVerificationOutcome(
	ctx context.Context, pool *pgxpool.Pool, run *state.VerificationRun,
	observedSource, observedTarget *int64, finalizeIssues []state.VerificationIssue,
) (*state.VerificationRun, bool, map[string]int64, error) {
	counts, err := verifyIssueSeverityCounts(ctx, pool, run.VerificationID)
	if err != nil {
		return nil, false, nil, err
	}
	pass := verificationPass(counts)
	for _, issue := range finalizeIssues {
		switch issue.Severity {
		case state.IssueMismatch, state.IssueCoverage, state.IssueUnverifiable:
			pass = false
		}
		counts[issue.Severity]++
	}
	finished, err := state.FinishVerification(ctx, pool, state.FinishVerificationInput{
		VerificationID:    run.VerificationID,
		Pass:              pass,
		ObservedSourceIDs: observedSource,
		ObservedTargetIDs: observedTarget,
		Issues:            finalizeIssues,
	})
	if err != nil {
		return nil, false, nil, err
	}
	// The reported severity split must match the durable issue table exactly.
	// A finalize issue that already exists durably (the drift provenance on an
	// extras-phase resume, re-proposed by every invocation) is deduplicated by
	// the insert, so counting the finalize list on top of the pre-commit
	// recount would inflate the summary. The post-commit recount is
	// authoritative (Codex review iteration 4, finding 4); the pre-commit
	// recount above stays the pass decision's basis, with the finalize list's
	// severity gate applied explicitly.
	//
	// The recount is report-only bookkeeping AFTER the durable verdict has
	// committed: it runs on a context detached from the run's cancellation,
	// because a cancellation here must not classify the pass as interrupted
	// — the run is already completed and can never resume (Codex review
	// iteration 5, finding 2).
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(context.Background()), dbOpTimeout)
	defer rcancel()
	counts, err = verifyIssueSeverityCounts(rctx, pool, run.VerificationID)
	if err != nil {
		return nil, false, nil, err
	}
	return finished, pass, counts, nil
}

// normalizeVerifyConfig applies the shared verification tunable defaults
// and bounds (workers, batch size, in-flight budget) for the verification
// passes and the retry pass. Per-command concurrency semantics differ and
// are documented on each input struct: workers controls real parallelism
// only in verify-decode's blob decoding; verify-content and retry-errors
// accept it (validated, recorded in run settings) without any concurrency —
// their sequential settlement is what holds their memory and
// ordering guarantees.
func normalizeVerifyConfig(workers, batchSize int, maxInflight int64) (int, int, int64, error) {
	if workers <= 0 {
		workers = DefaultWorkers
	}
	if workers > MaxWorkers {
		return 0, 0, 0, &ConfigError{msg: fmt.Sprintf("--workers must be 1..%d, got %d", MaxWorkers, workers)}
	}
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	if batchSize > MaxBatchSize {
		return 0, 0, 0, &ConfigError{msg: fmt.Sprintf("--batch-size must be 1..%d, got %d", MaxBatchSize, batchSize)}
	}
	if maxInflight <= 0 {
		maxInflight = DefaultMaxInflightBytes
	}
	if maxInflight < 1<<20 {
		return 0, 0, 0, &ConfigError{msg: fmt.Sprintf("--max-inflight-bytes must be at least 1 MiB, got %d", maxInflight)}
	}
	return workers, batchSize, maxInflight, nil
}
