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
	"sort"
	"strings"

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

// ReinitHint names the manual steps for an unreleased development schema that
// predates a structural change. Nothing in this package ever repairs or
// upgrades an existing schema: operators reinitialise it explicitly (UPGRADE.md).
const ReinitHint = "reinitialise the development schema by hand: stop ingest, run " +
	"`DROP SCHEMA magnetico_ingest CASCADE;` (records acknowledged but not yet processed are lost), " +
	"then `magnetico migrate ingest-schema-init` and `magnetico ingest crawler add <name>` (see UPGRADE.md)"

// ErrSchemaInvalid is matched by every *SchemaInvalidError: the schema has
// the supported version but is not structurally what this release expects.
var ErrSchemaInvalid = errors.New("the magnetico_ingest schema is structurally incompatible")

// SchemaInvalidError lists what is wrong with an existing magnetico_ingest.
type SchemaInvalidError struct{ Details []string }

func (e *SchemaInvalidError) Error() string {
	return "the magnetico_ingest schema is structurally incompatible (" + strings.Join(e.Details, "; ") +
		"); it is never repaired or upgraded automatically: " + ReinitHint
}

// Is makes errors.Is(err, ErrSchemaInvalid) true.
func (e *SchemaInvalidError) Is(target error) bool { return target == ErrSchemaInvalid }

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

	// The crawler registry (per-crawler credentials): the token is never
	// stored, only its SHA-256 digest. No triggers.
	`CREATE TABLE magnetico_ingest.crawlers (
    crawler_id UUID PRIMARY KEY,
    name TEXT NOT NULL CONSTRAINT crawlers_name_format
        CHECK (char_length(name) BETWEEN 1 AND 64 AND name ~ '^[A-Za-z0-9][A-Za-z0-9._-]*$'),
    token_hash BYTEA NOT NULL CONSTRAINT crawlers_token_hash_len CHECK (octet_length(token_hash) = 32),
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    token_rotated_at TIMESTAMPTZ,
    disabled_at TIMESTAMPTZ,
    CONSTRAINT crawlers_token_hash_key UNIQUE (token_hash)
)`,
	`CREATE UNIQUE INDEX crawlers_name_lower ON magnetico_ingest.crawlers (lower(name))`,

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
			return verifySchema(ctx, tx)
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
		if errors.Is(err, ErrSchemaMissing) || errors.Is(err, ErrSchemaInvalid) {
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

// verifySchema is the verification run by InitSchema (existing schema) and by
// Open: the version row, then the structure of the crawler registry.
func verifySchema(ctx context.Context, q rowQueryer) error {
	if err := verifyVersion(ctx, q); err != nil {
		return err
	}
	return verifyCrawlers(ctx, q)
}

// crawlerColumns is the exact column list of magnetico_ingest.crawlers.
var crawlerColumns = map[string]string{
	"crawler_id": "uuid", "name": "text", "token_hash": "bytea", "enabled": "boolean",
	"created_at": "timestamp with time zone", "token_rotated_at": "timestamp with time zone",
	"disabled_at": "timestamp with time zone",
}

// verifyCrawlers checks that the registry has exactly the expected columns,
// the unique digest and case-insensitive name indexes, the digest length
// check and no triggers. It only reads.
func verifyCrawlers(ctx context.Context, q rowQueryer) error {
	var details []string
	collect := func(sql string, fn func(rows pgx.Rows) error) error {
		rows, err := q.Query(ctx, sql)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := fn(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}

	var reg *string
	if err := collect(`SELECT to_regclass('magnetico_ingest.crawlers')::text`, func(r pgx.Rows) error { return r.Scan(&reg) }); err != nil {
		return err
	}
	if reg == nil {
		return &SchemaInvalidError{Details: []string{"missing table crawlers"}}
	}

	have := map[string]string{}
	if err := collect(`SELECT a.attname, format_type(a.atttypid, a.atttypmod)
FROM pg_attribute a WHERE a.attrelid = 'magnetico_ingest.crawlers'::regclass AND a.attnum > 0 AND NOT a.attisdropped`,
		func(r pgx.Rows) error {
			var n, t string
			if err := r.Scan(&n, &t); err != nil {
				return err
			}
			have[n] = t
			return nil
		}); err != nil {
		return err
	}
	for n, t := range crawlerColumns {
		if have[n] != t {
			if have[n] == "" {
				details = append(details, "crawlers lacks column "+n)
			} else {
				details = append(details, fmt.Sprintf("crawlers.%s has type %s, want %s", n, have[n], t))
			}
		}
	}
	for n := range have {
		if _, ok := crawlerColumns[n]; !ok {
			details = append(details, "crawlers has unexpected column "+n)
		}
	}

	var uniqueDigest, uniqueName bool
	if err := collect(`SELECT pg_get_indexdef(i.indexrelid) FROM pg_index i
WHERE i.indrelid = 'magnetico_ingest.crawlers'::regclass AND i.indisunique`, func(r pgx.Rows) error {
		var def string
		if err := r.Scan(&def); err != nil {
			return err
		}
		switch {
		case strings.Contains(def, "(token_hash)"):
			uniqueDigest = true
		case strings.Contains(def, "lower(name)"):
			uniqueName = true
		}
		return nil
	}); err != nil {
		return err
	}
	if !uniqueDigest {
		details = append(details, "crawlers lacks a unique index on token_hash")
	}
	if !uniqueName {
		details = append(details, "crawlers lacks a unique index on lower(name)")
	}

	var hashLenCheck bool
	if err := collect(`SELECT EXISTS (SELECT 1 FROM pg_constraint
WHERE conrelid = 'magnetico_ingest.crawlers'::regclass AND contype = 'c'
  AND pg_get_constraintdef(oid) LIKE '%octet_length(token_hash) = 32%')`, func(r pgx.Rows) error { return r.Scan(&hashLenCheck) }); err != nil {
		return err
	}
	if !hashLenCheck {
		details = append(details, "crawlers lacks the token_hash length check")
	}

	var triggers int
	if err := collect(`SELECT count(*) FROM pg_trigger
WHERE tgrelid = 'magnetico_ingest.crawlers'::regclass AND NOT tgisinternal`, func(r pgx.Rows) error { return r.Scan(&triggers) }); err != nil {
		return err
	}
	if triggers > 0 {
		details = append(details, "crawlers has a trigger (none is allowed)")
	}

	if len(details) > 0 {
		sort.Strings(details)
		return &SchemaInvalidError{Details: details}
	}
	return nil
}

// VerifySchema checks that magnetico_ingest exists with the supported version
// and the expected crawler-registry structure. It only reads; an invalid
// schema is reported, never repaired. Errors match ErrSchemaMissing,
// ErrSchemaInvalid or persistence.ErrStorageIO.
func VerifySchema(ctx context.Context, pool *pgxpool.Pool) error {
	if err := verifySchema(ctx, pool); err != nil {
		if errors.Is(err, ErrSchemaMissing) || errors.Is(err, ErrSchemaInvalid) {
			return err
		}
		return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	return nil
}
