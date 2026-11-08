package state

// Operational schema v2 integration tests: the v1 -> v2 upgrade, the
// definition-validated refusals (no DDL on refusal, snapshot-proven) and the
// unresolved-record CountErrors semantics.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// v1SchemaDDL is the exact 04A operational schema (progress without
// data_revision, errors, the record-unique index).
var v1SchemaDDL = []string{
	`CREATE SCHEMA magnetico_migration`,
	`CREATE TABLE magnetico_migration.progress (
        run_id UUID PRIMARY KEY,
        phase TEXT NOT NULL,
        source_fingerprint JSONB NOT NULL,
        source_adapter TEXT NOT NULL,
        contract_version INTEGER NOT NULL,
        codec_profile TEXT NOT NULL,
        expected_total BIGINT NOT NULL,
        source_min_id BIGINT NOT NULL,
        source_max_id BIGINT NOT NULL,
        source_file_rows BIGINT NOT NULL,
        last_processed_id BIGINT NOT NULL,
        scanned_count BIGINT NOT NULL,
        success_count BIGINT NOT NULL,
        error_count BIGINT NOT NULL,
        run_error_count BIGINT NOT NULL DEFAULT 0,
        blob_count BIGINT NOT NULL DEFAULT 0,
        defer_indexes BOOLEAN NOT NULL,
        indexes_deferred BOOLEAN NOT NULL DEFAULT FALSE,
        options JSONB NOT NULL,
        started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`,
	`CREATE TABLE magnetico_migration.errors (
        id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        run_id UUID NOT NULL REFERENCES magnetico_migration.progress(run_id),
        scope TEXT NOT NULL CHECK (scope IN ('record','run')),
        source_id BIGINT,
        source_info_hash BYTEA,
        stage TEXT NOT NULL,
        category TEXT NOT NULL,
        code TEXT NOT NULL,
        details JSONB,
        attempts INTEGER NOT NULL DEFAULT 1,
        resolved BOOLEAN NOT NULL DEFAULT FALSE,
        resolved_at TIMESTAMPTZ,
        created_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`,
	`CREATE UNIQUE INDEX errors_run_record_unique
        ON magnetico_migration.errors (run_id, source_id) WHERE scope = 'record'`,
}

// seedV1 creates the exact v1 schema plus one finished 04A run state.
func seedV1(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range v1SchemaDDL {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("v1 DDL: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.progress (
        run_id, phase, source_fingerprint, source_adapter, contract_version, codec_profile,
        expected_total, source_min_id, source_max_id, source_file_rows,
        last_processed_id, scanned_count, success_count, error_count, defer_indexes, options)
        VALUES ('11111111-1111-4111-8111-111111111111', 'import_complete_with_errors', '{}',
                'original-v0.12.0', 1, 'compact', 3, 1, 3, 4, 3, 3, 1, 2, false, '{}')`); err != nil {
		t.Fatalf("seed progress: %v", err)
	}
	runID := "11111111-1111-4111-8111-111111111111"
	// Record errors: two unresolved (one with two attempts), one resolved.
	// Run scope: one unresolved integrity incident, one operational incident.
	seeds := []struct {
		scope, stage, category, code string
		sourceID                     *int64
		attempts                     int
		resolved                     bool
	}{
		{ScopeRecord, "validate", CategoryIntegrity, "invalid_metadata", ptrInt64(2), 1, false},
		{ScopeRecord, "validate", CategoryIntegrity, "invalid_framing", ptrInt64(3), 2, false},
		{ScopeRecord, "read", CategoryOperational, "source_row_vanished", ptrInt64(4), 1, true},
		{ScopeRun, "read", CategoryIntegrity, "orphan_source_files", nil, 1, false},
		{ScopeRun, "write", CategoryOperational, "transient_write_failure", nil, 1, false},
	}
	for _, s := range seeds {
		var sid any
		if s.sourceID != nil {
			sid = *s.sourceID
		}
		if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.errors
            (run_id, scope, source_id, stage, category, code, attempts, resolved)
            VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)`,
			runID, s.scope, sid, s.stage, s.category, s.code, s.attempts, s.resolved); err != nil {
			t.Fatalf("seed error row: %v", err)
		}
	}
	return runID
}

// catalogSnapshot renders the entire magnetico_migration catalog — relations
// (including sequences and indexes), columns, constraints, index definitions
// and marker rows — as sorted lines. Two snapshots comparing equal proves a
// call left the schema byte-identical.
func catalogSnapshot(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	ctx := context.Background()
	var lines []string

	rels, err := pool.Query(ctx, `SELECT c.relname, c.relkind::text
        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'magnetico_migration' ORDER BY 1`)
	if err != nil {
		t.Fatalf("snapshot relations: %v", err)
	}
	for rels.Next() {
		var name, kind string
		if err := rels.Scan(&name, &kind); err != nil {
			t.Fatalf("snapshot relation scan: %v", err)
		}
		lines = append(lines, fmt.Sprintf("rel %s %s", name, kind))
	}
	rels.Close()

	cols, err := pool.Query(ctx, `SELECT table_name, ordinal_position, column_name, data_type,
            is_nullable, COALESCE(column_default,''), is_identity
        FROM information_schema.columns WHERE table_schema = 'magnetico_migration'
        ORDER BY table_name, ordinal_position`)
	if err != nil {
		t.Fatalf("snapshot columns: %v", err)
	}
	for cols.Next() {
		var table, nullable, def, identity string
		var pos int
		var name, dataType string
		if err := cols.Scan(&table, &pos, &name, &dataType, &nullable, &def, &identity); err != nil {
			t.Fatalf("snapshot column scan: %v", err)
		}
		lines = append(lines, fmt.Sprintf("col %s %d %s %s %s %s %s", table, pos, name, dataType, nullable, def, identity))
	}
	cols.Close()

	cons, err := pool.Query(ctx, `SELECT c.relname, con.conname, con.contype::text, pg_get_constraintdef(con.oid)
        FROM pg_constraint con JOIN pg_class c ON c.oid = con.conrelid
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'magnetico_migration' ORDER BY 1, 2`)
	if err != nil {
		t.Fatalf("snapshot constraints: %v", err)
	}
	for cons.Next() {
		var table, name, contype, def string
		if err := cons.Scan(&table, &name, &contype, &def); err != nil {
			t.Fatalf("snapshot constraint scan: %v", err)
		}
		lines = append(lines, fmt.Sprintf("con %s %s %s %s", table, name, contype, normalizeDef(def)))
	}
	cons.Close()

	idx, err := pool.Query(ctx, `SELECT t.relname, i.relname, pg_get_indexdef(i.oid)
        FROM pg_class t JOIN pg_index ix ON ix.indrelid = t.oid JOIN pg_class i ON i.oid = ix.indexrelid
        JOIN pg_namespace n ON n.oid = t.relnamespace
        WHERE n.nspname = 'magnetico_migration' ORDER BY 1, 2`)
	if err != nil {
		t.Fatalf("snapshot indexes: %v", err)
	}
	for idx.Next() {
		var table, name, def string
		if err := idx.Scan(&table, &name, &def); err != nil {
			t.Fatalf("snapshot index scan: %v", err)
		}
		lines = append(lines, fmt.Sprintf("idx %s %s %s", table, name, normalizeDef(def)))
	}
	idx.Close()

	markers, err := pool.Query(ctx, `SELECT component, version, upgraded_at::text
        FROM magnetico_migration.operational_schema ORDER BY component`)
	if err != nil {
		// Marker table absent (v1 fixture): one stable line.
		lines = append(lines, "marker <absent>")
	} else {
		for markers.Next() {
			var component string
			var version int32
			var upgradedAt string
			if err := markers.Scan(&component, &version, &upgradedAt); err != nil {
				t.Fatalf("snapshot marker scan: %v", err)
			}
			lines = append(lines, fmt.Sprintf("marker %s %d %s", component, version, upgradedAt))
		}
		markers.Close()
	}

	sort.Strings(lines)
	return lines
}

func schemaTestPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pool parse: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func ptrInt64(v int64) *int64 { return &v }

func TestEnsureOperationalSchemaAbsentIsFresh(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	if err := EnsureOperationalSchema(context.Background(), pool); err != nil {
		t.Fatalf("EnsureOperationalSchema() on absent schema error = %v", err)
	}
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM pg_namespace WHERE nspname = 'magnetico_migration'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("EnsureOperationalSchema created a schema on a fresh target")
	}
}

func TestEnsureOperationalSchemaUpgradesV1PreservingData(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	runID := seedV1(t, pool)
	ctx := context.Background()

	if err := EnsureOperationalSchema(ctx, pool); err != nil {
		t.Fatalf("EnsureOperationalSchema() upgrade error = %v", err)
	}

	// 04A data preserved verbatim; data_revision backfilled as 0.
	var scanned, success, errCount int64
	var dataRevision int64
	if err := pool.QueryRow(ctx, `SELECT scanned_count, success_count, error_count, data_revision
        FROM magnetico_migration.progress WHERE run_id = $1::uuid`, runID).
		Scan(&scanned, &success, &errCount, &dataRevision); err != nil {
		t.Fatalf("load upgraded progress: %v", err)
	}
	if scanned != 3 || success != 1 || errCount != 2 {
		t.Fatalf("upgraded counters = %d/%d/%d, want 3/1/2", scanned, success, errCount)
	}
	if dataRevision != 0 {
		t.Fatalf("backfilled data_revision = %d, want 0", dataRevision)
	}

	var registryRows, attempts int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(attempts),0)
        FROM magnetico_migration.errors WHERE run_id = $1::uuid`, runID).
		Scan(&registryRows, &attempts); err != nil {
		t.Fatal(err)
	}
	if registryRows != 5 || attempts != 6 {
		t.Fatalf("error rows %d attempts %d, want 5 rows / 6 attempts", registryRows, attempts)
	}

	// Marker recorded once, at the current version.
	var markerVersion int32
	var markerCount int64
	if err := pool.QueryRow(ctx, `SELECT version, COUNT(*) FROM magnetico_migration.operational_schema
        WHERE component = $1 GROUP BY version`, OperationalSchemaComponent).
		Scan(&markerVersion, &markerCount); err != nil {
		t.Fatalf("marker read: %v", err)
	}
	if markerCount != 1 || markerVersion != OperationalSchemaVersion {
		t.Fatalf("marker count/version = %d/%d, want 1/%d", markerCount, markerVersion, OperationalSchemaVersion)
	}

	// Idempotent: a second call changes nothing.
	before := catalogSnapshot(t, pool)
	if err := EnsureOperationalSchema(ctx, pool); err != nil {
		t.Fatalf("second EnsureOperationalSchema() error = %v", err)
	}
	after := catalogSnapshot(t, pool)
	if len(before) != len(after) {
		t.Fatalf("second call changed the catalog: %d -> %d lines", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("second call changed catalog line %d:\n before: %s\n after:  %s", i, before[i], after[i])
		}
	}

	// Load accepts the upgraded database and reports the backfilled revision.
	p, err := Load(ctx, pool)
	if err != nil {
		t.Fatalf("Load() on upgraded v1 database error = %v", err)
	}
	if p.DataRevision != 0 || p.ErrorCount != 2 {
		t.Fatalf("Load() = revision %d errors %d, want 0/2", p.DataRevision, p.ErrorCount)
	}
}

func TestEnsureOperationalSchemaV2NoopAndMarkerBackfill(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	ctx := context.Background()

	// Fresh v2 via InitRun's own DDL path, minus the marker: a v2 database
	// whose definitions are proven but whose advisory marker is missing.
	for _, stmt := range migrationSchemaDDL {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("fresh DDL: %v", err)
		}
	}
	for _, stmt := range operationalSchemaV2DDL {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("v2 DDL: %v", err)
		}
	}

	before := catalogSnapshot(t, pool)
	if err := EnsureOperationalSchema(ctx, pool); err != nil {
		t.Fatalf("EnsureOperationalSchema() v2 error = %v", err)
	}
	after := catalogSnapshot(t, pool)
	if len(after) != len(before)+1 {
		t.Fatalf("marker backfill changed %d lines, want exactly +1", len(after)-len(before))
	}
	var marker int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.operational_schema
        WHERE component = $1 AND version = $2`, OperationalSchemaComponent, OperationalSchemaVersion).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != 1 {
		t.Fatalf("marker rows = %d, want 1", marker)
	}

	// Second call: true no-op.
	stable := catalogSnapshot(t, pool)
	if err := EnsureOperationalSchema(ctx, pool); err != nil {
		t.Fatalf("second EnsureOperationalSchema() error = %v", err)
	}
	again := catalogSnapshot(t, pool)
	if len(stable) != len(again) {
		t.Fatalf("second call changed the catalog (%d -> %d lines)", len(stable), len(again))
	}
	for i := range stable {
		if stable[i] != again[i] {
			t.Fatalf("second call changed catalog line %d:\n before: %s\n after:  %s", i, stable[i], again[i])
		}
	}
}

func TestEnsureOperationalSchemaRefusesMalformedV1(t *testing.T) {
	t.Parallel()

	// Each case plants an exact-v1 inventory with one definition defect;
	// EnsureOperationalSchema must refuse BEFORE any DDL (snapshot-proven).
	cases := []struct {
		name    string
		tamper  string
		wantSub string // detail substring naming the offender
	}{
		{
			name:    "progress extra column",
			tamper:  `ALTER TABLE magnetico_migration.progress ADD COLUMN extra INTEGER NOT NULL DEFAULT 0`,
			wantSub: "progress.extra: unexpected column",
		},
		{
			name:    "progress missing blob_count",
			tamper:  `ALTER TABLE magnetico_migration.progress DROP COLUMN blob_count`,
			wantSub: "progress.blob_count: missing column",
		},
		{
			name:    "progress wrong column type",
			tamper:  `ALTER TABLE magnetico_migration.progress ALTER COLUMN scanned_count TYPE integer`,
			wantSub: "progress.scanned_count: type",
		},
		{
			name:    "errors missing unique index",
			tamper:  `DROP INDEX magnetico_migration.errors_run_record_unique`,
			wantSub: "errors: missing index errors_run_record_unique",
		},
		{
			name: "errors unique index replaced by non-unique",
			tamper: `DROP INDEX magnetico_migration.errors_run_record_unique;
                CREATE INDEX errors_run_record_unique ON magnetico_migration.errors (run_id, source_id) WHERE scope = 'record'`,
			wantSub: "errors: index errors_run_record_unique definition differs",
		},
		{
			name: "errors scope check narrowed",
			tamper: `DELETE FROM magnetico_migration.errors WHERE scope = 'run';
                ALTER TABLE magnetico_migration.errors DROP CONSTRAINT errors_scope_check;
                ALTER TABLE magnetico_migration.errors ADD CHECK (scope IN ('record'))`,
			wantSub: "errors: constraint errors_scope_check definition differs",
		},
		{
			name:    "errors wrong attempts type",
			tamper:  `ALTER TABLE magnetico_migration.errors ALTER COLUMN attempts TYPE bigint`,
			wantSub: "errors.attempts: type",
		},
		{
			name:    "errors extra default on run_id",
			tamper:  `ALTER TABLE magnetico_migration.errors ALTER COLUMN run_id SET DEFAULT '00000000-0000-0000-0000-000000000000'`,
			wantSub: "errors.run_id: default",
		},
		{
			name: "progress primary key widened",
			tamper: `ALTER TABLE magnetico_migration.errors DROP CONSTRAINT errors_run_id_fkey;
                ALTER TABLE magnetico_migration.progress DROP CONSTRAINT progress_pkey;
                ALTER TABLE magnetico_migration.progress ADD PRIMARY KEY (run_id, phase)`,
			wantSub: "progress: constraint progress_pkey definition differs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
			seedV1(t, pool)
			for _, stmt := range strings.Split(tc.tamper, ";") {
				if _, err := pool.Exec(context.Background(), stmt); err != nil {
					t.Fatalf("tamper DDL: %v", err)
				}
			}

			before := catalogSnapshot(t, pool)
			err := EnsureOperationalSchema(context.Background(), pool)
			var stateErr *StateError
			if !errors.As(err, &stateErr) || !errors.Is(err, ErrRunStateCorrupt) {
				t.Fatalf("EnsureOperationalSchema() error = %v, want ErrRunStateCorrupt", err)
			}
			joined := strings.Join(stateErr.Details, "\n")
			if !strings.Contains(joined, tc.wantSub) {
				t.Fatalf("refusal details missing %q:\n%s", tc.wantSub, joined)
			}
			if !strings.Contains(joined, "not exact v1") {
				t.Fatalf("refusal must say the schema is not exact v1:\n%s", joined)
			}

			after := catalogSnapshot(t, pool)
			if len(before) != len(after) {
				t.Fatalf("refusal mutated the catalog: %d -> %d lines", len(before), len(after))
			}
			for i := range before {
				if before[i] != after[i] {
					t.Fatalf("refusal mutated catalog line %d:\n before: %s\n after:  %s", i, before[i], after[i])
				}
			}
		})
	}
}

func TestEnsureOperationalSchemaRefusesBrokenV2AndForeignObjects(t *testing.T) {
	t.Parallel()

	buildV2 := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		for _, stmt := range append(append([]string{}, migrationSchemaDDL...), operationalSchemaV2DDL...) {
			if _, err := pool.Exec(context.Background(), stmt); err != nil {
				t.Fatalf("v2 DDL: %v", err)
			}
		}
	}

	cases := []struct {
		name    string
		tamper  string
		wantSub string
	}{
		{
			name:    "verification_issues dropped",
			tamper:  `DROP TABLE magnetico_migration.verification_issues`,
			wantSub: "missing table magnetico_migration.verification_issues",
		},
		{
			name:    "progress data_revision dropped",
			tamper:  `ALTER TABLE magnetico_migration.progress DROP COLUMN data_revision`,
			wantSub: "progress.data_revision: missing column",
		},
		{
			name:    "retry_attempts outcome check widened",
			tamper:  `ALTER TABLE magnetico_migration.retry_attempts DROP CONSTRAINT retry_attempts_outcome_check`,
			wantSub: "retry_attempts: missing constraint retry_attempts_outcome_check",
		},
		{
			name:    "verification_runs seq uniqueness removed",
			tamper:  `ALTER TABLE magnetico_migration.verification_runs DROP CONSTRAINT verification_runs_seq_key`,
			wantSub: "verification_runs: missing constraint verification_runs_seq_key",
		},
		{
			name:    "foreign table planted",
			tamper:  `CREATE TABLE magnetico_migration.sneaky_torrents (id int)`,
			wantSub: "unexpected relation magnetico_migration.sneaky_torrents (table)",
		},
		{
			name:    "stray sequence planted",
			tamper:  `CREATE SEQUENCE magnetico_migration.sneaky_seq`,
			wantSub: "magnetico_migration.sneaky_seq is a sequence",
		},
		{
			name:    "view planted",
			tamper:  `CREATE VIEW magnetico_migration.sneaky_view AS SELECT 1`,
			wantSub: "magnetico_migration.sneaky_view is a view",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
			buildV2(t, pool)
			if _, err := pool.Exec(context.Background(), tc.tamper); err != nil {
				t.Fatalf("tamper DDL: %v", err)
			}

			before := catalogSnapshot(t, pool)
			err := EnsureOperationalSchema(context.Background(), pool)
			var stateErr *StateError
			if !errors.As(err, &stateErr) || !errors.Is(err, ErrRunStateCorrupt) {
				t.Fatalf("EnsureOperationalSchema() error = %v, want ErrRunStateCorrupt", err)
			}
			joined := strings.Join(stateErr.Details, "\n")
			if !strings.Contains(joined, tc.wantSub) {
				t.Fatalf("refusal details missing %q:\n%s", tc.wantSub, joined)
			}

			after := catalogSnapshot(t, pool)
			if len(before) != len(after) {
				t.Fatalf("refusal mutated the catalog: %d -> %d lines", len(before), len(after))
			}
			for i := range before {
				if before[i] != after[i] {
					t.Fatalf("refusal mutated catalog line %d:\n before: %s\n after:  %s", i, before[i], after[i])
				}
			}
		})
	}
}

func TestEnsureOperationalSchemaRefusesForeignInventory(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE SCHEMA magnetico_migration;
        CREATE TABLE magnetico_migration.errors (id int primary key)`); err != nil {
		t.Fatalf("foreign fixture: %v", err)
	}

	err := EnsureOperationalSchema(ctx, pool)
	if !errors.Is(err, ErrRunStateCorrupt) {
		t.Fatalf("EnsureOperationalSchema() error = %v, want ErrRunStateCorrupt", err)
	}
	var stateErr *StateError
	if !errors.As(err, &stateErr) {
		t.Fatalf("error = %T, want *StateError", err)
	}
	joined := strings.Join(stateErr.Details, "\n")
	if !strings.Contains(joined, "unrecognized relation inventory") ||
		!strings.Contains(joined, "missing table magnetico_migration.progress") {
		t.Fatalf("refusal details incomplete:\n%s", joined)
	}
}

func TestLoadRefusesUnexpectedRelation(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	ctx := context.Background()
	for _, stmt := range append(append([]string{}, migrationSchemaDDL...), operationalSchemaV2DDL...) {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("v2 DDL: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE magnetico_migration.loose_ends (id int)`); err != nil {
		t.Fatal(err)
	}
	_, err := Load(ctx, pool)
	if !errors.Is(err, ErrRunStateCorrupt) {
		t.Fatalf("Load() error = %v, want ErrRunStateCorrupt", err)
	}
}

func TestCountErrorsUnresolvedSemantics(t *testing.T) {
	t.Parallel()

	pool := schemaTestPool(t, pgtest.IntegrationCluster(t).TestDatabase(t))
	ctx := context.Background()
	runID := "22222222-2222-4222-8222-222222222222"
	for _, stmt := range append(append([]string{}, migrationSchemaDDL...), operationalSchemaV2DDL...) {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("v2 DDL: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.progress (run_id, phase,
        source_fingerprint, source_adapter, contract_version, codec_profile, expected_total,
        source_min_id, source_max_id, source_file_rows, last_processed_id, scanned_count,
        success_count, error_count, defer_indexes, options)
        VALUES ($1::uuid, 'importing', '{}', 'original-v0.12.0', 1, 'compact', 0, 1, 0, 0, 0, 0, 0, 0, false, '{}')`,
		runID); err != nil {
		t.Fatal(err)
	}

	record := func(sourceID int64, code string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.errors
            (run_id, scope, source_id, stage, category, code)
            VALUES ($1::uuid, 'record', $2, 'validate', 'integrity', $3)`, runID, sourceID, code); err != nil {
			t.Fatal(err)
		}
	}
	record(1, "bad_a")
	record(2, "bad_b")
	record(3, "bad_c")
	if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.errors
        (run_id, scope, stage, category, code)
        VALUES ($1::uuid, 'run', 'read', 'integrity', 'orphan_source_files')`, runID); err != nil {
		t.Fatal(err)
	}

	rec, integrity, err := CountErrors(ctx, pool, runID)
	if err != nil {
		t.Fatalf("CountErrors() error = %v", err)
	}
	if rec != 3 || integrity != 1 {
		t.Fatalf("CountErrors() = (%d,%d), want (3,1)", rec, integrity)
	}

	// Resolving two records moves the UNRESOLVED count, not the history.
	if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.errors
        SET resolved = true, resolved_at = now() WHERE source_id IN (1,2)`); err != nil {
		t.Fatal(err)
	}
	rec, integrity, err = CountErrors(ctx, pool, runID)
	if err != nil {
		t.Fatalf("CountErrors() after resolve error = %v", err)
	}
	if rec != 1 || integrity != 1 {
		t.Fatalf("CountErrors() after resolve = (%d,%d), want (1,1)", rec, integrity)
	}
	var history int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.errors
        WHERE run_id = $1::uuid AND scope = 'record'`, runID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 3 {
		t.Fatalf("resolved history lost: %d record rows, want 3", history)
	}
}
