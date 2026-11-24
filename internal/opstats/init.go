package opstats

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/persistence"
)

// initAdvisoryKey serialises concurrent stats-schema-init runs.
const initAdvisoryKey int64 = 7881644658832750187

// InitSchema creates magnetico_stats. It is non-destructive and idempotent,
// runs under pg_advisory_xact_lock, requires a verified magneticod schema
// (persistence.MakeDatabase) and creates nothing else. An existing schema is
// accepted only when it verifies exactly; it is never repaired or altered.
func InitSchema(ctx context.Context, dsn string) error {
	store, err := persistence.MakeDatabase(ctx, dsn)
	if err != nil {
		return err
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	defer pool.Close()

	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", initAdvisoryKey); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, SchemaName).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return verify(ctx, tx)
		}
		if _, err := tx.Exec(ctx, "CREATE SCHEMA "+SchemaName); err != nil {
			return err
		}
		for _, t := range tables {
			if _, err := tx.Exec(ctx, t.ddl()); err != nil {
				return fmt.Errorf("creating %s: %w", t.name, err)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+SchemaName+`.stats_version (version) VALUES ($1)`, SchemaVersion); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO `+SchemaName+`.shared_state (id) VALUES (1)`)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrSchemaMissing) || errors.Is(err, ErrSchemaInvalid) {
			return err
		}
		return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	return nil
}
