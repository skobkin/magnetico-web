package devseed

import (
	"context"
	"crypto/md5" //nolint:gosec // test checksum only
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func tctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return c
}

func conn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func exec(t *testing.T, dsn, sql string, args ...any) {
	t.Helper()
	if _, err := conn(t, dsn).Exec(tctx(t), sql, args...); err != nil {
		t.Fatalf("%.60s: %v", sql, err)
	}
}

// initialized returns a disposable database with the 0.8.0 schema.
func initialized(t *testing.T) string {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := persistence.InitSchema(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	return dsn
}

// snapshot is a checksum of every torrents row and blob; equal snapshots
// mean byte-identical storage.
func snapshot(t *testing.T, dsn string) string {
	t.Helper()
	rows, err := conn(t, dsn).Query(tctx(t), `
		SELECT encode(t.info_hash,'hex'), t.name, t.total_size, t.discovered_on, t.file_count,
		       coalesce(encode(b.data,'hex'), '-')
		FROM magneticod.torrents t LEFT JOIN magneticod.torrent_file_blobs b ON b.torrent_id = t.id
		ORDER BY t.info_hash`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	h := md5.New() //nolint:gosec // test checksum
	n := 0
	for rows.Next() {
		var ih, name, blob string
		var size, disc int64
		var fc int32
		if err := rows.Scan(&ih, &name, &size, &disc, &fc, &blob); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%s|%s|%d|%d|%d|%s\n", ih, name, size, disc, fc, blob)
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d:%s", n, hex.EncodeToString(h.Sum(nil)))
}

func count(t *testing.T, dsn, sql string) int64 {
	t.Helper()
	var n int64
	if err := conn(t, dsn).QueryRow(tctx(t), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSeedEmptyDatabaseCountsAndPayloads(t *testing.T) {
	dsn := initialized(t)
	res, err := Seed(tctx(t), dsn, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Before != StateEmpty || res.Created != 101 || res.Existing != 0 || res.Torrents != 101 {
		t.Fatalf("result = %+v", res)
	}

	store, err := persistence.MakeDatabase(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stats, err := store.GetStatistics(tctx(t))
	if err != nil {
		t.Fatal(err)
	}
	data := Dataset()
	var wantFiles, wantBytes uint64
	for _, f := range data {
		wantFiles += uint64(len(f.Files))
		wantBytes += uint64(f.Torrent.TotalSize)
	}
	if stats.Torrents != 101 || stats.Files != wantFiles || stats.TotalSize != wantBytes || int(wantFiles) != res.Files {
		t.Fatalf("stats = %+v, want 101 torrents, %d files, %d bytes (result files %d)", stats, wantFiles, wantBytes, res.Files)
	}
	if got := count(t, dsn, `SELECT count(*) FROM magneticod.torrents WHERE file_count = 10000`); got != 1 {
		t.Errorf("torrents with exactly 10000 files = %d, want 1", got)
	}
	if got := count(t, dsn, `SELECT count(*) FROM magneticod.torrents WHERE file_count = 1`); got != 40 {
		t.Errorf("single-file torrents = %d, want 40", got)
	}
	if got := count(t, dsn, `SELECT count(*) FROM magneticod.torrents WHERE file_count BETWEEN 10 AND 100`); got != 60 {
		t.Errorf("multi-file torrents = %d, want 60", got)
	}

	// Every stored payload decodes to exactly the fixture's file list.
	noBlob := 0
	for _, f := range data {
		got, err := store.GetFiles(tctx(t), f.Torrent.InfoHash)
		if err != nil {
			t.Fatalf("%s: GetFiles: %v", f.Key, err)
		}
		if !sameMultiset(got, f.Files) {
			t.Errorf("%s: stored file list differs from the fixture", f.Key)
		}
		meta, err := store.GetTorrent(tctx(t), f.Torrent.InfoHash)
		if err != nil || meta.Name != f.Torrent.Name || meta.TotalSize != f.Torrent.TotalSize ||
			meta.DiscoveredOn != f.Torrent.DiscoveredOn || int(meta.FileCount) != len(f.Files) {
			t.Errorf("%s: stored metadata differs (%v)", f.Key, err)
		}
		if persistence.NoBlobEligible(f.Torrent, f.Files) {
			noBlob++
		}
	}
	// The blob table has one row per torrent except the no-blob singles.
	if got := count(t, dsn, `SELECT count(*) FROM magneticod.torrent_file_blobs`); got != int64(101-noBlob) {
		t.Errorf("blob rows = %d, want %d", got, 101-noBlob)
	}
	// Raw blobs decode with the production codec under production limits.
	rows, err := conn(t, dsn).Query(tctx(t), `SELECT data FROM magneticod.torrent_file_blobs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			t.Fatal(err)
		}
		if _, err := codec.Decode(blob, codec.DefaultLimits()); err != nil {
			t.Errorf("stored blob does not decode: %v", err)
		}
	}
}

func TestSeedIsDeterministicAcrossDatabases(t *testing.T) {
	a, b := initialized(t), initialized(t)
	for _, dsn := range []string{a, b} {
		if _, err := Seed(tctx(t), dsn, false); err != nil {
			t.Fatal(err)
		}
	}
	if sa, sb := snapshot(t, a), snapshot(t, b); sa != sb {
		t.Fatalf("two fresh databases differ after seeding:\n%s\n%s", sa, sb)
	}
	// Infohashes are the documented, stable ones.
	if got := count(t, a, fmt.Sprintf(`SELECT count(*) FROM magneticod.torrents WHERE info_hash = '\x%s'`, pinnedFirstInfoHash)); got != 1 {
		t.Errorf("pinned infohash %s not stored", pinnedFirstInfoHash)
	}
}

func TestSeedRerunIsAnExplicitNoOp(t *testing.T) {
	dsn := initialized(t)
	if _, err := Seed(tctx(t), dsn, false); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dsn)
	res, err := Seed(tctx(t), dsn, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Before != StateSeeded || res.Created != 0 || res.Existing != 101 {
		t.Fatalf("rerun result = %+v", res)
	}
	if after := snapshot(t, dsn); after != before {
		t.Fatal("a rerun changed the stored data")
	}
}

func TestSeedCompletesAnInterruptedRunWithoutTouchingTheRest(t *testing.T) {
	dsn := initialized(t)
	if _, err := Seed(tctx(t), dsn, false); err != nil {
		t.Fatal(err)
	}
	// Simulate an interruption: drop three fixtures.
	data := Dataset()
	for _, f := range data[10:13] {
		exec(t, dsn, `DELETE FROM magneticod.torrents WHERE info_hash = $1`, f.Torrent.InfoHash)
	}
	res, err := Seed(tctx(t), dsn, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Before != StatePartial || res.Created != 3 || res.Existing != 98 {
		t.Fatalf("result = %+v", res)
	}
	if got := count(t, dsn, `SELECT count(*) FROM magneticod.torrents`); got != 101 {
		t.Fatalf("torrents = %d", got)
	}
}

func TestSeedRefusesForeignTorrentsAndWritesNothing(t *testing.T) {
	dsn := initialized(t)
	store, err := persistence.MakeDatabase(tctx(t), dsn)
	if err != nil {
		t.Fatal(err)
	}
	real := persistence.Torrent{InfoHash: []byte("0123456789abcdef0123"), Name: "A real torrent", TotalSize: 5, DiscoveredOn: 1700000000}
	if _, _, err := store.AddTorrent(tctx(t), real, []persistence.File{{Path: "A real torrent", Size: 5}}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	before := snapshot(t, dsn)

	for _, dry := range []bool{true, false} {
		_, err = Seed(tctx(t), dsn, dry)
		if !errors.Is(err, ErrForeignTorrents) {
			t.Fatalf("dryRun=%v: err = %v, want ErrForeignTorrents", dry, err)
		}
	}
	if after := snapshot(t, dsn); after != before {
		t.Fatal("a refused run changed the database")
	}

	// A fixture-only database that ALSO has a foreign torrent is refused too.
	dsn2 := initialized(t)
	if _, err := Seed(tctx(t), dsn2, false); err != nil {
		t.Fatal(err)
	}
	store2, _ := persistence.MakeDatabase(tctx(t), dsn2)
	if _, _, err := store2.AddTorrent(tctx(t), real, []persistence.File{{Path: "A real torrent", Size: 5}}); err != nil {
		t.Fatal(err)
	}
	store2.Close()
	before2 := snapshot(t, dsn2)
	if _, err := Seed(tctx(t), dsn2, false); !errors.Is(err, ErrForeignTorrents) {
		t.Fatalf("err = %v, want ErrForeignTorrents", err)
	}
	if snapshot(t, dsn2) != before2 {
		t.Fatal("a refused run changed the database")
	}
}

func TestSeedRefusesAFixtureHashStoredWithDifferentMetadata(t *testing.T) {
	dsn := initialized(t)
	if _, err := Seed(tctx(t), dsn, false); err != nil {
		t.Fatal(err)
	}
	exec(t, dsn, `UPDATE magneticod.torrents SET name = name || ' (edited)' WHERE info_hash = $1`, Dataset()[3].Torrent.InfoHash)
	before := snapshot(t, dsn)
	if _, err := Seed(tctx(t), dsn, false); !errors.Is(err, ErrFixtureConflict) {
		t.Fatalf("err = %v, want ErrFixtureConflict", err)
	}
	if snapshot(t, dsn) != before {
		t.Fatal("a refused run changed the database")
	}
}

func TestSeedDryRunWritesNothing(t *testing.T) {
	dsn := initialized(t)
	res, err := Seed(tctx(t), dsn, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Before != StateEmpty || res.Created != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := count(t, dsn, `SELECT count(*) FROM magneticod.torrents`); got != 0 {
		t.Fatalf("a dry run stored %d torrents", got)
	}
}

func TestSeedFailsClosedOnIncompatibleDatabases(t *testing.T) {
	cl := pgtest.IntegrationCluster(t)

	t.Run("uninitialized", func(t *testing.T) {
		dsn := cl.TestDatabase(t)
		_, err := Seed(tctx(t), dsn, false)
		if !errors.Is(err, persistence.ErrUnsupportedSchema) && !errors.Is(err, persistence.ErrMissingExtension) {
			t.Fatalf("err = %v, want a schema refusal", err)
		}
		if got := count(t, dsn, `SELECT count(*) FROM pg_namespace WHERE nspname = 'magneticod'`); got != 0 {
			t.Fatal("the refused run created the magneticod schema")
		}
	})

	t.Run("legacy layout", func(t *testing.T) {
		dsn := cl.TestDatabase(t)
		exec(t, dsn, `CREATE TABLE public.torrents (id INTEGER PRIMARY KEY, info_hash BYTEA NOT NULL UNIQUE, name TEXT NOT NULL, total_size BIGINT NOT NULL, discovered_on INTEGER NOT NULL)`)
		exec(t, dsn, `CREATE TABLE public.files (id INTEGER PRIMARY KEY, torrent_id INTEGER, size BIGINT NOT NULL, path TEXT NOT NULL)`)
		exec(t, dsn, `INSERT INTO public.torrents VALUES (1, '\x0102030405060708091011121314151617181920', 'legacy', 1, 1)`)
		_, err := Seed(tctx(t), dsn, false)
		if err == nil {
			t.Fatal("a legacy database was accepted")
		}
		if got := count(t, dsn, `SELECT count(*) FROM public.torrents`); got != 1 {
			t.Fatal("the legacy table changed")
		}
		if got := count(t, dsn, `SELECT count(*) FROM pg_namespace WHERE nspname = 'magneticod'`); got != 0 {
			t.Fatal("the refused run created the magneticod schema")
		}
	})

	t.Run("wrong schema version", func(t *testing.T) {
		dsn := initialized(t)
		exec(t, dsn, `UPDATE magneticod.schema_version SET version = 2`)
		if _, err := Seed(tctx(t), dsn, false); !errors.Is(err, persistence.ErrUnsupportedSchema) {
			t.Fatalf("err = %v, want ErrUnsupportedSchema", err)
		}
		if got := count(t, dsn, `SELECT count(*) FROM magneticod.torrents`); got != 0 {
			t.Fatal("rows were written")
		}
	})

	t.Run("not a postgres url", func(t *testing.T) {
		if _, err := Seed(tctx(t), "sqlite:///tmp/x.db", false); !errors.Is(err, persistence.ErrStorageNotImplemented) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestSeedNeverTouchesOtherSchemas(t *testing.T) {
	dsn := initialized(t)
	exec(t, dsn, `CREATE SCHEMA bystander`)
	exec(t, dsn, `CREATE TABLE bystander.t (v text)`)
	exec(t, dsn, `INSERT INTO bystander.t VALUES ('keep')`)
	if _, err := Seed(tctx(t), dsn, false); err != nil {
		t.Fatal(err)
	}
	if got := count(t, dsn, `SELECT count(*) FROM bystander.t WHERE v = 'keep'`); got != 1 {
		t.Fatal("another schema changed")
	}
	for _, s := range []string{"magnetico_web", "magnetico_ingest", "magnetico_migration"} {
		if got := count(t, dsn, fmt.Sprintf(`SELECT count(*) FROM pg_namespace WHERE nspname = '%s'`, s)); got != 0 {
			t.Errorf("schema %s was created", s)
		}
	}
}

func TestSeedConcurrentRunIsRefused(t *testing.T) {
	dsn := initialized(t)
	holder := conn(t, dsn)
	var ok bool
	if err := holder.QueryRow(tctx(t), `SELECT pg_try_advisory_lock($1)`, advisoryKey).Scan(&ok); err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	if _, err := Seed(tctx(t), dsn, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if got := count(t, dsn, `SELECT count(*) FROM magneticod.torrents`); got != 0 {
		t.Fatal("rows were written")
	}
}

func TestSeedErrorsCarryNoDatabaseURL(t *testing.T) {
	dsn := initialized(t)
	exec(t, dsn, `UPDATE magneticod.schema_version SET version = 2`)
	_, err := Seed(tctx(t), dsn, false)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, leak := range []string{"postgres://", "password"} {
		if strings.Contains(strings.ToLower(err.Error()), leak) {
			t.Errorf("error text contains %q: %v", leak, err)
		}
	}
}

// swapBlob replaces the stored file-list blob of fixture f in dsn with one
// encoded (by the production store) from files, leaving the summary row
// untouched. files must keep the same count and total size so the row still
// decodes cleanly: the only difference left is the content.
func swapBlob(t *testing.T, dsn string, f Fixture, files []persistence.File) {
	t.Helper()
	other := initialized(t)
	store, err := persistence.MakeDatabase(tctx(t), other)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, _, err := store.AddTorrent(tctx(t), f.Torrent, files); err != nil {
		t.Fatalf("encoding the altered file list: %v", err)
	}
	var blob []byte
	if err := conn(t, other).QueryRow(tctx(t), `SELECT b.data FROM magneticod.torrent_file_blobs b
		JOIN magneticod.torrents t ON t.id = b.torrent_id WHERE t.info_hash = $1`, f.Torrent.InfoHash).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	exec(t, dsn, `UPDATE magneticod.torrent_file_blobs SET data = $2
		WHERE torrent_id = (SELECT id FROM magneticod.torrents WHERE info_hash = $1)`, f.Torrent.InfoHash, blob)
}

// swapFixture returns a multi-file fixture with at least two files whose
// sizes allow moving one byte between them.
func swapFixture(t *testing.T) Fixture {
	t.Helper()
	for _, f := range Dataset() {
		if f.Kind == KindMulti && len(f.Files) >= 3 && f.Files[1].Size > 0 {
			return f
		}
	}
	t.Fatal("no suitable multi-file fixture")
	return Fixture{}
}

func TestSeedRefusesStoredFileListsThatDifferFromTheFixture(t *testing.T) {
	f := swapFixture(t)
	variants := map[string]func([]persistence.File){
		"different path":                  func(fs []persistence.File) { fs[0].Path += "-changed" },
		"different sizes, same total":     func(fs []persistence.File) { fs[0].Size++; fs[1].Size-- },
		"a duplicate replaced by another": func(fs []persistence.File) { fs[2] = fs[1] },
	}
	for name, mutate := range variants {
		for _, dry := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/dry=%v", name, dry), func(t *testing.T) {
				dsn := initialized(t)
				if _, err := Seed(tctx(t), dsn, false); err != nil {
					t.Fatal(err)
				}
				files := append([]persistence.File(nil), f.Files...)
				mutate(files)
				if name == "a duplicate replaced by another" {
					// keep the total size: give file 2's size to file 1's twin
					var total int64
					for _, x := range files {
						total += x.Size
					}
					if total != f.Torrent.TotalSize {
						files[0].Size += f.Torrent.TotalSize - total
						if files[0].Size < 0 {
							t.Skip("cannot keep the total size")
						}
					}
				}
				swapBlob(t, dsn, f, files)
				before := snapshot(t, dsn)
				if _, err := Seed(tctx(t), dsn, dry); !errors.Is(err, ErrFixtureConflict) {
					t.Fatalf("err = %v, want ErrFixtureConflict", err)
				}
				if snapshot(t, dsn) != before {
					t.Fatal("a refused run changed the database")
				}
			})
		}
	}
}

func TestSeedFailsOnCorruptOrMissingFileListPayloads(t *testing.T) {
	f := swapFixture(t)
	cases := map[string]string{
		"corrupt": `UPDATE magneticod.torrent_file_blobs SET data = '\x0102030405'
			WHERE torrent_id = (SELECT id FROM magneticod.torrents WHERE info_hash = $1)`,
		"missing": `DELETE FROM magneticod.torrent_file_blobs
			WHERE torrent_id = (SELECT id FROM magneticod.torrents WHERE info_hash = $1)`,
	}
	for name, stmt := range cases {
		for _, dry := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/dry=%v", name, dry), func(t *testing.T) {
				dsn := initialized(t)
				if _, err := Seed(tctx(t), dsn, false); err != nil {
					t.Fatal(err)
				}
				exec(t, dsn, stmt, f.Torrent.InfoHash)
				before := snapshot(t, dsn)
				res, err := Seed(tctx(t), dsn, dry)
				if err == nil {
					t.Fatalf("seeding succeeded over a %s payload: %+v", name, res)
				}
				if snapshot(t, dsn) != before {
					t.Fatal("a refused run changed the database")
				}
			})
		}
	}
}
