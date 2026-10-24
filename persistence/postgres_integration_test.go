package persistence

// Integration suites for the pgxStore against a real disposable PostgreSQL
// (harness_test.go). Skips cleanly when no cluster is available.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/codec"
)

// newTestStore returns a Store backed by a fresh, fully initialized database.
func newTestStore(t *testing.T) (Store, string) {
	t.Helper()
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
	t.Cleanup(func() { _ = store.Close() })
	return store, dsn
}

// infoHash builds a deterministic, distinct 20-byte infohash from a tag.
func infoHash(tag string) []byte {
	h := make([]byte, 20)
	copy(h, tag)
	return h
}

func baseTorrent(name string, ih []byte, total int64) Torrent {
	return Torrent{
		InfoHash:     ih,
		Name:         name,
		TotalSize:    total,
		DiscoveredOn: 1700000000,
	}
}

func mustAdd(t *testing.T, store Store, meta Torrent, files []File) (bool, int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	created, id, err := store.AddTorrent(ctx, meta, files)
	if err != nil {
		t.Fatalf("AddTorrent(%s) error = %v", meta.Name, err)
	}
	return created, id
}

func TestAddTorrentAndReadBack(t *testing.T) {
	t.Parallel()

	store, dsn := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	t.Run("typical single file stores no blob", func(t *testing.T) {
		meta := baseTorrent("release-name", infoHash("typical-single-000000"), 500)
		files := []File{{Path: "release-name", Size: 500}}

		created, id := mustAdd(t, store, meta, files)
		if !created || id <= 0 {
			t.Fatalf("created = %v, id = %d; want true and a positive id", created, id)
		}

		got, err := store.GetFiles(ctx, meta.InfoHash)
		if err != nil {
			t.Fatalf("GetFiles() error = %v", err)
		}
		if len(got) != 1 || got[0].Path != "release-name" || got[0].Size != 500 {
			t.Errorf("GetFiles() = %+v, want the reconstructed single file", got)
		}

		if n := blobCount(t, dsn, id); n != 0 {
			t.Errorf("typical single file produced %d blob rows, want 0", n)
		}
	})

	t.Run("GetTorrent fields", func(t *testing.T) {
		meta := baseTorrent("get-torrent-name", infoHash("get-torrent-00000000"), 500)
		mustAdd(t, store, meta, []File{{Path: "release-name", Size: 500}})

		got, err := store.GetTorrent(ctx, meta.InfoHash)
		if err != nil {
			t.Fatalf("GetTorrent() error = %v", err)
		}
		if got.Name != meta.Name || got.TotalSize != meta.TotalSize ||
			got.DiscoveredOn != meta.DiscoveredOn || got.FileCount != 1 {
			t.Errorf("GetTorrent() = %+v, want the stored fields", got)
		}

		if _, err := store.GetTorrent(ctx, infoHash("missing-torrent-000")); !errors.Is(err, ErrTorrentNotFound) {
			t.Errorf("GetTorrent(unknown) error = %v, want ErrTorrentNotFound", err)
		}
		if _, err := store.GetFiles(ctx, infoHash("missing-torrent-000")); !errors.Is(err, ErrTorrentNotFound) {
			t.Errorf("GetFiles(unknown) error = %v, want ErrTorrentNotFound", err)
		}
	})

	t.Run("already-present infohash is a no-op", func(t *testing.T) {
		meta := baseTorrent("dup-name", infoHash("duplicate-0000000000"), 500)
		files := []File{{Path: "release-name", Size: 500}}
		_, firstID := mustAdd(t, store, meta, files)

		created, id, err := store.AddTorrent(ctx, meta, files)
		if err != nil || created || id != firstID {
			t.Errorf("second AddTorrent = (%v, %d, %v); want (false, %d, nil)", created, id, err, firstID)
		}

		// Same infohash with different metadata: still dropped, no error.
		other := baseTorrent("different-name", meta.InfoHash, 999)
		created, id, err = store.AddTorrent(ctx, other, []File{{Path: "different-name", Size: 999}})
		if err != nil || created || id != firstID {
			t.Errorf("conflicting AddTorrent = (%v, %d, %v); want (false, %d, nil)", created, id, err, firstID)
		}
	})

	t.Run("atypical single file keeps a blob (vector B shape)", func(t *testing.T) {
		meta := baseTorrent("release-name", infoHash("atypical-single-000"), 500)
		files := []File{{Path: "atypical.bin", Size: 500}}

		created, id := mustAdd(t, store, meta, files)
		if !created {
			t.Fatal("expected creation")
		}
		if n := blobCount(t, dsn, id); n != 1 {
			t.Fatalf("atypical single file produced %d blob rows, want 1", n)
		}
		got, err := store.GetFiles(ctx, meta.InfoHash)
		if err != nil {
			t.Fatalf("GetFiles() error = %v", err)
		}
		if len(got) != 1 || got[0].Path != "atypical.bin" || got[0].Size != 500 {
			t.Errorf("GetFiles() = %+v, want [{atypical.bin 500}]", got)
		}
	})

	t.Run("multi-file round trip", func(t *testing.T) {
		meta := baseTorrent("multi-release", infoHash("multi-00000000000000"), 300)
		files := []File{
			{Path: "sub/b.txt", Size: 200},
			{Path: "a.txt", Size: 100},
		}
		created, id := mustAdd(t, store, meta, files)
		if !created {
			t.Fatal("expected creation")
		}
		if n := blobCount(t, dsn, id); n != 1 {
			t.Fatalf("multi-file produced %d blob rows, want 1", n)
		}

		got, err := store.GetFiles(ctx, meta.InfoHash)
		if err != nil {
			t.Fatalf("GetFiles() error = %v", err)
		}
		// Canonical order after decode.
		want := []File{{Path: "a.txt", Size: 100}, {Path: "sub/b.txt", Size: 200}}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("GetFiles() = %+v, want %+v", got, want)
		}
	})

	t.Run("missing blob for multi-file", func(t *testing.T) {
		meta := baseTorrent("blobless-multi", infoHash("missing-blob-0000000"), 300)
		files := []File{{Path: "a.txt", Size: 100}, {Path: "b.txt", Size: 200}}
		_, id := mustAdd(t, store, meta, files)

		execSQL(t, dsn, `DELETE FROM magneticod.torrent_file_blobs WHERE torrent_id = $1`, id)

		_, err := store.GetFiles(ctx, meta.InfoHash)
		if !errors.Is(err, ErrIntegrityMissingBlob) {
			t.Errorf("GetFiles() error = %v, want ErrIntegrityMissingBlob", err)
		}
	})

	t.Run("corrupted blobs are rejected", func(t *testing.T) {
		newCorrupt := func(name, tag string, total int64, files []File) (Torrent, int32) {
			meta := baseTorrent(name, infoHash(tag), total)
			_, id := mustAdd(t, store, meta, files)
			return meta, id
		}

		// 1) garbage blob bytes.
		meta, id := newCorrupt("corrupt-garbage", "corrupt-garbage-000", 300,
			[]File{{Path: "a.txt", Size: 100}, {Path: "b.txt", Size: 200}})
		execSQL(t, dsn, `UPDATE magneticod.torrent_file_blobs SET data = '\x00deadbeef' WHERE torrent_id = $1`, id)
		if _, err := store.GetFiles(ctx, meta.InfoHash); !errors.Is(err, ErrCorruptBlob) {
			t.Errorf("garbage blob: GetFiles() error = %v, want ErrCorruptBlob", err)
		}

		// 2) strict-framing violation: a valid flat envelope with trailing junk.
		meta, id = newCorrupt("corrupt-trailing", "corrupt-trailing-000", 1,
			[]File{{Path: "a", Size: 1}})
		execSQL(t, dsn, `UPDATE magneticod.torrent_file_blobs
            SET data = '\x011001016101ff' WHERE torrent_id = $1`, id)
		if _, err := store.GetFiles(ctx, meta.InfoHash); !errors.Is(err, ErrCorruptBlob) {
			t.Errorf("trailing-junk blob: GetFiles() error = %v, want ErrCorruptBlob", err)
		}

		// 3) file_count disagrees with the decoded blob.
		meta, id = newCorrupt("corrupt-count", "corrupt-count-00000", 300,
			[]File{{Path: "a.txt", Size: 100}, {Path: "b.txt", Size: 200}})
		execSQL(t, dsn, `UPDATE magneticod.torrents SET file_count = 3 WHERE id = $1`, id)
		if _, err := store.GetFiles(ctx, meta.InfoHash); !errors.Is(err, ErrCorruptBlob) {
			t.Errorf("count mismatch: GetFiles() error = %v, want ErrCorruptBlob", err)
		}

		// 4) total_size disagrees with the decoded sizes.
		meta, id = newCorrupt("corrupt-total", "corrupt-total-00000", 300,
			[]File{{Path: "a.txt", Size: 100}, {Path: "b.txt", Size: 200}})
		execSQL(t, dsn, `UPDATE magneticod.torrents SET total_size = 999 WHERE id = $1`, id)
		if _, err := store.GetFiles(ctx, meta.InfoHash); !errors.Is(err, ErrCorruptBlob) {
			t.Errorf("total mismatch: GetFiles() error = %v, want ErrCorruptBlob", err)
		}
	})

	t.Run("FK cascade removes blobs", func(t *testing.T) {
		meta := baseTorrent("cascade-me", infoHash("cascade-00000000000"), 300)
		_, id := mustAdd(t, store, meta, []File{{Path: "a.txt", Size: 100}, {Path: "b.txt", Size: 200}})
		if n := blobCount(t, dsn, id); n != 1 {
			t.Fatalf("precondition: want 1 blob row, got %d", n)
		}
		execSQL(t, dsn, `DELETE FROM magneticod.torrents WHERE id = $1`, id)
		if n := blobCount(t, dsn, id); n != 0 {
			t.Errorf("after torrent delete %d blob rows remain, want 0 (cascade)", n)
		}
	})

	t.Run("explicit-ID insert proves GENERATED BY DEFAULT", func(t *testing.T) {
		// The future migrator's import path: inserting with explicit ids must
		// work, and the identity sequence keeps serving normal inserts.
		execSQL(t, dsn, `INSERT INTO magneticod.torrents
            (id, info_hash, name, total_size, discovered_on, file_count)
            VALUES (424242, $1, 'explicit-import', 100, 1700000001, 1)`, infoHash("explicit-id-000000000"))

		created, id := mustAdd(t, store,
			baseTorrent("after-explicit", infoHash("after-explicit-000000"), 1),
			[]File{{Path: "after-explicit", Size: 1}})
		if !created || id == 424242 || id <= 0 {
			t.Errorf("normal insert after explicit id: created = %v, id = %d", created, id)
		}
		var count int
		conn := openConn(t, dsn)
		err := conn.QueryRow(ctx,
			`SELECT COUNT(*) FROM magneticod.torrents WHERE id IN (424242, $1)`, id).Scan(&count)
		conn.Close(context.Background())
		if err != nil || count != 2 {
			t.Errorf("explicit-id rows: count = %d, err = %v; want 2", count, err)
		}
	})
}

// blobCount counts the blob rows of one torrent.
func blobCount(t *testing.T, dsn string, id int32) int {
	t.Helper()
	conn := openConn(t, dsn)
	defer conn.Close(context.Background())
	var n int
	err := conn.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM magneticod.torrent_file_blobs WHERE torrent_id = $1`, id).Scan(&n)
	if err != nil {
		t.Fatalf("blob count: %v", err)
	}
	return n
}

// exec runs one administrative statement as the test database's owner.
func execSQL(t *testing.T, dsn, sql string, args ...any) {
	t.Helper()
	conn := openConn(t, dsn)
	defer conn.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
	defer cancel()
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func TestAddTorrentConcurrentSameInfoHash(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)

	meta := baseTorrent("racer", infoHash("concurrent-race-00000"), 300)
	files := []File{{Path: "a.txt", Size: 100}, {Path: "b.txt", Size: 200}}

	const n = 8
	results := make([]struct {
		created bool
		id      int32
	}, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			results[i].created, results[i].id, errs[i] = store.AddTorrent(ctx, meta, files)
		}(i)
	}
	wg.Wait()

	createdCount := 0
	for i, err := range errs {
		if err != nil {
			t.Errorf("AddTorrent() %d error = %v", i, err)
			continue
		}
		if results[i].created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Errorf("created = true %d times, want exactly 1", createdCount)
	}
	// Exactly one row survived, and its stored fields are the offered ones.
	got, err := store.QueryTorrents(context.Background(), TorrentQuery{SearchString: "racer", MaxTuples: 10})
	if err != nil || len(got) != 1 {
		t.Fatalf("rows after the race = %d, err = %v; want exactly 1", len(got), err)
	}
	if got[0].Name != meta.Name || got[0].TotalSize != meta.TotalSize || got[0].FileCount != 2 {
		t.Errorf("raced row = %+v, want the offered metadata", got[0])
	}
}

func TestAddTorrentRollsBackOnBlobFailure(t *testing.T) {
	t.Parallel()

	store, dsn := newTestStore(t)

	// A trigger that rejects the blob insert for a marked torrent name: any
	// failure after the torrents INSERT must roll back the whole transaction.
	execSQL(t, dsn, `CREATE FUNCTION magneticod.reject_rollback_target() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN
            IF EXISTS (SELECT 1 FROM magneticod.torrents
                       WHERE id = NEW.torrent_id AND name = 'rollback-target') THEN
                RAISE EXCEPTION 'rejected blob for rollback-target';
            END IF;
            RETURN NEW;
        END $$`)
	execSQL(t, dsn, `CREATE TRIGGER trg_reject_rollback_target
        BEFORE INSERT ON magneticod.torrent_file_blobs
        FOR EACH ROW EXECUTE FUNCTION magneticod.reject_rollback_target()`)

	meta := baseTorrent("rollback-target", infoHash("rollback-target-00000"), 300)
	_, _, err := store.AddTorrent(context.Background(), meta,
		[]File{{Path: "a.txt", Size: 100}, {Path: "b.txt", Size: 200}})
	if !errors.Is(err, ErrStorageIO) {
		t.Fatalf("AddTorrent() error = %v, want ErrStorageIO", err)
	}

	if exists, err := store.DoesTorrentExist(context.Background(), meta.InfoHash); err != nil || exists {
		t.Errorf("torrent row survived the rolled-back transaction: exists = %v, err = %v", exists, err)
	}
}

func TestQueryTorrents(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	seed := []struct {
		name      string
		tag       string
		total     int64
		discovery int64
	}{
		{"100% Done", "q-percent-000000000", 500, 1700000010},
		{"under_score", "q-underscore-000000", 400, 1700000020},
		{`back\slash`, "q-backslash-00000", 300, 1700000030},
		{"O'Brien", "q-quote-0000000000", 200, 1700000040},
		{"Case Study", "q-case-00000000000", 100, 1700000040},
		// Decoys that would match if escaping were broken.
		{"00X Do", "q-decoy-1-00000000", 100, 1700000050},
		{"underXsco", "q-decoy-2-00000000", 100, 1700000050},
		{"backXslash", "q-decoy-3-00000000", 100, 1700000050},
	}
	for _, s := range seed {
		mustAdd(t, store, Torrent{
			InfoHash: infoHash(s.tag), Name: s.name,
			TotalSize: s.total, DiscoveredOn: s.discovery,
		}, []File{{Path: s.name, Size: s.total}})
	}

	// Literal metacharacters must match exactly.
	literal := []struct{ search, want string }{
		{"00% Do", "100% Done"},
		{"under_sco", "under_score"},
		{`ck\sla`, `back\slash`},
		{"o'brie", "O'Brien"},
	}
	for _, tt := range literal {
		got, err := store.QueryTorrents(ctx, TorrentQuery{SearchString: tt.search, MaxTuples: 10})
		if err != nil {
			t.Fatalf("QueryTorrents(%q) error = %v", tt.search, err)
		}
		if len(got) != 1 || got[0].Name != tt.want {
			names := make([]string, len(got))
			for i, r := range got {
				names[i] = r.Name
			}
			t.Errorf("QueryTorrents(%q) = %v, want exactly [%s]", tt.search, names, tt.want)
		}
	}

	// Case-insensitive substring.
	got, err := store.QueryTorrents(ctx, TorrentQuery{SearchString: "case ST", MaxTuples: 10})
	if err != nil || len(got) != 1 || got[0].Name != "Case Study" {
		t.Errorf("QueryTorrents(case-insensitive) = %+v, err = %v", got, err)
	}

	// Empty search: everything, newest discovery first.
	got, err = store.QueryTorrents(ctx, TorrentQuery{MaxTuples: 100})
	if err != nil {
		t.Fatalf("QueryTorrents(all) error = %v", err)
	}
	if len(got) != len(seed) {
		t.Fatalf("QueryTorrents(all) returned %d rows, want %d", len(got), len(seed))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].DiscoveredOn < got[i].DiscoveredOn {
			t.Errorf("row %d discovered_on %d sorts after %d; not ordered",
				i, got[i-1].DiscoveredOn, got[i].DiscoveredOn)
		}
	}
	// Deterministic tie-break: same discovered_on, higher id first.
	tieIdx := -1
	for i := 1; i < len(got); i++ {
		if got[i-1].DiscoveredOn == got[i].DiscoveredOn {
			tieIdx = i
			break
		}
	}
	if tieIdx > 0 && got[tieIdx-1].ID <= got[tieIdx].ID {
		t.Errorf("tie-break violated: ids %d then %d at index %d",
			got[tieIdx-1].ID, got[tieIdx].ID, tieIdx)
	}

	// Total-size ordering.
	got, err = store.QueryTorrents(ctx, TorrentQuery{OrderBy: ByTotalSizeDesc, MaxTuples: 3})
	if err != nil || len(got) != 3 {
		t.Fatalf("QueryTorrents(by size) = %d rows, err = %v", len(got), err)
	}
	if got[0].TotalSize != 500 {
		t.Errorf("largest total_size = %d, want 500", got[0].TotalSize)
	}

	// Limit.
	got, err = store.QueryTorrents(ctx, TorrentQuery{MaxTuples: 2})
	if err != nil || len(got) != 2 {
		t.Errorf("QueryTorrents(limit 2) = %d rows, err = %v", len(got), err)
	}

	// Invalid queries.
	if _, err := store.QueryTorrents(ctx, TorrentQuery{MaxTuples: 0}); !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("MaxTuples=0 error = %v, want ErrInvalidMetadata", err)
	}
	if _, err := store.QueryTorrents(ctx, TorrentQuery{MaxTuples: math.MaxInt32 + 1}); !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("MaxTuples>MaxInt32 error = %v, want ErrInvalidMetadata (must not truncate)", err)
	}
	if _, err := store.QueryTorrents(ctx, TorrentQuery{MaxTuples: 5, OrderBy: TorrentOrder(99)}); !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("unknown ordering error = %v, want ErrInvalidMetadata", err)
	}
}

func TestGetStatistics(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stats, err := store.GetStatistics(ctx)
	if err != nil || stats.Torrents != 0 || stats.Files != 0 || stats.TotalSize != 0 {
		t.Fatalf("empty statistics = %+v, err = %v", stats, err)
	}

	seeds := []struct {
		tag   string
		files []File
	}{
		{"stats-a-0000000000", []File{{Path: "stats-a", Size: 1000}}},
		{"stats-b-0000000000", []File{{Path: "x/a", Size: 10}, {Path: "x/b", Size: 20}}},
		{"stats-c-0000000000", []File{{Path: "stats-c", Size: 7}}},
	}
	var wantFiles, wantTotal uint64
	for _, s := range seeds {
		var sum int64
		for _, f := range s.files {
			sum += f.Size
		}
		mustAdd(t, store, Torrent{
			InfoHash: infoHash(s.tag), Name: s.tag,
			TotalSize: sum, DiscoveredOn: 1700000000,
		}, s.files)
		wantFiles += uint64(len(s.files))
		wantTotal += uint64(sum)
	}

	stats, err = store.GetStatistics(ctx)
	if err != nil {
		t.Fatalf("GetStatistics() error = %v", err)
	}
	if stats.Torrents != 3 || stats.Files != wantFiles || stats.TotalSize != wantTotal {
		t.Errorf("statistics = %+v, want {3 %d %d}", stats, wantFiles, wantTotal)
	}
}

func TestAddTorrentValidationErrors(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rejects := []struct {
		name  string
		meta  Torrent
		files []File
	}{
		{
			name:  "infohash 19 bytes",
			meta:  Torrent{InfoHash: make([]byte, 19), Name: "x", TotalSize: 1, DiscoveredOn: 1},
			files: []File{{Path: "x", Size: 1}},
		},
		{
			name:  "infohash 21 bytes",
			meta:  Torrent{InfoHash: make([]byte, 21), Name: "x", TotalSize: 1, DiscoveredOn: 1},
			files: []File{{Path: "x", Size: 1}},
		},
		{
			name:  "zero total size",
			meta:  Torrent{InfoHash: infoHash("invalid-zero-total"), Name: "x", TotalSize: 0, DiscoveredOn: 1},
			files: []File{{Path: "x", Size: 0}},
		},
		{
			name:  "sum mismatch",
			meta:  Torrent{InfoHash: infoHash("invalid-sum-00000"), Name: "x", TotalSize: 99, DiscoveredOn: 1},
			files: []File{{Path: "x", Size: 1}},
		},
		{
			name:  "empty file list",
			meta:  Torrent{InfoHash: infoHash("invalid-empty-0000"), Name: "x", TotalSize: 1, DiscoveredOn: 1},
			files: nil,
		},
		{
			name:  "NUL in path",
			meta:  Torrent{InfoHash: infoHash("invalid-nul-000000"), Name: "x", TotalSize: 2, DiscoveredOn: 1},
			files: []File{{Path: "a\x00b", Size: 2}},
		},
	}
	for _, tt := range rejects {
		_, _, err := store.AddTorrent(ctx, tt.meta, tt.files)
		if !errors.Is(err, ErrInvalidMetadata) {
			t.Errorf("%s: AddTorrent() error = %v, want ErrInvalidMetadata", tt.name, err)
		}
	}

	// A codec encode failure from oversized input is metadata-classified,
	// never a storage error: the caller drops the record instead of counting
	// it against the database.
	long := strings.Repeat("p", codec.DefaultLimits().MaxPathBytes+1)
	_, _, err := store.AddTorrent(ctx,
		Torrent{InfoHash: infoHash("invalid-longpath-000"), Name: "x", TotalSize: 1, DiscoveredOn: 1},
		[]File{{Path: long, Size: 1}})
	if !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("oversized path: AddTorrent() error = %v, want ErrInvalidMetadata", err)
	}
	if errors.Is(err, ErrStorageIO) {
		t.Errorf("oversized path must never surface as ErrStorageIO: %v", err)
	}
}

func TestBlobSizeCapClassifiedAsInvalidMetadata(t *testing.T) {
	t.Parallel()

	dsn := integrationCluster(t).testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := InitSchema(ctx, dsn); err != nil {
		t.Fatalf("InitSchema() error = %v", err)
	}

	// A store with a tiny blob budget: a multi-file list whose framed blob
	// exceeds it fails during encode, which is a metadata problem for the
	// caller, not an I/O error.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	limits := codec.DefaultLimits()
	limits.MaxStoredBytes = 1024
	store := newPostgresStore(pool, codec.ProfileCompact, limits)

	// Three files with distinct 2048-byte names of uncorrelated content:
	// even the best-compressed candidate of the compact profile exceeds the
	// tiny budget. (Names sharing a long prefix, or with internal
	// repetition, would keep some candidate under the cap and legitimately
	// succeed.)
	names := make([]string, 3)
	for i := range names {
		for k := 0; k < 32; k++ {
			h := sha256.Sum256([]byte(fmt.Sprintf("cap-%d-%d", i, k)))
			names[i] += hex.EncodeToString(h[:])
		}
	}
	files := []File{
		{Path: names[0], Size: 1},
		{Path: names[1], Size: 1},
		{Path: names[2], Size: 1},
	}
	_, _, err = store.AddTorrent(ctx,
		Torrent{InfoHash: infoHash("cap-exceeded-000000"), Name: "cap", TotalSize: 3, DiscoveredOn: 1},
		files)
	if !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("oversized blob: AddTorrent() error = %v, want ErrInvalidMetadata", err)
	}
	if errors.Is(err, ErrStorageIO) {
		t.Errorf("encode cap must never surface as ErrStorageIO: %v", err)
	}
	if exists, err := store.DoesTorrentExist(ctx, infoHash("cap-exceeded-000000")); err != nil || exists {
		t.Errorf("rejected torrent left a row behind: exists = %v, err = %v", exists, err)
	}
}

func TestAddTorrentLargeFileList(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("heavy round-trip, skipped in -short runs")
	}

	store, _ := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	const count = 250000
	files := make([]File, count)
	var sum int64
	for i := range files {
		files[i] = File{Path: fmt.Sprintf("d/f%06d.bin", i), Size: int64(i)}
		sum += int64(i)
	}

	meta := Torrent{
		InfoHash: infoHash("large-250k-00000000"), Name: "large",
		TotalSize: sum, DiscoveredOn: 1700000000,
	}
	created, _, err := store.AddTorrent(ctx, meta, files)
	if err != nil || !created {
		t.Fatalf("AddTorrent(250k) = (%v, %v)", created, err)
	}

	got, err := store.GetFiles(ctx, meta.InfoHash)
	if err != nil {
		t.Fatalf("GetFiles(250k) error = %v", err)
	}
	if len(got) != count {
		t.Fatalf("GetFiles(250k) returned %d files, want %d", len(got), count)
	}
	for i := range got {
		if got[i] != files[i] {
			t.Fatalf("file %d = %+v, want %+v (round-trip mismatch)", i, got[i], files[i])
		}
	}

	// One over the limit must be rejected by ingestion validation.
	oneOver := make([]File, count+1)
	copy(oneOver, files)
	oneOver[count] = File{Path: "d/overflow.bin", Size: 0}
	var overSum int64
	for _, f := range oneOver {
		overSum += f.Size
	}
	_, _, err = store.AddTorrent(ctx, Torrent{
		InfoHash: infoHash("large-250k+1-000000"), Name: "large+1",
		TotalSize: overSum, DiscoveredOn: 1700000000,
	}, oneOver)
	if !errors.Is(err, ErrInvalidMetadata) {
		t.Errorf("250001 files: AddTorrent() error = %v, want ErrInvalidMetadata", err)
	}
}

// TestRestrictedRoleNoBlobDecodeAccess is the honest no-blob proof: a login
// role that may read torrents and schema_version but has NO privileges on
// torrent_file_blobs can serve every torrent-column read path, while GetFiles
// fails specifically on blob-table permission (42501 naming torrent_file_blobs
// — not a schema-usage failure). The store is constructed directly: the role
// cannot even see torrent_file_blobs in the catalog, which is exactly the
// privilege boundary under test, so the owner-level startup verification is
// not part of this scenario.
func TestRestrictedRoleNoBlobDecodeAccess(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("role/permission setup, skipped in -short runs")
	}

	dsn := integrationCluster(t).testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := InitSchema(ctx, dsn); err != nil {
		t.Fatalf("InitSchema() error = %v", err)
	}

	role := "magnetico_test_role_" + randomSuffix()
	execSQL(t, dsn, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'restricted-test-pw'`, quoteIdent(role)))
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), dbOpTimeout)
		defer cancel()
		conn, err := pgx.Connect(cleanupCtx, dsn)
		if err != nil {
			return
		}
		defer conn.Close(context.Background())
		_, _ = conn.Exec(cleanupCtx, fmt.Sprintf(`DROP OWNED BY %s`, quoteIdent(role)))
		_, _ = conn.Exec(cleanupCtx, fmt.Sprintf(`DROP ROLE %s`, quoteIdent(role)))
	})

	execSQL(t, dsn, fmt.Sprintf(`GRANT USAGE ON SCHEMA magneticod TO %s`, quoteIdent(role)))
	execSQL(t, dsn, fmt.Sprintf(`GRANT SELECT ON magneticod.torrents, magneticod.schema_version TO %s`, quoteIdent(role)))
	execSQL(t, dsn, fmt.Sprintf(`REVOKE ALL ON magneticod.torrent_file_blobs FROM %s`, quoteIdent(role)))

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	parsed.User = url.UserPassword(role, "restricted-test-pw")

	restrictedPool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("restricted pool: %v", err)
	}
	defer restrictedPool.Close()
	restricted := newPostgresStore(restrictedPool, codec.DefaultProfile(), codec.DefaultLimits())

	// Seed a multi-file torrent as the owner.
	ownerStore, err := MakeDatabase(ctx, dsn)
	if err != nil {
		t.Fatalf("owner MakeDatabase() error = %v", err)
	}
	defer ownerStore.Close()
	meta := baseTorrent("restricted-probe", infoHash("restricted-probe-0000"), 300)
	if _, _, err := ownerStore.AddTorrent(ctx, meta,
		[]File{{Path: "a.txt", Size: 100}, {Path: "b.txt", Size: 200}}); err != nil {
		t.Fatalf("seed AddTorrent() error = %v", err)
	}

	// Torrent-column read paths work.
	if _, err := restricted.GetTorrent(ctx, meta.InfoHash); err != nil {
		t.Errorf("restricted GetTorrent() error = %v, want success", err)
	}
	if got, err := restricted.QueryTorrents(ctx, TorrentQuery{SearchString: "restricted", MaxTuples: 5}); err != nil || len(got) != 1 {
		t.Errorf("restricted QueryTorrents() = %d rows, err = %v; want 1 row", len(got), err)
	}
	if stats, err := restricted.GetStatistics(ctx); err != nil || stats.Torrents != 1 {
		t.Errorf("restricted GetStatistics() = %+v, err = %v; want 1 torrent", stats, err)
	}

	// GetFiles fails specifically on the blob table. PostgreSQL's
	// insufficient-privilege errors carry the denied relation in the message
	// text (the structured table field stays empty), so discriminate there:
	// a schema-usage failure would name the schema instead.
	_, err = restricted.GetFiles(ctx, meta.InfoHash)
	if !errors.Is(err, ErrStorageIO) {
		t.Fatalf("restricted GetFiles() error = %v, want ErrStorageIO", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("restricted GetFiles() error = %v, want an underlying *pgconn.PgError", err)
	}
	if pgErr.Code != "42501" {
		t.Errorf("SQLSTATE = %s, want 42501 (insufficient_privilege)", pgErr.Code)
	}
	if !strings.Contains(pgErr.Message, "torrent_file_blobs") {
		t.Errorf("denial message %q does not name torrent_file_blobs", pgErr.Message)
	}
}
