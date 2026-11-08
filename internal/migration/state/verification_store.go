package state

// Verification run store: durable, independently resumable state for the
// 04B verification passes (verify-decode / verify-content), kept strictly
// separate from progress and its import checkpoint. Completed runs are
// immutable history: nothing ever mutates a terminal row except the
// adopt-or-new classification of NON-terminal predecessors.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Verification kinds, states, phases and issue severities.
const (
	VerificationKindDecode  = "decode"
	VerificationKindContent = "content"

	VerificationRunning       = "running"
	VerificationInterrupted   = "interrupted"
	VerificationCompletedPass = "completed_pass"
	VerificationCompletedFail = "completed_fail"
	VerificationAborted       = "aborted"

	VerificationPhaseScanning = "scanning"
	VerificationPhaseExtras   = "extras"
	VerificationPhaseComplete = "complete"

	// Issue severities. Only mismatch/coverage/unverifiable issues fail a
	// pass; info issues are recorded provenance (e.g. source fingerprint
	// drift after an accepted repair).
	IssueInfo         = "info"
	IssueMismatch     = "mismatch"
	IssueCoverage     = "coverage"
	IssueUnverifiable = "unverifiable"
)

// VerificationRun is one row of `magnetico_migration.verification_runs`.
type VerificationRun struct {
	Seq            int64
	VerificationID string
	RunID          string
	Kind           string
	State          string
	Phase          string

	StartDataRevision int64

	// Source observation (content kind only; NULL for decode).
	SourceIdentityRaw    json.RawMessage
	SourceFingerprintRaw json.RawMessage

	// Checkpoints: the last committed page boundary per database. NULL
	// before the first commit (decode never advances the source side).
	SourceCheckpoint *int64
	TargetCheckpoint *int64

	// Coverage expectations and observations.
	ExpectedSourceIDs *int64
	ObservedSourceIDs *int64
	ExpectedTargetIDs *int64
	ObservedTargetIDs *int64

	VerifiedCount      int64
	MismatchCount      int64
	UnverifiableCount  int64
	AssumedSingleCount int64
	IssueCount         int64

	SettingsRaw json.RawMessage

	StartedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// VerificationIssue is one typed finding persisted with its page commit.
// Details must stay secret-free and bounded (no names, no paths, no DSNs) —
// 04C's forensic export fetches full records explicitly.
type VerificationIssue struct {
	SourceID *int64
	InfoHash []byte
	Code     string
	Stage    string
	Severity string
	Details  map[string]any
}

// CreateVerificationInput seeds a new verification run.
type CreateVerificationInput struct {
	RunID             string
	Kind              string
	StartDataRevision int64
	// Source observation; nil for the decode kind (no source connection).
	SourceIdentityRaw    json.RawMessage
	SourceFingerprintRaw json.RawMessage
	ExpectedSourceIDs    *int64
	ExpectedTargetIDs    *int64
	SettingsRaw          json.RawMessage
}

// verificationProjection is the column projection shared by the row
// readers (table name appended separately: RETURNING cannot carry FROM).
const verificationProjection = `seq, verification_id::text, run_id::text, kind, state, phase,
        start_data_revision, source_identity, source_fingerprint,
        source_checkpoint, target_checkpoint,
        expected_source_ids, observed_source_ids, expected_target_ids, observed_target_ids,
        verified_count, mismatch_count, unverifiable_count, assumed_single_count, issue_count,
        settings, started_at, updated_at, completed_at`

func scanVerificationRow(row pgx.Row) (*VerificationRun, error) {
	r := &VerificationRun{}
	var sourceIdentity, sourceFingerprint, settings []byte
	err := row.Scan(&r.Seq, &r.VerificationID, &r.RunID, &r.Kind, &r.State, &r.Phase,
		&r.StartDataRevision, &sourceIdentity, &sourceFingerprint,
		&r.SourceCheckpoint, &r.TargetCheckpoint,
		&r.ExpectedSourceIDs, &r.ObservedSourceIDs, &r.ExpectedTargetIDs, &r.ObservedTargetIDs,
		&r.VerifiedCount, &r.MismatchCount, &r.UnverifiableCount, &r.AssumedSingleCount, &r.IssueCount,
		&settings, &r.StartedAt, &r.UpdatedAt, &r.CompletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.SourceIdentityRaw = sourceIdentity
	r.SourceFingerprintRaw = sourceFingerprint
	r.SettingsRaw = settings
	return r, nil
}

// StartVerification adopts a compatible non-terminal run of the kind or
// creates a fresh one (adopt-or-new). Caller holds the run lock: a row left
// `running` under the lock proves its holder died, and a graceful interrupt
// leaves `interrupted` — both are resumable candidates.
//
// The resumable callback decides compatibility of a candidate: the runner
// checks `start_data_revision == progress.data_revision`, `phase !=
// complete` and — for content — `Fingerprint.Equivalent` of the stored
// source fingerprint against the fresh one (never byte-equal JSON: a source
// restart or transport switch must not abort a resumable pass).
//
// A compatible candidate is resumed in place (back to `running`, all
// checkpoints and counters preserved). An incompatible one is marked
// `aborted` (history preserved, never mutated again) and a fresh run
// starts. Completed runs are never resumed: a fresh invocation always
// creates a new run. More than one non-terminal row of a kind is corrupt
// state (only the lock serialized creators).
func StartVerification(ctx context.Context, pool *pgxpool.Pool, in CreateVerificationInput, resumable func(*VerificationRun) bool) (*VerificationRun, bool, error) {
	var candidate *VerificationRun
	// At most one non-terminal run per kind, regardless of state.
	var nonTerminal int64
	err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.verification_runs
        WHERE kind = $1 AND state IN ('running','interrupted')`, in.Kind).Scan(&nonTerminal)
	if err != nil {
		return nil, false, err
	}
	if nonTerminal > 1 {
		return nil, false, &StateError{Err: ErrRunStateCorrupt, Details: []string{
			fmt.Sprintf("%d non-terminal %s verification runs exist, want at most 1", nonTerminal, in.Kind)}}
	}
	if nonTerminal == 1 {
		row := pool.QueryRow(ctx, `SELECT `+verificationProjection+` FROM magnetico_migration.verification_runs
            WHERE kind = $1 AND state IN ('running','interrupted')`, in.Kind)
		candidate, err = scanVerificationRow(row)
		if err != nil {
			return nil, false, err
		}
	}

	if candidate != nil && resumable != nil && resumable(candidate) {
		if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.verification_runs
            SET state = 'running', updated_at = now()
            WHERE verification_id = $1::uuid AND state IN ('running','interrupted')`,
			candidate.VerificationID); err != nil {
			return nil, false, err
		}
		candidate.State = VerificationRunning
		return candidate, true, nil
	}
	if candidate != nil {
		if err := AbortVerification(ctx, pool, candidate.VerificationID); err != nil {
			return nil, false, err
		}
	}
	created, err := CreateVerification(ctx, pool, in)
	if err != nil {
		return nil, false, err
	}
	return created, false, nil
}

// CreateVerification inserts a fresh verification run.
func CreateVerification(ctx context.Context, pool *pgxpool.Pool, in CreateVerificationInput) (*VerificationRun, error) {
	settings := in.SettingsRaw
	if settings == nil {
		settings = []byte("{}")
	}
	verificationID, err := NewRunID()
	if err != nil {
		return nil, err
	}
	row := pool.QueryRow(ctx, `INSERT INTO magnetico_migration.verification_runs
        (verification_id, run_id, kind, state, phase, start_data_revision,
         source_identity, source_fingerprint, expected_source_ids, expected_target_ids, settings)
        VALUES ($1::uuid, $2::uuid, $3, 'running', 'scanning', $4, $5, $6, $7, $8, $9)
        RETURNING `+verificationProjection,
		verificationID, in.RunID, in.Kind, in.StartDataRevision,
		in.SourceIdentityRaw, in.SourceFingerprintRaw, in.ExpectedSourceIDs, in.ExpectedTargetIDs, settings)
	return scanVerificationRow(row)
}

// AbortVerification marks a non-terminal run aborted (terminal history;
// never mutated again).
func AbortVerification(ctx context.Context, pool *pgxpool.Pool, verificationID string) error {
	tag, err := pool.Exec(ctx, `UPDATE magnetico_migration.verification_runs
        SET state = 'aborted', completed_at = now(), updated_at = now()
        WHERE verification_id = $1::uuid AND state IN ('running','interrupted')`, verificationID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &StateError{Err: ErrRunStateCorrupt, Details: []string{
			"verification run " + verificationID + " is not non-terminal; cannot abort"}}
	}
	return nil
}

// MarkInterrupted records a graceful shutdown: the pass is resumable exactly
// as if it had crashed (the next StartVerification adopts it).
func MarkInterrupted(ctx context.Context, pool *pgxpool.Pool, verificationID string) error {
	tag, err := pool.Exec(ctx, `UPDATE magnetico_migration.verification_runs
        SET state = 'interrupted', updated_at = now()
        WHERE verification_id = $1::uuid AND state = 'running'`, verificationID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &StateError{Err: ErrRunStateCorrupt, Details: []string{
			"verification run " + verificationID + " is not running; cannot mark interrupted"}}
	}
	return nil
}

// VerificationPageCommit is one atomic page boundary: typed issues, counter
// deltas and BOTH compare-and-swap checkpoints in one transaction. A failed
// CAS or any error rolls the whole page back — issues never persist without
// their checkpoint.
type VerificationPageCommit struct {
	VerificationID string
	Issues         []VerificationIssue
	// Counter DELTAS added to the stored run.
	VerifiedDelta      int64
	MismatchDelta      int64
	UnverifiableDelta  int64
	AssumedSingleDelta int64
	// SourceCheckpoint/TargetCheckpoint are the expected current values
	// (nil = still NULL); NewSourceCheckpoint/NewTargetCheckpoint the
	// post-page values. The decode pass leaves the source side NULL forever.
	SourceCheckpoint    *int64
	TargetCheckpoint    *int64
	NewSourceCheckpoint *int64
	NewTargetCheckpoint *int64
	// NewPhase advances the run's phase atomically with this commit
	// ("" leaves the phase unchanged). The extras phase transition commits
	// together with the extras findings, so a resumed pass can tell the
	// extras scan already ran durably.
	NewPhase string
	// NewObservedSourceIDs/NewObservedTargetIDs record the CUMULATIVE
	// classified-row counts (nil leaves the column unchanged; the decode
	// pass never sets the source side). The content pass commits both so a
	// resumed pass resumes with honest coverage mirrors — the counters alone
	// cannot reconstruct how many source-only rows were classified.
	NewObservedSourceIDs *int64
	NewObservedTargetIDs *int64
}

// errCheckpointCASLost is returned by the page transaction when the
// compare-and-swap matched zero rows: the whole transaction (issues
// included) rolls back, so a rejected page can never leave issues persisted
// without their checkpoint and counter movement.
var errCheckpointCASLost = errors.New("verification page checkpoint compare-and-swap matched zero rows")

// CommitVerificationPage applies one page commit. It returns false when the
// compare-and-swap lost (the checkpoints moved underneath us — the caller
// must re-read the run and reconcile) or the run is not `running`. A lost
// CAS rolls the page back entirely: nothing is persisted.
func CommitVerificationPage(ctx context.Context, pool *pgxpool.Pool, in VerificationPageCommit) (bool, error) {
	var committed bool
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Insert the issues first; ON CONFLICT DO NOTHING keeps a replayed
		// page (ambiguous COMMIT) idempotent, and the counter delta counts
		// only rows that actually landed.
		inserted := int64(0)
		for _, issue := range in.Issues {
			details := []byte("null")
			if issue.Details != nil {
				b, err := json.Marshal(issue.Details)
				if err != nil {
					return err
				}
				details = b
			}
			var sourceID, infoHash any
			if issue.SourceID != nil {
				sourceID = *issue.SourceID
			}
			if issue.InfoHash != nil {
				infoHash = issue.InfoHash
			}
			tag, err := tx.Exec(ctx, `INSERT INTO magnetico_migration.verification_issues
                (verification_id, source_id, info_hash, code, stage, severity, details)
                VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
                ON CONFLICT DO NOTHING`,
				in.VerificationID, sourceID, infoHash, issue.Code, issue.Stage, issue.Severity, details)
			if err != nil {
				return err
			}
			inserted += tag.RowsAffected()
		}

		tag, err := tx.Exec(ctx, `UPDATE magnetico_migration.verification_runs SET
            phase = COALESCE(NULLIF($11, ''), phase),
            source_checkpoint = $4,
            target_checkpoint = $5,
            verified_count = verified_count + $6,
            mismatch_count = mismatch_count + $7,
            unverifiable_count = unverifiable_count + $8,
            assumed_single_count = assumed_single_count + $9,
            issue_count = issue_count + $10,
            observed_source_ids = COALESCE($12, observed_source_ids),
            observed_target_ids = COALESCE($13, observed_target_ids),
            updated_at = now()
        WHERE verification_id = $1::uuid AND state = 'running'
          AND source_checkpoint IS NOT DISTINCT FROM $2
          AND target_checkpoint IS NOT DISTINCT FROM $3`,
			in.VerificationID,
			in.SourceCheckpoint, in.TargetCheckpoint,
			in.NewSourceCheckpoint, in.NewTargetCheckpoint,
			in.VerifiedDelta, in.MismatchDelta, in.UnverifiableDelta, in.AssumedSingleDelta,
			inserted, in.NewPhase,
			in.NewObservedSourceIDs, in.NewObservedTargetIDs,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			// The CAS lost: roll the whole transaction back — the issues
			// inserted above must never persist without their checkpoints
			// and counter deltas.
			return errCheckpointCASLost
		}
		committed = true
		return nil
	})
	if err != nil {
		if errors.Is(err, errCheckpointCASLost) {
			return false, nil
		}
		return false, err
	}
	return committed, nil
}

// FinishVerificationInput closes a run: state completed_pass/completed_fail,
// phase complete, observed coverage recorded. Issues supplied here (e.g. the
// finalize coverage finding) commit atomically with the state flip, so a
// crash between the finding and the verdict replays idempotently (the
// NULL-source partial unique index dedupes them).
type FinishVerificationInput struct {
	VerificationID    string
	Pass              bool
	ObservedSourceIDs *int64
	ObservedTargetIDs *int64
	Issues            []VerificationIssue
}

// FinishVerification completes a run atomically. The authoritative verdict
// is whatever this run ends as — the latest seq wins (see LatestVerification).
func FinishVerification(ctx context.Context, pool *pgxpool.Pool, in FinishVerificationInput) (*VerificationRun, error) {
	target := VerificationCompletedFail
	if in.Pass {
		target = VerificationCompletedPass
	}
	var r *VerificationRun
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		inserted := int64(0)
		for _, issue := range in.Issues {
			details := []byte("null")
			if issue.Details != nil {
				b, err := json.Marshal(issue.Details)
				if err != nil {
					return err
				}
				details = b
			}
			var sourceID, infoHash any
			if issue.SourceID != nil {
				sourceID = *issue.SourceID
			}
			if issue.InfoHash != nil {
				infoHash = issue.InfoHash
			}
			tag, err := tx.Exec(ctx, `INSERT INTO magnetico_migration.verification_issues
                (verification_id, source_id, info_hash, code, stage, severity, details)
                VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
                ON CONFLICT DO NOTHING`,
				in.VerificationID, sourceID, infoHash, issue.Code, issue.Stage, issue.Severity, details)
			if err != nil {
				return err
			}
			inserted += tag.RowsAffected()
		}
		row := tx.QueryRow(ctx, `UPDATE magnetico_migration.verification_runs SET
            state = $2, phase = 'complete', completed_at = now(), updated_at = now(),
            observed_source_ids = $3, observed_target_ids = $4,
            issue_count = issue_count + $5
        WHERE verification_id = $1::uuid AND state = 'running'
        RETURNING `+verificationProjection,
			in.VerificationID, target, in.ObservedSourceIDs, in.ObservedTargetIDs, inserted)
		var err error
		r, err = scanVerificationRow(row)
		return err
	})
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, &StateError{Err: ErrRunStateCorrupt, Details: []string{
			"verification run " + in.VerificationID + " is not running; cannot finish"}}
	}
	return r, nil
}

// GetVerification reads one run by ID (nil when absent).
func GetVerification(ctx context.Context, pool *pgxpool.Pool, verificationID string) (*VerificationRun, error) {
	return scanVerificationRow(pool.QueryRow(ctx, `SELECT `+verificationProjection+` FROM magnetico_migration.verification_runs
        WHERE verification_id = $1::uuid`, verificationID))
}

// LatestVerification returns the run with the HIGHEST seq of the kind —
// the AUTHORITATIVE verification result. It is a current pass iff
// `state = 'completed_pass' AND start_data_revision = progress.data_revision`;
// a newer completed_fail supersedes any older pass at the same revision,
// and a newer interrupted/aborted run leaves NO current verdict. Nil when
// the kind has no runs.
func LatestVerification(ctx context.Context, pool *pgxpool.Pool, kind string) (*VerificationRun, error) {
	return scanVerificationRow(pool.QueryRow(ctx, `SELECT `+verificationProjection+` FROM magnetico_migration.verification_runs
        WHERE kind = $1 ORDER BY seq DESC LIMIT 1`, kind))
}

// ListVerificationIssues reads a bounded page of a run's issues (stable
// order) for reporting; 04C's forensic export fetches full records itself.
func ListVerificationIssues(ctx context.Context, pool *pgxpool.Pool, verificationID string, limit int) ([]VerificationIssue, error) {
	rows, err := pool.Query(ctx, `SELECT source_id, info_hash, code, stage, severity, details
        FROM magnetico_migration.verification_issues
        WHERE verification_id = $1::uuid
        ORDER BY id
        LIMIT $2`, verificationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var issues []VerificationIssue
	for rows.Next() {
		var issue VerificationIssue
		var sourceID, infoHash any
		var details []byte
		if err := rows.Scan(&sourceID, &infoHash, &issue.Code, &issue.Stage, &issue.Severity, &details); err != nil {
			return nil, err
		}
		if sourceID != nil {
			id := sourceID.(int64)
			issue.SourceID = &id
		}
		if infoHash != nil {
			issue.InfoHash = infoHash.([]byte)
		}
		if string(details) != "null" {
			if err := json.Unmarshal(details, &issue.Details); err != nil {
				return nil, err
			}
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}
