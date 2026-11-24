package main

// Safety checks of the W9 benchmark harness. The harness creates accounts,
// revokes credentials and starts services against MAGNETICO_W9_DSN, so before it
// mutates anything it must be sure the target is the disposable benchmark
// database and nothing else: the owner confirms the database by name, the host
// must be local, the database must hold the expected benchmark dataset, and the
// only accounts in it may be the harness's own generated identities.

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// w9ConfirmEnv must hold the name of the database in MAGNETICO_W9_DSN: an explicit,
// per-database statement that the target is disposable.
const (
	w9ConfirmEnv = "MAGNETICO_W9_CONFIRM_DISPOSABLE_DB"
	w9TokenLabel = "w9" // label of every credential the harness mints
)

// w9Marker is the database comment that identifies a W9 benchmark database. The
// seeder sets it before it writes the first row; nothing else does.
const w9Marker = "magnetico-w9-benchmark:w9-large-v1"

func w9MarkDatabase(ctx context.Context, conn *pgx.Conn) error {
	var db string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		return err
	}
	_, err := conn.Exec(ctx, `COMMENT ON DATABASE `+pgx.Identifier{db}.Sanitize()+` IS '`+w9Marker+`'`)
	return err
}

// w9UserNames are the exact usernames the harness creates, and the only ones it
// may touch.
func w9UserNames() []string {
	names := make([]string, w9LoadUsers)
	for i := range names {
		names[i] = fmt.Sprintf("w9u%02d", i)
	}
	return names
}

// w9CheckTarget validates the DSN without connecting: the database named by
// confirm, on a loopback or unix-socket host.
func w9CheckTarget(dsn, confirm string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("MAGNETICO_W9_DSN is not a valid DSN")
	}
	if confirm == "" {
		return fmt.Errorf("%s is not set: name the disposable benchmark database (%q) to confirm that mutating it is allowed", w9ConfirmEnv, cfg.Database)
	}
	if confirm != cfg.Database {
		return fmt.Errorf("%s=%q does not match the DSN's database %q", w9ConfirmEnv, confirm, cfg.Database)
	}
	// Every host pgx may try: the primary and each fallback (a DSN with several
	// hosts, or a host= list, would otherwise let the harness reach a remote server).
	hosts := []string{cfg.Host}
	for _, fb := range cfg.Fallbacks {
		hosts = append(hosts, fb.Host)
	}
	for _, h := range hosts {
		if !w9LocalHost(h) {
			return fmt.Errorf("every database host must be this host (loopback or unix socket), not %q", h)
		}
	}
	return nil
}

func w9LocalHost(host string) bool {
	if strings.HasPrefix(host, "/") || host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && ip.IsLoopback()
}

// w9CheckDataset reads (never writes) the database and refuses it unless it is a
// W9 benchmark database: the confirmed name is the connected database, it carries
// the dedicated benchmark marker (a database comment set by the seeder, so foreign
// torrents of any shape cannot pass for benchmark data), and every existing web
// account is a generated identity.
func w9CheckDataset(ctx context.Context, dsn, confirm string) error {
	if err := w9CheckTarget(dsn, confirm); err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.Background())
	var current string
	if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&current); err != nil {
		return err
	}
	if current != confirm {
		return fmt.Errorf("connected to database %q, confirmed %q", current, confirm)
	}
	var marker *string
	if err := conn.QueryRow(ctx, `SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = current_database()`).Scan(&marker); err != nil {
		return err
	}
	if marker == nil || *marker != w9Marker {
		return fmt.Errorf("the database does not carry the W9 benchmark marker %q: it was not created by the W9 seeder (see 07J-measurements/README.md to mark a seeded database)", w9Marker)
	}
	var reg *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('magneticod.torrents')::text`).Scan(&reg); err != nil {
		return err
	}
	if reg == nil {
		return fmt.Errorf("magneticod.torrents does not exist")
	}
	if err := conn.QueryRow(ctx, `SELECT to_regclass('magnetico_web.users')::text`).Scan(&reg); err != nil {
		return err
	}
	if reg != nil {
		var foreign int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM magnetico_web.users WHERE NOT (username = ANY($1))`, w9UserNames()).Scan(&foreign); err != nil {
			return err
		}
		if foreign != 0 {
			return fmt.Errorf("magnetico_web.users holds %d accounts that are not benchmark identities: this database is not disposable", foreign)
		}
	}
	return nil
}

// w9RequireDisposable fails the test unless the target passes w9CheckDataset.
func w9RequireDisposable(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := w9CheckDataset(ctx, dsn, os.Getenv(w9ConfirmEnv)); err != nil {
		t.Fatalf("refusing to touch MAGNETICO_W9_DSN: %v", err)
	}
}

func TestW9CheckTarget(t *testing.T) {
	for _, c := range []struct {
		name, dsn, confirm string
		ok                 bool
	}{
		{"confirmed loopback", "postgres://u:p@127.0.0.1:5432/w9bench", "w9bench", true},
		{"confirmed localhost", "postgres://u:p@localhost/w9bench", "w9bench", true},
		{"confirmed ipv6 loopback", "postgres://u:p@[::1]:5432/w9bench", "w9bench", true},
		{"confirmed unix socket", "postgres://u:p@/w9bench?host=/var/run/postgresql", "w9bench", true},
		{"no confirmation", "postgres://u:p@127.0.0.1/w9bench", "", false},
		{"wrong database", "postgres://u:p@127.0.0.1/magnetico", "w9bench", false},
		{"remote host", "postgres://u:p@db.internal.example/w9bench", "w9bench", false},
		{"remote ip", "postgres://u:p@10.0.0.5/w9bench", "w9bench", false},
		{"remote fallback host", "postgres://u:p@127.0.0.1:5432,db.internal.example:5432/w9bench", "w9bench", false},
		{"remote first, local fallback", "postgres://u:p@db.internal.example,127.0.0.1/w9bench", "w9bench", false},
		{"two local hosts", "postgres://u:p@127.0.0.1:5432,localhost:5433/w9bench", "w9bench", true},
		{"garbage", "not a dsn %%", "x", false},
	} {
		err := w9CheckTarget(c.dsn, c.confirm)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
		if err != nil && (strings.Contains(err.Error(), "u:p") || strings.Contains(err.Error(), ":p@")) {
			t.Errorf("%s: the error leaks the credentials: %v", c.name, err)
		}
	}
}

func TestW9CheckDatasetRefusesAnythingButTheBenchmarkDatabase(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	cfg, _ := pgx.ParseConfig(dsn)
	name := cfg.Database
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(why string) {
		t.Helper()
		if err := w9CheckDataset(ctx, dsn, name); err == nil {
			t.Fatalf("accepted: %s", why)
		}
	}

	if err := w9CheckDataset(ctx, dsn, "other"); err == nil {
		t.Fatal("accepted a mismatching confirmation")
	}
	refused("an empty, unmarked database")

	// Foreign data of any shape, however benchmark-like its names: no marker, no go.
	exec(`CREATE SCHEMA magneticod`)
	exec(`CREATE TABLE magneticod.torrents (id bigserial PRIMARY KEY, name text, file_count int)`)
	exec(`INSERT INTO magneticod.torrents (name, file_count) SELECT '1080p x264 multi small ' || i, 12 FROM generate_series(1, 50) i`)
	exec(`INSERT INTO magneticod.torrents (name, file_count) VALUES ('a real single-file torrent', 1)`)
	refused("benchmark-looking names without the marker")

	exec(`COMMENT ON DATABASE ` + pgx.Identifier{name}.Sanitize() + ` IS 'some other comment'`)
	refused("a different database comment")

	if err := w9MarkDatabase(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err := w9CheckDataset(ctx, dsn, name); err != nil {
		t.Fatalf("refused the marked database: %v", err)
	}

	exec(`CREATE SCHEMA magnetico_web`)
	exec(`CREATE TABLE magnetico_web.users (id bigserial PRIMARY KEY, username text)`)
	exec(`INSERT INTO magnetico_web.users (username) VALUES ('w9u00'), ('w9u07')`)
	if err := w9CheckDataset(ctx, dsn, name); err != nil {
		t.Fatalf("refused a database that holds only generated identities: %v", err)
	}
	exec(`INSERT INTO magnetico_web.users (username) VALUES ('w9user-real')`) // matches the old LIKE 'w9u%' pattern
	refused("a real account whose name merely starts with w9u")
}

// TestW9MarkBenchmarkDatabase marks an already seeded database (one seeded before
// the marker existed). Gated and explicit: MAGNETICO_W9_MARK=1 plus the usual
// confirmation. It refuses a database that holds accounts other than the generated
// ones, and one without the seeder's multi-file torrents.
func TestW9MarkBenchmarkDatabase(t *testing.T) {
	if os.Getenv("MAGNETICO_W9_MARK") != "1" {
		t.Skip("set MAGNETICO_W9_MARK=1 (with MAGNETICO_W9_DSN and the confirmation) to mark a seeded benchmark database")
	}
	dsn := os.Getenv("MAGNETICO_W9_DSN")
	if err := w9CheckTarget(dsn, os.Getenv(w9ConfirmEnv)); err != nil {
		t.Fatalf("refusing to mark: %v", err)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var multi, foreign, users int
	if err := conn.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE name NOT LIKE '% multi %' AND name NOT LIKE '% w9ing %') FROM magneticod.torrents WHERE file_count > 1`).Scan(&multi, &foreign); err != nil {
		t.Fatal(err)
	}
	if multi == 0 || foreign != 0 {
		t.Fatalf("not a W9 seeded database: %d multi-file rows, %d not seeder-named", multi, foreign)
	}
	if err := conn.QueryRow(ctx, `SELECT CASE WHEN to_regclass('magnetico_web.users') IS NULL THEN 0 ELSE (SELECT count(*) FROM magnetico_web.users WHERE NOT (username = ANY($1))) END`, w9UserNames()).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Fatalf("%d accounts are not generated identities", users)
	}
	if err := w9MarkDatabase(ctx, conn); err != nil {
		t.Fatal(err)
	}
	t.Logf("marked with %q", w9Marker)
}
