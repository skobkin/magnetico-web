package original

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/migration/pgidentity"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// Suite ①: exact source v0.12.0 detection and refusal of everything else,
// with proof that probes and refusals never write to the source.

const (
	hashAA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashBB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hashCC = "cccccccccccccccccccccccccccccccccccccccc"
)

func TestOpenAcceptsOriginalV0Schema(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	seedTorrent(t, dsn, 1, hashAA, "alpha", 100, 1700000000)
	seedTorrent(t, dsn, 3, hashBB, "beta", 200, 1700000100)
	owner := int32(1)
	seedFile(t, dsn, 1, &owner, "alpha/one.txt", 60)
	seedFile(t, dsn, 2, &owner, "alpha/two.txt", 40)
	seedFile(t, dsn, 3, nil, "unclaimed.txt", 200) // NULL-owned: counted, never attached

	src := openSource(t, dsn)

	fp, err := src.Fingerprint(context.Background())
	if err != nil {
		t.Fatalf("Fingerprint() error = %v", err)
	}
	if fp.Torrents != 2 || fp.MinID != 1 || fp.MaxID != 3 {
		t.Fatalf("fingerprint counts = (%d, %d, %d), want (2, 1, 3)", fp.Torrents, fp.MinID, fp.MaxID)
	}
	if fp.FileRows != 3 || fp.NullOwnerFiles != 1 || fp.OrphanFiles != 0 {
		t.Fatalf("fingerprint file accounting = (%d, %d, %d), want (3, 1, 0)",
			fp.FileRows, fp.NullOwnerFiles, fp.OrphanFiles)
	}
	if fp.AnchorMinInfoHash != hashAA || fp.AnchorMaxInfoHash != hashBB {
		t.Fatalf("anchors = (%s, %s), want (%s, %s)",
			fp.AnchorMinInfoHash, fp.AnchorMaxInfoHash, hashAA, hashBB)
	}
	if fp.Database == "" || fp.PostmasterStart.IsZero() {
		t.Fatalf("identity incomplete: %+v", fp)
	}

	// A read-only-role connection works the same way (the adapter only
	// needs SELECT).
	_ = dsn
}

func TestOpenAcceptsEmptySource(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	src := openSource(t, dsn)

	fp, err := src.Fingerprint(context.Background())
	if err != nil {
		t.Fatalf("Fingerprint() error = %v", err)
	}
	if fp.Torrents != 0 || fp.FileRows != 0 || fp.AnchorMinInfoHash != "" || fp.AnchorMaxInfoHash != "" {
		t.Fatalf("empty-source fingerprint = %+v", fp)
	}
}

func TestOpenRefusesEmptyDatabase(t *testing.T) {
	t.Parallel()

	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	before := catalogSnapshot(t, dsn)
	errText := openFailsWith(t, dsn, ErrSourceNotMagnetico, before)
	if strings.Contains(errText, "postgres://") {
		t.Fatalf("refusal text leaks a DSN: %s", errText)
	}
}

func TestOpenRefusesV1ForkByPartitionedFiles(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	// Fork the files table into a partitioned parent (tgragnato v1 shape).
	execSQL(t, dsn, `DROP TABLE magneticod.files;
        CREATE TABLE magneticod.files (
            id          INTEGER NOT NULL,
            torrent_id  INTEGER,
            size        BIGINT NOT NULL,
            path        TEXT NOT NULL
        ) PARTITION BY HASH (id);
        CREATE TABLE magneticod.files_p0 PARTITION OF magneticod.files
            FOR VALUES WITH (modulus 2, remainder 0);
        CREATE TABLE magneticod.files_p1 PARTITION OF magneticod.files
            FOR VALUES WITH (modulus 2, remainder 1);`)

	before := catalogSnapshot(t, dsn)
	errText := openFailsWith(t, dsn, ErrSourceV1Fork, before)
	if !strings.Contains(errText, "partitioned") {
		t.Fatalf("fork refusal does not name partitioning: %s", errText)
	}
}

func TestOpenRefusesV1ForkBySchemaVersion(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	execSQL(t, dsn, `UPDATE magneticod.migrations SET schema_version = 1`)
	before := catalogSnapshot(t, dsn)
	openFailsWith(t, dsn, ErrSourceV1Fork, before)
}

func TestOpenRefusesUnsupportedLayouts(t *testing.T) {
	t.Parallel()

	breakages := []struct {
		name         string
		sql          string
		wantInDetail string
	}{
		{"missing column", `ALTER TABLE magneticod.files DROP COLUMN path`, "columns"},
		{"wrong column type", `ALTER TABLE magneticod.torrents ALTER COLUMN discovered_on TYPE bigint`, "column 5"},
		{"extra column", `ALTER TABLE magneticod.torrents ADD COLUMN extra text`, "columns"},
		{"unexpected table", `CREATE TABLE magneticod.unexpected (id int)`, "unexpected relation"},
		{"unexpected view", `CREATE VIEW magneticod.v_torrents AS SELECT id FROM magneticod.torrents`, "view"},
		{"dropped lookup index", `DROP INDEX magneticod.idx_files_torrent_id`, "missing index"},
		{"wrong lookup index key", `DROP INDEX magneticod.idx_files_torrent_id;
            CREATE INDEX idx_files_torrent_id ON magneticod.files (path)`, "does not key files(torrent_id)"},
		{"extra index", `CREATE INDEX idx_torrents_name_extra ON magneticod.torrents (name)`, "unexpected index"},
		{"invalid index", `DROP INDEX magneticod.idx_torrents_total_size;
            CREATE INDEX idx_torrents_total_size ON magneticod.torrents (total_size);
            UPDATE pg_index SET indisvalid = false
            WHERE indexrelid = 'magneticod.idx_torrents_total_size'::regclass`, "is not valid"},
		{"empty migrations", `DELETE FROM magneticod.migrations`, "migrations rows"},
		{"unknown version", `UPDATE magneticod.migrations SET schema_version = 7`, "migrations rows"},
		{"missing unique info_hash", `ALTER TABLE magneticod.torrents DROP CONSTRAINT torrents_info_hash_key`, "unique constraint"},
		{"missing files PK", `ALTER TABLE magneticod.files DROP CONSTRAINT files_pkey`, "primary key missing"},
	}

	for _, tc := range breakages {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := createLegacySource(t)
			execSQL(t, dsn, tc.sql)
			before := catalogSnapshot(t, dsn)
			errText := openFailsWith(t, dsn, ErrSourceUnsupported, before)
			if !strings.Contains(errText, tc.wantInDetail) {
				t.Fatalf("refusal detail %q missing %q", errText, tc.wantInDetail)
			}
		})
	}
}

func TestProbeAndRefusalLeaveSourceUntouched(t *testing.T) {
	t.Parallel()

	// A fully valid source: open, fingerprint, read a window, close — then
	// prove the catalog and data are byte-identical.
	dsn := createLegacySource(t)
	seedTorrent(t, dsn, 1, hashAA, "alpha", 100, 1700000000)
	owner := int32(1)
	seedFile(t, dsn, 1, &owner, "one.txt", 100)

	before := catalogSnapshot(t, dsn)
	src := openSource(t, dsn)
	if _, err := src.Fingerprint(context.Background()); err != nil {
		t.Fatalf("Fingerprint() error = %v", err)
	}
	if _, err := src.Window(context.Background(), 0, 10, defaultWindowLimits()); err != nil {
		t.Fatalf("Window() error = %v", err)
	}
	src.Close()

	if after := catalogSnapshot(t, dsn); after != before {
		t.Fatalf("read-only guarantee violated:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestSourceSessionIsReadOnly(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	src := openSource(t, dsn)

	// Even with full owner grants, the adapter's session-level read-only
	// default must make any accidental write impossible.
	_, err := src.pool.Exec(context.Background(),
		`INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
         VALUES (9, decode('`+hashCC+`','hex'), 'x', 1, 1)`)
	if err == nil {
		t.Fatal("write through the source handle succeeded; read-only enforcement is broken")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("write error is not a read-only violation: %v", err)
	}
}

func TestIdentitySameInstanceDifferentDatabases(t *testing.T) {
	t.Parallel()

	cluster := pgtest.IntegrationCluster(t)
	source := createLegacySource(t)
	target := cluster.TestDatabase(t) // same instance, different database

	src := openSource(t, source)
	srcIdentity, err := src.Identity(context.Background())
	if err != nil {
		t.Fatalf("source Identity() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, target)
	if err != nil {
		t.Fatalf("target connect: %v", err)
	}
	defer conn.Close(context.Background())
	tgtIdentity, err := pgidentity.Read(ctx, conn)
	if err != nil {
		t.Fatalf("target identity: %v", err)
	}

	same, ok := pgidentity.SameDatabase(srcIdentity, tgtIdentity)
	if !ok {
		t.Fatal("identity could not be established")
	}
	if same {
		t.Fatal("distinct databases on one instance judged same")
	}
}

func TestIdentitySameDatabaseViaSocketAndTCP(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	socketDSN := unixSocketDSN(t, dsn)

	src := openSource(t, dsn)
	srcIdentity, err := src.Identity(context.Background())
	if err != nil {
		t.Fatalf("source Identity() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, socketDSN)
	if err != nil {
		t.Fatalf("socket connect (%s): %v", socketDSN, err)
	}
	defer conn.Close(context.Background())
	socketIdentity, err := pgidentity.Read(ctx, conn)
	if err != nil {
		t.Fatalf("socket identity: %v", err)
	}

	same, ok := pgidentity.SameDatabase(srcIdentity, socketIdentity)
	if !ok {
		t.Fatal("identity could not be established across transports")
	}
	if !same {
		t.Fatal("same database judged distinct across TCP and Unix socket")
	}
}
