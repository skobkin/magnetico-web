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
	Verified        int64
	Mismatched      int64
	Unverifiable    int64
	AssumedSingle   int64

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
// a coverage shortfall forces fail with its issue (committed atomically
// with the state flip — a crash between finding and verdict replays
// idempotently). Returns the finished run, the pass decision and the
// final severity counts.
func finishVerificationOutcome(
	ctx context.Context, pool *pgxpool.Pool, run *state.VerificationRun,
	observed int64, coverageIssue *state.VerificationIssue,
) (*state.VerificationRun, bool, map[string]int64, error) {
	counts, err := verifyIssueSeverityCounts(ctx, pool, run.VerificationID)
	if err != nil {
		return nil, false, nil, err
	}
	pass := verificationPass(counts)
	finIssues := []state.VerificationIssue{}
	if coverageIssue != nil {
		finIssues = append(finIssues, *coverageIssue)
		pass = false
	}
	finished, err := state.FinishVerification(ctx, pool, state.FinishVerificationInput{
		VerificationID:    run.VerificationID,
		Pass:              pass,
		ObservedTargetIDs: &observed,
		Issues:            finIssues,
	})
	if err != nil {
		return nil, false, nil, err
	}
	if coverageIssue != nil {
		counts[coverageIssue.Severity]++
	}
	return finished, pass, counts, nil
}
