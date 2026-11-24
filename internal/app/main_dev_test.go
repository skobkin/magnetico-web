package app

// Real-binary test of `magnetico dev seed-torrents`: disposable PostgreSQL
// only, synthetic data only, no network beyond the loopback database.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/devseed"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func datasetFiles() int {
	n := 0
	for _, f := range devseed.Dataset() {
		n += len(f.Files)
	}
	return n
}

func runDevSeed(t *testing.T, bin, dsn string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"dev", "seed-torrents"}, args...)...)
	cmd.Env = webEnv("MAGNETICO_DEV_DATABASE=" + dsn)
	var buf bytes.Buffer
	cmd.Stderr, cmd.Stdout = &buf, &buf
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return buf.String(), 0
	case errors.As(err, &ee):
		return buf.String(), ee.ExitCode()
	}
	t.Fatalf("run: %v", err)
	return "", -1
}

func devQueryInt(t *testing.T, dsn, sql string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var n int
	if err := c.QueryRow(ctx, sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func torrentRows(t *testing.T, dsn string) int {
	t.Helper()
	return devQueryInt(t, dsn, `SELECT count(*) FROM magneticod.torrents`)
}

func TestDevSeedTorrentsBinary(t *testing.T) {
	bin := buildBinary(t)
	dsn := webTarget(t)
	secretDSN := withPassword(t, dsn, "s3cr3t-dev-password")

	// Without the confirmation flag nothing is written.
	out, code := runDevSeed(t, bin, secretDSN)
	if code == 0 || !strings.Contains(out, "--confirm-development-database") {
		t.Fatalf("unconfirmed run: code=%d out=%s", code, out)
	}
	if n := torrentRows(t, dsn); n != 0 {
		t.Fatalf("unconfirmed run wrote %d torrents", n)
	}

	// A dry run reports and writes nothing.
	out, code = runDevSeed(t, bin, secretDSN, "--dry-run")
	if code != 0 || !strings.Contains(out, "would add 101 torrents") || !strings.Contains(out, "database was empty") {
		t.Fatalf("dry run: code=%d out=%s", code, out)
	}
	if n := torrentRows(t, dsn); n != 0 {
		t.Fatalf("dry run wrote %d torrents", n)
	}

	// The real run.
	out, code = runDevSeed(t, bin, secretDSN, "--confirm-development-database")
	if code != 0 || !strings.Contains(out, "added 101 torrents") ||
		!strings.Contains(out, fmt.Sprintf("%d torrents, %d files", devseed.TotalTorrents, datasetFiles())) {
		t.Fatalf("seed: code=%d out=%s", code, out)
	}
	if n := torrentRows(t, dsn); n != 101 {
		t.Fatalf("torrents = %d, want 101", n)
	}

	// Rerun: defined no-op.
	out, code = runDevSeed(t, bin, secretDSN, "--confirm-development-database")
	if code != 0 || !strings.Contains(out, "database was seeded") || !strings.Contains(out, "added 0 torrents") {
		t.Fatalf("rerun: code=%d out=%s", code, out)
	}

	// A foreign torrent makes every later run refuse.
	store, err := persistence.MakeDatabase(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddTorrent(context.Background(),
		persistence.Torrent{InfoHash: []byte("0123456789abcdef0123"), Name: "real", TotalSize: 1, DiscoveredOn: 1700000000},
		[]persistence.File{{Path: "real", Size: 1}}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	out, code = runDevSeed(t, bin, secretDSN, "--confirm-development-database")
	if code == 0 || !strings.Contains(out, "not part of the development fixture") {
		t.Fatalf("foreign run: code=%d out=%s", code, out)
	}
	if n := torrentRows(t, dsn); n != 102 {
		t.Fatalf("torrents = %d, want 102 (untouched)", n)
	}

	// No output ever carries the credential.
	if strings.Contains(out, "s3cr3t-dev-password") {
		t.Fatalf("output leaks the password:\n%s", out)
	}
}

func TestDevSeedTorrentsBinaryRefusesAnUninitializedDatabase(t *testing.T) {
	bin := buildBinary(t)
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	secretDSN := withPassword(t, dsn, "s3cr3t-dev-password")
	out, code := runDevSeed(t, bin, secretDSN, "--confirm-development-database")
	if code == 0 || strings.Contains(out, "s3cr3t-dev-password") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if devQueryInt(t, dsn, `SELECT count(*) FROM pg_namespace WHERE nspname = 'magneticod'`) != 0 {
		t.Fatal("the refused run created the schema")
	}
}
