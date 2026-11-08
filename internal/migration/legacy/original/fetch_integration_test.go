package original

// FetchByIDs and WindowSnapshot: the sparse-ID set-oriented fetch (retry
// passes) and the per-page REPEATABLE READ snapshot read (verify-content).

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestFetchByIDsSparseSetSortedDeduped(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	seedSample(t, dsn)
	before := catalogSnapshot(t, dsn)
	src := openSource(t, dsn)
	ctx := context.Background()

	// Unordered, duplicated, and one absent ID: the fetch must normalize
	// internally, attach complete file lists and never invent rows.
	w, remaining, err := src.FetchByIDs(ctx, []int64{5, 2, 2, 99}, defaultWindowLimits())
	if err != nil {
		t.Fatalf("FetchByIDs() error = %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining = %v, want nil (no budget cut)", remaining)
	}
	if len(w.Torrents) != 2 || w.Torrents[0].ID != 2 || w.Torrents[1].ID != 5 {
		t.Fatalf("torrents = %+v, want IDs [2 5] ascending", w.Torrents)
	}
	beta := w.Files[2]
	if len(beta) != 2 || beta[0].Path != "dup.bin" || beta[1].Path != "dup.bin" ||
		beta[0].Size != 25 || beta[1].Size != 25 {
		t.Fatalf("beta duplicates not preserved: %+v", beta)
	}
	if len(w.Files[5]) != 1 || w.Files[5][0].Path != "delta/single" {
		t.Fatalf("delta files = %+v", w.Files[5])
	}
	if w.OrphanFiles != 0 {
		t.Fatalf("orphans = %d, want 0", w.OrphanFiles)
	}
	if after := catalogSnapshot(t, dsn); after != before {
		t.Fatalf("FetchByIDs changed the source:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestFetchByIDsBudgetSplitWithoutLoss(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	src := openSource(t, dsn)
	ctx := context.Background()

	// 8 torrents × two 200-byte paths; a 500-byte budget splits the batch.
	long := strings.Repeat("p", 200)
	ids := make([]int64, 0, 8)
	for id := int32(1); id <= 8; id++ {
		seedTorrent(t, dsn, id, fmt.Sprintf("%040x", id), fmt.Sprintf("t%d", id), 2, 1700000000+id)
		for i := 0; i < 2; i++ {
			seedFile(t, dsn, 100*id+int32(i), &id, fmt.Sprintf("%s.%d", long, i), 1)
		}
		ids = append(ids, int64(id))
	}

	limits := WindowLimits{MaxFiles: 100, MaxPathBytes: 1 << 20, WindowMaxBytes: 500}
	fetched := map[int32]int{}
	batches := 0
	for {
		w, remaining, err := src.FetchByIDs(ctx, ids, limits)
		if err != nil {
			t.Fatalf("FetchByIDs() error = %v", err)
		}
		batches++
		if len(w.Torrents) == 0 {
			break
		}
		// No metadata-without-files corruption across the split.
		for _, tr := range w.Torrents {
			if len(w.Files[tr.ID]) != 2 {
				t.Fatalf("torrent %d kept %d file rows, want its complete list of 2", tr.ID, len(w.Files[tr.ID]))
			}
			fetched[tr.ID]++
		}
		if len(remaining) == 0 {
			break
		}
		ids = remaining
		if batches > 16 {
			t.Fatal("budget-split iteration did not terminate")
		}
	}
	if len(fetched) != 8 {
		t.Fatalf("fetched %d distinct torrents across %d batches, want all 8", len(fetched), batches)
	}
	for id, n := range fetched {
		if n != 1 {
			t.Fatalf("torrent %d fetched %d times, want exactly once", id, n)
		}
	}
}

func TestFetchByIDsAdmitsOversizedSolo(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	huge := strings.Repeat("big", 700) // 2100 bytes against a 1 KiB budget
	seedTorrent(t, dsn, 1, hashAA, huge, 1, 1700000001)
	seedTorrent(t, dsn, 2, hashBB, "normal", 1, 1700000002)
	owner := int32(1)
	seedFile(t, dsn, 1, &owner, "huge/file", 1)
	owner2 := int32(2)
	seedFile(t, dsn, 2, &owner2, "normal/file", 1)
	src := openSource(t, dsn)
	ctx := context.Background()

	limits := WindowLimits{MaxFiles: 100, MaxPathBytes: 1 << 20, WindowMaxBytes: 1024}
	w, remaining, err := src.FetchByIDs(ctx, []int64{1, 2}, limits)
	if err != nil {
		t.Fatalf("FetchByIDs() error = %v", err)
	}
	if len(w.Torrents) != 1 || w.Torrents[0].ID != 1 || w.Torrents[0].Name != huge {
		t.Fatalf("torrents = %+v, want exactly the oversized torrent admitted alone", w.Torrents)
	}
	if len(w.Files[1]) != 1 {
		t.Fatalf("oversized torrent lost its file rows: %+v", w.Files[1])
	}
	if len(remaining) != 1 || remaining[0] != 2 {
		t.Fatalf("remaining = %v, want [2]", remaining)
	}

	w2, rem2, err := src.FetchByIDs(ctx, remaining, limits)
	if err != nil {
		t.Fatalf("follow-up FetchByIDs() error = %v", err)
	}
	if len(w2.Torrents) != 1 || w2.Torrents[0].ID != 2 || rem2 != nil {
		t.Fatalf("follow-up = %+v remaining %v, want torrent 2 and no remainder", w2.Torrents, rem2)
	}
	if len(w2.Files[2]) != 1 {
		t.Fatalf("follow-up files = %+v", w2.Files[2])
	}
}

func TestFetchByIDsOverflowDrained(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	seedTorrent(t, dsn, 1, hashAA, "normal", 3, 1700000000)
	seedTorrent(t, dsn, 2, hashBB, "oversized", 2, 1700000100)
	owner1, owner2 := int32(1), int32(2)
	for i := int32(0); i < 3; i++ {
		seedFile(t, dsn, 10+i, &owner1, fmt.Sprintf("n%d", i), 1)
	}
	for i := int32(0); i < 10; i++ {
		seedFile(t, dsn, 50+i, &owner2, fmt.Sprintf("big/%d", i), 0)
	}
	src := openSource(t, dsn)

	w, remaining, err := src.FetchByIDs(context.Background(), []int64{1, 2},
		WindowLimits{MaxFiles: 5, MaxPathBytes: 1 << 20})
	if err != nil {
		t.Fatalf("FetchByIDs() error = %v", err)
	}
	if remaining != nil {
		t.Fatalf("remaining = %v, want nil", remaining)
	}
	if ov := w.Overflow[2]; ov.Observed != 10 || ov.Reason != OverflowFileCount {
		t.Fatalf("overflow accounting = %+v, want 10 rows of %q", ov, OverflowFileCount)
	}
	if len(w.Files[2]) != 0 {
		t.Fatalf("drained torrent kept %d rows, want none", len(w.Files[2]))
	}
	if len(w.Files[1]) != 3 {
		t.Fatalf("normal torrent kept %d rows, want 3", len(w.Files[1]))
	}
}

func TestFetchByIDsEmptyAndValidation(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	src := openSource(t, dsn)
	ctx := context.Background()

	w, remaining, err := src.FetchByIDs(ctx, nil, defaultWindowLimits())
	if err != nil {
		t.Fatalf("FetchByIDs(nil) error = %v", err)
	}
	if len(w.Torrents) != 0 || remaining != nil {
		t.Fatalf("empty fetch = %+v remaining %v, want empty/nil", w.Torrents, remaining)
	}
	if _, _, err := src.FetchByIDs(ctx, []int64{1}, WindowLimits{MaxFiles: 0, MaxPathBytes: 1 << 20}); err == nil {
		t.Fatal("FetchByIDs accepted MaxFiles=0")
	}
	if _, _, err := src.FetchByIDs(ctx, []int64{1}, WindowLimits{MaxFiles: 10, MaxPathBytes: 0}); err == nil {
		t.Fatal("FetchByIDs accepted MaxPathBytes=0")
	}
}

func TestWindowSnapshotMatchesWindowAndSeesFreshData(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	seedSample(t, dsn)
	src := openSource(t, dsn)
	ctx := context.Background()

	w, err := src.WindowSnapshot(ctx, 0, 10, defaultWindowLimits())
	if err != nil {
		t.Fatalf("WindowSnapshot() error = %v", err)
	}
	ref, err := src.Window(ctx, 0, 10, defaultWindowLimits())
	if err != nil {
		t.Fatalf("Window() error = %v", err)
	}
	if len(w.Torrents) != len(ref.Torrents) {
		t.Fatalf("snapshot torrents = %d, window = %d", len(w.Torrents), len(ref.Torrents))
	}
	for i := range ref.Torrents {
		want, got := ref.Torrents[i], w.Torrents[i]
		if got.ID != want.ID || got.Name != want.Name || got.TotalSize != want.TotalSize ||
			got.DiscoveredOn != want.DiscoveredOn || !bytesEqual(got.InfoHash, want.InfoHash) {
			t.Fatalf("snapshot torrent %d = %+v, window = %+v", i, got, want)
		}
		if len(w.Files[ref.Torrents[i].ID]) != len(ref.Files[ref.Torrents[i].ID]) {
			t.Fatalf("snapshot files for %d: %d rows, window: %d",
				ref.Torrents[i].ID, len(w.Files[ref.Torrents[i].ID]), len(ref.Files[ref.Torrents[i].ID]))
		}
	}
	if w.OrphanFiles != ref.OrphanFiles {
		t.Fatalf("snapshot orphans %d != window %d", w.OrphanFiles, ref.OrphanFiles)
	}
	if w.RetainedBytes != ref.RetainedBytes {
		t.Fatalf("snapshot retained %d != window %d", w.RetainedBytes, ref.RetainedBytes)
	}
	if w.Truncated() {
		t.Fatal("snapshot reported truncated on a complete range")
	}

	// The snapshot transaction is fully released: a later read sees writes.
	execSQL(t, dsn, `UPDATE magneticod.torrents SET name = 'renamed' WHERE id = 1`)
	w2, err := src.WindowSnapshot(ctx, 0, 10, defaultWindowLimits())
	if err != nil {
		t.Fatalf("second WindowSnapshot() error = %v", err)
	}
	if w2.Torrents[0].Name != "renamed" {
		t.Fatalf("second snapshot name = %q, want the committed update", w2.Torrents[0].Name)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFetchByIDsBudgetChargesInfohashBytes(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	// 32 torrents with distinct 65,538-byte infohash bytea (the legacy
	// schema imposes no hash-length constraint) and short names: the
	// retained hash bytes alone are ~2 MiB against a 1 MiB window budget,
	// so the budget must stop the metadata scan well before the last row.
	// Not charging the hash retained every torrent while reporting a
	// fraction of the real bytes (Codex review iteration 4, finding 2).
	big := strings.Repeat("ab", 65536) // the shared tail; the id prefixes it
	ids := make([]int64, 0, 32)
	for id := int32(1); id <= 32; id++ {
		seedTorrent(t, dsn, id, fmt.Sprintf("%02xab", id)+big, fmt.Sprintf("t%d", id), 1, 1700000000+id)
		ids = append(ids, int64(id))
	}
	src := openSource(t, dsn)
	ctx := context.Background()

	w, remaining, err := src.FetchByIDs(ctx, ids,
		WindowLimits{MaxFiles: 100, MaxPathBytes: 1 << 20, WindowMaxBytes: 1 << 20})
	if err != nil {
		t.Fatalf("FetchByIDs() error = %v", err)
	}
	if len(w.Torrents) >= 32 || len(remaining) == 0 {
		t.Fatalf("window kept %d of 32 torrents with remaining %d — infohash bytes were not charged to the window budget",
			len(w.Torrents), len(remaining))
	}
	if w.RetainedBytes > 1<<20 {
		t.Fatalf("retained %d bytes over the 1 MiB window budget", w.RetainedBytes)
	}
}
