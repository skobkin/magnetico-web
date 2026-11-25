//go:build loadtest

package stats

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/opstats"
)

func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestMeasureStatisticsHeap is the memory acceptance row for the recorder and
// the writer's buffers: 1,024 distinct active crawlers (the per-drain cap) in one
// window, the 26 hours of pending hourly deltas the writer may buffer, and the
// rolling rings. The authenticator's cache is measured in internal/ingest/crawlerauth.
//
//	MAGNETICO_STATS_OUT=<dir> go test -tags loadtest -run TestMeasureStatisticsHeap -v ./internal/ingest/stats
func TestMeasureStatisticsHeap(t *testing.T) {
	out := os.Getenv("MAGNETICO_STATS_OUT")
	if out == "" {
		t.Skip("MAGNETICO_STATS_OUT is not set")
	}
	base := liveHeap()
	clock := newClock(time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC))
	rec := New(clock.Now)
	ip := netip.MustParseAddr("198.51.100.7")
	var carry opstats.Delta // what the writer buffers during an outage: every drained delta is merged into its carry
	pendingHours := 0
	for h := 0; h < 26; h++ {
		for c := 0; c < 1024; c++ {
			id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", c)
			rec.SubmitAuthenticated(id)
			rec.SubmitValidated(id, ip, 10)
			rec.SubmitAcked(id, 8, 2, 4000)
			rec.ExistsRequest(id)
		}
		d := rec.Drain() // each drain carries its hour and the 1,024 crawlers
		opstats.Merge(&carry, &d)
		pendingHours++
		clock.Advance(time.Hour)
	}
	// And a full live window of the current hour.
	for c := 0; c < 1024; c++ {
		id := fmt.Sprintf("%08x-2222-4111-8111-111111111111", c)
		rec.SubmitValidated(id, ip, 10)
		rec.SubmitAcked(id, 10, 0, 4000)
	}
	used := liveHeap() - base
	runtime.KeepAlive(rec)
	runtime.KeepAlive(carry)
	res := map[string]any{"heap_bytes": used, "heap_mib": float64(used) / (1 << 20), "pending_hours": pendingHours, "crawlers_per_window": 1024}
	b, _ := json.Marshal(res)
	t.Logf("%s", b)
	f, _ := os.OpenFile(out+"/memory_heap.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
	if used > 8<<20 {
		t.Errorf("the statistics heap is %.1f MiB, want ≤ 8 MiB", float64(used)/(1<<20))
	}
}
