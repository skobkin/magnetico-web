package persistence

// Integration suites for InitSchema and the startup verification. Every test
// runs against its own database from the disposable cluster (harness_test.go)
// and skips cleanly when no cluster is available.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// catalogSnapshot renders the magnetico-relevant catalog state as
// deterministic text, to prove refusals leave a database byte-identical.
func catalogSnapshot(t *testing.T, dsn string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for snapshot: %v", err)
	}
	defer conn.Close(context.Background())

	rows, err := conn.Query(ctx, `
        SELECT kind, name, extra FROM (
            SELECT 'schema' AS kind, nspname AS name, '' AS extra
                FROM pg_namespace
                WHERE nspname <> 'information_schema' AND nspname NOT LIKE 'pg\_%' ESCAPE '\'
            UNION ALL
            SELECT 'table', n.nspname || '.' || c.relname, c.relkind::text
                FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
                WHERE c.relkind IN ('r','p','v','m','f')
                  AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%' ESCAPE '\'
            UNION ALL
            SELECT 'index', schemaname || '.' || indexname, indexdef
                FROM pg_indexes
                WHERE schemaname <> 'information_schema' AND schemaname NOT LIKE 'pg\_%'
            UNION ALL
            SELECT 'extension', extname, extversion FROM pg_extension
        ) s
        ORDER BY 1, 2, 3`)
	if err != nil {
		t.Fatalf("snapshot query: %v", err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var kind, name, extra string
		if err := rows.Scan(&kind, &name, &extra); err != nil {
			t.Fatalf("snapshot scan: %v", err)
		}
		lines = append(lines, kind+"|"+name+"|"+extra)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	return strings.Join(lines, "\n")
}

// createLegacyLayout installs a legacy magnetico shape (boramalper plain or
// the fork's partitioned files) in the given schema.
func createLegacyLayout(t *testing.T, dsn, schema string, partitioned bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())

	stmts := []string{
		fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, quoteIdent(schema)),
		fmt.Sprintf(`CREATE TABLE %s.torrents (
            id INTEGER PRIMARY KEY,
            info_hash BYTEA NOT NULL UNIQUE,
            name TEXT NOT NULL,
            total_size BIGINT NOT NULL,
            discovered_on INTEGER NOT NULL)`, quoteIdent(schema)),
	}
	filesDDL := fmt.Sprintf(`CREATE TABLE %s.files (
            id INTEGER PRIMARY KEY,
            torrent_id INTEGER,
            size BIGINT NOT NULL,
            path TEXT NOT NULL)`, quoteIdent(schema))
	if partitioned {
		filesDDL = fmt.Sprintf(`CREATE TABLE %s.files (
            id INTEGER NOT NULL,
            torrent_id INTEGER,
            size BIGINT NOT NULL,
            path TEXT NOT NULL) PARTITION BY HASH (id)`, quoteIdent(schema))
	}
	stmts = append(stmts, filesDDL)
	for _, stmt := range stmts {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("legacy layout: %v", err)
		}
	}
}

// trgmInstalled reports whether pg_trgm is currently installed.
func trgmInstalled(t *testing.T, dsn string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	var installed bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm')`).Scan(&installed); err != nil {
		t.Fatalf("extension check: %v", err)
	}
	return installed
}

func TestInitSchemaFreshAndIdempotent(t *testing.T) {
	t.Parallel()

	dsn := integrationCluster(t).testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := InitSchema(ctx, dsn); err != nil {
		t.Fatalf("InitSchema() error = %v", err)
	}
	// Idempotent re-run.
	if err := InitSchema(ctx, dsn); err != nil {
		t.Fatalf("second InitSchema() error = %v", err)
	}

	conn := openConn(t, dsn)
	defer conn.Close(context.Background())

	// Exact version state: exactly one row, carrying SchemaVersion.
	var count int
	var version int64
	if err := conn.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(MAX(version), 0) FROM magneticod.schema_version`).Scan(&count, &version); err != nil {
		t.Fatalf("version query: %v", err)
	}
	if count != 1 || version != SchemaVersion {
		t.Errorf("schema_version rows = %d, max = %d; want exactly one row with version %d",
			count, version, SchemaVersion)
	}
	for _, table := range []string{"torrents", "torrent_file_blobs"} {
		var exists bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='magneticod' AND table_name=$1)`,
			table).Scan(&exists); err != nil || !exists {
			t.Errorf("table magneticod.%s missing (err %v)", table, err)
		}
	}
}

func openConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return conn
}

func TestInitSchemaConcurrentInvocations(t *testing.T) {
	t.Parallel()

	dsn := integrationCluster(t).testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = InitSchema(ctx, dsn)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent InitSchema() %d error = %v", i, err)
		}
	}

	conn := openConn(t, dsn)
	defer conn.Close(context.Background())
	var count int
	if err := conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM magneticod.schema_version`).Scan(&count); err != nil {
		t.Fatalf("version query: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_version rows = %d after %d concurrent inits, want exactly 1", count, n)
	}
}

func TestInitSchemaRefusesLegacyLayouts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		build       func(t *testing.T, dsn string)
		schema      string
		partitioned bool
	}{
		{
			name:   "boramalper plain layout in public",
			schema: "public",
			build: func(t *testing.T, dsn string) {
				createLegacyLayout(t, dsn, "public", false)
			},
		},
		{
			name:        "partitioned tgragnato layout in public",
			schema:      "public",
			partitioned: true,
			build: func(t *testing.T, dsn string) {
				createLegacyLayout(t, dsn, "public", true)
			},
		},
		{
			name:   "boramalper plain layout in magneticod",
			schema: "magneticod",
			build: func(t *testing.T, dsn string) {
				createLegacyLayout(t, dsn, "magneticod", false)
			},
		},
		{
			name:        "partitioned tgragnato layout in magneticod",
			schema:      "magneticod",
			partitioned: true,
			build: func(t *testing.T, dsn string) {
				createLegacyLayout(t, dsn, "magneticod", true)
			},
		},
		{
			name:   "new-shape torrents without file_count",
			schema: "magneticod",
			build: func(t *testing.T, dsn string) {
				conn := openConn(t, dsn)
				defer conn.Close(context.Background())
				ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
				defer cancel()
				if _, err := conn.Exec(ctx, `CREATE SCHEMA magneticod`); err != nil {
					t.Fatal(err)
				}
				if _, err := conn.Exec(ctx, `CREATE TABLE magneticod.torrents (
                    id INTEGER PRIMARY KEY, info_hash BYTEA NOT NULL UNIQUE, name TEXT NOT NULL,
                    total_size BIGINT NOT NULL, discovered_on INTEGER NOT NULL)`); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dsn := integrationCluster(t).testDatabase(t)
			tt.build(t, dsn)
			before := catalogSnapshot(t, dsn)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := InitSchema(ctx, dsn)

			var stateErr *SchemaStateError
			if !errors.As(err, &stateErr) {
				t.Fatalf("InitSchema() error = %v, want a SchemaStateError", err)
			}
			if !errors.Is(err, ErrLegacySchema) {
				t.Errorf("InitSchema() error = %v, want ErrLegacySchema", err)
			}
			if len(stateErr.Details) == 0 {
				t.Error("refusal must name the found objects")
			}

			after := catalogSnapshot(t, dsn)
			if after != before {
				t.Errorf("refusal changed the catalog:\nbefore:\n%s\nafter:\n%s", before, after)
			}

			// The refusal happens before any extension attempt, so a legacy
			// database stays untouched even when pg_trgm is absent.
			if trgmInstalled(t, dsn) {
				t.Error("pg_trgm was installed during a refusal; the database must stay untouched")
			}
		})
	}
}

func TestInitSchemaRefusesPartialState(t *testing.T) {
	t.Parallel()

	dsn := integrationCluster(t).testDatabase(t)

	conn := openConn(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()
	if _, err := conn.Exec(ctx, `CREATE SCHEMA magneticod`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE magneticod.torrents (
        id INTEGER GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
        info_hash BYTEA NOT NULL UNIQUE, name TEXT NOT NULL,
        total_size BIGINT NOT NULL, discovered_on BIGINT NOT NULL,
        file_count INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	conn.Close(context.Background())

	before := catalogSnapshot(t, dsn)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	err := InitSchema(ctx2, dsn)
	if !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("InitSchema() error = %v, want ErrUnsupportedSchema", err)
	}
	if got := catalogSnapshot(t, dsn); got != before {
		t.Error("partial-state refusal changed the catalog")
	}
}

// An unknown table inside magneticod makes the schema state unverifiable:
// both InitSchema and the startup verification refuse it, and the refusal
// leaves the database byte-identical.
func TestUnknownTableInsideMagneticodRefused(t *testing.T) {
	t.Parallel()

	dsn := integrationCluster(t).testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := InitSchema(ctx, dsn); err != nil {
		t.Fatalf("InitSchema() error = %v", err)
	}

	conn := openConn(t, dsn)
	execCtx, execCancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer execCancel()
	if _, err := conn.Exec(execCtx,
		`CREATE TABLE magneticod.operator_notes (id INTEGER PRIMARY KEY, note TEXT NOT NULL)`); err != nil {
		t.Fatalf("create unexpected table: %v", err)
	}
	conn.Close(context.Background())

	before := catalogSnapshot(t, dsn)

	initCtx, initCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer initCancel()
	err := InitSchema(initCtx, dsn)
	var stateErr *SchemaStateError
	if !errors.As(err, &stateErr) || !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("InitSchema() error = %v, want ErrUnsupportedSchema as SchemaStateError", err)
	}
	found := false
	for _, d := range stateErr.Details {
		if strings.Contains(d, "unexpected table magneticod.operator_notes") {
			found = true
		}
	}
	if !found {
		t.Errorf("refusal details %q do not name the unexpected table", stateErr.Details)
	}

	dbCtx, dbCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dbCancel()
	if _, dbErr := MakeDatabase(dbCtx, dsn); !errors.Is(dbErr, ErrUnsupportedSchema) {
		t.Errorf("MakeDatabase() error = %v, want ErrUnsupportedSchema", dbErr)
	}

	if got := catalogSnapshot(t, dsn); got != before {
		t.Error("refusal changed the catalog")
	}
}

// Unrelated schemas coexist with a fresh magneticod database; only
// magneticod itself is held to the expected inventory.
func TestUnrelatedSchemaCoexists(t *testing.T) {
	t.Parallel()

	dsn := integrationCluster(t).testDatabase(t)

	conn := openConn(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()
	if _, err := conn.Exec(ctx, `CREATE SCHEMA users`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx,
		`CREATE TABLE users.preferences (id INTEGER PRIMARY KEY, pref TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	conn.Close(context.Background())

	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	if err := InitSchema(ctx2, dsn); err != nil {
		t.Fatalf("InitSchema() error = %v, want an unrelated schema to be ignored", err)
	}
}

// Unknown non-table relations inside magneticod — views, materialized views,
// and foreign tables through the same guard — make the schema state
// unverifiable exactly like unknown tables.
func TestUnknownNonTableRelationsRefused(t *testing.T) {
	t.Parallel()

	dsn := integrationCluster(t).testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := InitSchema(ctx, dsn); err != nil {
		t.Fatalf("InitSchema() error = %v", err)
	}

	conn := openConn(t, dsn)
	execCtx, execCancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer execCancel()
	if _, err := conn.Exec(execCtx, `CREATE VIEW magneticod.operator_view AS SELECT 1`); err != nil {
		t.Fatalf("create view: %v", err)
	}
	if _, err := conn.Exec(execCtx,
		`CREATE MATERIALIZED VIEW magneticod.operator_cache AS SELECT 1 AS one`); err != nil {
		t.Fatalf("create materialized view: %v", err)
	}
	conn.Close(context.Background())

	before := catalogSnapshot(t, dsn)

	initCtx, initCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer initCancel()
	err := InitSchema(initCtx, dsn)
	var stateErr *SchemaStateError
	if !errors.As(err, &stateErr) || !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("InitSchema() error = %v, want ErrUnsupportedSchema as SchemaStateError", err)
	}
	joined := strings.Join(stateErr.Details, "\n")
	if !strings.Contains(joined, "unexpected view magneticod.operator_view") {
		t.Errorf("details %q do not name the unexpected view", stateErr.Details)
	}
	if !strings.Contains(joined, "unexpected materialized view magneticod.operator_cache") {
		t.Errorf("details %q do not name the unexpected materialized view", stateErr.Details)
	}

	dbCtx, dbCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dbCancel()
	if _, dbErr := MakeDatabase(dbCtx, dsn); !errors.Is(dbErr, ErrUnsupportedSchema) {
		t.Errorf("MakeDatabase() error = %v, want ErrUnsupportedSchema", dbErr)
	}

	if got := catalogSnapshot(t, dsn); got != before {
		t.Error("refusal changed the catalog")
	}
}

// In a degenerate state (an unexpected object AND missing expected tables)
// the refusal names both the missing tables and the unexpected object.
func TestDegenerateStateNamesEverything(t *testing.T) {
	t.Parallel()

	dsn := integrationCluster(t).testDatabase(t)

	conn := openConn(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()
	if _, err := conn.Exec(ctx, `CREATE SCHEMA magneticod`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE magneticod.stray (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	conn.Close(context.Background())

	initCtx, initCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer initCancel()
	err := InitSchema(initCtx, dsn)
	var stateErr *SchemaStateError
	if !errors.As(err, &stateErr) {
		t.Fatalf("InitSchema() error = %v, want a SchemaStateError", err)
	}
	joined := strings.Join(stateErr.Details, "\n")
	if !strings.Contains(joined, "unexpected table magneticod.stray") {
		t.Errorf("details %q do not name the unexpected table", stateErr.Details)
	}
	if !strings.Contains(joined, "magneticod.torrents") {
		t.Errorf("details %q do not name the missing tables", stateErr.Details)
	}
}

// A view carrying an expected family name must be classified as an
// unsupported unexpected view, never as a legacy table: only real and
// partitioned tables (relkind r/p) feed the legacy detection path.
func TestFamilyNamedViewIsUnsupportedNotLegacy(t *testing.T) {
	t.Parallel()

	t.Run("view named magneticod.files", func(t *testing.T) {
		t.Parallel()

		dsn := integrationCluster(t).testDatabase(t)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := InitSchema(ctx, dsn); err != nil {
			t.Fatalf("InitSchema() error = %v", err)
		}

		conn := openConn(t, dsn)
		execCtx, execCancel := context.WithTimeout(context.Background(), dbOpTimeout)
		defer execCancel()
		if _, err := conn.Exec(execCtx, `CREATE VIEW magneticod.files AS SELECT 1`); err != nil {
			t.Fatal(err)
		}
		conn.Close(context.Background())

		before := catalogSnapshot(t, dsn)

		initCtx, initCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer initCancel()
		err := InitSchema(initCtx, dsn)
		var stateErr *SchemaStateError
		if !errors.As(err, &stateErr) {
			t.Fatalf("InitSchema() error = %v, want a SchemaStateError", err)
		}
		if errors.Is(err, ErrLegacySchema) {
			t.Errorf("InitSchema() error = %v, want ErrUnsupportedSchema, not the legacy sentinel", err)
		}
		if !errors.Is(err, ErrUnsupportedSchema) {
			t.Fatalf("InitSchema() error = %v, want ErrUnsupportedSchema", err)
		}
		found := false
		for _, d := range stateErr.Details {
			if strings.Contains(d, "unexpected view magneticod.files") {
				found = true
			}
		}
		if !found {
			t.Errorf("refusal details %q do not name the unexpected view", stateErr.Details)
		}

		dbCtx, dbCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dbCancel()
		if _, dbErr := MakeDatabase(dbCtx, dsn); !errors.Is(dbErr, ErrUnsupportedSchema) {
			t.Errorf("MakeDatabase() error = %v, want ErrUnsupportedSchema", dbErr)
		}

		if got := catalogSnapshot(t, dsn); got != before {
			t.Error("refusal changed the catalog")
		}
	})

	// Without the relkind guard, the column probe would read the view's
	// columns, miss file_count and misclassify the state as a legacy layout.
	t.Run("view named magneticod.torrents", func(t *testing.T) {
		t.Parallel()

		dsn := integrationCluster(t).testDatabase(t)
		conn := openConn(t, dsn)
		ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
		defer cancel()
		if _, err := conn.Exec(ctx, `CREATE SCHEMA magneticod`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx,
			`CREATE VIEW magneticod.torrents AS SELECT 1 AS id`); err != nil {
			t.Fatal(err)
		}
		conn.Close(context.Background())

		initCtx, initCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer initCancel()
		err := InitSchema(initCtx, dsn)
		if errors.Is(err, ErrLegacySchema) {
			t.Errorf("InitSchema() error = %v, want ErrUnsupportedSchema, not the legacy sentinel", err)
		}
		var stateErr *SchemaStateError
		if !errors.As(err, &stateErr) || !errors.Is(err, ErrUnsupportedSchema) {
			t.Fatalf("InitSchema() error = %v, want ErrUnsupportedSchema as SchemaStateError", err)
		}
		joined := strings.Join(stateErr.Details, "\n")
		if !strings.Contains(joined, "unexpected view magneticod.torrents") {
			t.Errorf("details %q do not name the unexpected view", stateErr.Details)
		}
		if strings.Contains(joined, "without file_count") {
			t.Errorf("details %q treat the view as a legacy torrents table", stateErr.Details)
		}
	})
}

func TestMakeDatabaseStartupVerification(t *testing.T) {
	t.Parallel()

	t.Run("absent schema fails with the init hint", func(t *testing.T) {
		t.Parallel()
		dsn := integrationCluster(t).testDatabase(t)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		store, err := MakeDatabase(ctx, dsn)
		if !errors.Is(err, ErrUnsupportedSchema) {
			t.Fatalf("MakeDatabase() error = %v, want ErrUnsupportedSchema", err)
		}
		if !strings.Contains(err.Error(), "magnetico migrate schema-init") {
			t.Errorf("error lacks the operator hint: %v", err)
		}
		if store != nil {
			t.Error("store must be nil after a failed verification")
		}
	})

	t.Run("initialized schema opens", func(t *testing.T) {
		t.Parallel()
		dsn := integrationCluster(t).testDatabase(t)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := InitSchema(ctx, dsn); err != nil {
			t.Fatalf("InitSchema() error = %v", err)
		}
		store, err := MakeDatabase(ctx, dsn)
		if err != nil {
			t.Fatalf("MakeDatabase() error = %v", err)
		}
		store.Close()
	})

	t.Run("missing pg_trgm fails with the extension hint", func(t *testing.T) {
		t.Parallel()
		dsn := integrationCluster(t).testDatabase(t)

		// Build the schema objects WITHOUT the extension.
		if err := InitSchema(context.Background(), dsn); err != nil {
			t.Fatalf("InitSchema() error = %v", err)
		}
		conn := openConn(t, dsn)
		ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
		defer cancel()
		// CASCADE: the trgm GIN index on torrents depends on the extension.
		if _, err := conn.Exec(ctx, `DROP EXTENSION pg_trgm CASCADE`); err != nil {
			t.Fatalf("drop extension: %v", err)
		}
		conn.Close(context.Background())

		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel2()
		_, err := MakeDatabase(ctx2, dsn)
		if !errors.Is(err, ErrMissingExtension) {
			t.Fatalf("MakeDatabase() error = %v, want ErrMissingExtension", err)
		}
		if !strings.Contains(err.Error(), "CREATE EXTENSION") {
			t.Errorf("error lacks the operator hint: %v", err)
		}
	})
}
