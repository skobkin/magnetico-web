package pgqueue

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

// catalogDigest renders everything about magnetico_ingest that a repair or an
// upgrade could change: relations, columns, indexes, triggers and the rows of
// the accounting tables.
func catalogDigest(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var sb strings.Builder
	for _, q := range []string{
		`SELECT table_name||'.'||column_name||':'||data_type FROM information_schema.columns
		  WHERE table_schema='magnetico_ingest' ORDER BY 1`,
		`SELECT indexdef FROM pg_indexes WHERE schemaname='magnetico_ingest' ORDER BY 1`,
		`SELECT tgname FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid
		  JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='magnetico_ingest' AND NOT tgisinternal ORDER BY 1`,
		`SELECT 'queue='||count(*) FROM magnetico_ingest.queue`,
		`SELECT 'quarantine='||count(*) FROM magnetico_ingest.quarantine`,
		`SELECT 'used='||used_bytes FROM magnetico_ingest.usage`,
		`SELECT 'version='||version FROM magnetico_ingest.ingest_version`,
	} {
		rows, err := conn.Query(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			sb.WriteString(s + "\n")
		}
		rows.Close()
	}
	return sb.String()
}

func TestInitCreatesTheCrawlersTableInSchemaVersionOne(t *testing.T) {
	e := newEnv(t)
	d := catalogDigest(t, e.dsn)
	for _, want := range []string{
		"crawlers.crawler_id:uuid", "crawlers.name:text", "crawlers.token_hash:bytea",
		"crawlers.enabled:boolean", "crawlers.created_at:timestamp with time zone",
		"crawlers.token_rotated_at:timestamp with time zone", "crawlers.disabled_at:timestamp with time zone",
		"version=1\n",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("catalog lacks %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "version=2") || SchemaVersion != 1 {
		t.Fatalf("the schema version must stay 1 (const %d)", SchemaVersion)
	}
	if !strings.Contains(d, "lower(name)") {
		t.Errorf("no case-insensitive unique name index:\n%s", d)
	}
	q, err := Open(context.Background(), e.dsn, Options{})
	if err != nil {
		t.Fatalf("Open on a fresh schema: %v", err)
	}
	q.Close()
}

func TestInitIsIdempotentAndRefusesUnknownVersions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	before := catalogDigest(t, e.dsn)
	for i := 0; i < 2; i++ {
		if err := InitSchema(ctx, e.dsn); err != nil {
			t.Fatalf("InitSchema again: %v", err)
		}
	}
	if after := catalogDigest(t, e.dsn); after != before {
		t.Fatalf("an idempotent init changed the schema:\n%s\n---\n%s", before, after)
	}
	conn, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `UPDATE magnetico_ingest.ingest_version SET version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := InitSchema(ctx, e.dsn); !errors.Is(err, ErrSchemaMissing) {
		t.Fatalf("InitSchema over version 2: %v, want ErrSchemaMissing", err)
	}
}

// legacyDevelopmentSchema drops the crawlers table to imitate an unreleased
// development schema created before the registry existed.
func legacyDevelopmentSchema(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `DROP TABLE magnetico_ingest.crawlers`); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRefusesAnOlderDevelopmentSchemaWithoutCrawlersAndNamesTheReinitSteps(t *testing.T) {
	e := newEnv(t)
	legacyDevelopmentSchema(t, e.dsn)
	_, err := Open(context.Background(), e.dsn, Options{})
	if !errors.Is(err, ErrSchemaInvalid) {
		t.Fatalf("Open: %v, want ErrSchemaInvalid", err)
	}
	for _, want := range []string{"crawlers", "DROP SCHEMA magnetico_ingest CASCADE", "ingest-schema-init", "UPGRADE.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestStartupNeverRepairsOrUpgradesAnIncompatibleSchema(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	if _, err := q.Admit(context.Background(), []Item{payloadFor(t, 1, 100)}); err != nil {
		t.Fatal(err)
	}
	legacyDevelopmentSchema(t, e.dsn)
	before := catalogDigest(t, e.dsn)
	if _, err := Open(context.Background(), e.dsn, Options{}); !errors.Is(err, ErrSchemaInvalid) {
		t.Fatalf("Open: %v", err)
	}
	if err := InitSchema(context.Background(), e.dsn); !errors.Is(err, ErrSchemaInvalid) {
		t.Fatalf("InitSchema over an incompatible schema: %v, want ErrSchemaInvalid", err)
	}
	if after := catalogDigest(t, e.dsn); after != before {
		t.Fatalf("a refused open or init changed the schema:\n%s\n---\n%s", before, after)
	}
}

func TestVerifyDetectsMissingCrawlersColumnIndexOrForeignTrigger(t *testing.T) {
	cases := []struct {
		name, sql, detail string
	}{
		{"column", `ALTER TABLE magnetico_ingest.crawlers DROP COLUMN disabled_at`, "disabled_at"},
		{"extra column", `ALTER TABLE magnetico_ingest.crawlers ADD COLUMN note text`, "note"},
		{"hash index", `ALTER TABLE magnetico_ingest.crawlers DROP CONSTRAINT crawlers_token_hash_key`, "token_hash"},
		{"name index", `DROP INDEX magnetico_ingest.crawlers_name_lower`, "lower(name)"},
		{"trigger", `CREATE FUNCTION magnetico_ingest.noop() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
			CREATE TRIGGER foreign_trg BEFORE INSERT ON magnetico_ingest.crawlers FOR EACH ROW EXECUTE FUNCTION magnetico_ingest.noop()`, "trigger"},
		{"hash length check", `ALTER TABLE magnetico_ingest.crawlers DROP CONSTRAINT crawlers_token_hash_len`, "token_hash"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			conn, err := pgx.Connect(ctx, e.dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			if _, err := conn.Exec(ctx, c.sql); err != nil {
				t.Fatal(err)
			}
			_, err = Open(ctx, e.dsn, Options{})
			if !errors.Is(err, ErrSchemaInvalid) {
				t.Fatalf("Open: %v, want ErrSchemaInvalid", err)
			}
			if !strings.Contains(err.Error(), c.detail) {
				t.Errorf("error %q does not name %q", err, c.detail)
			}
		})
	}
}

// TestReinitializingTheIngestSchemaLeavesMagneticodAndWebUntouched runs the
// documented reinitialisation statements and compares the other schemas.
func TestReinitializingTheIngestSchemaLeavesMagneticodAndWebUntouched(t *testing.T) {
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	ctx := context.Background()
	if err := persistence.InitSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	if err := InitSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// A web schema with a user, standing in for magnetico_web.
	for _, s := range []string{
		`CREATE SCHEMA magnetico_web`,
		`CREATE TABLE magnetico_web.users (id bigserial primary key, username text)`,
		`INSERT INTO magnetico_web.users (username) VALUES ('alice'), ('bob')`,
		`INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
		   VALUES ('\x0102030405060708090a0b0c0d0e0f1011121314', 't1', 5, 1, 1)`,
	} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	digest := func() string {
		var sb strings.Builder
		for _, q := range []string{
			`SELECT md5(string_agg(t::text, ',' ORDER BY info_hash)) FROM magneticod.torrents t`,
			`SELECT md5(string_agg(u::text, ',' ORDER BY id)) FROM magnetico_web.users u`,
			`SELECT count(*)::text FROM magneticod.torrent_file_blobs`,
		} {
			var s *string
			if err := conn.QueryRow(ctx, q).Scan(&s); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			if s != nil {
				sb.WriteString(*s)
			}
		}
		return sb.String()
	}
	before := digest()
	// The documented steps (UPGRADE.md): drop only magnetico_ingest, re-init.
	if _, err := conn.Exec(ctx, `DROP SCHEMA magnetico_ingest CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := InitSchema(ctx, dsn); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if after := digest(); after != before {
		t.Fatalf("reinitialisation changed other schemas: %q vs %q", before, after)
	}
	q, err := Open(ctx, dsn, Options{})
	if err != nil {
		t.Fatalf("Open after reinit: %v", err)
	}
	q.Close()
}
