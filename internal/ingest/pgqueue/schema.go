// Package pgqueue is the server-side durable ingest queue
// (docs/specs/crawler-ingest-delivery-v1.md §6): the magnetico_ingest
// PostgreSQL schema, all-or-nothing admission with a transactional byte
// quota, quarantine, and the single sequential consumer.
//
// Table names deliberately avoid torrents/files/torrent_file_blobs/
// schema_version: persistence treats such tables in ANY schema as legacy
// markers.
package pgqueue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/persistence"
)

// SchemaVersion is the only accepted magnetico_ingest.ingest_version row.
const SchemaVersion = 1

// FormatRecordV1 is the payload format code of record v1 JSON.
const FormatRecordV1 = 1

// initAdvisoryKey serialises concurrent ingest-schema-init runs.
const initAdvisoryKey int64 = 7881644658832750186

// ErrSchemaMissing reports that magnetico_ingest is absent or not exactly
// the expected version.
var ErrSchemaMissing = errors.New(
	"the magnetico_ingest schema is missing or has an unsupported version; run `magnetico migrate ingest-schema-init`")

// schemaStatements create the queue schema. usage is maintained exclusively
// by statement-level triggers so every write path (including manual operator
// SQL) keeps the accounting exact.
var schemaStatements = []string{
	`CREATE SCHEMA magnetico_ingest`,

	`CREATE TABLE magnetico_ingest.ingest_version (version INTEGER PRIMARY KEY)`,

	`CREATE TABLE magnetico_ingest.queue (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    info_hash BYTEA NOT NULL UNIQUE,
    format SMALLINT NOT NULL,
    payload BYTEA NOT NULL,
    payload_bytes INTEGER NOT NULL CHECK (payload_bytes >= 0),
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error_code TEXT
) WITH (autovacuum_vacuum_scale_factor = 0.02, autovacuum_vacuum_threshold = 200,
        autovacuum_vacuum_cost_delay = 2)`,

	`CREATE TABLE magnetico_ingest.quarantine (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    info_hash BYTEA NOT NULL,
    format SMALLINT NOT NULL,
    payload BYTEA NOT NULL,
    payload_bytes INTEGER NOT NULL CHECK (payload_bytes >= 0),
    reason_code TEXT NOT NULL,
    detail TEXT,
    attempts INTEGER NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    quarantined_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`,

	`CREATE TABLE magnetico_ingest.usage (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    used_bytes BIGINT NOT NULL CHECK (used_bytes >= 0)
)`,
	`INSERT INTO magnetico_ingest.usage (id, used_bytes) VALUES (1, 0)`,

	// Transition-table triggers: one UPDATE of the usage row per statement.
	// The row lock it takes is held until COMMIT, which serialises
	// admissions and bounds the total across any number of ingest instances.
	`CREATE FUNCTION magnetico_ingest.usage_add() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE d BIGINT;
BEGIN
    SELECT COALESCE(SUM(payload_bytes), 0) INTO d FROM newtab;
    IF d <> 0 THEN
        UPDATE magnetico_ingest.usage SET used_bytes = used_bytes + d WHERE id = 1;
    END IF;
    RETURN NULL;
END $$`,

	`CREATE FUNCTION magnetico_ingest.usage_sub() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE d BIGINT;
BEGIN
    SELECT COALESCE(SUM(payload_bytes), 0) INTO d FROM oldtab;
    IF d <> 0 THEN
        UPDATE magnetico_ingest.usage SET used_bytes = used_bytes - d WHERE id = 1;
    END IF;
    RETURN NULL;
END $$`,

	`CREATE FUNCTION magnetico_ingest.reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'magnetico_ingest: % on %.% is not allowed (it would desynchronise usage accounting)',
        TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME;
END $$`,

	`CREATE TRIGGER queue_usage_ins AFTER INSERT ON magnetico_ingest.queue
    REFERENCING NEW TABLE AS newtab FOR EACH STATEMENT EXECUTE FUNCTION magnetico_ingest.usage_add()`,
	`CREATE TRIGGER queue_usage_del AFTER DELETE ON magnetico_ingest.queue
    REFERENCING OLD TABLE AS oldtab FOR EACH STATEMENT EXECUTE FUNCTION magnetico_ingest.usage_sub()`,
	`CREATE TRIGGER quarantine_usage_ins AFTER INSERT ON magnetico_ingest.quarantine
    REFERENCING NEW TABLE AS newtab FOR EACH STATEMENT EXECUTE FUNCTION magnetico_ingest.usage_add()`,
	`CREATE TRIGGER quarantine_usage_del AFTER DELETE ON magnetico_ingest.quarantine
    REFERENCING OLD TABLE AS oldtab FOR EACH STATEMENT EXECUTE FUNCTION magnetico_ingest.usage_sub()`,

	`CREATE TRIGGER queue_no_payload_update BEFORE UPDATE OF payload_bytes, payload ON magnetico_ingest.queue
    FOR EACH ROW EXECUTE FUNCTION magnetico_ingest.reject_mutation()`,
	`CREATE TRIGGER quarantine_no_payload_update BEFORE UPDATE OF payload_bytes, payload ON magnetico_ingest.quarantine
    FOR EACH ROW EXECUTE FUNCTION magnetico_ingest.reject_mutation()`,
	`CREATE TRIGGER queue_no_truncate BEFORE TRUNCATE ON magnetico_ingest.queue
    FOR EACH STATEMENT EXECUTE FUNCTION magnetico_ingest.reject_mutation()`,
	`CREATE TRIGGER quarantine_no_truncate BEFORE TRUNCATE ON magnetico_ingest.quarantine
    FOR EACH STATEMENT EXECUTE FUNCTION magnetico_ingest.reject_mutation()`,
}

// InitSchema creates magnetico_ingest. It is non-destructive and idempotent,
// runs under pg_advisory_xact_lock, requires a verified magneticod schema
// (persistence.MakeDatabase) and creates nothing else. An existing schema
// is accepted only when its version row is exactly {SchemaVersion}.
func InitSchema(ctx context.Context, dsn string) error {
	// Requirement: a verified magneticod schema at the supported version.
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
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'magnetico_ingest')`).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return verifyVersion(ctx, tx)
		}
		for _, stmt := range schemaStatements {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO magnetico_ingest.ingest_version (version) VALUES ($1)`, SchemaVersion)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrSchemaMissing) {
			return err
		}
		return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	return nil
}

type rowQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// verifyVersion checks ingest_version == {SchemaVersion}. A missing table
// (partial schema) is reported as ErrSchemaMissing.
func verifyVersion(ctx context.Context, q rowQueryer) error {
	var reg *string
	rows, err := q.Query(ctx, `SELECT to_regclass('magnetico_ingest.ingest_version')::text`)
	if err != nil {
		return err
	}
	if rows.Next() {
		if err := rows.Scan(&reg); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	if reg == nil {
		return ErrSchemaMissing
	}
	vrows, err := q.Query(ctx, `SELECT version FROM magnetico_ingest.ingest_version`)
	if err != nil {
		return err
	}
	defer vrows.Close()
	var versions []int32
	for vrows.Next() {
		var v int32
		if err := vrows.Scan(&v); err != nil {
			return err
		}
		versions = append(versions, v)
	}
	if err := vrows.Err(); err != nil {
		return err
	}
	if len(versions) != 1 || versions[0] != SchemaVersion {
		return fmt.Errorf("%w (found versions %v)", ErrSchemaMissing, versions)
	}
	return nil
}
