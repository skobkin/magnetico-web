package original

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// The immutability assertions of this package compare catalogSnapshot before
// and after a probe or a refusal. They only prove something if the snapshot
// changes whenever the source data changes, including in the deliberately
// broken layouts the refusal tests build.
func TestSourceSnapshotDetectsDataChanges(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		layout string // applied before the baseline snapshot
		change string
	}{
		{"torrent renamed", "", `UPDATE magneticod.torrents SET name = 'renamed' WHERE id = 1`},
		{"torrent added", "", `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
            VALUES (2, decode('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','hex'), 'beta', 7, 1700000001)`},
		{"torrent deleted", "", `DELETE FROM magneticod.torrents WHERE id = 1`},
		{"file path changed", "", `UPDATE magneticod.files SET path = 'other.txt' WHERE id = 1`},
		{"file size changed", "", `UPDATE magneticod.files SET size = 101 WHERE id = 1`},
		{"file deleted", "", `DELETE FROM magneticod.files WHERE id = 1`},
		{"schema version changed", "", `UPDATE magneticod.migrations SET schema_version = 3`},
		// The refusal tests snapshot sources that the adapter rejects; a change
		// to their data must still be visible.
		{"broken layout: missing column", `ALTER TABLE magneticod.files DROP COLUMN path`,
			`UPDATE magneticod.files SET size = 101 WHERE id = 1`},
		{"broken layout: unexpected table", `CREATE TABLE magneticod.unexpected (id int);
            INSERT INTO magneticod.unexpected VALUES (1)`,
			`UPDATE magneticod.unexpected SET id = 2`},
		{"broken layout: partitioned files", `DROP TABLE magneticod.files;
            CREATE TABLE magneticod.files (id INTEGER NOT NULL, torrent_id INTEGER, size BIGINT NOT NULL, path TEXT NOT NULL)
                PARTITION BY HASH (id);
            CREATE TABLE magneticod.files_p0 PARTITION OF magneticod.files FOR VALUES WITH (modulus 2, remainder 0);
            CREATE TABLE magneticod.files_p1 PARTITION OF magneticod.files FOR VALUES WITH (modulus 2, remainder 1);
            INSERT INTO magneticod.files VALUES (1, 1, 100, 'one.txt')`,
			`UPDATE magneticod.files SET path = 'other.txt' WHERE id = 1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := createLegacySource(t)
			seedTorrent(t, dsn, 1, hashAA, "alpha", 100, 1700000000)
			owner := int32(1)
			seedFile(t, dsn, 1, &owner, "one.txt", 100)
			if tc.layout != "" {
				execSQL(t, dsn, tc.layout)
			}

			before := catalogSnapshot(t, dsn)
			if again := catalogSnapshot(t, dsn); again != before {
				t.Fatalf("snapshot is not deterministic:\nfirst:\n%s\nsecond:\n%s", before, again)
			}
			execSQL(t, dsn, tc.change)
			if after := catalogSnapshot(t, dsn); after == before {
				t.Fatalf("data change %q is invisible to the snapshot:\n%s", tc.change, before)
			}
		})
	}
}

// A data query that fails must fail the snapshot, not degrade into a marker
// that compares equal before and after.
func TestSourceSnapshotReportsUnreadableData(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	seedTorrent(t, dsn, 1, hashAA, "alpha", 100, 1700000000)

	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()

	// Another session holds an exclusive lock on the files table: reading it
	// times out, while the catalog part of the snapshot stays readable.
	holder, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("lock holder connect: %v", err)
	}
	defer holder.Close(context.Background())
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("lock holder begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE magneticod.files IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock files: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("snapshot connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `SET lock_timeout = '200ms'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}

	snap, err := sourceSnapshot(ctx, conn)
	if err == nil {
		t.Fatalf("snapshot of an unreadable table succeeded:\n%s", snap)
	}
	if !strings.Contains(err.Error(), "files") {
		t.Fatalf("snapshot error does not name the unreadable table: %v", err)
	}
}
