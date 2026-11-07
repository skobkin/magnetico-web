package migration

// Suites ④ (record semantics) and ⑥ (crash/resume, ambiguity, orphans,
// interrupted runs) at the runner level, against disposable source and
// target databases from the shared harness.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

// Fixture DDL reproducing the frozen boramalper/magnetico v0.12.0 source
// layout, schema-qualified for a fresh harness database (same DDL as the
// source-adapter fixtures; duplicated because _test files cannot import
// each other across packages).
const runnerSourceDDL = `
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

func runnerSource(t *testing.T) string {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	execSourceSQL(t, dsn, runnerSourceDDL)
	return dsn
}

func execSourceSQL(t *testing.T, dsn, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("fixture connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("fixture exec: %v", err)
	}
}

func seedSourceTorrent(t *testing.T, dsn string, id int32, name string, totalSize int64, discoveredOn int32) {
	t.Helper()
	execSourceSQL(t, dsn, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        VALUES ($1, decode($2, 'hex'), $3, $4, $5)`,
		id, hexHash(id), name, totalSize, discoveredOn)
}

func hexHash(id int32) string {
	return fmt.Sprintf("%040x", id)
}

func seedSourceFile(t *testing.T, dsn string, torrentID int32, path string, size int64) {
	t.Helper()
	execSourceSQL(t, dsn, `INSERT INTO magneticod.files (torrent_id, path, size) VALUES ($1, $2, $3)`,
		torrentID, path, size)
}

// runTorrents executes one ImportTorrents pass with sane test defaults.
func runTorrents(t *testing.T, ctx context.Context, in TorrentsInput) error {
	t.Helper()
	if in.Workers == 0 {
		in.Workers = 2
	}
	if in.BatchSize == 0 {
		in.BatchSize = 5
	}
	if in.MaxInflightBytes == 0 {
		in.MaxInflightBytes = DefaultMaxInflightBytes
	}
	return ImportTorrents(ctx, in)
}

// sourceCounts reads the target's data/counters after a run.
type targetSnapshot struct {
	torrents, blobs, recordErrors, runErrors int64
	progress                                 *state.Progress
}

func snapshotTarget(t *testing.T, dsn string) targetSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	pool, err := target.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenPool() error = %v", err)
	}
	defer pool.Close()
	var s targetSnapshot
	if err := pool.QueryRow(ctx, `SELECT
            (SELECT COUNT(*) FROM magneticod.torrents),
            (SELECT COUNT(*) FROM magneticod.torrent_file_blobs),
            (SELECT COUNT(*) FROM magnetico_migration.errors WHERE scope = 'record'),
            (SELECT COUNT(*) FROM magnetico_migration.errors WHERE scope = 'run' AND category = 'integrity')`).Scan(
		&s.torrents, &s.blobs, &s.recordErrors, &s.runErrors); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	p, err := state.Load(ctx, pool)
	if err != nil {
		t.Fatalf("state.Load() error = %v", err)
	}
	s.progress = p
	return s
}

func errorCodes(t *testing.T, dsn string) map[string]int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	pool, err := target.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenPool() error = %v", err)
	}
	defer pool.Close()
	rows, err := pool.Query(ctx,
		`SELECT code, COUNT(*) FROM magnetico_migration.errors WHERE scope = 'record' GROUP BY code`)
	if err != nil {
		t.Fatalf("error codes: %v", err)
	}
	defer rows.Close()
	codes := map[string]int64{}
	for rows.Next() {
		var code string
		var n int64
		if err := rows.Scan(&code, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		codes[code] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return codes
}

// --- suite ④: record semantics ----------------------------------------------

func TestRunnerRecordSemantics(t *testing.T) {
	t.Parallel()

	src := runnerSource(t)
	tgt := createTargetForRunner(t)

	// Torrent 1: multi-file with duplicate paths (including a zero-size
	// member) — 3 rows (within the test limit of 3).
	seedSourceTorrent(t, src, 1, "multi", 4, 1700000001)
	seedSourceFile(t, src, 1, "a.txt", 0) // duplicate path, zero size
	seedSourceFile(t, src, 1, "a.txt", 1)
	seedSourceFile(t, src, 1, "z.txt", 3)
	// Torrent 2: single typical file (path == name, size == total) — no blob.
	seedSourceTorrent(t, src, 2, "single2", 5, 1700000002)
	seedSourceFile(t, src, 2, "single2", 5)
	// Torrent 3: single atypical file — keeps its blob.
	seedSourceTorrent(t, src, 3, "three", 7, 1700000003)
	seedSourceFile(t, src, 3, "inner.bin", 7)
	// Torrent 4: total_size contradicts the file sum — invalid metadata.
	seedSourceTorrent(t, src, 4, "bad-total", 100, 1700000004)
	seedSourceFile(t, src, 4, "x", 50)
	// Torrent 5: no file rows at all.
	seedSourceTorrent(t, src, 5, "no-files", 1, 1700000005)
	// Torrent 6: four file rows — over the test limit (3) → read overflow.
	seedSourceTorrent(t, src, 6, "overflow", 4, 1700000006)
	for i := 0; i < 4; i++ {
		seedSourceFile(t, src, 6, fmt.Sprintf("f%d", i), 1)
	}

	limits := codec.DefaultLimits()
	limits.MaxFiles = 3
	err := runTorrents(t, context.Background(), TorrentsInput{
		SourceDSN: src, TargetDSN: tgt, Limits: &limits,
	})
	if err != nil {
		t.Fatalf("ImportTorrents() error = %v", err)
	}

	snap := snapshotTarget(t, tgt)
	p := snap.progress
	// Record-level errors force import_complete_with_errors (never plain).
	if p.Phase != state.PhaseCompleteWithErrors {
		t.Fatalf("phase = %s, want import_complete_with_errors", p.Phase)
	}
	// A non-deferred run keeps all three indexes maintained throughout.
	wantSecondaryIndexes(t, tgt, map[string]bool{
		"idx_torrents_total_size": true, "idx_torrents_discovered_on": true, "idx_torrents_name_gin_trgm": true,
	})
	if p.LastProcessedID != 6 || p.ScannedCount != 6 || p.SuccessCount != 3 ||
		p.ErrorCount != 3 || p.BlobCount != 2 || p.RunErrorCount != 0 {
		t.Fatalf("progress = %+v", p)
	}
	if snap.torrents != 3 || snap.blobs != 2 || snap.recordErrors != 3 {
		t.Fatalf("snapshot = %+v", snap)
	}

	codes := errorCodes(t, tgt)
	if codes["invalid_metadata"] != 1 || codes["missing_files"] != 1 || codes["file_count_exceeded"] != 1 {
		t.Fatalf("error codes = %v", codes)
	}

	// Exact IDs preserved; file counts right; blob presence by shape.
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	pool, err := target.OpenPool(ctx, tgt)
	if err != nil {
		t.Fatalf("OpenPool() error = %v", err)
	}
	defer pool.Close()
	var fc1, fc2, fc3 int32
	var blobs1, blobs2, blobs3 bool
	if err := pool.QueryRow(ctx, `SELECT
            (SELECT file_count FROM magneticod.torrents WHERE id = 1),
            (SELECT file_count FROM magneticod.torrents WHERE id = 2),
            (SELECT file_count FROM magneticod.torrents WHERE id = 3),
            (SELECT EXISTS (SELECT 1 FROM magneticod.torrent_file_blobs WHERE torrent_id = 1)),
            (SELECT EXISTS (SELECT 1 FROM magneticod.torrent_file_blobs WHERE torrent_id = 2)),
            (SELECT EXISTS (SELECT 1 FROM magneticod.torrent_file_blobs WHERE torrent_id = 3))`).Scan(
		&fc1, &fc2, &fc3, &blobs1, &blobs2, &blobs3); err != nil {
		t.Fatalf("read imported rows: %v", err)
	}
	if fc1 != 3 || fc2 != 1 || fc3 != 1 {
		t.Fatalf("file counts = %d/%d/%d, want 3/1/1", fc1, fc2, fc3)
	}
	if !blobs1 || blobs2 || !blobs3 {
		t.Fatalf("blob presence = %v/%v/%v, want true/false/true", blobs1, blobs2, blobs3)
	}

	// Torrent 1's blob decodes to the canonical, duplicate-preserving list.
	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT data FROM magneticod.torrent_file_blobs WHERE torrent_id = 1`).Scan(&raw); err != nil {
		t.Fatalf("read blob: %v", err)
	}
	files, err := codec.Decode(raw, codec.DefaultLimits())
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	want := []codec.File{{Path: "a.txt", Size: 0}, {Path: "a.txt", Size: 1}, {Path: "z.txt", Size: 3}}
	if len(files) != len(want) {
		t.Fatalf("decoded %d files, want %d", len(files), len(want))
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("decoded file %d = %+v, want %+v", i, files[i], want[i])
		}
	}
}

func TestRunnerEmptySourceCompletes(t *testing.T) {
	t.Parallel()

	src := runnerSource(t) // zero torrents
	tgt := createTargetForRunner(t)

	if err := runTorrents(t, context.Background(), TorrentsInput{SourceDSN: src, TargetDSN: tgt}); err != nil {
		t.Fatalf("ImportTorrents() error = %v", err)
	}
	p := snapshotTarget(t, tgt).progress
	if p.Phase != state.PhaseImportComplete || p.LastProcessedID != -1 || p.ScannedCount != 0 {
		t.Fatalf("empty-source progress = %+v", p)
	}
	// Documented empty-import behavior: the identity sequence is left at its
	// initial state (next generated ID would be 1).
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	pool := poolForRunner(t, tgt)
	defer pool.Close()
	var lastValue int64
	var isCalled bool
	if err := pool.QueryRow(ctx,
		`SELECT last_value, is_called FROM magneticod.torrents_id_seq`).Scan(&lastValue, &isCalled); err != nil {
		t.Fatalf("read identity sequence: %v", err)
	}
	if lastValue != 1 || isCalled {
		t.Fatalf("identity sequence = (%d, %v), want initial state (1, false)", lastValue, isCalled)
	}
}

// --- suite ⑥: interruption, resume, idempotence ------------------------------

func TestRunnerInterruptedThenResumes(t *testing.T) {
	t.Parallel()

	src := runnerSource(t)
	tgt := createTargetForRunner(t)
	for id := int32(1); id <= 40; id++ {
		seedSourceTorrent(t, src, id, fmt.Sprintf("t%d", id), 2, 1700000000+int32(id))
		seedSourceFile(t, src, id, fmt.Sprintf("t%d.bin", id), 2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reports int
	err := runTorrents(t, ctx, TorrentsInput{
		SourceDSN: src, TargetDSN: tgt,
		Progress: func(Progress) {
			reports++
			if reports == 2 {
				cancel() // cancel right after the second committed batch
			}
		},
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("first run error = %v, want ErrInterrupted", err)
	}

	snap := snapshotTarget(t, tgt)
	p := snap.progress
	if p.Phase != state.PhaseImporting || p.LastProcessedID <= 0 || p.LastProcessedID >= 40 {
		t.Fatalf("interrupted progress = %+v", p)
	}
	if snap.torrents != p.SuccessCount {
		t.Fatalf("torrents %d != success_count %d", snap.torrents, p.SuccessCount)
	}

	// Resume completes the import.
	if err := runTorrents(t, context.Background(), TorrentsInput{SourceDSN: src, TargetDSN: tgt}); err != nil {
		t.Fatalf("resume error = %v", err)
	}
	snap = snapshotTarget(t, tgt)
	p = snap.progress
	if p.Phase != state.PhaseImportComplete || p.LastProcessedID != 40 ||
		p.ScannedCount != 40 || p.SuccessCount != 40 || p.ErrorCount != 0 || p.BlobCount != 40 {
		t.Fatalf("resumed progress = %+v", p)
	}
	if snap.torrents != 40 || snap.blobs != 40 {
		t.Fatalf("resumed snapshot = %+v", snap)
	}

	// Repeated resume after data_loaded is an idempotent no-op.
	if err := runTorrents(t, context.Background(), TorrentsInput{SourceDSN: src, TargetDSN: tgt}); err != nil {
		t.Fatalf("second resume error = %v", err)
	}
}

func TestRunnerRefusesChangedSource(t *testing.T) {
	t.Parallel()

	src := runnerSource(t)
	tgt := createTargetForRunner(t)
	for id := int32(1); id <= 30; id++ {
		seedSourceTorrent(t, src, id, fmt.Sprintf("t%d", id), 1, 1700000000+int32(id))
		seedSourceFile(t, src, id, fmt.Sprintf("t%d.bin", id), 1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reports int
	err := runTorrents(t, ctx, TorrentsInput{
		SourceDSN: src, TargetDSN: tgt,
		Progress: func(Progress) {
			reports++
			if reports == 2 {
				cancel()
			}
		},
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("first run error = %v, want ErrInterrupted", err)
	}

	// The source changed under the frozen fingerprint: resume must refuse.
	seedSourceTorrent(t, src, 31, "late", 1, 1700000031)
	seedSourceFile(t, src, 31, "late.bin", 1)
	err = runTorrents(t, context.Background(), TorrentsInput{SourceDSN: src, TargetDSN: tgt})
	if !errors.Is(err, state.ErrRunStateCorrupt) {
		t.Fatalf("resume after source change error = %v, want ErrRunStateCorrupt", err)
	}
}

// --- suite ⑥: ambiguous COMMIT reconciliation -------------------------------

type flakyCommitter struct {
	inner     batchCommitter
	mu        sync.Mutex
	failsLeft int
	commits   int
}

func (f *flakyCommitter) CommitBatch(ctx context.Context, b target.Batch) error {
	err := f.inner.CommitBatch(ctx, b)
	if err == nil {
		f.mu.Lock()
		f.commits++
		fail := f.failsLeft > 0
		if fail {
			f.failsLeft--
		}
		f.mu.Unlock()
		if fail {
			// The commit LANDED; simulate losing the acknowledgement.
			return errors.New("connection reset while committing (simulated)")
		}
	}
	return err
}

func TestRunnerAmbiguousCommitReconciles(t *testing.T) {
	t.Parallel()

	src := runnerSource(t)
	tgt := createTargetForRunner(t)
	for id := int32(1); id <= 20; id++ {
		seedSourceTorrent(t, src, id, fmt.Sprintf("t%d", id), 1, 1700000000+int32(id))
		seedSourceFile(t, src, id, fmt.Sprintf("t%d.bin", id), 1)
	}

	pool := poolForRunner(t, tgt)
	flaky := &flakyCommitter{inner: target.NewBatchCommitter(pool), failsLeft: 2}
	if err := runTorrents(t, context.Background(), TorrentsInput{
		SourceDSN: src, TargetDSN: tgt, committer: flaky,
	}); err != nil {
		t.Fatalf("ImportTorrents() error = %v", err)
	}

	snap := snapshotTarget(t, tgt)
	p := snap.progress
	if p.Phase != state.PhaseImportComplete || p.SuccessCount != 20 || p.ScannedCount != 20 {
		t.Fatalf("progress = %+v", p)
	}
	if snap.torrents != 20 {
		t.Fatalf("torrents = %d, want exactly 20 (no duplicates from replay)", snap.torrents)
	}
	// Batch sizes are scheduling-dependent (liveness over fill), so only a
	// lower bound holds: 20 records / batch cap 5 ⇒ at least 4 commits.
	if flaky.commits < 4 {
		t.Fatalf("committed batches = %d, want >= 4", flaky.commits)
	}
}

// --- suite ⑥/④: orphan policy ----------------------------------------------

func TestRunnerOrphansExcludedAndRecorded(t *testing.T) {
	t.Parallel()

	src := runnerSource(t)
	tgt := createTargetForRunner(t)

	seedSourceTorrent(t, src, 10, "multi", 3, 1700000010)
	seedSourceFile(t, src, 10, "a", 1)
	seedSourceFile(t, src, 10, "b", 2)
	seedSourceTorrent(t, src, 11, "single11", 4, 1700000011)
	seedSourceFile(t, src, 11, "single11", 4)

	// Plant one dangling (non-NULL, unowned) file row and one NULL-owned
	// row; the FK must be dropped to plant the orphan, then re-added.
	execSourceSQL(t, src, `ALTER TABLE magneticod.files DROP CONSTRAINT files_torrent_id_fkey`)
	execSourceSQL(t, src, `INSERT INTO magneticod.files (id, torrent_id, path, size)
        VALUES (999, 99, 'orphan.bin', 5)`)
	execSourceSQL(t, src, `INSERT INTO magneticod.files (id, torrent_id, path, size)
        VALUES (998, NULL, 'null-owned.bin', 6)`)
	execSourceSQL(t, src, `ALTER TABLE magneticod.files
        ADD CONSTRAINT files_torrent_id_fkey FOREIGN KEY (torrent_id)
        REFERENCES magneticod.torrents(id) ON DELETE CASCADE NOT VALID`)

	if err := runTorrents(t, context.Background(), TorrentsInput{SourceDSN: src, TargetDSN: tgt}); err != nil {
		t.Fatalf("ImportTorrents() error = %v", err)
	}

	snap := snapshotTarget(t, tgt)
	p := snap.progress
	// Unresolved integrity run-scope errors (the orphan incident) force
	// import_complete_with_errors even with zero record errors.
	if p.Phase != state.PhaseCompleteWithErrors {
		t.Fatalf("phase = %s, want import_complete_with_errors (orphans are integrity errors)", p.Phase)
	}
	if p.RunErrorCount != 1 || snap.runErrors != 1 {
		t.Fatalf("run_error_count = %d / integrity rows = %d, want 1/1", p.RunErrorCount, snap.runErrors)
	}
	// Torrent 11 is a typical single file (no blob): exactly one blob.
	if p.SuccessCount != 2 || p.BlobCount != 1 || p.ErrorCount != 0 {
		t.Fatalf("progress = %+v", p)
	}

	// The incident carries aggregate counts and ID ranges, no paths.
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	pool, err := target.OpenPool(ctx, tgt)
	if err != nil {
		t.Fatalf("OpenPool() error = %v", err)
	}
	defer pool.Close()
	var details []byte
	if err := pool.QueryRow(ctx,
		`SELECT details FROM magnetico_migration.errors
         WHERE scope = 'run' AND code = $1`, CodeSourceOrphanFiles).Scan(&details); err != nil {
		t.Fatalf("read orphan incident: %v", err)
	}
	for _, want := range []string{`"orphan_files": 1`, `"null_owner_files": 1`, `"orphan_id_min": 99`} {
		if !bytes.Contains(details, []byte(want)) {
			t.Fatalf("incident details %s missing %s", details, want)
		}
	}
	if bytes.Contains(details, []byte("orphan.bin")) {
		t.Fatal("incident details must never contain file paths")
	}

	// Torrent 10's blob excludes the orphan rows: exactly 2 files.
	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT data FROM magneticod.torrent_file_blobs WHERE torrent_id = 10`).Scan(&raw); err != nil {
		t.Fatalf("read blob: %v", err)
	}
	files, err := codec.Decode(raw, codec.DefaultLimits())
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(files) != 2 || files[0].Path != "a" || files[1].Path != "b" {
		t.Fatalf("decoded files = %+v, want exactly a and b (orphans excluded)", files)
	}
}

// --- suite ⑦: index deferral and finalization -------------------------------

// secondaryIndexPresence reads which of the three droppable indexes exist.
func secondaryIndexPresence(t *testing.T, dsn string) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	pool := poolForRunner(t, dsn)
	defer pool.Close()
	rows, err := pool.Query(ctx, `SELECT i.relname FROM pg_index x
        JOIN pg_class i ON i.oid = x.indexrelid
        JOIN pg_class t ON t.oid = x.indrelid
        JOIN pg_namespace n ON n.oid = t.relnamespace
        WHERE n.nspname = 'magneticod'
          AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = i.oid)`)
	if err != nil {
		t.Fatalf("index inventory: %v", err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return present
}

// wantSecondaryIndexes asserts the exact presence set of the three droppable
// indexes.
func wantSecondaryIndexes(t *testing.T, dsn string, want map[string]bool) {
	t.Helper()
	got := secondaryIndexPresence(t, dsn)
	for _, name := range []string{
		"idx_torrents_total_size", "idx_torrents_discovered_on", "idx_torrents_name_gin_trgm",
	} {
		if got[name] != want[name] {
			t.Fatalf("index %s present = %v, want %v (inventory %v)", name, got[name], want[name], got)
		}
	}
}

// integrityConstraintsIntact counts PK/UNIQUE/FK constraints on the data
// tables: torrents PK + UNIQUE(info_hash), blobs PK + FK, schema_version PK.
func integrityConstraintsIntact(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	pool := poolForRunner(t, dsn)
	defer pool.Close()
	var n int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pg_constraint con
        JOIN pg_class c ON c.oid = con.conrelid
        JOIN pg_namespace ns ON ns.oid = con.connamespace
        WHERE ns.nspname = 'magneticod' AND con.contype IN ('p','u','f')`).Scan(&n); err != nil {
		t.Fatalf("constraint count: %v", err)
	}
	if n != 5 {
		t.Fatalf("integrity constraints = %d, want 5 (2×torrents PK+UNIQUE, 2×blobs PK+FK, schema_version PK)", n)
	}
}

func TestRunnerDeferredIndexesInterruptedResume(t *testing.T) {
	t.Parallel()

	src := runnerSource(t)
	tgt := createTargetForRunner(t)
	for id := int32(1); id <= 12; id++ {
		seedSourceTorrent(t, src, id, fmt.Sprintf("t%d", id), 2, 1700000000+int32(id))
		seedSourceFile(t, src, id, fmt.Sprintf("f%d.bin", id), 2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reports int
	err := runTorrents(t, ctx, TorrentsInput{
		SourceDSN: src, TargetDSN: tgt, DeferIndexes: true,
		Progress: func(Progress) {
			reports++
			if reports == 2 {
				cancel() // after the drop+mark and the first committed batch
			}
		},
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("interrupted deferred run error = %v, want ErrInterrupted", err)
	}

	// The authorized index-less state mid-import: drop applied, marker set,
	// the application's strict verification refuses it.
	p := snapshotTarget(t, tgt).progress
	if p.Phase != state.PhaseImporting || !p.IndexesDeferred || !p.DeferIndexes {
		t.Fatalf("interrupted deferred progress = %+v", p)
	}
	wantSecondaryIndexes(t, tgt, map[string]bool{
		"idx_torrents_total_size": false, "idx_torrents_discovered_on": false, "idx_torrents_name_gin_trgm": false,
	})
	integrityConstraintsIntact(t, tgt)
	ctxNC, cancelNC := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancelNC()
	if _, err := persistence.MakeDatabase(ctxNC, tgt); err == nil {
		t.Fatal("MakeDatabase accepted the index-less mid-import state")
	}

	// Resume rebuilds the indexes at finalization and completes.
	if err := runTorrents(t, context.Background(), TorrentsInput{
		SourceDSN: src, TargetDSN: tgt, DeferIndexes: true,
	}); err != nil {
		t.Fatalf("resume error = %v", err)
	}
	p = snapshotTarget(t, tgt).progress
	if p.Phase != state.PhaseImportComplete || p.SuccessCount != 12 {
		t.Fatalf("resumed progress = %+v", p)
	}
	if !p.IndexesDeferred {
		t.Fatal("indexes_deferred must stay true after the rebuild")
	}
	wantSecondaryIndexes(t, tgt, map[string]bool{
		"idx_torrents_total_size": true, "idx_torrents_discovered_on": true, "idx_torrents_name_gin_trgm": true,
	})
	integrityConstraintsIntact(t, tgt)
	// The rebuilt state is application-strict-valid again.
	if _, err := persistence.MakeDatabase(ctxNC, tgt); err != nil {
		t.Fatalf("MakeDatabase() after rebuild error = %v", err)
	}
}

func TestRunnerDeferredDropPerformedOnResume(t *testing.T) {
	t.Parallel()

	src := runnerSource(t)
	tgt := createTargetForRunner(t)
	for id := int32(1); id <= 6; id++ {
		seedSourceTorrent(t, src, id, fmt.Sprintf("t%d", id), 2, 1700000000+int32(id))
		seedSourceFile(t, src, id, fmt.Sprintf("f%d.bin", id), 2)
	}

	// Crash simulation: cancellation lands after preflight created the run
	// state but before the atomic drop (the first progress report fires
	// exactly in that window). The exact error taxonomy of this narrow
	// window varies; the resumable state is what matters: run state exists,
	// importing, drop not yet performed, indexes intact.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := runTorrents(t, ctx, TorrentsInput{
		SourceDSN: src, TargetDSN: tgt, DeferIndexes: true,
		Progress: func(Progress) {
			cancel() // the initial report fires between preflight and the drop
		},
	})
	if err == nil {
		t.Fatal("first run must fail on the cancelled context")
	}
	p := snapshotTarget(t, tgt).progress
	if p.Phase != state.PhaseImporting || p.IndexesDeferred {
		t.Fatalf("pre-drop crash progress = %+v, want importing with the drop pending", p)
	}
	wantSecondaryIndexes(t, tgt, map[string]bool{
		"idx_torrents_total_size": true, "idx_torrents_discovered_on": true, "idx_torrents_name_gin_trgm": true,
	})

	// Resume performs the drop (strict verification passed with the indexes
	// still present), then imports index-less and rebuilds at finalization.
	if err := runTorrents(t, context.Background(), TorrentsInput{
		SourceDSN: src, TargetDSN: tgt, DeferIndexes: true,
	}); err != nil {
		t.Fatalf("resume error = %v", err)
	}
	p = snapshotTarget(t, tgt).progress
	if p.Phase != state.PhaseImportComplete || p.SuccessCount != 6 || !p.IndexesDeferred {
		t.Fatalf("resumed progress = %+v", p)
	}
	wantSecondaryIndexes(t, tgt, map[string]bool{
		"idx_torrents_total_size": true, "idx_torrents_discovered_on": true, "idx_torrents_name_gin_trgm": true,
	})
}

// deadCommitter simulates transport loss BEFORE the commit: nothing lands,
// reconciliation classifies the failure as replayable, and a persistent
// failure exhausts the replays into a fatal (which records an operational
// incident).
type deadCommitter struct {
	inner     batchCommitter
	mu        sync.Mutex
	failsLeft int
}

func (d *deadCommitter) CommitBatch(ctx context.Context, b target.Batch) error {
	d.mu.Lock()
	fail := d.failsLeft > 0
	if fail {
		d.failsLeft--
	}
	d.mu.Unlock()
	if fail {
		return errors.New("connection reset before commit (simulated)")
	}
	return d.inner.CommitBatch(ctx, b)
}

func TestRunnerOperationalIncidentThenComplete(t *testing.T) {
	t.Parallel()

	src := runnerSource(t)
	tgt := createTargetForRunner(t)
	for id := int32(1); id <= 20; id++ {
		seedSourceTorrent(t, src, id, fmt.Sprintf("t%d", id), 1, 1700000000+int32(id))
		seedSourceFile(t, src, id, fmt.Sprintf("t%d.bin", id), 1)
	}

	// Run 1: every commit attempt dies before COMMIT; after the three
	// replays the run stops fatally and records an operational incident.
	pool := poolForRunner(t, tgt)
	dead := &deadCommitter{inner: target.NewBatchCommitter(pool), failsLeft: 99}
	err := runTorrents(t, context.Background(), TorrentsInput{
		SourceDSN: src, TargetDSN: tgt, committer: dead,
	})
	var fatal *FatalError
	if !errors.As(err, &fatal) {
		t.Fatalf("first run error = %v (%T), want FatalError", err, err)
	}

	// The incident is history: operational category, run_error_count 0,
	// nothing landed.
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	var category, code string
	var runErrors int64
	if err := pool.QueryRow(ctx, `SELECT category, code FROM magnetico_migration.errors
        WHERE scope = 'run' AND code = 'fatal_incident' LIMIT 1`).Scan(&category, &code); err != nil {
		t.Fatalf("read incident: %v", err)
	}
	if category != state.CategoryOperational {
		t.Fatalf("incident category = %s, want operational", category)
	}
	p := snapshotTarget(t, tgt).progress
	if p.RunErrorCount != 0 || p.SuccessCount != 0 || p.Phase != state.PhaseImporting {
		t.Fatalf("post-fatal progress = %+v", p)
	}

	// Run 2 recovers and completes as plain import_complete — the transient
	// incident must NOT force import_complete_with_errors — and the
	// incident row survives as history.
	if err := runTorrents(t, context.Background(), TorrentsInput{SourceDSN: src, TargetDSN: tgt}); err != nil {
		t.Fatalf("resume error = %v", err)
	}
	p = snapshotTarget(t, tgt).progress
	if p.Phase != state.PhaseImportComplete || p.SuccessCount != 20 || p.RunErrorCount != 0 {
		t.Fatalf("recovered progress = %+v, want plain import_complete", p)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM magnetico_migration.errors
        WHERE scope = 'run' AND code = 'fatal_incident' AND category = 'operational'`).Scan(&runErrors); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	if runErrors != 1 {
		t.Fatalf("operational incident rows = %d, want 1 (preserved history)", runErrors)
	}
}

// --- fixtures ----------------------------------------------------------------

// createTargetForRunner creates a fresh, schema-initialized target database.
func createTargetForRunner(t *testing.T) string {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	if err := persistence.InitSchema(ctx, dsn); err != nil {
		t.Fatalf("InitSchema() error = %v", err)
	}
	return dsn
}

func poolForRunner(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pool parse: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
