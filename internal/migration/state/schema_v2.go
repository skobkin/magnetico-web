package state

// Operational schema v2: the 04B extension of `magnetico_migration`
// (data_revision on progress, the version marker, verification runs/issues
// and retry runs/attempts), plus EnsureOperationalSchema — the
// definition-validated upgrade path from 04A's v1 schema.
//
// Upgrade discipline (definitions before DDL): an existing schema is
// classified by its exact relation inventory, then every expected relation's
// full definitions (columns, constraints, indexes) are validated against the
// catalog BEFORE any DDL runs. Anything unexpected — a malformed v1, a
// partial or tampered v2, a foreign object — refuses with ErrRunStateCorrupt
// and leaves the database untouched (snapshot-proven in tests). The
// declarations below are the reference renderings of PostgreSQL 17
// (information_schema.columns and pg_get_constraintdef/pg_get_indexdef under
// the default search_path); a server that renders differently refuses —
// fail-closed by design, never guessed past.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OperationalSchemaComponent and OperationalSchemaVersion identify the
// current operational schema in magnetico_migration.operational_schema. The
// marker is advisory bookkeeping only: EnsureOperationalSchema always proves
// the definitions themselves and merely inserts the marker when missing.
const (
	OperationalSchemaComponent = "core"
	OperationalSchemaVersion   = 2
)

// operationalSchemaV2DDL brings an exact-v1 schema to v2. Every statement is
// safe to re-run; the data_revision ALTER backfills the existing progress
// row with 0. Also executed by InitRun on fresh targets (no-op there for the
// ALTER, CREATE IF NOT EXISTS for the rest).
var operationalSchemaV2DDL = []string{
	`ALTER TABLE magnetico_migration.progress
        ADD COLUMN IF NOT EXISTS data_revision BIGINT NOT NULL DEFAULT 0`,
	`CREATE TABLE IF NOT EXISTS magnetico_migration.operational_schema (
        component TEXT PRIMARY KEY,
        version INTEGER NOT NULL,
        upgraded_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`,
	`CREATE TABLE IF NOT EXISTS magnetico_migration.verification_runs (
        seq BIGINT GENERATED ALWAYS AS IDENTITY,
        verification_id UUID PRIMARY KEY,
        run_id UUID NOT NULL REFERENCES magnetico_migration.progress(run_id),
        kind TEXT NOT NULL CHECK (kind IN ('decode','content')),
        state TEXT NOT NULL CHECK (state IN ('running','interrupted','completed_pass','completed_fail','aborted')),
        phase TEXT NOT NULL CHECK (phase IN ('scanning','extras','complete')),
        start_data_revision BIGINT NOT NULL,
        source_identity JSONB,
        source_fingerprint JSONB,
        source_checkpoint BIGINT,
        target_checkpoint BIGINT,
        expected_source_ids BIGINT,
        observed_source_ids BIGINT,
        expected_target_ids BIGINT,
        observed_target_ids BIGINT,
        verified_count BIGINT NOT NULL DEFAULT 0,
        mismatch_count BIGINT NOT NULL DEFAULT 0,
        unverifiable_count BIGINT NOT NULL DEFAULT 0,
        assumed_single_count BIGINT NOT NULL DEFAULT 0,
        issue_count BIGINT NOT NULL DEFAULT 0,
        settings JSONB NOT NULL,
        started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
        completed_at TIMESTAMPTZ,
        UNIQUE (seq)
    )`,
	`CREATE INDEX IF NOT EXISTS verification_runs_recent
        ON magnetico_migration.verification_runs (kind, started_at DESC)`,
	`CREATE TABLE IF NOT EXISTS magnetico_migration.verification_issues (
        id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        verification_id UUID NOT NULL REFERENCES magnetico_migration.verification_runs(verification_id),
        source_id BIGINT,
        info_hash BYTEA,
        code TEXT NOT NULL,
        stage TEXT NOT NULL,
        severity TEXT NOT NULL CHECK (severity IN ('info','mismatch','coverage','unverifiable')),
        details JSONB,
        created_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`,
	`CREATE UNIQUE INDEX IF NOT EXISTS verification_issues_unique
        ON magnetico_migration.verification_issues (verification_id, source_id, code)
        WHERE source_id IS NOT NULL`,
	`CREATE INDEX IF NOT EXISTS verification_issues_by_run
        ON magnetico_migration.verification_issues (verification_id)`,
	`CREATE TABLE IF NOT EXISTS magnetico_migration.retry_runs (
        retry_id UUID PRIMARY KEY,
        run_id UUID NOT NULL REFERENCES magnetico_migration.progress(run_id),
        state TEXT NOT NULL CHECK (state IN ('running','interrupted','completed_clean','completed_remaining','aborted')),
        start_data_revision BIGINT NOT NULL,
        current_data_revision BIGINT NOT NULL,
        unresolved_at_start BIGINT NOT NULL,
        last_source_id BIGINT,
        attempted_count BIGINT NOT NULL DEFAULT 0,
        fixed_count BIGINT NOT NULL DEFAULT 0,
        still_invalid_count BIGINT NOT NULL DEFAULT 0,
        missing_count BIGINT NOT NULL DEFAULT 0,
        source_fingerprint JSONB,
        accepted_source_repair BOOLEAN NOT NULL DEFAULT FALSE,
        previous_fingerprint JSONB,
        settings JSONB NOT NULL,
        started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
        completed_at TIMESTAMPTZ
    )`,
	`CREATE INDEX IF NOT EXISTS retry_runs_by_run
        ON magnetico_migration.retry_runs (run_id)`,
	`CREATE TABLE IF NOT EXISTS magnetico_migration.retry_attempts (
        id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        retry_id UUID NOT NULL REFERENCES magnetico_migration.retry_runs(retry_id),
        run_id UUID NOT NULL,
        source_id BIGINT NOT NULL,
        source_info_hash BYTEA,
        outcome TEXT NOT NULL CHECK (outcome IN ('fixed','still_invalid','missing_source')),
        stage TEXT NOT NULL,
        code TEXT NOT NULL,
        details JSONB,
        attempted_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`,
	`CREATE UNIQUE INDEX IF NOT EXISTS retry_attempts_unique
        ON magnetico_migration.retry_attempts (retry_id, source_id)`,
	`CREATE INDEX IF NOT EXISTS retry_attempts_by_run
        ON magnetico_migration.retry_attempts (run_id, attempted_at)`,
}

// insertOperationalMarker records the schema version, never overwriting an
// existing marker (the definitions themselves are the truth).
const insertOperationalMarker = `INSERT INTO magnetico_migration.operational_schema (component, version)
    VALUES ($1, $2) ON CONFLICT (component) DO NOTHING`

// expectedColumn mirrors one information_schema.columns row.
type expectedColumn struct {
	name     string
	dataType string // information_schema data_type rendering
	nullable bool
	defalt   string // normalized column_default; "" for none
	identity bool   // is_identity = 'YES' (GENERATED ALWAYS AS IDENTITY)
}

// expectedDef is one named constraint or index with its normalized
// definition.
type expectedDef struct {
	name string
	kind byte // constraints only: contype ('p','f','c','u')
	def  string
}

// expectedTable is the complete declared shape of one operational relation.
type expectedTable struct {
	columns     []expectedColumn // ordinal order
	constraints []expectedDef    // compared as a set
	indexes     []expectedDef    // non-constraint indexes, compared as a set
}

func col(name, dataType string, nullable bool, defalt string, identity bool) expectedColumn {
	return expectedColumn{name: name, dataType: dataType, nullable: nullable, defalt: defalt, identity: identity}
}

// normalizeDef collapses whitespace so hand-formatted DDL and catalog
// renderings compare equal.
func normalizeDef(s string) string { return strings.Join(strings.Fields(s), " ") }

// The reference constraint/index definitions below are PostgreSQL 17's
// renderings of the DDL in this package (verified against a live catalog).

var errorsConstraints = []expectedDef{
	{"errors_pkey", 'p', normalizeDef(`PRIMARY KEY (id)`)},
	{"errors_run_id_fkey", 'f', normalizeDef(
		`FOREIGN KEY (run_id) REFERENCES magnetico_migration.progress(run_id)`)},
	{"errors_scope_check", 'c', normalizeDef(
		`CHECK ((scope = ANY (ARRAY['record'::text, 'run'::text])))`)},
}

var errorsTable = expectedTable{
	columns: []expectedColumn{
		col("id", "bigint", false, "", true),
		col("run_id", "uuid", false, "", false),
		col("scope", "text", false, "", false),
		col("source_id", "bigint", true, "", false),
		col("source_info_hash", "bytea", true, "", false),
		col("stage", "text", false, "", false),
		col("category", "text", false, "", false),
		col("code", "text", false, "", false),
		col("details", "jsonb", true, "", false),
		col("attempts", "integer", false, "1", false),
		col("resolved", "boolean", false, "false", false),
		col("resolved_at", "timestamp with time zone", true, "", false),
		col("created_at", "timestamp with time zone", false, "now()", false),
	},
	constraints: errorsConstraints,
	indexes: []expectedDef{{
		"errors_run_record_unique", 0, normalizeDef(
			`CREATE UNIQUE INDEX errors_run_record_unique ON magnetico_migration.errors USING btree (run_id, source_id) WHERE (scope = 'record'::text)`),
	}},
}

var operationalSchemaTable = expectedTable{
	columns: []expectedColumn{
		col("component", "text", false, "", false),
		col("version", "integer", false, "", false),
		col("upgraded_at", "timestamp with time zone", false, "now()", false),
	},
	constraints: []expectedDef{
		{"operational_schema_pkey", 'p', normalizeDef(`PRIMARY KEY (component)`)},
	},
}

// progressColumns are the v2 progress columns: the 04A set plus data_revision.
var progressColumns = []expectedColumn{
	col("run_id", "uuid", false, "", false),
	col("phase", "text", false, "", false),
	col("source_fingerprint", "jsonb", false, "", false),
	col("source_adapter", "text", false, "", false),
	col("contract_version", "integer", false, "", false),
	col("codec_profile", "text", false, "", false),
	col("expected_total", "bigint", false, "", false),
	col("source_min_id", "bigint", false, "", false),
	col("source_max_id", "bigint", false, "", false),
	col("source_file_rows", "bigint", false, "", false),
	col("last_processed_id", "bigint", false, "", false),
	col("scanned_count", "bigint", false, "", false),
	col("success_count", "bigint", false, "", false),
	col("error_count", "bigint", false, "", false),
	col("run_error_count", "bigint", false, "0", false),
	col("blob_count", "bigint", false, "0", false),
	col("defer_indexes", "boolean", false, "", false),
	col("indexes_deferred", "boolean", false, "false", false),
	col("options", "jsonb", false, "", false),
	col("started_at", "timestamp with time zone", false, "now()", false),
	col("updated_at", "timestamp with time zone", false, "now()", false),
	col("data_revision", "bigint", false, "0", false),
}

var progressConstraints = []expectedDef{
	{"progress_pkey", 'p', normalizeDef(`PRIMARY KEY (run_id)`)},
}

var verificationRunsTable = expectedTable{
	columns: []expectedColumn{
		col("seq", "bigint", false, "", true),
		col("verification_id", "uuid", false, "", false),
		col("run_id", "uuid", false, "", false),
		col("kind", "text", false, "", false),
		col("state", "text", false, "", false),
		col("phase", "text", false, "", false),
		col("start_data_revision", "bigint", false, "", false),
		col("source_identity", "jsonb", true, "", false),
		col("source_fingerprint", "jsonb", true, "", false),
		col("source_checkpoint", "bigint", true, "", false),
		col("target_checkpoint", "bigint", true, "", false),
		col("expected_source_ids", "bigint", true, "", false),
		col("observed_source_ids", "bigint", true, "", false),
		col("expected_target_ids", "bigint", true, "", false),
		col("observed_target_ids", "bigint", true, "", false),
		col("verified_count", "bigint", false, "0", false),
		col("mismatch_count", "bigint", false, "0", false),
		col("unverifiable_count", "bigint", false, "0", false),
		col("assumed_single_count", "bigint", false, "0", false),
		col("issue_count", "bigint", false, "0", false),
		col("settings", "jsonb", false, "", false),
		col("started_at", "timestamp with time zone", false, "now()", false),
		col("updated_at", "timestamp with time zone", false, "now()", false),
		col("completed_at", "timestamp with time zone", true, "", false),
	},
	constraints: []expectedDef{
		{"verification_runs_pkey", 'p', normalizeDef(`PRIMARY KEY (verification_id)`)},
		{"verification_runs_run_id_fkey", 'f', normalizeDef(
			`FOREIGN KEY (run_id) REFERENCES magnetico_migration.progress(run_id)`)},
		{"verification_runs_kind_check", 'c', normalizeDef(
			`CHECK ((kind = ANY (ARRAY['decode'::text, 'content'::text])))`)},
		{"verification_runs_phase_check", 'c', normalizeDef(
			`CHECK ((phase = ANY (ARRAY['scanning'::text, 'extras'::text, 'complete'::text])))`)},
		{"verification_runs_seq_key", 'u', normalizeDef(`UNIQUE (seq)`)},
		{"verification_runs_state_check", 'c', normalizeDef(
			`CHECK ((state = ANY (ARRAY['running'::text, 'interrupted'::text, 'completed_pass'::text, 'completed_fail'::text, 'aborted'::text])))`)},
	},
	indexes: []expectedDef{{
		"verification_runs_recent", 0, normalizeDef(
			`CREATE INDEX verification_runs_recent ON magnetico_migration.verification_runs USING btree (kind, started_at DESC)`),
	}},
}

var verificationIssuesTable = expectedTable{
	columns: []expectedColumn{
		col("id", "bigint", false, "", true),
		col("verification_id", "uuid", false, "", false),
		col("source_id", "bigint", true, "", false),
		col("info_hash", "bytea", true, "", false),
		col("code", "text", false, "", false),
		col("stage", "text", false, "", false),
		col("severity", "text", false, "", false),
		col("details", "jsonb", true, "", false),
		col("created_at", "timestamp with time zone", false, "now()", false),
	},
	constraints: []expectedDef{
		{"verification_issues_pkey", 'p', normalizeDef(`PRIMARY KEY (id)`)},
		{"verification_issues_severity_check", 'c', normalizeDef(
			`CHECK ((severity = ANY (ARRAY['info'::text, 'mismatch'::text, 'coverage'::text, 'unverifiable'::text])))`)},
		{"verification_issues_verification_id_fkey", 'f', normalizeDef(
			`FOREIGN KEY (verification_id) REFERENCES magnetico_migration.verification_runs(verification_id)`)},
	},
	indexes: []expectedDef{
		{"verification_issues_by_run", 0, normalizeDef(
			`CREATE INDEX verification_issues_by_run ON magnetico_migration.verification_issues USING btree (verification_id)`)},
		{"verification_issues_unique", 0, normalizeDef(
			`CREATE UNIQUE INDEX verification_issues_unique ON magnetico_migration.verification_issues USING btree (verification_id, source_id, code) WHERE (source_id IS NOT NULL)`)},
	},
}

var retryRunsTable = expectedTable{
	columns: []expectedColumn{
		col("retry_id", "uuid", false, "", false),
		col("run_id", "uuid", false, "", false),
		col("state", "text", false, "", false),
		col("start_data_revision", "bigint", false, "", false),
		col("current_data_revision", "bigint", false, "", false),
		col("unresolved_at_start", "bigint", false, "", false),
		col("last_source_id", "bigint", true, "", false),
		col("attempted_count", "bigint", false, "0", false),
		col("fixed_count", "bigint", false, "0", false),
		col("still_invalid_count", "bigint", false, "0", false),
		col("missing_count", "bigint", false, "0", false),
		col("source_fingerprint", "jsonb", true, "", false),
		col("accepted_source_repair", "boolean", false, "false", false),
		col("previous_fingerprint", "jsonb", true, "", false),
		col("settings", "jsonb", false, "", false),
		col("started_at", "timestamp with time zone", false, "now()", false),
		col("updated_at", "timestamp with time zone", false, "now()", false),
		col("completed_at", "timestamp with time zone", true, "", false),
	},
	constraints: []expectedDef{
		{"retry_runs_pkey", 'p', normalizeDef(`PRIMARY KEY (retry_id)`)},
		{"retry_runs_run_id_fkey", 'f', normalizeDef(
			`FOREIGN KEY (run_id) REFERENCES magnetico_migration.progress(run_id)`)},
		{"retry_runs_state_check", 'c', normalizeDef(
			`CHECK ((state = ANY (ARRAY['running'::text, 'interrupted'::text, 'completed_clean'::text, 'completed_remaining'::text, 'aborted'::text])))`)},
	},
	indexes: []expectedDef{{
		"retry_runs_by_run", 0, normalizeDef(
			`CREATE INDEX retry_runs_by_run ON magnetico_migration.retry_runs USING btree (run_id)`),
	}},
}

var retryAttemptsTable = expectedTable{
	columns: []expectedColumn{
		col("id", "bigint", false, "", true),
		col("retry_id", "uuid", false, "", false),
		col("run_id", "uuid", false, "", false),
		col("source_id", "bigint", false, "", false),
		col("source_info_hash", "bytea", true, "", false),
		col("outcome", "text", false, "", false),
		col("stage", "text", false, "", false),
		col("code", "text", false, "", false),
		col("details", "jsonb", true, "", false),
		col("attempted_at", "timestamp with time zone", false, "now()", false),
	},
	constraints: []expectedDef{
		{"retry_attempts_outcome_check", 'c', normalizeDef(
			`CHECK ((outcome = ANY (ARRAY['fixed'::text, 'still_invalid'::text, 'missing_source'::text])))`)},
		{"retry_attempts_pkey", 'p', normalizeDef(`PRIMARY KEY (id)`)},
		{"retry_attempts_retry_id_fkey", 'f', normalizeDef(
			`FOREIGN KEY (retry_id) REFERENCES magnetico_migration.retry_runs(retry_id)`)},
	},
	indexes: []expectedDef{
		{"retry_attempts_by_run", 0, normalizeDef(
			`CREATE INDEX retry_attempts_by_run ON magnetico_migration.retry_attempts USING btree (run_id, attempted_at)`)},
		{"retry_attempts_unique", 0, normalizeDef(
			`CREATE UNIQUE INDEX retry_attempts_unique ON magnetico_migration.retry_attempts USING btree (retry_id, source_id)`)},
	},
}

// expectedSchemaV1 is the exact 04A operational schema.
var expectedSchemaV1 = map[string]expectedTable{
	"progress": {columns: progressColumns[:len(progressColumns)-1], constraints: progressConstraints},
	"errors":   errorsTable,
}

// expectedSchemaV2 is the exact current operational schema (04B).
var expectedSchemaV2 = map[string]expectedTable{
	"progress":            {columns: progressColumns, constraints: progressConstraints},
	"errors":              errorsTable,
	"operational_schema":  operationalSchemaTable,
	"verification_runs":   verificationRunsTable,
	"verification_issues": verificationIssuesTable,
	"retry_runs":          retryRunsTable,
	"retry_attempts":      retryAttemptsTable,
}

// EnsureOperationalSchema classifies an existing magnetico_migration schema
// and brings it to the current operational version:
//
//   - schema absent: fresh target, nothing to do (InitRun creates v2);
//   - exact v1 (04A): full definition validation, then one atomic upgrade
//     transaction (04A data is preserved; data_revision backfills as 0);
//   - exact v2: full definition validation, then marker-if-missing;
//   - anything else: ErrRunStateCorrupt BEFORE any DDL — the database is
//     left byte-identical.
//
// Caller must hold the run lock; classification and upgrade are otherwise
// racy against a concurrent migrator.
func EnsureOperationalSchema(ctx context.Context, pool *pgxpool.Pool) error {
	var schemaExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'magnetico_migration')`,
	).Scan(&schemaExists); err != nil {
		return err
	}
	if !schemaExists {
		return nil // fresh target
	}

	inventory, details := readInventory(ctx, pool)
	if len(details) > 0 {
		return &StateError{Err: ErrRunStateCorrupt, Details: details}
	}

	switch {
	case sameNames(inventory, expectedSchemaV1):
		if bad := validateOperationalSchema(ctx, pool, expectedSchemaV1); len(bad) > 0 {
			return &StateError{Err: ErrRunStateCorrupt, Details: appendDetails(
				[]string{"existing operational schema is not exact v1; refusing the upgrade"}, bad)}
		}
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			for _, stmt := range operationalSchemaV2DDL {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, insertOperationalMarker, OperationalSchemaComponent, OperationalSchemaVersion)
			return err
		})
	case sameNames(inventory, expectedSchemaV2):
		if bad := validateOperationalSchema(ctx, pool, expectedSchemaV2); len(bad) > 0 {
			return &StateError{Err: ErrRunStateCorrupt, Details: appendDetails(
				[]string{"existing operational schema is not exact v2"}, bad)}
		}
		// Definitions already proven; only the advisory marker may be missing.
		_, err := pool.Exec(ctx, insertOperationalMarker, OperationalSchemaComponent, OperationalSchemaVersion)
		return err
	default:
		details = append([]string{
			"magnetico_migration contains an unrecognized relation inventory; refusing to upgrade"}, details...)
		for _, name := range sortedNames(inventory) {
			if _, known := expectedSchemaV2[name]; !known {
				details = append(details, fmt.Sprintf("unexpected relation magnetico_migration.%s (%s)",
					name, relKindName(inventory[name])))
			}
		}
		for name := range expectedSchemaV2 {
			if inventory[name] == "" {
				details = append(details, "missing table magnetico_migration."+name)
			}
		}
		return &StateError{Err: ErrRunStateCorrupt, Details: details}
	}
}

// readInventory reads the schema's relations (name -> kind), refusing any
// non-table relation outright. Identity columns' owned sequences are
// excluded (they are structural, proven by the column identity flags);
// stray sequences count as foreign objects.
func readInventory(ctx context.Context, pool *pgxpool.Pool) (map[string]string, []string) {
	rows, err := pool.Query(ctx, `SELECT c.relname, c.relkind::text
        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'magnetico_migration' AND c.relkind IN ('r','p','v','m','f','S')
          AND NOT (c.relkind = 'S' AND EXISTS (
              SELECT 1 FROM pg_depend d
              WHERE d.classid = 'pg_class'::regclass
                AND d.objid = c.oid AND d.deptype IN ('i','a')))
        ORDER BY 1`)
	if err != nil {
		return nil, []string{err.Error()}
	}
	inventory := map[string]string{}
	var details []string
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			rows.Close()
			return nil, []string{err.Error()}
		}
		if kind != "r" {
			details = append(details, fmt.Sprintf(
				"magnetico_migration.%s is a %s, not a table", name, relKindName(kind)))
			continue
		}
		inventory[name] = kind
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, []string{err.Error()}
	}
	sort.Strings(details)
	return inventory, details
}

func relKindName(kind string) string {
	switch kind {
	case "r":
		return "table"
	case "p":
		return "partitioned table"
	case "v":
		return "view"
	case "m":
		return "materialized view"
	case "f":
		return "foreign table"
	case "S":
		return "sequence"
	case "i", "I":
		return "index"
	default:
		return kind
	}
}

func sameNames(inventory map[string]string, expected map[string]expectedTable) bool {
	if len(inventory) != len(expected) {
		return false
	}
	for name := range expected {
		if inventory[name] == "" {
			return false
		}
	}
	return true
}

func sortedNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func appendDetails(head, tail []string) []string {
	return append(append([]string{}, head...), tail...)
}

// validateOperationalSchema compares every expected relation's full
// definitions against the catalog. Read-only: it never issues DDL, so a
// refusal leaves the database untouched. Returns sorted mismatch details.
func validateOperationalSchema(ctx context.Context, pool *pgxpool.Pool, expected map[string]expectedTable) []string {
	var details []string
	for _, name := range sortedDefNames(expected) {
		want := expected[name]
		regclass := "magnetico_migration." + name

		rows, err := pool.Query(ctx, `SELECT column_name, data_type, is_nullable,
                COALESCE(column_default, ''), is_identity
            FROM information_schema.columns
            WHERE table_schema = 'magnetico_migration' AND table_name = $1
            ORDER BY ordinal_position`, name)
		if err != nil {
			details = append(details, err.Error())
			continue
		}
		var haveCols []expectedColumn
		for rows.Next() {
			var c expectedColumn
			var nullable, identity string
			if err := rows.Scan(&c.name, &c.dataType, &nullable, &c.defalt, &identity); err != nil {
				rows.Close()
				details = append(details, err.Error())
				break
			}
			c.nullable = nullable == "YES"
			c.defalt = normalizeDef(c.defalt)
			c.identity = identity == "YES"
			haveCols = append(haveCols, c)
		}
		rows.Close()
		details = appendColumnDetails(details, name, want.columns, haveCols)

		rows, err = pool.Query(ctx, `SELECT conname, contype::text, pg_get_constraintdef(oid)
            FROM pg_constraint WHERE conrelid = to_regclass($1) ORDER BY conname`, regclass)
		if err != nil {
			details = append(details, err.Error())
			continue
		}
		var haveCons []expectedDef
		for rows.Next() {
			var d expectedDef
			var kind string
			if err := rows.Scan(&d.name, &kind, &d.def); err != nil {
				rows.Close()
				details = append(details, err.Error())
				break
			}
			if kind != "" {
				d.kind = kind[0]
			}
			d.def = normalizeDef(d.def)
			haveCons = append(haveCons, d)
		}
		rows.Close()
		details = appendDefDetails(details, name, "constraint", want.constraints, haveCons)

		rows, err = pool.Query(ctx, `SELECT i.relname, ''::text, pg_get_indexdef(i.oid)
            FROM pg_index ix
            JOIN pg_class i ON i.oid = ix.indexrelid
            WHERE ix.indrelid = to_regclass($1) AND NOT ix.indisprimary
              AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = i.oid)
            ORDER BY i.relname`, regclass)
		if err != nil {
			details = append(details, err.Error())
			continue
		}
		var haveIdx []expectedDef
		for rows.Next() {
			var d expectedDef
			var unused string
			if err := rows.Scan(&d.name, &unused, &d.def); err != nil {
				rows.Close()
				details = append(details, err.Error())
				break
			}
			d.def = normalizeDef(d.def)
			haveIdx = append(haveIdx, d)
		}
		rows.Close()
		details = appendDefDetails(details, name, "index", want.indexes, haveIdx)
	}
	sort.Strings(details)
	return details
}

func sortedDefNames(m map[string]expectedTable) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func appendColumnDetails(details []string, table string, want, have []expectedColumn) []string {
	byName := make(map[string]expectedColumn, len(have))
	for _, c := range have {
		byName[c.name] = c
	}
	for i, w := range want {
		h, ok := byName[w.name]
		if !ok {
			details = append(details, fmt.Sprintf("%s.%s: missing column", table, w.name))
			continue
		}
		if i < len(have) && have[i].name != w.name {
			details = append(details, fmt.Sprintf("%s: column order differs at ordinal %d (want %s, have %s)",
				table, i+1, w.name, have[i].name))
		}
		if h.dataType != w.dataType {
			details = append(details, fmt.Sprintf("%s.%s: type %s != %s", table, w.name, h.dataType, w.dataType))
		}
		if h.nullable != w.nullable {
			details = append(details, fmt.Sprintf("%s.%s: nullability %v != %v", table, w.name, h.nullable, w.nullable))
		}
		if h.defalt != w.defalt {
			details = append(details, fmt.Sprintf("%s.%s: default %q != %q", table, w.name, h.defalt, w.defalt))
		}
		if h.identity != w.identity {
			details = append(details, fmt.Sprintf("%s.%s: identity %v != %v", table, w.name, h.identity, w.identity))
		}
	}
	for _, c := range have {
		found := false
		for _, w := range want {
			if w.name == c.name {
				found = true
				break
			}
		}
		if !found {
			details = append(details, fmt.Sprintf("%s.%s: unexpected column", table, c.name))
		}
	}
	return details
}

func appendDefDetails(details []string, table, kind string, want, have []expectedDef) []string {
	byName := make(map[string]expectedDef, len(have))
	for _, d := range have {
		byName[d.name] = d
	}
	for _, w := range want {
		h, ok := byName[w.name]
		if !ok {
			details = append(details, fmt.Sprintf("%s: missing %s %s", table, kind, w.name))
			continue
		}
		if w.kind != 0 && h.kind != w.kind {
			details = append(details, fmt.Sprintf("%s: %s %s has type %q, want %q", table, kind, w.name, h.kind, w.kind))
		}
		if h.def != w.def {
			details = append(details, fmt.Sprintf("%s: %s %s definition differs:\n  want: %s\n  have: %s",
				table, kind, w.name, w.def, h.def))
		}
	}
	for _, d := range have {
		found := false
		for _, w := range want {
			if w.name == d.name {
				found = true
				break
			}
		}
		if !found {
			details = append(details, fmt.Sprintf("%s: unexpected %s %s", table, kind, d.name))
		}
	}
	return details
}
