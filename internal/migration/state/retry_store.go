package state

// Retry run store: durable pass state for `torrents retry-errors`
// (retry_runs) plus the append-only attempt history (retry_attempts).
//
// The resume contract is revision-tracked: a pass bumps progress
// data_revision AND its own current_data_revision in the SAME transaction
// as every attempt, so the two stay equal by construction — any OTHER
// revision movement breaks resumability and the pass is aborted as stale.
// The pass also records its own source fingerprint; resume revalidates the
// fresh source with Fingerprint.Equivalent against it (never byte-equal
// JSON), so a restart or transport switch does not abort, but a real
// source change between interrupted attempts does — including for
// already-authorized repair passes, which then need fresh authorization.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Retry run states and attempt outcomes.
const (
	RetryRunning            = "running"
	RetryInterrupted        = "interrupted"
	RetryCompletedClean     = "completed_clean"
	RetryCompletedRemaining = "completed_remaining"
	RetryAborted            = "aborted"

	RetryOutcomeFixed        = "fixed"
	RetryOutcomeStillInvalid = "still_invalid"
	RetryOutcomeMissing      = "missing_source"
)

// RetryRun is one row of `magnetico_migration.retry_runs`.
type RetryRun struct {
	RetryID                string
	RunID                  string
	State                  string
	StartDataRevision      int64
	CurrentDataRevision    int64
	UnresolvedAtStart      int64
	LastSourceID           *int64
	AttemptedCount         int64
	FixedCount             int64
	StillInvalidCount      int64
	MissingCount           int64
	SourceFingerprintRaw   json.RawMessage
	AcceptedSourceRepair   bool
	PreviousFingerprintRaw json.RawMessage
	SettingsRaw            json.RawMessage
	StartedAt              time.Time
	UpdatedAt              time.Time
	CompletedAt            *time.Time
}

// CreateRetryRunInput seeds a new retry pass.
type CreateRetryRunInput struct {
	RunID                string
	StartDataRevision    int64
	UnresolvedAtStart    int64
	SourceFingerprintRaw json.RawMessage
	AcceptedSourceRepair bool
	// PreviousFingerprintRaw records the ORIGINAL import fingerprint when
	// --accept-source-repairs authorized a changed source (provenance only;
	// the stored import fingerprint itself is never mutated).
	PreviousFingerprintRaw json.RawMessage
	SettingsRaw            json.RawMessage
}

const retryProjection = `retry_id::text, run_id::text, state,
        start_data_revision, current_data_revision, unresolved_at_start, last_source_id,
        attempted_count, fixed_count, still_invalid_count, missing_count,
        source_fingerprint, accepted_source_repair, previous_fingerprint, settings,
        started_at, updated_at, completed_at`

func scanRetryRow(row pgx.Row) (*RetryRun, error) {
	r := &RetryRun{}
	var sourceFP, previousFP, settings []byte
	err := row.Scan(&r.RetryID, &r.RunID, &r.State,
		&r.StartDataRevision, &r.CurrentDataRevision, &r.UnresolvedAtStart, &r.LastSourceID,
		&r.AttemptedCount, &r.FixedCount, &r.StillInvalidCount, &r.MissingCount,
		&sourceFP, &r.AcceptedSourceRepair, &previousFP, &settings,
		&r.StartedAt, &r.UpdatedAt, &r.CompletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.SourceFingerprintRaw = sourceFP
	r.PreviousFingerprintRaw = previousFP
	r.SettingsRaw = settings
	return r, nil
}

// StartRetryRun adopts a compatible non-terminal retry pass or creates a
// fresh one. Caller holds the run lock (a leftover `running` row proves its
// holder dead; a graceful interrupt leaves `interrupted`).
//
// The resumable callback implements the revision-tracked resume discipline:
// `state IN (running,interrupted) AND current_data_revision ==
// progress.data_revision AND Fingerprint.Equivalent(fresh,
// retry_runs.source_fingerprint)`. An incompatible candidate is aborted
// (history preserved) and a fresh pass starts over the still-unresolved
// IDs; more than one non-terminal pass is corrupt state.
func StartRetryRun(ctx context.Context, pool *pgxpool.Pool, in CreateRetryRunInput, resumable func(*RetryRun) bool) (*RetryRun, bool, error) {
	var candidate *RetryRun
	var nonTerminal int64
	err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.retry_runs
        WHERE state IN ('running','interrupted')`).Scan(&nonTerminal)
	if err != nil {
		return nil, false, err
	}
	if nonTerminal > 1 {
		return nil, false, &StateError{Err: ErrRunStateCorrupt, Details: []string{
			fmt.Sprintf("%d non-terminal retry runs exist, want at most 1", nonTerminal)}}
	}
	if nonTerminal == 1 {
		row := pool.QueryRow(ctx, `SELECT `+retryProjection+` FROM magnetico_migration.retry_runs
            WHERE state IN ('running','interrupted')`)
		candidate, err = scanRetryRow(row)
		if err != nil {
			return nil, false, err
		}
	}

	if candidate != nil && resumable != nil && resumable(candidate) {
		if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.retry_runs
            SET state = 'running', updated_at = now()
            WHERE retry_id = $1::uuid AND state IN ('running','interrupted')`,
			candidate.RetryID); err != nil {
			return nil, false, err
		}
		candidate.State = RetryRunning
		return candidate, true, nil
	}
	if candidate != nil {
		if err := MarkRetryAborted(ctx, pool, candidate.RetryID); err != nil {
			return nil, false, err
		}
	}
	created, err := CreateRetryRun(ctx, pool, in)
	if err != nil {
		return nil, false, err
	}
	return created, false, nil
}

// CreateRetryRun inserts a fresh retry pass; current_data_revision starts
// equal to start_data_revision (the progress revision at adoption).
func CreateRetryRun(ctx context.Context, pool *pgxpool.Pool, in CreateRetryRunInput) (*RetryRun, error) {
	settings := in.SettingsRaw
	if settings == nil {
		settings = []byte("{}")
	}
	retryID, err := NewRunID()
	if err != nil {
		return nil, err
	}
	row := pool.QueryRow(ctx, `INSERT INTO magnetico_migration.retry_runs
        (retry_id, run_id, state, start_data_revision, current_data_revision,
         unresolved_at_start, source_fingerprint, accepted_source_repair,
         previous_fingerprint, settings)
        VALUES ($1::uuid, $2::uuid, 'running', $3, $3, $4, $5, $6, $7, $8)
        RETURNING `+retryProjection,
		retryID, in.RunID, in.StartDataRevision, in.UnresolvedAtStart,
		in.SourceFingerprintRaw, in.AcceptedSourceRepair, in.PreviousFingerprintRaw, settings)
	return scanRetryRow(row)
}

// MarkRetryAborted marks a non-terminal pass aborted (terminal history).
func MarkRetryAborted(ctx context.Context, pool *pgxpool.Pool, retryID string) error {
	tag, err := pool.Exec(ctx, `UPDATE magnetico_migration.retry_runs
        SET state = 'aborted', completed_at = now(), updated_at = now()
        WHERE retry_id = $1::uuid AND state IN ('running','interrupted')`, retryID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &StateError{Err: ErrRunStateCorrupt, Details: []string{
			"retry run " + retryID + " is not non-terminal; cannot abort"}}
	}
	return nil
}

// MarkRetryInterrupted records a graceful shutdown; resumable exactly like
// a crash (the next StartRetryRun adopts it).
func MarkRetryInterrupted(ctx context.Context, pool *pgxpool.Pool, retryID string) error {
	tag, err := pool.Exec(ctx, `UPDATE magnetico_migration.retry_runs
        SET state = 'interrupted', updated_at = now()
        WHERE retry_id = $1::uuid AND state = 'running'`, retryID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &StateError{Err: ErrRunStateCorrupt, Details: []string{
			"retry run " + retryID + " is not running; cannot mark interrupted"}}
	}
	return nil
}

// FinishRetryRun closes a pass: completed_clean when no unresolved record
// remains, completed_remaining otherwise.
func FinishRetryRun(ctx context.Context, pool *pgxpool.Pool, retryID string, clean bool) (*RetryRun, error) {
	target := RetryCompletedRemaining
	if clean {
		target = RetryCompletedClean
	}
	row := pool.QueryRow(ctx, `UPDATE magnetico_migration.retry_runs SET
        state = $2, completed_at = now(), updated_at = now()
    WHERE retry_id = $1::uuid AND state = 'running'
    RETURNING `+retryProjection, retryID, target)
	r, err := scanRetryRow(row)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, &StateError{Err: ErrRunStateCorrupt, Details: []string{
			"retry run " + retryID + " is not running; cannot finish"}}
	}
	return r, nil
}

// RetryAttempt is one append-only diagnostic row: the original record error
// is attempt 1 and stays untouched; every retry appends here (the unique
// (retry_id, source_id) index mechanically prevents repeating a settled
// attempt within one pass and doubles as the lost-COMMIT marker).
type RetryAttempt struct {
	RetryID        string
	RunID          string
	SourceID       int64
	SourceInfoHash []byte
	Outcome        string
	Stage          string
	Code           string
	Details        map[string]any
}

// InsertRetryAttempt appends one attempt row inside the caller's
// transaction. A duplicate (retry, source) fails the unique index and
// aborts the transaction — a settled attempt is never silently repeated.
func InsertRetryAttempt(ctx context.Context, tx pgx.Tx, in RetryAttempt) error {
	details := []byte("null")
	if in.Details != nil {
		b, err := json.Marshal(in.Details)
		if err != nil {
			return err
		}
		details = b
	}
	var infoHash any
	if in.SourceInfoHash != nil {
		infoHash = in.SourceInfoHash
	}
	_, err := tx.Exec(ctx, `INSERT INTO magnetico_migration.retry_attempts
        (retry_id, run_id, source_id, source_info_hash, outcome, stage, code, details)
        VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8)`,
		in.RetryID, in.RunID, in.SourceID, infoHash, in.Outcome, in.Stage, in.Code, details)
	return err
}

// AdvanceRetryInput is the per-attempt retry-run update applied in the SAME
// transaction as the attempt's data mutations: counter deltas, the
// revision bump and the cursor compare-and-swap.
type AdvanceRetryInput struct {
	RetryID string
	// ExpectedLastSourceID is the cursor CAS (nil = still NULL).
	ExpectedLastSourceID *int64
	NewLastSourceID      *int64
	FixedDelta           int64
	StillInvalidDelta    int64
	MissingDelta         int64
}

// AdvanceRetryRun updates the retry run inside the caller's transaction.
// It returns false when the cursor CAS lost or the pass is not `running`
// (the caller must roll back and reconcile).
func AdvanceRetryRun(ctx context.Context, tx pgx.Tx, in AdvanceRetryInput) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE magnetico_migration.retry_runs SET
        attempted_count = attempted_count + 1,
        fixed_count = fixed_count + $3,
        still_invalid_count = still_invalid_count + $4,
        missing_count = missing_count + $5,
        current_data_revision = current_data_revision + 1,
        last_source_id = $6,
        updated_at = now()
    WHERE retry_id = $1::uuid AND state = 'running'
      AND last_source_id IS NOT DISTINCT FROM $2`,
		in.RetryID, in.ExpectedLastSourceID, in.FixedDelta,
		in.StillInvalidDelta, in.MissingDelta, in.NewLastSourceID,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// GetRetryRun reads one pass by ID (nil when absent).
func GetRetryRun(ctx context.Context, pool *pgxpool.Pool, retryID string) (*RetryRun, error) {
	return scanRetryRow(pool.QueryRow(ctx, `SELECT `+retryProjection+` FROM magnetico_migration.retry_runs
        WHERE retry_id = $1::uuid`, retryID))
}

// LatestRetryRun returns the most recent pass of the run (latest start) —
// the authoritative retry history for reporting. Nil when the run has no
// retry passes.
func LatestRetryRun(ctx context.Context, pool *pgxpool.Pool, runID string) (*RetryRun, error) {
	return scanRetryRow(pool.QueryRow(ctx, `SELECT `+retryProjection+` FROM magnetico_migration.retry_runs
        WHERE run_id = $1::uuid ORDER BY started_at DESC LIMIT 1`, runID))
}

// RetryAttemptRow is the durable form of one attempt (for reconciliation
// and 04C export reads).
type RetryAttemptRow struct {
	SourceID  int64
	Outcome   string
	Code      string
	Stage     string
	Attempted time.Time
}

// GetRetryAttempt reads one settled attempt (nil when absent) — the
// lost-COMMIT marker probe.
func GetRetryAttempt(ctx context.Context, pool *pgxpool.Pool, retryID string, sourceID int64) (*RetryAttemptRow, error) {
	row := &RetryAttemptRow{}
	err := pool.QueryRow(ctx, `SELECT source_id, outcome, code, stage, attempted_at
        FROM magnetico_migration.retry_attempts
        WHERE retry_id = $1::uuid AND source_id = $2`, retryID, sourceID).
		Scan(&row.SourceID, &row.Outcome, &row.Code, &row.Stage, &row.Attempted)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return row, nil
}

// ListRetryAttempts reads a bounded page of a pass's attempts (stable
// order) for reporting.
func ListRetryAttempts(ctx context.Context, pool *pgxpool.Pool, retryID string, limit int) ([]RetryAttemptRow, error) {
	rows, err := pool.Query(ctx, `SELECT source_id, outcome, code, stage, attempted_at
        FROM magnetico_migration.retry_attempts
        WHERE retry_id = $1::uuid
        ORDER BY id
        LIMIT $2`, retryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetryAttemptRow
	for rows.Next() {
		var r RetryAttemptRow
		if err := rows.Scan(&r.SourceID, &r.Outcome, &r.Code, &r.Stage, &r.Attempted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
