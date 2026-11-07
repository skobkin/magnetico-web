package target

// Index deferral and finalization: the authorized drop of the three
// secondary indexes (atomic with its progress marker, on the lock-owning
// connection), the crash-recovering rebuild during building_indexes, the
// identity-sequence reseed, statistics and the completion phase.
//
// All index DDL executes on the lock-owning connection (serialized with the
// liveness monitor by RunLock.With), tying DDL authority to held ownership.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/persistence"
)

// secondaryIndexNames is the fixed drop/rebuild order of the three secondary
// indexes (definition fragments: secondaryIndexDefinitions in verify.go).
var secondaryIndexNames = []string{
	"idx_torrents_total_size",
	"idx_torrents_discovered_on",
	"idx_torrents_name_gin_trgm",
}

// secondaryIndexDDL creates each index exactly as persistence's schema-init
// does (schemaStatements), so a rebuilt index is definition-identical to a
// schema-init one.
var secondaryIndexDDL = map[string]string{
	"idx_torrents_total_size":    `CREATE INDEX idx_torrents_total_size ON magneticod.torrents (total_size)`,
	"idx_torrents_discovered_on": `CREATE INDEX idx_torrents_discovered_on ON magneticod.torrents (discovered_on)`,
	"idx_torrents_name_gin_trgm": `CREATE INDEX idx_torrents_name_gin_trgm ON magneticod.torrents USING GIN (name gin_trgm_ops)`,
}

// ErrIdentityExhausted reports a torrent ID at the signed 32-bit limit: the
// identity sequence cannot be reseeded beyond it.
var ErrIdentityExhausted = errors.New(
	"torrent identity sequence exhausted (max id reached the signed 32-bit limit); " +
		"the target schema cannot hold further torrents")

// catalogRow queries the migration catalog.
type catalogRow interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// secondaryIndexState reads one secondary index's catalog state. present
// excludes constraint-backing indexes — the migrator never touches those.
func secondaryIndexState(ctx context.Context, q catalogRow, name string) (valid bool, def string, present bool, err error) {
	err = q.QueryRow(ctx, `SELECT ix.indisvalid, pg_get_indexdef(ix.indexrelid)
        FROM pg_index ix
        JOIN pg_class i ON i.oid = ix.indexrelid
        JOIN pg_class t ON t.oid = ix.indrelid
        JOIN pg_namespace n ON n.oid = t.relnamespace
        WHERE n.nspname = 'magneticod' AND i.relname = $1
          AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = i.oid)`, name,
	).Scan(&valid, &def)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", false, nil
	}
	if err != nil {
		return false, "", false, err
	}
	return valid, def, true, nil
}

// verifyIndexDefinition refuses a present index that is invalid or does not
// match the schema-init definition: never trust the name alone, never drop
// and recreate silently.
func verifyIndexDefinition(name, def string, valid bool) error {
	var details []string
	if !valid {
		details = append(details, fmt.Sprintf("index magneticod.%s is not valid", name))
	}
	if !strings.Contains(def, secondaryIndexDefinitions[name]) {
		details = append(details, fmt.Sprintf(
			"index magneticod.%s does not match the expected definition (%s)", name, secondaryIndexDefinitions[name]))
	}
	if len(details) > 0 {
		return &persistence.SchemaStateError{Err: persistence.ErrUnsupportedSchema, Details: details}
	}
	return nil
}

// DropSecondaryIndexes atomically removes exactly the three secondary
// indexes and marks the run indexes_deferred — one transaction on the
// lock-owning connection, so either both happen or neither. The target must
// currently carry all three (strict state); anything else is refused without
// touching it.
func DropSecondaryIndexes(ctx context.Context, lock *state.RunLock, runID string) error {
	return lock.With(ctx, func(conn *pgx.Conn) error {
		return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			var details []string
			for _, name := range secondaryIndexNames {
				valid, def, present, err := secondaryIndexState(ctx, tx, name)
				if err != nil {
					return err
				}
				if !present {
					details = append(details, "cannot defer: index magneticod."+name+" is already absent")
					continue
				}
				if err := verifyIndexDefinition(name, def, valid); err != nil {
					details = append(details, fmt.Sprintf("cannot defer: %v", err))
				}
			}
			if len(details) > 0 {
				return &persistence.SchemaStateError{Err: persistence.ErrUnsupportedSchema, Details: details}
			}
			for _, name := range secondaryIndexNames {
				if _, err := tx.Exec(ctx, `DROP INDEX magneticod.`+name); err != nil {
					return err
				}
			}
			tag, err := tx.Exec(ctx, `UPDATE magnetico_migration.progress
                SET indexes_deferred = TRUE, updated_at = now()
                WHERE run_id = $1::uuid AND indexes_deferred = FALSE`, runID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("%w: the deferred-drop marker did not apply (indexes_deferred already true or run state moved)",
					ErrTargetDrift)
			}
			return nil
		})
	})
}

// BuildSecondaryIndexes reconciles the building_indexes phase against the
// actual catalog, one index per transaction on the lock-owning connection:
// a present, valid, definition-identical index is kept; a missing one is
// created (plain CREATE INDEX, never CONCURRENTLY) and verified inside the
// same transaction. Partial progress after a crash is valid input.
func BuildSecondaryIndexes(ctx context.Context, lock *state.RunLock) error {
	for _, name := range secondaryIndexNames {
		err := lock.With(ctx, func(conn *pgx.Conn) error {
			return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				valid, def, present, err := secondaryIndexState(ctx, tx, name)
				if err != nil {
					return err
				}
				if present {
					return verifyIndexDefinition(name, def, valid)
				}
				if _, err := tx.Exec(ctx, secondaryIndexDDL[name]); err != nil {
					return err
				}
				// Re-read inside the creating transaction: the index must now
				// exist, be valid and match the schema-init definition.
				valid, def, present, err = secondaryIndexState(ctx, tx, name)
				if err != nil {
					return err
				}
				if !present {
					return fmt.Errorf("index magneticod.%s did not appear after CREATE INDEX", name)
				}
				return verifyIndexDefinition(name, def, valid)
			})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ReseedIdentity sets the torrents identity sequence past the imported max
// ID, so the application's next generated ID cannot collide with an imported
// one. An empty import leaves the sequence at its initial state (documented).
// Reports whether a reseed was performed.
func ReseedIdentity(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var maxID *int64
	if err := pool.QueryRow(ctx,
		`SELECT MAX(id) FROM magneticod.torrents`).Scan(&maxID); err != nil {
		return false, err
	}
	if maxID == nil {
		// Empty import: the sequence stays at its initial state (next ID 1).
		return false, nil
	}
	if *maxID == math.MaxInt32 {
		return false, fmt.Errorf("%w: max torrent id %d", ErrIdentityExhausted, *maxID)
	}
	// is_called=false: nextval returns exactly max+1.
	if _, err := pool.Exec(ctx,
		`SELECT setval(pg_get_serial_sequence('magneticod.torrents', 'id'), $1, false)`, *maxID+1); err != nil {
		return false, err
	}
	return true, nil
}

// AnalyzeTarget refreshes planner statistics for the imported tables.
func AnalyzeTarget(ctx context.Context, pool *pgxpool.Pool) error {
	for _, stmt := range []string{
		`ANALYZE magneticod.torrents`,
		`ANALYZE magneticod.torrent_file_blobs`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// CompleteImport commits the completion phase: import_complete only with
// zero record errors AND zero unresolved integrity run-scope errors
// (run_error_count); recorded operational incidents are history and never
// force import_complete_with_errors. Returns the committed phase.
func CompleteImport(ctx context.Context, pool *pgxpool.Pool, runID string, checkpoint int64) (state.Phase, error) {
	var errCount, runErrCount int64
	if err := pool.QueryRow(ctx, `SELECT error_count, run_error_count
        FROM magnetico_migration.progress WHERE run_id = $1::uuid`, runID,
	).Scan(&errCount, &runErrCount); err != nil {
		return "", err
	}
	phase := state.PhaseImportComplete
	if errCount > 0 || runErrCount > 0 {
		phase = state.PhaseCompleteWithErrors
	}
	if err := TransitionPhase(ctx, pool, runID, state.AdvanceInput{
		RunID:          runID,
		ExpectedPhase:  state.PhaseBuildingIndexes,
		NewPhase:       phase,
		ExpectedLastID: checkpoint,
		NewLastID:      checkpoint,
	}); err != nil {
		return "", err
	}
	return phase, nil
}
