package forensics

// Export integration tests (04C metadata slice): v1 incomplete export with
// the no-upgrade proof, failed-reconcile state exported verbatim, the
// canonical order/shape/authority contract, filters, caps as hard errors,
// the manifest lifecycle, and the registry-digest transition regressions
// (owner review finding 2).
//
// Every fixture lives on a disposable per-test database WITHOUT any
// magneticod production schema: the metadata export must never read it —
// succeeding here is the proof.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

const (
	forensicsRunID = "55555555-5555-4555-8555-555555555555"
	v1RunID        = "11111111-1111-4111-8111-111111111111"
)

func ptrInt64(v int64) *int64 { return &v }

func forensicsPool(t *testing.T, dsn string) *pgxpool.Pool {
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

// v1SchemaForensicsDDL is the exact 04A operational schema (a verbatim copy
// of the state package's v1 fixture; classification tests pin the shape).
var v1SchemaForensicsDDL = []string{
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

// seedV1Forensics creates the exact v1 schema plus a finished 04A run with
// five error rows (two unresolved record errors, one resolved, two run-scope).
func seedV1Forensics(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range v1SchemaForensicsDDL {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("v1 DDL: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.progress (
        run_id, phase, source_fingerprint, source_adapter, contract_version, codec_profile,
        expected_total, source_min_id, source_max_id, source_file_rows,
        last_processed_id, scanned_count, success_count, error_count, defer_indexes, options)
        VALUES ('`+v1RunID+`', 'import_complete_with_errors', '{"torrents":3}', 'original-v0.12.0',
                1, 'compact', 3, 1, 3, 4, 3, 3, 1, 2, false, '{"workers":2}')`); err != nil {
		t.Fatalf("seed v1 progress: %v", err)
	}
	seeds := []struct {
		scope, stage, category, code string
		sourceID                     *int64
		attempts                     int
		resolved                     bool
	}{
		{"record", "validate", "integrity", "invalid_metadata", ptrInt64(2), 1, false},
		{"record", "validate", "integrity", "invalid_framing", ptrInt64(3), 2, false},
		{"record", "read", "operational", "source_row_vanished", ptrInt64(4), 1, true},
		{"run", "read", "integrity", "orphan_source_files", nil, 1, false},
		{"run", "write", "operational", "transient_write_failure", nil, 1, false},
	}
	for _, s := range seeds {
		var sid any
		if s.sourceID != nil {
			sid = *s.sourceID
		}
		if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.errors
            (run_id, scope, source_id, stage, category, code, attempts, resolved)
            VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)`,
			v1RunID, s.scope, sid, s.stage, s.category, s.code, s.attempts, s.resolved); err != nil {
			t.Fatalf("seed v1 error row: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.errors
        SET resolved_at = now() WHERE resolved`); err != nil {
		t.Fatal(err)
	}
	return v1RunID
}

// seedV2Forensics builds a v2 run via the production InitRun path, then
// shapes the progress row and registry directly — full control over the
// states the authority/digest assertions need. The database never gets a
// magneticod schema.
func seedV2Forensics(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	runID, err := state.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.InitRun(ctx, pool, state.InitInput{
		RunID:           runID,
		FingerprintRaw:  json.RawMessage(`{"torrents":100,"min_id":1,"max_id":100,"database":"srcdb"}`),
		SourceAdapter:   "original-v0.12.0",
		ContractVersion: 1,
		CodecProfile:    "compact",
		ExpectedTotal:   100,
		SourceMinID:     1,
		SourceMaxID:     100,
		SourceFileRows:  250,
		DeferIndexes:    false,
		Options:         state.Options{Workers: 2, BatchSize: 8, MaxInflightBytes: 1 << 20},
	}); err != nil {
		t.Fatalf("InitRun: %v", err)
	}
	return runID
}

// seedV2Registry adds errors, a verification history with authority nuance
// (a historic decode pass, an authoritative decode run in progress, an
// authoritative content pass), issues, and retry history.
func seedV2Registry(t *testing.T, pool *pgxpool.Pool, runID string) (decodeHistoric, decodeRunning, contentPass string) {
	t.Helper()
	ctx := context.Background()
	// Errors: 3 record-scope (one resolved, one with hostile details), 1 run-scope.
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		rows := []struct {
			scope, stage, category, code string
			sourceID                     *int64
			resolved                     bool
			details                      string
		}{
			{"record", "validate", "integrity", "invalid_metadata", ptrInt64(5), false,
				`{"password":"secret","error":"postgres://user:pass@host/db","actual":42,"debug_note":"API token: abc123"}`},
			{"record", "decode", "codec", "stored_bytes", ptrInt64(6), false,
				`{"limit":"stored_bytes","actual":33000000,"allowed":33554432}`},
			{"record", "read", "operational", "source_row_vanished", ptrInt64(7), true, `{}`},
			{"run", "read", "integrity", "orphan_source_files", nil, false,
				`{"orphan_files":2,"orphan_id_min":9,"orphan_id_max":10}`},
		}
		for _, r := range rows {
			var sid any
			if r.sourceID != nil {
				sid = *r.sourceID
			}
			if _, err := tx.Exec(ctx, `INSERT INTO magnetico_migration.errors
                (run_id, scope, source_id, stage, category, code, details, resolved)
                VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::jsonb, $8)`,
				runID, r.scope, sid, r.stage, r.category, r.code, r.details, r.resolved); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE magnetico_migration.errors
            SET resolved_at = now() WHERE resolved`); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed errors: %v", err)
	}

	ver := func(kind, runState, phase string, startRev int64, completed bool) string {
		t.Helper()
		id, err := state.NewRunID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.verification_runs
            (verification_id, run_id, kind, state, phase, start_data_revision, completed_at,
             verified_count, issue_count, settings)
            VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, `+coalesceNow(completed)+`,
            $7, $8, '{"workers":4}'::jsonb)`,
			id, runID, kind, runState, phase, startRev, 5, 2); err != nil {
			t.Fatalf("seed verification run: %v", err)
		}
		return id
	}
	decodeHistoric = ver("decode", "completed_pass", "complete", 0, true) // seq 1: historic pass
	contentPass = ver("content", "completed_pass", "complete", 4, true)   // seq 2: authoritative + current (start 4 == revision)
	decodeRunning = ver("decode", "running", "scanning", 0, false)        // seq 3: authoritative decode, not current

	// Issues: mismatch (source 5), coverage (no source id → unavailable),
	// info (source 6), attached to the content pass.
	issues := []struct {
		code, stage, severity string
		sourceID              *int64
	}{
		{"mismatch_name", "compare", "mismatch", ptrInt64(5)},
		{"unverifiable_missing", "compare", "coverage", nil},
		{"fingerprint_drift", "observe", "info", ptrInt64(6)},
	}
	for _, i := range issues {
		var sid any
		if i.sourceID != nil {
			sid = *i.sourceID
		}
		if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.verification_issues
            (verification_id, source_id, info_hash, code, stage, severity, details)
            VALUES ($1::uuid, $2, $3, $4, $5, $6, '{"count":1}'::jsonb)`,
			contentPass, sid, []byte{0xab, 0xcd}, i.code, i.stage, i.severity); err != nil {
			t.Fatalf("seed issue: %v", err)
		}
	}

	// Retry history: two passes (older first), three attempts.
	r1, r2 := mustNewID(t), mustNewID(t)
	if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.retry_runs
        (retry_id, run_id, state, start_data_revision, current_data_revision, unresolved_at_start,
         last_source_id, attempted_count, fixed_count, still_invalid_count, missing_count, settings)
        VALUES ($1::uuid, $2::uuid, 'completed_clean', 0, 2, 3, 9, 3, 2, 1, 0, '{"workers":2}'::jsonb),
               ($3::uuid, $2::uuid, 'completed_remaining', 2, 4, 1, 11, 1, 0, 1, 0, '{"workers":2}'::jsonb)`,
		r1, runID, r2); err != nil {
		t.Fatalf("seed retry runs: %v", err)
	}
	// Distinct started_at ordering: force the second pass later.
	if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.retry_runs
        SET started_at = started_at - interval '1 hour', updated_at = updated_at - interval '1 hour'
        WHERE retry_id = $1::uuid`, r1); err != nil {
		t.Fatal(err)
	}
	attempts := []struct {
		retryID, outcome, stage, code string
		sourceID                      int64
	}{
		{r1, "fixed", "write", "ok", 8},
		{r1, "still_invalid", "decode", "stored_bytes", 9},
		{r2, "missing_source", "read", "missing_source", 11},
	}
	for _, a := range attempts {
		if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.retry_attempts
            (retry_id, run_id, source_id, source_info_hash, outcome, stage, code, details)
            VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, '{"attempts":1}'::jsonb)`,
			a.retryID, runID, a.sourceID, []byte{0x11, 0x22}, a.outcome, a.stage, a.code); err != nil {
			t.Fatalf("seed attempt: %v", err)
		}
	}
	// Give the run a finished import shape: 100 scanned, all good except the
	// registry, revision bumped past the verification start.
	if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.progress
        SET phase = 'import_complete', scanned_count = 100, success_count = 97,
            error_count = 3, data_revision = 4
        WHERE run_id = $1::uuid`, runID); err != nil {
		t.Fatal(err)
	}
	return decodeHistoric, decodeRunning, contentPass
}

func coalesceNow(b bool) string {
	if b {
		return "now()"
	}
	return "NULL"
}

func mustNewID(t *testing.T) string {
	t.Helper()
	id, err := state.NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// jsonLines parses the artifact into ordered generic maps with exact
// integers (UseNumber).
func jsonLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	dec := json.NewDecoder(bufio.NewReader(f))
	dec.UseNumber()
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("line %d: %v", len(out)+1, err)
		}
		out = append(out, m)
	}
	return out
}

func num(t *testing.T, m map[string]any, key string) json.Number {
	t.Helper()
	v, ok := m[key].(json.Number)
	if !ok {
		t.Fatalf("key %q = %v (%T), want a number", key, m[key], m[key])
	}
	return v
}

func intOf(t *testing.T, m map[string]any, key string) int64 {
	t.Helper()
	v, err := num(t, m, key).Int64()
	if err != nil {
		t.Fatalf("key %q = %v: %v", key, m[key], err)
	}
	return v
}

func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("key %q = %v (%T), want a string", key, m[key], m[key])
	}
	return v
}

func relNames(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT relname FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'magnetico_migration' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	return out
}

func TestExportErrorsV1Incomplete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	pool := forensicsPool(t, dsn)
	seedV1Forensics(t, pool)
	before := relNames(t, pool)

	out := filepath.Join(t.TempDir(), "v1-export.jsonl")
	res, err := ExportErrors(ctx, ExportInput{
		Options:   ExportOptions{OutputPath: out, Limits: DefaultLimits()},
		TargetDSN: dsn,
	})
	if err != nil {
		t.Fatalf("ExportErrors() error = %v", err)
	}

	// No upgrade: the relation inventory is byte-identical — and no marker
	// table exists (v1 predates it; classification never created one).
	after := relNames(t, pool)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("export changed the catalog: %v -> %v", before, after)
	}
	for _, name := range after {
		if name == "operational_schema" {
			t.Fatal("marker table exists on a v1 fixture — something upgraded")
		}
	}

	lines := jsonLines(t, out)
	if len(lines) == 0 {
		t.Fatal("empty export")
	}
	header := lines[0]
	if str(t, header, "type") != TypeExportHeader {
		t.Fatalf("first line type = %v", header["type"])
	}
	if str(t, header, "operational_schema") != "v1" {
		t.Fatalf("operational_schema = %v", header["operational_schema"])
	}
	unavail := header["unavailable_collections"].([]any)
	if len(unavail) != 4 {
		t.Fatalf("unavailable_collections = %v, want the 4 04B collections", unavail)
	}
	if str(t, header, "coverage") != "complete" {
		t.Fatalf("coverage = %v", header["coverage"])
	}

	// Envelope on every line; import_run verbatim; 5 error events.
	for i, l := range lines {
		if str(t, l, "format") != Format || intOf(t, l, "format_version") != int64(FormatVersion) {
			t.Fatalf("line %d envelope wrong: %v", i, l)
		}
	}
	var errEvents, runEvents int
	for _, l := range lines {
		switch str(t, l, "type") {
		case TypeImportError:
			errEvents++
		case TypeImportRun:
			runEvents++
			ir := l
			if str(t, ir, "phase") != "import_complete_with_errors" {
				t.Fatalf("v1 phase = %v", ir["phase"])
			}
			if intOf(t, ir, "data_revision") != 0 {
				t.Fatalf("v1 data_revision = %v, want the 0 projection", ir["data_revision"])
			}
			if str(t, ir, "reconcile_status") != "not_reconciled" {
				t.Fatalf("reconcile_status = %v", ir["reconcile_status"])
			}
		case TypeVerificationRun, TypeVerificationIssue, TypeRetryRun, TypeRetryAttempt:
			t.Fatalf("v1 export emitted a 04B event: %v", l["type"])
		}
	}
	if errEvents != 5 || runEvents != 1 {
		t.Fatalf("events: %d errors %d import_runs, want 5/1", errEvents, runEvents)
	}

	// Ascending error IDs, resolved rows included.
	lastID := int64(0)
	for _, l := range lines {
		if str(t, l, "type") != TypeImportError {
			continue
		}
		id := intOf(t, l, "error_id")
		if id <= lastID {
			t.Fatalf("error ids not ascending: %d after %d", id, lastID)
		}
		lastID = id
	}

	// Footer last, inside the checksum, with ACTUAL counts.
	footer := lines[len(lines)-1]
	if str(t, footer, "type") != TypeExportFooter {
		t.Fatalf("last line = %v", footer["type"])
	}
	if intOf(t, footer, "events") != int64(len(lines)) {
		t.Fatalf("footer events %v != %d lines", footer["events"], len(lines))
	}
	counts := footer["counts"].(map[string]any)
	if fmt.Sprint(counts[TypeImportError]) != "5" {
		t.Fatalf("footer counts = %v", counts)
	}

	// Checksum binds the exact published bytes.
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != fmt.Sprintf("%x", res.SHA256) || int64(len(body)) != res.Bytes {
		t.Fatalf("result checksum/bytes do not match the file")
	}
}

func TestExportErrorsFailedReconcile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	pool := forensicsPool(t, dsn)

	// v2 with counters that violate every Load invariant and an unknown
	// phase — forensic export reports verbatim; Load refuses (contrast).
	runID := seedV2Forensics(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE magnetico_migration.progress
        SET phase = 'paused_by_aliens', scanned_count = 10, success_count = 3,
            error_count = 2, last_processed_id = 1
        WHERE run_id = $1::uuid`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.errors
        (run_id, scope, source_id, stage, category, code)
        VALUES ($1::uuid, 'record', 2, 'validate', 'integrity', 'invalid_metadata')`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Load(ctx, pool); err == nil {
		t.Fatal("state.Load() accepted inconsistent counters — the contrast failed")
	}

	out := filepath.Join(t.TempDir(), "broken-export.jsonl")
	res, err := ExportErrors(ctx, ExportInput{
		Options:   ExportOptions{OutputPath: out, Limits: DefaultLimits()},
		TargetDSN: dsn,
	})
	if err != nil {
		t.Fatalf("ExportErrors() on inconsistent state error = %v", err)
	}
	lines := jsonLines(t, out)
	header := lines[0]
	if str(t, header, "operational_schema") != "v2" {
		t.Fatalf("operational_schema = %v", header["operational_schema"])
	}
	for _, l := range lines {
		if str(t, l, "type") == TypeImportRun {
			if str(t, l, "phase") != "paused_by_aliens" {
				t.Fatalf("phase = %v, want the unknown phase verbatim", l["phase"])
			}
			if intOf(t, l, "scanned_count") != 10 || intOf(t, l, "success_count") != 3 {
				t.Fatalf("counters = %v/%v, want verbatim 10/3", l["scanned_count"], l["success_count"])
			}
		}
	}
	if res.RunID != runID {
		t.Fatalf("RunID = %s", res.RunID)
	}
}

func TestExportErrorsShapeOrderAuthority(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	pool := forensicsPool(t, dsn)
	runID := seedV2Forensics(t, pool)
	seedV2Registry(t, pool, runID)

	out := filepath.Join(t.TempDir(), "full-export.jsonl")
	if _, err := ExportErrors(ctx, ExportInput{
		Options:   ExportOptions{OutputPath: out, WriteManifest: true, Limits: DefaultLimits()},
		TargetDSN: dsn,
	}); err != nil {
		t.Fatalf("ExportErrors() error = %v", err)
	}
	lines := jsonLines(t, out)

	// Canonical global order: header, import_run, errors(4), verification
	// runs(seq 1,2,3), issues(3), retry runs(2), attempts(3), footer.
	wantTypes := []string{
		TypeExportHeader, TypeImportRun,
		TypeImportError, TypeImportError, TypeImportError, TypeImportError,
		TypeVerificationRun, TypeVerificationRun, TypeVerificationRun,
		TypeVerificationIssue, TypeVerificationIssue, TypeVerificationIssue,
		TypeRetryRun, TypeRetryRun,
		TypeRetryAttempt, TypeRetryAttempt, TypeRetryAttempt,
		TypeExportFooter,
	}
	if len(lines) != len(wantTypes) {
		t.Fatalf("got %d lines, want %d", len(lines), len(wantTypes))
	}
	for i, want := range wantTypes {
		if got := str(t, lines[i], "type"); got != want {
			t.Fatalf("line %d type = %s, want %s", i, got, want)
		}
	}

	// Error events carry the run_id envelope and redacted hostile details.
	hostile := lines[2]
	if str(t, hostile, "run_id") != runID {
		t.Fatalf("error envelope run_id = %v", hostile["run_id"])
	}
	details := hostile["details"].(map[string]any)
	for _, forbidden := range []string{"password", "debug_note", "secret", "postgres://"} {
		if strings.Contains(fmt.Sprint(details), forbidden) {
			t.Fatalf("hostile details leaked %q: %v", forbidden, details)
		}
	}
	if details["actual"] != json.Number("42") {
		t.Fatalf("safe numeric actual lost: %v", details["actual"])
	}
	if _, has := details["redacted"]; !has {
		t.Fatalf("redaction stats missing: %v", details)
	}

	// Authority flags: decode seq1 is historic (not authoritative); content
	// seq2 is authoritative AND current (start_data_revision 4 == the live
	// revision); decode seq3 is authoritative but running — never current.
	vr := lines[6:9]
	if intOf(t, vr[0], "seq") != 1 || intOf(t, vr[1], "seq") != 2 || intOf(t, vr[2], "seq") != 3 {
		t.Fatalf("verification runs out of seq order: %v/%v/%v", vr[0]["seq"], vr[1]["seq"], vr[2]["seq"])
	}
	if fmt.Sprint(vr[0]["authoritative"]) == "true" {
		t.Fatalf("historic decode pass must not be authoritative: %v", vr[0])
	}
	if fmt.Sprint(vr[1]["authoritative"]) != "true" || fmt.Sprint(vr[1]["current_pass"]) != "true" {
		t.Fatalf("content seq2: authoritative=%v current=%v, want true/true (authoritative completed_pass at the live revision)",
			vr[1]["authoritative"], vr[1]["current_pass"])
	}
	if fmt.Sprint(vr[2]["authoritative"]) != "true" || fmt.Sprint(vr[2]["current_pass"]) != "false" {
		t.Fatalf("decode seq3: authoritative=%v current=%v, want true/false (running)",
			vr[2]["authoritative"], vr[2]["current_pass"])
	}
	if str(t, vr[2], "kind") != "decode" || str(t, vr[2], "state") != "running" {
		t.Fatalf("decode seq3 = %v/%v", vr[2]["kind"], vr[2]["state"])
	}

	// Issues: the run-scope issue (source_id NULL) is explicitly marked.
	var sawUnavailable bool
	for _, l := range lines[9:12] {
		if l["source_id"] == nil && str(t, l, "record_unavailable_reason") != "no_source_id" {
			t.Fatalf("NULL-source issue not marked: %v", l)
		}
		if l["source_id"] == nil {
			sawUnavailable = true
		}
		if fmt.Sprint(l["severity"]) == "mismatch" && l["verification_seq"] == nil {
			t.Fatalf("issue verification_seq missing: %v", l)
		}
	}
	if !sawUnavailable {
		t.Fatal("no run-scope issue found in the fixture output")
	}

	// Retry runs ascending by (started_at, retry_id): the backdated pass first.
	rr := lines[12:14]
	if fmt.Sprint(rr[0]["state"]) != "completed_clean" || fmt.Sprint(rr[1]["state"]) != "completed_remaining" {
		t.Fatalf("retry order wrong: %v then %v", rr[0]["state"], rr[1]["state"])
	}

	// infohashes lowercase hex.
	att := lines[14]
	ih := str(t, att, "info_hash")
	if ih != "1122" {
		t.Fatalf("info_hash = %q, want lowercase hex", ih)
	}
}

func TestExportErrorsFiltersMakePartial(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	pool := forensicsPool(t, dsn)
	runID := seedV2Forensics(t, pool)
	_, _, contentPass := seedV2Registry(t, pool, runID)

	dir := t.TempDir()

	t.Run("only-unresolved drops resolved errors, marks partial", func(t *testing.T) {
		out := filepath.Join(dir, "unresolved.jsonl")
		res, err := ExportErrors(ctx, ExportInput{
			Options: ExportOptions{OutputPath: out, WriteManifest: true,
				OnlyUnresolved: true, Limits: DefaultLimits()},
			TargetDSN: dsn,
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Coverage != "partial" {
			t.Fatalf("coverage = %s", res.Coverage)
		}
		var resolved int
		for _, l := range jsonLines(t, out) {
			if str(t, l, "type") == TypeImportError && fmt.Sprint(l["resolved"]) == "true" {
				resolved++
			}
		}
		if resolved != 0 {
			t.Fatalf("%d resolved rows leaked through --only-unresolved", resolved)
		}
		m := readManifest(t, out+".manifest.json")
		if m.Coverage != "partial" || !m.Filters.OnlyUnresolved {
			t.Fatalf("manifest = %s %+v", m.Coverage, m.Filters)
		}
		// Partial evidence must NOT validate for cleanup.
		if _, err := ValidateManifestFile(out + ".manifest.json"); !errors.Is(err, ErrEvidenceRejected) {
			t.Fatalf("partial manifest validated: %v", err)
		}
	})

	t.Run("verification-id narrows verification events only", func(t *testing.T) {
		out := filepath.Join(dir, "byid.jsonl")
		if _, err := ExportErrors(ctx, ExportInput{
			Options:   ExportOptions{OutputPath: out, VerificationID: contentPass, Limits: DefaultLimits()},
			TargetDSN: dsn,
		}); err != nil {
			t.Fatal(err)
		}
		var runs, issues, errs int
		for _, l := range jsonLines(t, out) {
			switch str(t, l, "type") {
			case TypeVerificationRun:
				runs++
				if str(t, l, "verification_id") != contentPass {
					t.Fatalf("foreign verification leaked: %v", l["verification_id"])
				}
			case TypeVerificationIssue:
				issues++
			case TypeImportError:
				errs++ // untouched by the verification filter
			}
		}
		if runs != 1 || issues != 3 || errs != 4 {
			t.Fatalf("filtered export: %d runs %d issues %d errors, want 1/3/4", runs, issues, errs)
		}
	})
}

func TestExportErrorsCapsAreHardErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	pool := forensicsPool(t, dsn)
	runID := seedV2Forensics(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO magnetico_migration.errors
        (run_id, scope, source_id, stage, category, code)
        VALUES ($1::uuid, 'record', 2, 'validate', 'integrity', 'invalid_metadata'),
               ($1::uuid, 'record', 3, 'validate', 'integrity', 'invalid_framing')`, runID); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	t.Run("event cap", func(t *testing.T) {
		out := filepath.Join(dir, "capped-events.jsonl")
		limits := DefaultLimits()
		limits.MaxEvents = 3 // header + import_run + 1 error, then breach
		_, err := ExportErrors(ctx, ExportInput{
			Options:   ExportOptions{OutputPath: out, Limits: limits},
			TargetDSN: dsn,
		})
		if !errors.Is(err, ErrExportCapExceeded) || !strings.Contains(err.Error(), "max-events") {
			t.Fatalf("error = %v, want the max-events cap", err)
		}
		assertNoOutput(t, dir, "capped-events.jsonl")
	})

	t.Run("byte cap", func(t *testing.T) {
		out := filepath.Join(dir, "capped-bytes.jsonl")
		limits := DefaultLimits()
		limits.MaxOutputBytes = 512
		_, err := ExportErrors(ctx, ExportInput{
			Options:   ExportOptions{OutputPath: out, Limits: limits},
			TargetDSN: dsn,
		})
		if !errors.Is(err, ErrExportCapExceeded) || !strings.Contains(err.Error(), "max-output-bytes") {
			t.Fatalf("error = %v, want the max-output-bytes cap", err)
		}
		assertNoOutput(t, dir, "capped-bytes.jsonl")
	})
}

func assertNoOutput(t *testing.T, dir, final string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, final)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final path exists after a failed export: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp %s left behind after a failed export", e.Name())
		}
	}
}

func TestExportErrorsOptionRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name    string
		opts    ExportOptions
		wantErr error
	}{
		{"records need the later slice", ExportOptions{OutputPath: "x", IncludeRecords: true, Limits: DefaultLimits()}, nil},
		{"blobs need records", ExportOptions{OutputPath: "x", IncludeBlobs: true, Limits: DefaultLimits()}, nil},
		{"stdout + manifest", ExportOptions{OutputPath: StdoutPath, WriteManifest: true, Limits: DefaultLimits()}, nil},
		{"bad limits", ExportOptions{OutputPath: "x", Limits: Limits{MaxEvents: -1, MaxRecords: 1, MaxOutputBytes: 1, MaxBlobBytes: 1}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ExportErrors(ctx, ExportInput{Options: tc.opts, TargetDSN: "postgres://127.0.0.1:1/none"})
			var optsErr *OptionsError
			if !errors.As(err, &optsErr) {
				t.Fatalf("error = %v, want *OptionsError", err)
			}
		})
	}

	t.Run("absent schema is a typed refusal", func(t *testing.T) {
		t.Parallel()
		dsn := pgtest.IntegrationCluster(t).TestDatabase(t) // fresh: no magnetico_migration
		_, err := ExportErrors(ctx, ExportInput{
			Options:   ExportOptions{OutputPath: filepath.Join(t.TempDir(), "x.jsonl"), Limits: DefaultLimits()},
			TargetDSN: dsn,
		})
		if !errors.Is(err, ErrNoMigrationHistory) {
			t.Fatalf("error = %v, want ErrNoMigrationHistory", err)
		}
	})
}

func readManifest(t *testing.T, path string) Manifest {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	return m
}
