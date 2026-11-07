package original

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Suite ① (reader part): bounded keyset windows, duplicate preservation,
// NULL-owner exclusion, orphan accounting, and the oversized-torrent drain.

func seedSample(t *testing.T, dsn string) {
	t.Helper()
	// Torrent IDs with a gap at 4 (deleted in the source's history).
	seedTorrent(t, dsn, 1, hashAA, "alpha", 100, 1700000000)
	seedTorrent(t, dsn, 2, hashBB, "beta", 50, 1700000100)
	seedTorrent(t, dsn, 3, hashCC, "gamma", 30, 1700000200)
	seedTorrent(t, dsn, 5, "5555555555555555555555555555555555555555", "delta", 7, 1700000300)

	one, two, three, five := int32(1), int32(2), int32(3), int32(5)
	seedFile(t, dsn, 1, &one, "alpha/a.txt", 60)
	seedFile(t, dsn, 2, &one, "alpha/b.txt", 40)
	// Duplicate (path, size) pairs must survive.
	seedFile(t, dsn, 3, &two, "dup.bin", 25)
	seedFile(t, dsn, 4, &two, "dup.bin", 25)
	seedFile(t, dsn, 5, &three, "gamma/zero", 0) // zero-size member is legal
	seedFile(t, dsn, 6, nil, "null-owned.txt", 1)
	seedFile(t, dsn, 7, nil, "null-owned-2.txt", 2)
	seedFile(t, dsn, 8, &five, "delta/single", 7)
}

func TestReaderWindowsPreserveDuplicatesAndGaps(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	seedSample(t, dsn)
	src := openSource(t, dsn)
	ctx := context.Background()

	// Window size 2 forces three windows across the ID gap.
	var got []TorrentRow
	totalOrphans := int64(0)
	after := int64(0)
	for {
		w, err := src.Window(ctx, after, 2, defaultWindowLimits())
		if err != nil {
			t.Fatalf("Window(after=%d) error = %v", after, err)
		}
		got = append(got, w.Torrents...)
		totalOrphans += w.OrphanFiles
		if len(w.Torrents) == 0 {
			break
		}
		if len(w.Torrents) > 2 {
			t.Fatalf("window returned %d torrents, want at most 2", len(w.Torrents))
		}
		after = int64(w.Torrents[len(w.Torrents)-1].ID)
	}
	if len(got) != 4 {
		t.Fatalf("read %d torrents, want 4", len(got))
	}
	for i, want := range []int32{1, 2, 3, 5} {
		if got[i].ID != want {
			t.Fatalf("torrent %d: got id %d, want %d (order and gap handling broken)", i, got[i].ID, want)
		}
	}
	if totalOrphans != 0 {
		t.Fatalf("observed %d orphans, want 0", totalOrphans)
	}

	// File grouping: duplicates preserved, NULL-owner rows never attached.
	w, err := src.Window(ctx, 0, 10, defaultWindowLimits())
	if err != nil {
		t.Fatalf("Window() error = %v", err)
	}
	alpha := w.Files[1]
	if len(alpha) != 2 || alpha[0].Path != "alpha/a.txt" || alpha[1].Path != "alpha/b.txt" {
		t.Fatalf("alpha files = %+v", alpha)
	}
	beta := w.Files[2]
	if len(beta) != 2 || beta[0].Path != "dup.bin" || beta[1].Path != "dup.bin" ||
		beta[0].Size != 25 || beta[1].Size != 25 {
		t.Fatalf("beta duplicates not preserved: %+v", beta)
	}
	if len(w.Files[3]) != 1 || w.Files[3][0].Size != 0 {
		t.Fatalf("gamma files = %+v, want the zero-size member", w.Files[3])
	}
	if _, ok := w.Files[4]; ok {
		t.Fatal("gap ID 4 got file rows attached")
	}
}

func TestReaderCountsOrphansWithoutAttaching(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	seedTorrent(t, dsn, 1, hashAA, "alpha", 100, 1700000000)
	seedTorrent(t, dsn, 5, "5555555555555555555555555555555555555555", "delta", 7, 1700000300)
	owner := int32(1)
	seedFile(t, dsn, 1, &owner, "alpha/one.txt", 100)
	// Orphan rows cannot be inserted while the FK enforces them (NOT VALID
	// only skips pre-existing rows), so: drop the FK, insert the orphan,
	// re-add the FK NOT VALID — the historical broken row stays, the probe
	// still sees the constraint, and no new writes are allowed to orphan.
	// The orphan's torrent_id=3 sits in the ID gap between real torrents 1
	// and 5, i.e. inside a scanned window range.
	orphan := int32(3) // no owning torrent
	execSQL(t, dsn, `ALTER TABLE magneticod.files DROP CONSTRAINT files_torrent_id_fkey;`)
	seedFile(t, dsn, 2, &orphan, "orphan.txt", 5)
	execSQL(t, dsn, `ALTER TABLE magneticod.files ADD CONSTRAINT files_torrent_id_fkey
        FOREIGN KEY (torrent_id) REFERENCES magneticod.torrents(id) NOT VALID;`)

	before := catalogSnapshot(t, dsn)
	src := openSource(t, dsn)

	// The fingerprint's anti-join already sees the orphan at preflight.
	fp, err := src.Fingerprint(context.Background())
	if err != nil {
		t.Fatalf("Fingerprint() error = %v", err)
	}
	if fp.OrphanFiles != 1 {
		t.Fatalf("fingerprint orphans = %d, want 1", fp.OrphanFiles)
	}

	w, err := src.Window(context.Background(), 0, 10, defaultWindowLimits())
	if err != nil {
		t.Fatalf("Window() error = %v", err)
	}
	if w.OrphanFiles != 1 {
		t.Fatalf("window orphans = %d, want 1", w.OrphanFiles)
	}
	if got := w.Files[1]; len(got) != 1 || got[0].Path != "alpha/one.txt" {
		t.Fatalf("alpha files = %+v", got)
	}
	if _, ok := w.Files[3]; ok {
		t.Fatal("orphan file rows were attached to a record")
	}
	if after := catalogSnapshot(t, dsn); after != before {
		t.Fatalf("read changed the source:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestReaderDrainsOversizedTorrentWithoutRetention(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	seedTorrent(t, dsn, 1, hashAA, "normal", 3, 1700000000)
	seedTorrent(t, dsn, 2, hashBB, "oversized", 2, 1700000100)
	seedTorrent(t, dsn, 3, hashCC, "after", 1, 1700000200)
	owner1, owner2, owner3 := int32(1), int32(2), int32(3)
	seedFile(t, dsn, 1, &owner1, "n1", 1)
	seedFile(t, dsn, 2, &owner1, "n2", 1)
	seedFile(t, dsn, 3, &owner1, "n3", 1)
	for i := int32(10); i < 20; i++ { // 10 rows for torrent 2
		seedFile(t, dsn, i, &owner2, fmt.Sprintf("big/%d", i), 0)
	}
	seedFile(t, dsn, 20, &owner3, "a3", 1)

	src := openSource(t, dsn)
	w, err := src.Window(context.Background(), 0, 10, WindowLimits{MaxFiles: 5, MaxPathBytes: 1 << 20}) // MaxFiles=5: torrent 2 overflows
	if err != nil {
		t.Fatalf("Window() error = %v", err)
	}
	if len(w.Torrents) != 3 {
		t.Fatalf("torrents = %d, want 3", len(w.Torrents))
	}
	if kept := len(w.Files[2]); kept != 0 {
		t.Fatalf("oversized torrent retained %d rows, want 0 (uncontrolled slice)", kept)
	}
	if ov := w.Overflow[2]; ov.Observed != 10 || ov.Reason != OverflowFileCount {
		t.Fatalf("overflow accounting = %+v, want 10 rows of %q", ov, OverflowFileCount)
	}
	if len(w.Files[1]) != 3 || len(w.Files[3]) != 1 {
		t.Fatalf("normal torrents lost rows: alpha=%d after=%d", len(w.Files[1]), len(w.Files[3]))
	}

	// maxFiles above the row count retains everything, no overflow.
	w2, err := src.Window(context.Background(), 0, 10, defaultWindowLimits())
	if err != nil {
		t.Fatalf("Window() error = %v", err)
	}
	if len(w2.Files[2]) != 10 || len(w2.Overflow) != 0 {
		t.Fatalf("under-limit window misbehaved: kept=%d overflow=%v", len(w2.Files[2]), w2.Overflow)
	}
}

// defaultWindowLimits mirrors the runner's production-derived reader bounds
// for tests that don't exercise the caps themselves.
func defaultWindowLimits() WindowLimits {
	return WindowLimits{MaxFiles: 250000, MaxPathBytes: 1 << 26, WindowMaxBytes: 1 << 26}
}

// The window byte budget truncates the window under backpressure: the
// window stops after the budget fills, the caller's cursor stays at the
// last kept torrent, and iterating windows still reads EVERY torrent
// exactly once — the budget bounds buffering, never coverage.
func TestReaderWindowByteBudgetTruncatesWithoutLoss(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	src := openSource(t, dsn)
	ctx := context.Background()

	// 8 torrents, each with two 200-byte paths (400 bytes retained each).
	long := strings.Repeat("p", 200)
	for id := int32(1); id <= 8; id++ {
		seedTorrent(t, dsn, id, fmt.Sprintf("%040x", id), fmt.Sprintf("t%d", id), 2, 1700000000+int32(id))
		for i := 0; i < 2; i++ {
			seedFile(t, dsn, 100*int32(id)+int32(i), &id, fmt.Sprintf("%s.%d", long, i), 1)
		}
	}

	// Budget of 500 bytes: the window stops at the first group boundary
	// after the budget trips — the torrent in flight completes (the bound
	// is budget plus one full group, itself capped by MaxPathBytes), the
	// rest moves to the next window.
	pathLen := int64(len(long) + 2) // "%s.%d" over a 200-byte string
	// One full group under the weight model: the torrent row's structure
	// and name plus both file rows (path bytes + per-row overhead).
	perGroup := retainedTorrentOverhead + 3 + 2*(pathLen+retainedFileOverhead)
	limits := WindowLimits{MaxFiles: 100, MaxPathBytes: 1 << 20, WindowMaxBytes: 500}
	var got []TorrentRow
	totalOrphans := int64(0)
	after := int64(0)
	windows := 0
	for {
		w, err := src.Window(ctx, after, 4, limits)
		if err != nil {
			t.Fatalf("Window(after=%d) error = %v", after, err)
		}
		windows++
		if w.RetainedBytes > 500+perGroup {
			t.Fatalf("window retained %d bytes, want at most the budget plus one full group (%d)", w.RetainedBytes, 500+perGroup)
		}
		got = append(got, w.Torrents...)
		totalOrphans += w.OrphanFiles
		// Every returned torrent must carry its COMPLETE file list: a
		// torrent present without its files would be misclassified as
		// missing_files downstream and checkpointed past — data loss the
		// truncation must never cause. (Regression: an early version broke
		// out of the read loop without truncating and shipped exactly that.)
		for _, tr := range w.Torrents {
			if len(w.Files[tr.ID]) != 2 {
				t.Fatalf("window(after=%d) torrent %d kept %d file rows, want its complete list of 2", after, tr.ID, len(w.Files[tr.ID]))
			}
		}
		if len(w.Torrents) == 0 {
			break
		}
		after = int64(w.Torrents[len(w.Torrents)-1].ID)
		if windows > 16 {
			t.Fatal("window iteration did not terminate")
		}
	}
	if len(got) != 8 {
		t.Fatalf("read %d torrents across %d windows, want all 8 with no loss", len(got), windows)
	}
	if totalOrphans != 0 {
		t.Fatalf("orphans = %d, want 0", totalOrphans)
	}
}

// The budget must charge structural weight, not just path bytes: a torrent
// of many very short DUPLICATE paths costs ~65 B of real memory per row
// while its path-byte total stays tiny. Under a small budget each torrent
// overflows the budget alone, the window stops at the first group boundary,
// and iterating windows still reads every torrent with its complete,
// duplicate-preserving file list.
func TestReaderWindowShortDuplicatePathsChargedStructurally(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	src := openSource(t, dsn)
	ctx := context.Background()

	// 3 torrents × 100 identical one-byte paths: ~250 B of path bytes per
	// torrent, ~6.6 KB of real retained weight.
	for id := int32(1); id <= 3; id++ {
		seedTorrent(t, dsn, id, fmt.Sprintf("%040x", id), fmt.Sprintf("t%d", id), 100, 1700000000+id)
		for i := int32(1); i <= 100; i++ {
			seedFile(t, dsn, 100*id+i, &id, "a", 1)
		}
	}

	limits := WindowLimits{MaxFiles: 100, MaxPathBytes: 1 << 20, WindowMaxBytes: 200}
	var got []TorrentRow
	after := int64(0)
	windows := 0
	for {
		w, err := src.Window(ctx, after, 4, limits)
		if err != nil {
			t.Fatalf("Window(after=%d) error = %v", after, err)
		}
		windows++
		if len(w.Torrents) > 1 {
			t.Fatalf("window returned %d torrents; a 200-byte budget charged structurally must cut at the first torrent's group", len(w.Torrents))
		}
		for _, tr := range w.Torrents {
			rows := w.Files[tr.ID]
			if len(rows) != 100 {
				t.Fatalf("torrent %d kept %d file rows, want its complete list of 100", tr.ID, len(rows))
			}
			if rows[0].Path != "a" || rows[0].Size != 1 || rows[99].Path != "a" || rows[99].Size != 1 {
				t.Fatalf("duplicate rows corrupted: first %+v last %+v", rows[0], rows[99])
			}
			want := int64(retainedTorrentOverhead+len(fmt.Sprintf("t%d", tr.ID))) + 100*(1+retainedFileOverhead)
			if w.RetainedBytes < want {
				t.Fatalf("window retained %d bytes, want at least %d (per-row structural charging)", w.RetainedBytes, want)
			}
		}
		got = append(got, w.Torrents...)
		if len(w.Torrents) == 0 {
			break
		}
		after = int64(w.Torrents[len(w.Torrents)-1].ID)
		if windows > 8 {
			t.Fatal("window iteration did not terminate")
		}
	}
	if len(got) != 3 {
		t.Fatalf("read %d torrents across %d windows, want all 3 with complete lists", len(got), windows)
	}
}

// A torrent whose aggregate path bytes exceed MaxPathBytes is drained and
// recorded as a path_bytes overflow — the read-side mirror of the codec's
// own aggregate cap, never a migration-only limit.
func TestReaderPathByteOverflowDrainedWithoutRetention(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	src := openSource(t, dsn)
	ctx := context.Background()

	long := strings.Repeat("x", 40)
	seedTorrent(t, dsn, 1, fmt.Sprintf("%040x", 1), "fat", 3, 1700000001)
	seedTorrent(t, dsn, 2, fmt.Sprintf("%040x", 2), "slim", 1, 1700000002)
	for i := 0; i < 3; i++ { // 3 × 40 = 120 bytes > 100-byte cap
		owner := int32(1)
		seedFile(t, dsn, 10+int32(i), &owner, fmt.Sprintf("%s.%d", long, i), 1)
	}
	owner2 := int32(2)
	seedFile(t, dsn, 20, &owner2, "ok.bin", 1)

	w, err := src.Window(ctx, 0, 10, WindowLimits{MaxFiles: 100, MaxPathBytes: 100})
	if err != nil {
		t.Fatalf("Window() error = %v", err)
	}
	ov, has := w.Overflow[1]
	if !has || ov.Reason != OverflowPathBytes {
		t.Fatalf("Overflow[1] = %+v, want a path_bytes overflow", ov)
	}
	if ov.Observed < 120 {
		t.Fatalf("observed = %d, want the overrun byte total (≥120)", ov.Observed)
	}
	if rows := w.Files[1]; len(rows) != 0 {
		t.Fatalf("drained torrent kept %d rows, want none", len(rows))
	}
	if rows := w.Files[2]; len(rows) != 1 {
		t.Fatalf("healthy torrent kept %d rows, want 1", len(rows))
	}
}

// The window byte budget must bound the METADATA scan too, not just the
// file rows: a keyset page far larger than the budget, full of long-named
// torrents with no files (which never reach the file-row pass), used to be
// retained in full before any budget check. Regression: the metadata phase
// truncates at exact budget arithmetic, and iterating windows still reads
// every torrent exactly once — no skips, no duplicates.
func TestReaderWindowBudgetBoundsMetadataScan(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	src := openSource(t, dsn)
	ctx := context.Background()

	// 60 file-less torrents with 512-byte names: 128+512 = 640 B of
	// metadata weight each, zero file rows.
	name := strings.Repeat("n", 512)
	for id := int32(1); id <= 60; id++ {
		seedTorrent(t, dsn, id, fmt.Sprintf("%040x", id), name, 1, 1700000000+id)
	}

	const budget = 16384 // 16 KiB: floor(budget/640) = 25 torrents per window
	perTorrent := int64(retainedTorrentOverhead + 512)
	limits := WindowLimits{MaxFiles: 100, MaxPathBytes: 1 << 20, WindowMaxBytes: budget}
	var got []TorrentRow
	after := int64(0)
	windows := 0
	for {
		w, err := src.Window(ctx, after, 1000, limits)
		if err != nil {
			t.Fatalf("Window(after=%d) error = %v", after, err)
		}
		windows++
		if len(w.Torrents) > int(budget/perTorrent) {
			t.Fatalf("window(after=%d) returned %d torrents, want at most %d (budget / per-torrent metadata weight)",
				after, len(w.Torrents), budget/perTorrent)
		}
		// An occupied window never exceeds the budget: the cut happens
		// BEFORE retention (the only over-budget window is a single
		// oversized torrent admitted into an empty one).
		if len(w.Torrents) > 1 && w.RetainedBytes > budget {
			t.Fatalf("window(after=%d) retained %d bytes, want at most %d", after, w.RetainedBytes, budget)
		}
		if windows <= 2 {
			// The first two windows start from a full page: the cut must be
			// exactly floor(budget/perTorrent) and flagged as truncated.
			if len(w.Torrents) != int(budget/perTorrent) {
				t.Fatalf("window %d returned %d torrents, want exactly %d", windows, len(w.Torrents), budget/perTorrent)
			}
			if !w.Truncated() {
				t.Fatalf("window %d reports not truncated, want a metadata-phase budget cut", windows)
			}
		}
		if len(w.Torrents) == 0 {
			break
		}
		got = append(got, w.Torrents...)
		after = int64(w.Torrents[len(w.Torrents)-1].ID)
		if windows > 8 {
			t.Fatal("window iteration did not terminate")
		}
	}
	if len(got) != 60 {
		t.Fatalf("read %d torrents across %d windows, want all 60 with no skips", len(got), windows)
	}
	for i, tr := range got {
		if tr.ID != int32(i+1) {
			t.Fatalf("torrent %d: got id %d, want %d (ascending, no gaps, no duplicates)", i, tr.ID, i+1)
		}
	}
}

// When the metadata phase makes the cut, every retained torrent must still
// carry its complete file list — a torrent present without its files would
// corrupt into a missing_files error past the checkpoint (the truncation
// regression class), so the budget may never cut inside a torrent.
func TestReaderMetadataBudgetKeepsFileListsComplete(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	src := openSource(t, dsn)
	ctx := context.Background()

	// 40 torrents with 512-byte names and two one-byte paths each.
	name := strings.Repeat("n", 512)
	for id := int32(1); id <= 40; id++ {
		seedTorrent(t, dsn, id, fmt.Sprintf("%040x", id), name, 2, 1700000000+id)
		owner := id
		seedFile(t, dsn, 10*id, &owner, "a", 1)
		seedFile(t, dsn, 10*id+1, &owner, "b", 1)
	}

	// Budget 2 KiB: the metadata phase alone (640 B/torrent) cuts at three
	// torrents; the file rows then trip it again at group boundaries.
	limits := WindowLimits{MaxFiles: 100, MaxPathBytes: 1 << 20, WindowMaxBytes: 2048}
	var got []TorrentRow
	after := int64(0)
	windows := 0
	for {
		w, err := src.Window(ctx, after, 100, limits)
		if err != nil {
			t.Fatalf("Window(after=%d) error = %v", after, err)
		}
		windows++
		// Bound: the budget plus one torrent's metadata plus one completed
		// file group — the same invariant the file-row truncation documents.
		if max := int64(2048) + 640 + 2*(1+retainedFileOverhead); w.RetainedBytes > max {
			t.Fatalf("window(after=%d) retained %d bytes, want at most %d", after, w.RetainedBytes, max)
		}
		for _, tr := range w.Torrents {
			rows := w.Files[tr.ID]
			if len(rows) != 2 || rows[0].Path != "a" || rows[1].Path != "b" {
				t.Fatalf("window(after=%d) torrent %d kept %+v, want its complete list of 2 (a, b)",
					after, tr.ID, rows)
			}
		}
		if len(w.Torrents) == 0 {
			break
		}
		got = append(got, w.Torrents...)
		after = int64(w.Torrents[len(w.Torrents)-1].ID)
		if windows > 64 {
			t.Fatal("window iteration did not terminate")
		}
	}
	if len(got) != 40 {
		t.Fatalf("read %d torrents across %d windows, want all 40 with complete file lists", len(got), windows)
	}
	for i, tr := range got {
		if tr.ID != int32(i+1) {
			t.Fatalf("torrent %d: got id %d, want %d (ascending, no gaps, no duplicates)", i, tr.ID, i+1)
		}
	}
}

// A torrent whose metadata alone exceeds the whole window budget is still
// admitted — alone — into an empty window: the read-side mirror of the
// pipeline's oversized-solo admission. Refusing it would stall the run
// forever; admitting the FOLLOW-UP torrents with it would defeat the budget.
func TestReaderEmptyWindowAdmitsOversizedMetadata(t *testing.T) {
	t.Parallel()

	dsn := createLegacySource(t)
	huge := strings.Repeat("big", 700) // 2100 bytes against a 1 KiB budget
	seedTorrent(t, dsn, 1, hashAA, huge, 1, 1700000001)
	seedTorrent(t, dsn, 2, hashBB, "normal", 1, 1700000002)
	src := openSource(t, dsn)
	ctx := context.Background()

	limits := WindowLimits{MaxFiles: 100, MaxPathBytes: 1 << 20, WindowMaxBytes: 1024}
	w, err := src.Window(ctx, 0, 10, limits)
	if err != nil {
		t.Fatalf("Window() error = %v", err)
	}
	if len(w.Torrents) != 1 || w.Torrents[0].ID != 1 || w.Torrents[0].Name != huge {
		t.Fatalf("window = %+v, want exactly the oversized torrent admitted alone", w.Torrents)
	}
	if w.RetainedBytes != int64(retainedTorrentOverhead+len(huge)) {
		t.Fatalf("retained %d bytes, want exactly the oversized torrent's weight %d",
			w.RetainedBytes, retainedTorrentOverhead+len(huge))
	}
	if !w.Truncated() {
		t.Fatal("the follow-up torrent must be left for the next window (truncated)")
	}

	w2, err := src.Window(ctx, 1, 10, limits)
	if err != nil {
		t.Fatalf("Window(after=1) error = %v", err)
	}
	if len(w2.Torrents) != 1 || w2.Torrents[0].ID != 2 {
		t.Fatalf("next window = %+v, want the follow-up torrent", w2.Torrents)
	}
	if w2.Truncated() {
		t.Fatal("next window read its whole range, must not report truncated")
	}
}
