package original

// Forensic snapshot integration tests (04C): complete, uncapped file-list
// streaming, sparse metadata fetch, and the one-snapshot discipline.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	t.Cleanup(cancel)
	return ctx
}

func ptrInt32(v int32) *int32 { return &v }

func TestStreamFileRowsBeyondCodecLimits(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	dsn := createLegacySource(t)
	src := openSource(t, dsn)

	seedTorrent(t, dsn, 1, fmt.Sprintf("%040x", 1), "huge", 300, 1700000000)
	// 300 file rows — far past any typical window — including exact
	// duplicate (path, size) entries. Every row must arrive, with its
	// source file ID and duplicates preserved: no forensic truncation.
	const wantFiles = 300
	for i := 1; i <= wantFiles; i++ {
		path := fmt.Sprintf("dir/file_%03d.bin", i)
		if i%50 == 0 {
			path = "dir/duplicate.bin"
		}
		seedFile(t, dsn, int32(i), ptrInt32(1), path, int64(i))
	}

	var got []FileRow
	err := src.WithSnapshot(ctx, func(_ context.Context, snap *Snapshot) error {
		return snap.StreamFileRows(ctx, []int64{1}, func(f FileRow) error {
			got = append(got, f)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("StreamFileRows error = %v", err)
	}
	if len(got) != wantFiles {
		t.Fatalf("streamed %d file rows, want all %d", len(got), wantFiles)
	}
	lastID := int32(0)
	for i, f := range got {
		if f.TorrentID != 1 {
			t.Fatalf("row %d has torrent_id %d", i, f.TorrentID)
		}
		if f.ID <= lastID {
			t.Fatalf("row %d id %d not ascending (previous %d)", i, f.ID, lastID)
		}
		lastID = f.ID
	}
	dups := 0
	for _, f := range got {
		if f.Path == "dir/duplicate.bin" {
			dups++
		}
	}
	if dups != wantFiles/50 {
		t.Fatalf("duplicate (path,size) entries = %d, want %d", dups, wantFiles/50)
	}

	// Empty selection streams nothing.
	err = src.WithSnapshot(ctx, func(_ context.Context, snap *Snapshot) error {
		return snap.StreamFileRows(ctx, nil, func(FileRow) error {
			t.Error("stream invoked with no torrents selected")
			return nil
		})
	})
	if err != nil {
		t.Fatalf("empty StreamFileRows error = %v", err)
	}
}

func TestFetchTorrentRowsSparse(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	dsn := createLegacySource(t)
	src := openSource(t, dsn)

	seedTorrent(t, dsn, 1, fmt.Sprintf("%040x", 1), "one", 10, 1700000001)
	seedTorrent(t, dsn, 3, fmt.Sprintf("%040x", 3), "three", 30, 1700000003)
	seedTorrent(t, dsn, 7, fmt.Sprintf("%040x", 7), "seven", 70, 1700000007)

	var rows []TorrentRow
	err := src.WithSnapshot(ctx, func(_ context.Context, snap *Snapshot) error {
		var err error
		rows, err = snap.FetchTorrentRows(ctx, []int64{7, 1, 999, 1})
		return err
	})
	if err != nil {
		t.Fatalf("fetch error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (present IDs only, deduplicated, absent simply absent)", len(rows))
	}
	if rows[0].ID != 1 || rows[1].ID != 7 {
		t.Fatalf("rows not ordered by id: %+v", rows)
	}
	if string(rows[0].InfoHash) == "" || rows[0].Name != "one" || rows[0].TotalSize != 10 {
		t.Fatalf("row 0 fields lost: %+v", rows[0])
	}
}

func TestWithSnapshotRepeatableReadIsolation(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	dsn := createLegacySource(t)
	src := openSource(t, dsn)

	seedTorrent(t, dsn, 1, fmt.Sprintf("%040x", 1), "one", 10, 1700000001)
	seedFile(t, dsn, 1, ptrInt32(1), "a.bin", 10)

	// A second connection mutates and COMMITS after the snapshot's first
	// read: REPEATABLE READ must keep the snapshot's SECOND query (the file
	// stream) at the pre-commit state — proving both batch queries share
	// one snapshot.
	mutator, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer mutator.Close(context.Background())

	var seen []FileRow
	err = src.WithSnapshot(ctx, func(ctx context.Context, snap *Snapshot) error {
		rows, err := snap.FetchTorrentRows(ctx, []int64{1})
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].TotalSize != 10 {
			t.Fatalf("initial read = %+v", rows)
		}
		if _, err := mutator.Exec(ctx, `UPDATE magneticod.files SET size = 999 WHERE id = 1`); err != nil {
			return err
		}
		if _, err := mutator.Exec(ctx, `INSERT INTO magneticod.files (id, torrent_id, size, path)
            VALUES (2, 1, 5, 'late.bin')`); err != nil {
			return err
		}
		return snap.StreamFileRows(ctx, []int64{1}, func(f FileRow) error {
			seen = append(seen, f)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("WithSnapshot error = %v", err)
	}
	if len(seen) != 1 || seen[0].ID != 1 || seen[0].Size != 10 {
		t.Fatalf("snapshot saw %v, want exactly the pre-commit single row — the two batch queries did not share one snapshot", seen)
	}

	// A fresh snapshot sees the committed state: the mutation did land.
	var post []FileRow
	err = src.WithSnapshot(ctx, func(ctx context.Context, snap *Snapshot) error {
		return snap.StreamFileRows(ctx, []int64{1}, func(f FileRow) error {
			post = append(post, f)
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(post) != 2 {
		t.Fatalf("post-commit snapshot sees %d rows, want 2 (mutation committed)", len(post))
	}
}

func TestWithSnapshotFnErrorRollsBack(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	dsn := createLegacySource(t)
	src := openSource(t, dsn)
	seedTorrent(t, dsn, 1, fmt.Sprintf("%040x", 1), "one", 10, 1700000001)

	boom := errors.New("boom")
	if err := src.WithSnapshot(ctx, func(ctx context.Context, snap *Snapshot) error {
		rows, err := snap.FetchTorrentRows(ctx, []int64{1})
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			t.Fatalf("read = %+v", rows)
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("WithSnapshot error = %v, want the fn error propagated", err)
	}
	// The read-only session guarantees no mutation; the rollback path must
	// at least leave the source readable for the next snapshot.
	var n int
	if err := pgx.BeginFunc(ctx, src.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM magneticod.torrents`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("post-rollback read = %d rows, err %v", n, err)
	}
}
