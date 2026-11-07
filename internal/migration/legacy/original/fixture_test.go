package original

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// Fixture DDL reproducing the frozen boramalper/magnetico v0.12.0 source
// layout (pkg/persistence/postgres.go, setupDatabase), schema-qualified so
// it applies to a fresh harness database as-is.
const legacySourceDDL = `
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

// createLegacySource applies the frozen v0.12.0 DDL to a fresh harness
// database and returns its DSN.
func createLegacySource(t *testing.T) string {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	execSQL(t, dsn, legacySourceDDL)
	return dsn
}

// execSQL runs one administrative statement as the fixture database's owner
// (fixture setup only; the adapter itself stays SELECT-only).
func execSQL(t *testing.T, dsn, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("fixture connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("fixture exec %q: %v", firstLine(sql), err)
	}
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

func seedTorrent(t *testing.T, dsn string, id int32, infoHashHex, name string, totalSize int64, discoveredOn int32) {
	t.Helper()
	execSQL(t, dsn, fmt.Sprintf(
		`INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
         VALUES (%d, decode('%s','hex'), %s, %d, %d)`,
		id, infoHashHex, pgtest.QuoteLiteral(name), totalSize, discoveredOn))
}

func seedFile(t *testing.T, dsn string, id int32, torrentID *int32, path string, size int64) {
	t.Helper()
	owner := "NULL"
	if torrentID != nil {
		owner = fmt.Sprintf("%d", *torrentID)
	}
	execSQL(t, dsn, fmt.Sprintf(
		`INSERT INTO magneticod.files (id, torrent_id, size, path)
         VALUES (%d, %s, %d, %s)`,
		id, owner, size, pgtest.QuoteLiteral(path)))
}

// openSource opens a verified source or fails the test.
func openSource(t *testing.T, dsn string) *Source {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	src, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(src.Close)
	return src
}

// catalogSnapshot renders a deterministic summary of the source catalog and
// data, used to prove probes and refusals leave the source untouched.
func catalogSnapshot(t *testing.T, dsn string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("snapshot connect: %v", err)
	}
	defer conn.Close(context.Background())

	var out *string
	const catalogQuery = `SELECT string_agg(line, E'\n' ORDER BY line) FROM (
        SELECT 'rel ' || c.relname || ' ' || c.relkind::text AS line
          FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
          WHERE n.nspname = 'magneticod' AND c.relkind IN ('r','p','v','m','f','S')
        UNION ALL
        SELECT 'col ' || table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable
          FROM information_schema.columns WHERE table_schema = 'magneticod'
        UNION ALL
        SELECT 'con ' || conname || ' ' || contype::text FROM pg_constraint con
          JOIN pg_namespace n ON n.oid = con.connamespace WHERE n.nspname = 'magneticod'
        UNION ALL
        SELECT 'idx ' || i.relname || ' valid=' || ix.indisvalid::text FROM pg_index ix
          JOIN pg_class i ON i.oid = ix.indexrelid
          JOIN pg_class tt ON tt.oid = ix.indrelid
          JOIN pg_namespace n ON n.oid = tt.relnamespace
          WHERE n.nspname = 'magneticod'
    ) q`
	if err := conn.QueryRow(ctx, catalogQuery).Scan(&out); err != nil {
		t.Fatalf("catalog snapshot: %v", err)
	}
	if out == nil {
		return ""
	}
	lines := *out

	// Data lines are only available in an unbroken schema; a broken one
	// yields a stable marker instead, so before/after comparisons stay fair.
	const dataQuery = `SELECT string_agg(line, E'\n' ORDER BY line) FROM (
        SELECT 'row torrents ' || id || ' ' || encode(info_hash,'hex') || ' ' || name || ' ' || total_size || ' ' || discovered_on
          FROM magneticod.torrents
        UNION ALL
        SELECT 'row files ' || id || ' ' || COALESCE(torrent_id::text,'NULL') || ' ' || size || ' ' || path
          FROM magneticod.files
        UNION ALL
        SELECT 'mig ' || schema_version::text FROM magneticod.migrations
    ) q`
	var data *string
	if err := conn.QueryRow(ctx, dataQuery).Scan(&data); err != nil || data == nil {
		lines += "\ndata (unreadable)"
		return lines
	}
	return lines + "\n" + *data
}

// openFailsWith asserts Open refuses with the expected sentinel and leaves
// the source catalog untouched, returning the error text for detail
// assertions.
func openFailsWith(t *testing.T, dsn string, want error, before string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	src, err := Open(ctx, dsn)
	if err == nil {
		src.Close()
		t.Fatalf("Open() unexpectedly succeeded; want %v", want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("Open() error = %v, want %v", err, want)
	}
	if after := catalogSnapshot(t, dsn); after != before {
		t.Fatalf("refused source changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	return err.Error()
}

// unixSocketDSN returns a DSN to the same cluster via its Unix socket, or
// skips when the cluster has no socket directory (Docker/external servers
// expose TCP only).
func unixSocketDSN(t *testing.T, tcpDSN string) string {
	t.Helper()
	sock := pgtest.IntegrationCluster(t).SocketDir
	if sock == "" {
		t.Skip("cluster has no Unix socket directory (Docker/external server)")
	}
	parsed, err := url.Parse(tcpDSN)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	replacement := *parsed
	replacement.Host = ""
	replacement.RawQuery = "host=" + url.QueryEscape(sock)
	if replacement.RawPath == "" && parsed.Path != "" {
		replacement.Path = parsed.Path
	}
	return replacement.String()
}
