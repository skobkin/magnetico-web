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

type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
