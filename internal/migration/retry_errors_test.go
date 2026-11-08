package migration

// Unit tests for the retry batch settlement seam (no database): they pin
// the per-window settlement ORDERING that the integration fixtures cannot
// observe (Codex review iteration 2, finding 5 — both memory regressions
// passed against their pre-fix implementations).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
)

type settleEvent struct {
	kind string // "fetch" or "settle"
	id   int64
}

// TestSettleBatchWindowsSettlesPerWindow pins the per-window settlement
// property (Codex review iteration 1, finding 6): every window is settled
// fully BEFORE the next window is fetched, so a failure injected on the
// second fetch proves window one was settled first. The pre-fix
// accumulator (fetch every window into maps, then settle any ID) cannot
// produce this event sequence.
func TestSettleBatchWindowsSettlesPerWindow(t *testing.T) {
	t.Parallel()

	ids := []int64{2, 4, 6, 8, 10}
	calls := 0
	var events []settleEvent
	fetch := func(_ context.Context, want []int64, _ original.WindowLimits) (*original.Window, []int64, error) {
		calls++
		events = append(events, settleEvent{kind: "fetch", id: want[0]})
		if calls == 2 {
			return nil, nil, errors.New("second window fetch failed")
		}
		// Each call returns the first two IDs as torrent rows; the rest
		// come back as the ascending suffix (budget-split semantics).
		n := len(want)
		if n > 2 {
			n = 2
		}
		w := &original.Window{Files: map[int32][]original.FileRow{}}
		for _, id := range want[:n] {
			w.Torrents = append(w.Torrents, original.TorrentRow{ID: int32(id)})
		}
		return w, want[n:], nil
	}

	_, settled, err := settleBatchWindows(context.Background(), ids, fetch, original.WindowLimits{},
		func(sourceID int64, _ *original.TorrentRow, _ []original.FileRow, _ *original.Overflow) error {
			events = append(events, settleEvent{kind: "settle", id: sourceID})
			return nil
		}, func() {})

	if err == nil || !strings.Contains(err.Error(), "second window") {
		t.Fatalf("error = %v, want the injected second-fetch failure", err)
	}
	if settled {
		t.Error("settled = true, want false (the batch never completed)")
	}
	want := []settleEvent{
		{kind: "fetch", id: 2},
		{kind: "settle", id: 2},
		{kind: "settle", id: 4},
		{kind: "fetch", id: 6}, // fails: window one fully settled before it
	}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i, e := range want {
		if events[i] != e {
			t.Fatalf("event %d = %+v, want %+v (full sequence %v)", i, events[i], e, events)
		}
	}
}

// TestSettleBatchWindowsFullBatchOrdering pins the settlement order of a
// completing batch: registry IDs the source lacks settle as missing
// interleaved below each fetched ID, the tail settles the rest, and the
// returned cursor is the last settled ID.
func TestSettleBatchWindowsFullBatchOrdering(t *testing.T) {
	t.Parallel()

	ids := []int64{2, 4, 6, 8, 10}
	present := map[int64]bool{4: true, 8: true}
	var fetches, settles []int64
	var fetched []bool
	fetch := func(_ context.Context, want []int64, _ original.WindowLimits) (*original.Window, []int64, error) {
		fetches = append(fetches, want[0])
		var rows []original.TorrentRow
		for _, id := range want {
			if present[id] {
				rows = append(rows, original.TorrentRow{ID: int32(id)})
			}
		}
		// Ascending prefix + the ids strictly AFTER the last returned row
		// as the remaining suffix (FetchByIDs / idsAfter semantics); an
		// empty window drops its first ID so a vanished set cannot replay
		// itself forever.
		rest := want
		if len(rows) > 0 {
			lastID := int64(rows[len(rows)-1].ID)
			for len(rest) > 0 && rest[0] <= lastID {
				rest = rest[1:]
			}
		} else if len(rest) > 0 {
			rest = rest[1:]
		}
		return &original.Window{Torrents: rows, Files: map[int32][]original.FileRow{}}, rest, nil
	}

	last, settled, err := settleBatchWindows(context.Background(), ids, fetch, original.WindowLimits{},
		func(sourceID int64, row *original.TorrentRow, _ []original.FileRow, _ *original.Overflow) error {
			settles = append(settles, sourceID)
			fetched = append(fetched, row != nil)
			return nil
		}, func() {})

	if err != nil {
		t.Fatalf("settleBatchWindows() error = %v", err)
	}
	if !settled || last != 10 {
		t.Fatalf("last = %d settled = %v, want 10/true", last, settled)
	}
	wantIDs := []int64{2, 4, 6, 8, 10}
	wantFetched := []bool{false, true, false, true, false}
	if len(settles) != len(wantIDs) {
		t.Fatalf("settles = %v, want %v", settles, wantIDs)
	}
	for i := range wantIDs {
		if settles[i] != wantIDs[i] || fetched[i] != wantFetched[i] {
			t.Fatalf("settlement %d = (%d, fetched %v), want (%d, %v)",
				i, settles[i], fetched[i], wantIDs[i], wantFetched[i])
		}
	}
	if len(fetches) != 2 {
		t.Fatalf("fetches = %v, want 2 windows", fetches)
	}
}
