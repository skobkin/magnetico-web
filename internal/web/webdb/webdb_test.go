package webdb

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func exec(t *testing.T, dsn, sql string) {
	t.Helper()
	conn, err := pgx.Connect(ctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx(t), sql); err != nil {
		t.Fatal(err)
	}
}

func TestOpenUnreachableFailsWithoutEchoingTheURL(t *testing.T) {
	_, err := Open(ctx(t), "postgres://user:s3cret@127.0.0.1:1/none?sslmode=disable&connect_timeout=2", 4)
	if !errors.Is(err, persistence.ErrStorageIO) || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Open(ctx(t), "://bad", 4); !errors.Is(err, persistence.ErrStorageIO) {
		t.Fatalf("bad URL: %v", err)
	}
}

func TestVerifyFailsClosed(t *testing.T) {
	cl := pgtest.IntegrationCluster(t)

	t.Run("fresh database: torrent schema absent", func(t *testing.T) {
		dsn := cl.TestDatabase(t)
		b, err := Open(ctx(t), dsn, 4)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		if err := b.Verify(ctx(t)); err == nil || errors.Is(err, ErrWebSchemaMissing) {
			t.Fatalf("a fresh database must fail on the torrent schema first: %v", err)
		}
	})

	t.Run("torrent schema ok, magnetico_web absent", func(t *testing.T) {
		dsn := cl.TestDatabase(t)
		if err := persistence.InitSchema(ctx(t), dsn); err != nil {
			t.Fatal(err)
		}
		b, err := Open(ctx(t), dsn, 4)
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		if err := b.Verify(ctx(t)); !errors.Is(err, ErrWebSchemaMissing) {
			t.Fatalf("err = %v, want ErrWebSchemaMissing", err)
		}
		if err := b.Ping(ctx(t)); err != nil {
			t.Fatalf("ping: %v", err)
		}
	})

	t.Run("web schema present without version table", func(t *testing.T) {
		dsn := cl.TestDatabase(t)
		if err := persistence.InitSchema(ctx(t), dsn); err != nil {
			t.Fatal(err)
		}
		exec(t, dsn, `CREATE SCHEMA magnetico_web`)
		b, _ := Open(ctx(t), dsn, 4)
		defer b.Close()
		if err := b.Verify(ctx(t)); !errors.Is(err, ErrWebSchemaMissing) {
			t.Fatalf("err = %v", err)
		}
	})

	// A test-local stand-in for the W2 schema, created only inside a
	// disposable database, proves the success path of the verifier. It is
	// not the production DDL and is never created by production code.
	t.Run("both schemas present", func(t *testing.T) {
		dsn := cl.TestDatabase(t)
		if err := persistence.InitSchema(ctx(t), dsn); err != nil {
			t.Fatal(err)
		}
		exec(t, dsn, `CREATE SCHEMA magnetico_web; CREATE TABLE magnetico_web.schema_version (version int)`)
		b, _ := Open(ctx(t), dsn, 4)
		defer b.Close()
		if err := b.Verify(ctx(t)); err != nil {
			t.Fatalf("verify: %v", err)
		}
	})

	t.Run("incompatible torrent schema is refused", func(t *testing.T) {
		dsn := cl.TestDatabase(t)
		if err := persistence.InitSchema(ctx(t), dsn); err != nil {
			t.Fatal(err)
		}
		exec(t, dsn, `CREATE SCHEMA magnetico_web; CREATE TABLE magnetico_web.schema_version (version int);
			UPDATE magneticod.schema_version SET version = version + 100`)
		b, _ := Open(ctx(t), dsn, 4)
		defer b.Close()
		err := b.Verify(ctx(t))
		if err == nil || errors.Is(err, ErrWebSchemaMissing) {
			t.Fatalf("a wrong torrent schema version must be refused: %v", err)
		}
	})
}

func TestPoolIsBounded(t *testing.T) {
	cl := pgtest.IntegrationCluster(t)
	b, err := Open(ctx(t), cl.TestDatabase(t), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := b.pool.Config().MaxConns; got != 4 {
		t.Fatalf("MaxConns = %d", got)
	}
}
