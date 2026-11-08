package state

// Error-registry primitives. The batch writer composes these into its
// transaction; incident recording runs in its own transaction.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InsertErrorRecords appends error rows inside the caller's transaction.
// Details maps are marshalled as JSONB; callers must keep them secret-free
// and small (values are capped at 4 KiB by the writer before it calls this).
func InsertErrorRecords(ctx context.Context, tx pgx.Tx, runID string, recs []ErrorRecord) error {
	for _, rec := range recs {
		details := []byte("null")
		if rec.Details != nil {
			b, err := json.Marshal(rec.Details)
			if err != nil {
				return fmt.Errorf("error record details: %w", err)
			}
			details = b
		}
		var sourceID any
		if rec.SourceID != nil {
			sourceID = *rec.SourceID
		}
		var infoHash any
		if rec.SourceInfoHash != nil {
			infoHash = rec.SourceInfoHash
		}
		if _, err := tx.Exec(ctx, `INSERT INTO magnetico_migration.errors
            (run_id, scope, source_id, source_info_hash, stage, category, code, details)
            VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)`,
			runID, rec.Scope, sourceID, infoHash, rec.Stage, rec.Category, rec.Code, string(details),
		); err != nil {
			return err
		}
	}
	return nil
}

// RecordIncident writes one run-scope incident in its own transaction when
// the target is reachable. Integrity incidents bump run_error_count;
// operational incidents are history only.
func RecordIncident(ctx context.Context, pool *pgxpool.Pool, runID string, rec ErrorRecord) error {
	if rec.Scope != ScopeRun {
		return fmt.Errorf("incident scope must be %q", ScopeRun)
	}
	rec.Scope = ScopeRun
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := InsertErrorRecords(ctx, tx, runID, []ErrorRecord{rec}); err != nil {
			return err
		}
		if rec.Category == CategoryIntegrity {
			if _, err := tx.Exec(ctx, `UPDATE magnetico_migration.progress
                SET run_error_count = run_error_count + 1, updated_at = now()
                WHERE run_id = $1::uuid`, runID); err != nil {
				return err
			}
		}
		return nil
	})
}

// EnsureIncident records a run-scope incident exactly once per (run, code):
// if an unresolved incident with the same code already exists, it is left
// untouched and nothing is counted. Resume-safe — a crash between run-state
// creation and incident recording is repaired on the next start. Integrity
// incidents bump run_error_count; operational incidents are history only.
func EnsureIncident(ctx context.Context, pool *pgxpool.Pool, runID string, rec ErrorRecord) (inserted bool, err error) {
	if rec.Scope != ScopeRun {
		return false, fmt.Errorf("incident scope must be %q", ScopeRun)
	}
	rec.Scope = ScopeRun
	inserted = false
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var existing int64
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.errors
            WHERE run_id = $1::uuid AND scope = 'run' AND code = $2 AND NOT resolved`,
			runID, rec.Code).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			return nil
		}
		if err := InsertErrorRecords(ctx, tx, runID, []ErrorRecord{rec}); err != nil {
			return err
		}
		if rec.Category == CategoryIntegrity {
			if _, err := tx.Exec(ctx, `UPDATE magnetico_migration.progress
                SET run_error_count = run_error_count + 1, updated_at = now()
                WHERE run_id = $1::uuid`, runID); err != nil {
				return err
			}
		}
		inserted = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return inserted, nil
}

// CountErrors returns the registry counts compared by Reconcile. The record
// count is UNRESOLVED record errors: after retry passes resolve their
// records, progress.error_count must track the unresolved set while the full
// history (resolved rows included) stays in the registry. Backward
// compatible with 04A data, where record errors always start unresolved.
func CountErrors(ctx context.Context, q queryer, runID string) (record int64, unresolvedIntegrity int64, err error) {
	if err := q.QueryRow(ctx, `SELECT
            COUNT(*) FILTER (WHERE scope = 'record' AND NOT resolved),
            COUNT(*) FILTER (WHERE scope = 'run' AND category = 'integrity' AND NOT resolved)
        FROM magnetico_migration.errors
        WHERE run_id = $1::uuid`, runID).Scan(&record, &unresolvedIntegrity); err != nil {
		return 0, 0, err
	}
	return record, unresolvedIntegrity, nil
}

// CountUnresolvedRecords returns the number of UNRESOLVED record-scope
// errors — the work list a retry pass reports against.
func CountUnresolvedRecords(ctx context.Context, q queryer, runID string) (int64, error) {
	var n int64
	if err := q.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.errors
        WHERE run_id = $1::uuid AND scope = 'record' AND NOT resolved`, runID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// UnresolvedRecordIDs reads one keyset page of unresolved record-scope IDs
// (ascending; limit-bounded). A retry pass re-queries it per batch —
// resolving IDs shrinks the set, so no stale work list accumulates.
func UnresolvedRecordIDs(ctx context.Context, q queryer, runID string, afterID int64, limit int) ([]int64, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("unresolved ID page limit must be positive, got %d", limit)
	}
	rows, err := q.Query(ctx, `SELECT source_id FROM magnetico_migration.errors
        WHERE run_id = $1::uuid AND scope = 'record' AND NOT resolved
          AND source_id > $2::bigint
        ORDER BY source_id
        LIMIT $3`, runID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// UncoveredUnresolvedRecords counts the unresolved record-scope registry
// entries of the import run that carry NO per-row unresolved_record_error
// issue for the given verification run. A merge scan emits that issue only
// when it classifies the entry's source row — an entry whose source row has
// vanished is never seen, so the content pass closes the gap with this
// aggregate check at finalize: the equality gate must never pass while an
// unresolved record is unaccounted for. Returns the count and the min/max
// source IDs of the uncovered entries (0/0/0 when fully covered).
func UncoveredUnresolvedRecords(ctx context.Context, q queryer, runID, verificationID string) (count, minID, maxID int64, err error) {
	err = q.QueryRow(ctx, `SELECT
        COUNT(*), COALESCE(MIN(e.source_id), 0), COALESCE(MAX(e.source_id), 0)
    FROM magnetico_migration.errors e
    WHERE e.run_id = $1::uuid AND e.scope = 'record' AND NOT e.resolved
      AND NOT EXISTS (
          SELECT 1 FROM magnetico_migration.verification_issues vi
          WHERE vi.verification_id = $2::uuid
            AND vi.code = 'unresolved_record_error'
            AND vi.source_id = e.source_id)`, runID, verificationID).Scan(&count, &minID, &maxID)
	return count, minID, maxID, err
}
