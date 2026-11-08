package main

// Suite ⑧ end-to-end binary smoke: a disposable v0.12.0 source and a fresh
// 0.8.0 target, `magnetico migrate torrents` interrupted with SIGTERM
// mid-run (exit 2, checkpoint kept), then resumed to completion (exit 0,
// all torrents present). Skips with the harness when no test PostgreSQL is
// available.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/migration"
	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/opflags"
	"github.com/skobkin/magnetico-web/persistence"
)

const smokeSourceDDL = `
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE SCHEMA IF NOT EXISTS magneticod;
CREATE SEQUENCE IF NOT EXISTS magneticod.seq_torrents_id;
CREATE SEQUENCE IF NOT EXISTS magneticod.seq_files_id;
CREATE TABLE IF NOT EXISTS magneticod.torrents (
    id             INTEGER PRIMARY KEY DEFAULT nextval('magneticod.seq_torrents_id'),
    info_hash      bytea NOT NULL UNIQUE,
    name           TEXT NOT NULL,
    total_size     BIGINT NOT NULL CHECK(total_size > 0),
    discovered_on  INTEGER NOT NULL CHECK(discovered_on > 0)
);
CREATE INDEX IF NOT EXISTS idx_torrents_total_size ON magneticod.torrents (total_size);
CREATE INDEX IF NOT EXISTS idx_torrents_discovered_on ON magneticod.torrents (discovered_on);
CREATE INDEX IF NOT EXISTS idx_torrents_name_gin_trgm ON magneticod.torrents USING GIN (name gin_trgm_ops);
CREATE TABLE IF NOT EXISTS magneticod.files (
    id          INTEGER PRIMARY KEY DEFAULT nextval('magneticod.seq_files_id'),
    torrent_id  INTEGER REFERENCES magneticod.torrents ON DELETE CASCADE ON UPDATE RESTRICT,
    size        BIGINT NOT NULL,
    path        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_files_torrent_id ON magneticod.files (torrent_id);
CREATE TABLE IF NOT EXISTS magneticod.migrations (
    schema_version SMALLINT NOT NULL UNIQUE
);
INSERT INTO magneticod.migrations (schema_version) VALUES (0) ON CONFLICT DO NOTHING;
`

const smokeTorrents = 50000

func execSmokeSQL(t *testing.T, dsn, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("smoke connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("smoke exec: %v", err)
	}
}

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "magnetico")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// runMigrateTorrentsBinary runs one `migrate torrents` invocation. When
// signalAfter > 0, SIGTERM is delivered after that delay (a zero value or
// an already-exited process skips the signal).
func runMigrateTorrentsBinary(t *testing.T, bin, sourceDSN, targetDSN string, signalAfter time.Duration) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "migrate", "torrents",
		"--database", targetDSN, "--batch-size", "10")
	cmd.Env = append(os.Environ(), "MAGNETICO_MIGRATION_SOURCE_DSN="+sourceDSN)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if signalAfter > 0 {
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		timer := time.AfterFunc(signalAfter, func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		})
		defer timer.Stop()
		waitErr := cmd.Wait()
		t.Logf("interrupted-run output: %s", out.String())
		if waitErr == nil {
			return 0
		}
		exitErr, ok := waitErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("wait: %v", waitErr)
		}
		return exitErr.ExitCode()
	}

	err := cmd.Run()
	t.Logf("migration output: %s", out.String())
	if err == nil {
		return 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("wait: %v", err)
	}
	return exitErr.ExitCode()
}

func smokeProgress(t *testing.T, dsn string) *state.Progress {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	pool, err := target.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenPool() error = %v", err)
	}
	defer pool.Close()
	p, err := state.Load(ctx, pool)
	if err != nil {
		t.Fatalf("state.Load() error = %v", err)
	}
	return p
}

func smokeTorrentCount(t *testing.T, dsn string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	pool, err := target.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenPool() error = %v", err)
	}
	defer pool.Close()
	var n int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM magneticod.torrents`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestMigrateTorrentsBinarySmoke(t *testing.T) {
	pgtest.IntegrationCluster(t) // skip when no disposable test PostgreSQL is available

	src := pgtest.IntegrationCluster(t).TestDatabase(t)
	execSmokeSQL(t, src, smokeSourceDDL)
	execSmokeSQL(t, src, fmt.Sprintf(`
        INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        SELECT g, decode(lpad(to_hex(g), 40, '0'), 'hex'), 't' || g, 2, 1700000000 + g
        FROM generate_series(1, %d) g;
        INSERT INTO magneticod.files (torrent_id, path, size)
        SELECT g, 'f' || g || '.bin', 2 FROM generate_series(1, %d) g;`,
		smokeTorrents, smokeTorrents))

	tgt := pgtest.IntegrationCluster(t).TestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	if err := persistence.InitSchema(ctx, tgt); err != nil {
		t.Fatalf("InitSchema() error = %v", err)
	}

	bin := buildBinary(t)

	// Run 1: SIGTERM mid-run → exit 2, phase importing, checkpoint advanced.
	code := runMigrateTorrentsBinary(t, bin, src, tgt, 3*time.Second)
	if code != 2 {
		t.Fatalf("interrupted run exit code = %d, want 2", code)
	}
	p := smokeProgress(t, tgt)
	if p.Phase != state.PhaseImporting || p.LastProcessedID <= 0 {
		t.Fatalf("interrupted progress = %+v, want importing with an advanced checkpoint", p)
	}

	// Run 2: uninterrupted resume → exit 0, everything present, completed.
	code = runMigrateTorrentsBinary(t, bin, src, tgt, 0)
	if code != 0 {
		t.Fatalf("resume exit code = %d, want 0", code)
	}
	p = smokeProgress(t, tgt)
	if p.Phase != state.PhaseImportComplete || p.SuccessCount != smokeTorrents {
		t.Fatalf("resumed progress = %+v, want import_complete with %d successes", p, smokeTorrents)
	}
	if n := smokeTorrentCount(t, tgt); n != smokeTorrents {
		t.Fatalf("torrents = %d, want %d (no duplicates from the resume)", n, smokeTorrents)
	}

	// Run 3: re-running a completed migration is an idempotent exit 0.
	code = runMigrateTorrentsBinary(t, bin, src, tgt, 0)
	if code != 0 {
		t.Fatalf("completed-run re-run exit code = %d, want 0", code)
	}
}

// Source-schema refusals must reach the operator as their fixed sentinel
// plus a bounded, quoted catalog-detail list — never as an opaque
// "unexpected error (*original.SourceStateError)" that hides whether the
// source is a v1 fork or has an unexpected layout. (Review finding:
// Codex pass 1, #6.)
func TestMigrationFailureMessageRendersSourceStateRefusals(t *testing.T) {
	v1Fork := &original.SourceStateError{
		Err:     original.ErrSourceV1Fork,
		Details: []string{`relation "magneticod.files_p0"`, `migrations schema_version 1`},
	}
	got := migrationFailureMessage(v1Fork)
	if !strings.Contains(got, original.ErrSourceV1Fork.Error()) ||
		!strings.Contains(got, "magneticod.files_p0") {
		t.Fatalf("migrationFailureMessage() = %q, want the v1-fork sentinel and the catalog details", got)
	}
	if strings.Contains(got, "unexpected error") {
		t.Fatalf("migrationFailureMessage() = %q, want the typed refusal, not %T", got, v1Fork)
	}

	// Detail bounding: many/long catalog names are quoted and cut at the
	// bounded list size, never echoed raw and unbounded.
	var longDetails []string
	for i := 0; i < 20; i++ {
		longDetails = append(longDetails, strings.Repeat("n", 300)+fmt.Sprint(i))
	}
	overlong := &original.SourceStateError{Err: original.ErrSourceUnsupported, Details: longDetails}
	got = migrationFailureMessage(overlong)
	if len(got) > 4096 {
		t.Fatalf("failure message is %d bytes, want a bounded diagnostic", len(got))
	}
	if strings.Contains(got, strings.Repeat("n", 300)) {
		t.Fatalf("failure message echoes an unbounded detail verbatim: %q", got)
	}
}

// verifyResult builds a VerificationResult with the shared bookkeeping the
// message builders rely on.
func verifyResult() *migration.VerificationResult {
	expected := int64(3)
	return &migration.VerificationResult{
		Kind:                   migration.VerifyKindDecode,
		VerificationID:         "00000000-0000-0000-0000-000000000001",
		Seq:                    1,
		Pass:                   true,
		StartDataRevision:      7,
		ExpectedTargets:        &expected,
		ObservedTargets:        3,
		SourceObserved:         3,
		Verified:               2,
		AssumedSingle:          1,
		IssuesBySeverity:       map[string]int64{"info": 1},
		UnresolvedRecordErrors: 4,
		UnresolvedIntegrity:    2,
	}
}

// The summary lines must scope what a scan actually proved: verify-decode is
// explicitly a target decode pass only, verify-content is the full equality
// scan, the assumed-single blind spot and the separately-counted import
// registry are always surfaced, and a passing verdict states its data
// revision (state staleness).
func TestVerificationOutcomeMessageScopesAndStaleness(t *testing.T) {
	res := verifyResult()

	decodePass := verificationOutcomeMessage("verify-decode", "postgres://u:p@localhost/t", res, true)
	if !strings.Contains(decodePass, "target decode pass only") {
		t.Fatalf("decode pass message = %q, want the target-only scope phrase", decodePass)
	}
	if strings.Contains(decodePass, "postgres://u:p@localhost/t") {
		t.Fatalf("decode pass message echoes the raw URL: %q", decodePass)
	}

	contentPass := verificationOutcomeMessage("verify-content", "postgres://u:p@localhost/t", res, true)
	if !strings.Contains(contentPass, "full source↔target equality") ||
		!strings.Contains(contentPass, "3 source rows classified") {
		t.Fatalf("content pass message = %q, want the equality scope and the source count", contentPass)
	}

	if !strings.Contains(decodePass, "3/3 target rows observed") {
		t.Fatalf("decode pass message = %q, want the observed count", decodePass)
	}

	for _, msg := range []string{decodePass, contentPass} {
		for _, want := range []string{
			"1 assumed single-file", "1 issue(s)",
			"4 unresolved import record error(s)", "2 unresolved integrity incident(s)",
			"data revision 7", "postgres://localhost/t",
		} {
			if !strings.Contains(msg, want) {
				t.Fatalf("message %q missing %q", msg, want)
			}
		}
	}

	fail := verificationOutcomeMessage("verify-decode", "postgres://u:p@localhost/t", res, false)
	if !strings.Contains(fail, "FAIL") || !strings.Contains(fail, "verification_issues") {
		t.Fatalf("fail message = %q, want the FAIL verdict and the issues-table pointer", fail)
	}
	if strings.Contains(fail, "data revision 7") {
		t.Fatalf("fail message claims a current verdict revision: %q", fail)
	}
}

// The retry summary must state the remaining-record advice and the cutover
// rule, and record an authorized repair as provenance.
func TestRetryOutcomeMessageAdviceAndProvenance(t *testing.T) {
	clean := retryOutcomeMessage("postgres://u:p@localhost/legacy", "postgres://u:p@localhost/t", &migration.RetryResult{
		Attempted: 3, Fixed: 2, StillInvalid: 1, Missing: 0,
		UnresolvedAtStart: 3, UnresolvedRemaining: 0, PhaseFlipped: true, AcceptedSourceRepair: true,
	})
	for _, want := range []string{
		"3 attempted", "2 fixed", "1 still invalid", "0 unresolved record error(s) remain",
		"import_complete", "authorized source repair", "run verify-content before cutover",
		"postgres://localhost/t", "postgres://localhost/legacy",
	} {
		if !strings.Contains(clean, want) {
			t.Fatalf("clean retry message %q missing %q", clean, want)
		}
	}
	if strings.Contains(clean, "postgres://u:p@localhost") {
		t.Fatalf("retry message echoes a raw URL: %q", clean)
	}

	remaining := retryOutcomeMessage("postgres://u:p@localhost/legacy", "postgres://u:p@localhost/t", &migration.RetryResult{
		Attempted: 3, Fixed: 0, StillInvalid: 3, Missing: 0,
		UnresolvedAtStart: 3, UnresolvedRemaining: 3,
	})
	if !strings.Contains(remaining, "re-run retry-errors") || !strings.Contains(remaining, "3 unresolved record error(s) remain") {
		t.Fatalf("remaining retry message = %q", remaining)
	}
}

// sentinelDetail must accept only the two wrap shapes this codebase
// composes, and only after a full sentinel-prefix match — a foreign error
// whose text merely begins with similar words must never leak its text.
func TestSentinelDetailExtraction(t *testing.T) {
	sentinel := migration.ErrSourceDrift

	colon := fmt.Errorf("%w: error row for source 42 vanished", target.ErrRetryConflict)
	if got := sentinelDetail(colon, target.ErrRetryConflict); got != "error row for source 42 vanished" {
		t.Fatalf("colon detail = %q", got)
	}

	paren := fmt.Errorf("%w (source torrents 3→4, id range [1,3]→[1,4]; repair the source or pass --accept-source-repairs)", sentinel)
	got := sentinelDetail(paren, sentinel)
	if !strings.Contains(got, "--accept-source-repairs") || strings.HasPrefix(got, "(") {
		t.Fatalf("parenthesized detail = %q", got)
	}

	// A full-sentinel prefix followed by free text (neither wrap shape)
	// contributes nothing — the detail only surfaces through the two
	// composed shapes.
	if d := sentinelDetail(errors.New("source fingerprint differs from the recorded import fingerprint and something else entirely"), sentinel); d != "" {
		t.Fatalf("non-wrap-shaped suffix leaked a detail: %q", d)
	}

	// A prefix-mismatched error contributes nothing.
	if d := sentinelDetail(errors.New("totally unrelated"), sentinel); d != "" {
		t.Fatalf("foreign error leaked a detail: %q", d)
	}

	// Overlong details are bounded.
	long := fmt.Errorf("%w (%s)", sentinel, strings.Repeat("x", 500))
	if d := sentinelDetail(long, sentinel); len(d) != 128 {
		t.Fatalf("bounded detail length = %d, want 128", len(d))
	}
}

// The 04B refusals must render as their typed sentinels with the actionable
// wrap detail — never as "unexpected error".
func TestMigrationFailureMessageRenders04BRefusals(t *testing.T) {
	drift := fmt.Errorf("%w (source torrents 3→4, id range [1,3]→[1,4]; repair the source or pass --accept-source-repairs)", migration.ErrSourceDrift)
	got := migrationFailureMessage(drift)
	if !strings.Contains(got, migration.ErrSourceDrift.Error()) ||
		!strings.Contains(got, "--accept-source-repairs") ||
		strings.Contains(got, "unexpected error") {
		t.Fatalf("drift refusal = %q", got)
	}

	conflict := fmt.Errorf("%w: target already has torrent id 42", target.ErrRetryConflict)
	got = migrationFailureMessage(conflict)
	if !strings.Contains(got, target.ErrRetryConflict.Error()) || !strings.Contains(got, "torrent id 42") {
		t.Fatalf("conflict refusal = %q", got)
	}

	notComplete := fmt.Errorf("%w (phase %q — finish `migrate torrents` first)", target.ErrRunNotComplete, state.PhaseImporting)
	got = migrationFailureMessage(notComplete)
	if !strings.Contains(got, target.ErrRunNotComplete.Error()) || !strings.Contains(got, "migrate torrents") {
		t.Fatalf("not-complete refusal = %q", got)
	}

	opFlags := &opflags.VerifyOptions{}
	if err := (errors.Join(target.ErrSourceRequired)); err != nil {
		got = migrationFailureMessage(err)
		if !strings.Contains(got, target.ErrSourceRequired.Error()) {
			t.Fatalf("source-required refusal = %q", got)
		}
	}
	_ = opFlags
}

// runMigrateBinary runs one magnetico invocation and returns its exit code
// and combined output. The source DSN, when non-empty, is passed through
// MAGNETICO_MIGRATION_SOURCE_DSN (it has no CLI flag by design); an empty
// value leaves the variable empty, which the flag layer treats as unset.
func runMigrateBinary(t *testing.T, bin, sourceDSN string, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "MAGNETICO_MIGRATION_SOURCE_DSN="+sourceDSN)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	t.Logf("magnetico %v exit-output: %s", args, out.String())
	if err == nil {
		return 0, out.String()
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("wait: %v", err)
	}
	return exitErr.ExitCode(), out.String()
}

func smokeScalar(t *testing.T, dsn, sql string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("smoke connect: %v", err)
	}
	defer conn.Close(context.Background())
	var n int64
	if err := conn.QueryRow(ctx, sql).Scan(&n); err != nil {
		t.Fatalf("smoke query %q: %v", sql, err)
	}
	return n
}

// Small end-to-end exit-code smoke for the 04B commands (the 50k import
// smoke stays untouched): a three-torrent source — two importable, one
// invalid — drives the real binary through the whole recovery lifecycle:
//
//	import (exit 0, registry holds the bad record)
//	→ verify-decode PASS (exit 0), verify-content FAIL (exit 3, coverage)
//	→ source corrected → retry-errors clean (exit 0, phase flip)
//	→ verify-content PASS (exit 0)
//	→ corrupted blob → verify-decode FAIL (exit 3)
//	→ tampered source row → verify-content FAIL (exit 3)
//	→ source grew → retry-errors drift refusal (exit 1)
//	→ missing source DSN → exit 1 without touching any database.
//
// The exit-2 interrupted path is exercised at the runner level (the
// interrupt-resume integration tests) and by the torrents smoke; a
// three-torrent scan finishes too fast to interrupt reliably.
func TestMigrateVerifyRetryBinarySmoke(t *testing.T) {
	pgtest.IntegrationCluster(t) // skip when no disposable test PostgreSQL is available

	src := pgtest.IntegrationCluster(t).TestDatabase(t)
	execSmokeSQL(t, src, smokeSourceDDL)
	execSmokeSQL(t, src, `
        INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        VALUES (1, decode(lpad(to_hex(1), 40, '0'), 'hex'), 't1', 4, 1700000001),
               (2, decode(lpad(to_hex(2), 40, '0'), 'hex'), 't2', 2, 1700000002),
               (3, decode(lpad(to_hex(3), 40, '0'), 'hex'), 't3', 100, 1700000003);
        INSERT INTO magneticod.files (torrent_id, path, size)
        VALUES (1, 'a.bin', 2), (1, 'b.bin', 2),
               (2, 'f2.bin', 2),
               (3, 'f3.bin', 50);`)

	tgt := pgtest.IntegrationCluster(t).TestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	if err := persistence.InitSchema(ctx, tgt); err != nil {
		t.Fatalf("InitSchema() error = %v", err)
	}

	bin := buildBinary(t)

	// Import: the invalid record 3 lands in the registry; exit 0 (completed
	// with errors is a completed migration — never a verified one).
	if code, _ := runMigrateBinary(t, bin, src, "migrate", "torrents", "--database", tgt, "--batch-size", "10"); code != 0 {
		t.Fatalf("import exit code = %d, want 0", code)
	}
	if n := smokeScalar(t, tgt, `SELECT error_count FROM magnetico_migration.progress`); n != 1 {
		t.Fatalf("error_count = %d, want 1 (the invalid record)", n)
	}

	// verify-decode: the two stored rows decode cleanly → PASS, exit 0.
	code, out := runMigrateBinary(t, bin, "", "migrate", "verify-decode", "--database", tgt)
	if code != 0 || !strings.Contains(out, "PASS") || !strings.Contains(out, "target decode pass only") {
		t.Fatalf("verify-decode exit = %d, output = %s; want exit 0 PASS with the target-only scope", code, out)
	}

	// verify-content: source row 3 is unresolved in the registry → coverage
	// finding → completed-fail, exit 3.
	code, out = runMigrateBinary(t, bin, src, "migrate", "verify-content", "--database", tgt)
	if code != 3 || !strings.Contains(out, "FAIL") {
		t.Fatalf("verify-content exit = %d, output = %s; want exit 3 FAIL", code, out)
	}

	// Repair the source, then retry: the record becomes processable and is
	// repaired transactionally; the pass is clean and flips the phase.
	execSmokeSQL(t, src, `UPDATE magneticod.torrents SET total_size = 50 WHERE id = 3`)
	code, out = runMigrateBinary(t, bin, src, "migrate", "torrents", "retry-errors", "--database", tgt)
	if code != 0 || !strings.Contains(out, "run verify-content before cutover") {
		t.Fatalf("retry-errors exit = %d, output = %s; want exit 0 with the cutover advice", code, out)
	}
	if p := smokeProgress(t, tgt); p.Phase != state.PhaseImportComplete {
		t.Fatalf("phase after clean retry = %s, want import_complete", p.Phase)
	}

	// verify-content now sees full equality → PASS, exit 0.
	code, out = runMigrateBinary(t, bin, src, "migrate", "verify-content", "--database", tgt)
	if code != 0 || !strings.Contains(out, "PASS") || !strings.Contains(out, "full source↔target equality") {
		t.Fatalf("verify-content exit = %d, output = %s; want exit 0 PASS", code, out)
	}

	// A corrupted stored blob is a decode finding → completed-fail, exit 3.
	execSmokeSQL(t, tgt, `UPDATE magneticod.torrent_file_blobs SET data = decode('ffff', 'hex') WHERE torrent_id = 1`)
	code, _ = runMigrateBinary(t, bin, "", "migrate", "verify-decode", "--database", tgt)
	if code != 3 {
		t.Fatalf("verify-decode after blob corruption exit = %d, want 3", code)
	}

	// A tampered source row is a content mismatch → completed-fail, exit 3.
	execSmokeSQL(t, src, `UPDATE magneticod.torrents SET name = 'tampered' WHERE id = 2`)
	code, _ = runMigrateBinary(t, bin, src, "migrate", "verify-content", "--database", tgt)
	if code != 3 {
		t.Fatalf("verify-content after source tamper exit = %d, want 3", code)
	}

	// A grown source refuses a retry pass outright (drift, no authorization)
	// → exit 1. The stable evidence (ID bounds) also changed, so even an
	// authorized pass would refuse; the default refusal is what runs here.
	execSmokeSQL(t, src, `
        INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        VALUES (4, decode(lpad(to_hex(4), 40, '0'), 'hex'), 't4', 2, 1700000004);
        INSERT INTO magneticod.files (torrent_id, path, size) VALUES (4, 'f4.bin', 2);`)
	code, out = runMigrateBinary(t, bin, src, "migrate", "torrents", "retry-errors", "--database", tgt)
	if code != 1 || !strings.Contains(out, "source fingerprint differs") {
		t.Fatalf("retry-errors after source growth exit = %d, output = %s; want exit 1 drift refusal", code, out)
	}

	// Missing source DSN refuses before any database contact → exit 1.
	code, out = runMigrateBinary(t, bin, "", "migrate", "verify-content", "--database", tgt)
	if code != 1 || !strings.Contains(out, "source") {
		t.Fatalf("verify-content without a source exit = %d, output = %s; want exit 1", code, out)
	}
	code, _ = runMigrateBinary(t, bin, "", "migrate", "torrents", "retry-errors", "--database", tgt)
	if code != 1 {
		t.Fatalf("retry-errors without a source exit = %d, want 1", code)
	}

	// The new subcommands are registered: --help exits 0 (an unknown
	// command would exit 1).
	if code, _ := runMigrateBinary(t, bin, "", "migrate", "verify-decode", "--help"); code != 0 {
		t.Fatalf("verify-decode --help exit = %d, want 0", code)
	}
}
