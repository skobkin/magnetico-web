package main

// Suite ⑧ end-to-end binary smoke: a disposable v0.12.0 source and a fresh
// 0.8.0 target, `magnetico migrate torrents` interrupted with SIGTERM
// mid-run (exit 2, checkpoint kept), then resumed to completion (exit 0,
// all torrents present). Skips with the harness when no test PostgreSQL is
// available.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
	"github.com/skobkin/magnetico-web/internal/pgtest"
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
