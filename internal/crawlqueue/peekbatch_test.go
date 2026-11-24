package crawlqueue

import (
	"testing"
	"time"
)

func appendSizes(t *testing.T, q *Queue, sizes ...int) {
	t.Helper()
	for i, sz := range sizes {
		if ok, err := q.Append(bg, h(i+1), FormatJSONv1, payload(i+1, sz)); err != nil || !ok {
			t.Fatalf("append %d: %v %v", i, ok, err)
		}
	}
}

// TestPeekBatchReportsWhyItStopped: exhausted queue, record cap reached, the
// next record would not fit (however empty the batch still is), oversized head.
func TestPeekBatchReportsWhyItStopped(t *testing.T) {
	t.Parallel()
	const budget = 1000
	for _, tc := range []struct {
		name  string
		sizes []int
		max   int
		stop  StopReason
		n     int
		bytes int64
	}{
		{"exhausted", []int{100, 200}, 10, StopNone, 2, 300},
		{"exactly the record cap", []int{10, 10, 10}, 3, StopCount, 3, 30},
		{"record cap with more queued", []int{10, 10, 10, 10}, 3, StopCount, 3, 30},
		{"next record does not fit", []int{400, 400, 300}, 10, StopBytes, 2, 800},
		{"nearly empty batch, next does not fit", []int{100, 950}, 10, StopBytes, 1, 100},
		{"fits exactly", []int{500, 500}, 10, StopNone, 2, 1000},
		{"oversized head alone", []int{budget + 1}, 10, StopOversizeHead, 1, budget + 1},
		{"head exactly the budget", []int{budget, 5}, 10, StopOversizeHead, 1, budget},
		{"oversized head with followers", []int{5000, 10, 10}, 10, StopOversizeHead, 1, 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := open(t, nil)
			appendSizes(t, q, tc.sizes...)
			b, err := q.PeekBatch(bg, tc.max, budget)
			if err != nil {
				t.Fatal(err)
			}
			if b.Stop != tc.stop || len(b.Items) != tc.n || b.Bytes != tc.bytes {
				t.Fatalf("PeekBatch = {%d items, %d bytes, stop %v}, want {%d, %d, %v}", len(b.Items), b.Bytes, b.Stop, tc.n, tc.bytes, tc.stop)
			}
			// The plan agrees with the batch, and fetching it returns the same records.
			p, err := q.PlanBatch(bg, tc.max, budget)
			if err != nil || p.Stop != tc.stop || p.Count != tc.n || p.Bytes != tc.bytes || p.HeadID != b.Items[0].ID {
				t.Fatalf("PlanBatch = %+v, %v", p, err)
			}
			items, err := q.Fetch(bg, p)
			if err != nil || len(items) != tc.n || items[0].ID != b.Items[0].ID {
				t.Fatalf("Fetch = %d items, %v", len(items), err)
			}
		})
	}
	q := open(t, nil)
	if b, err := q.PeekBatch(bg, 10, budget); err != nil || len(b.Items) != 0 || b.Stop != StopNone {
		t.Fatalf("empty queue: %+v %v", b, err)
	}
	if p, err := q.PlanBatch(bg, 10, budget); err != nil || p.Count != 0 {
		t.Fatalf("empty plan: %+v %v", p, err)
	}
}

func TestPlanBatchCarriesTheHeadEnqueueTimeAndLeavesPeekUnchanged(t *testing.T) {
	t.Parallel()
	q := open(t, nil)
	before := time.Now().Add(-time.Second)
	appendSizes(t, q, 10, 10)
	p, err := q.PlanBatch(bg, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if p.HeadEnqueuedAt.Before(before) || p.HeadEnqueuedAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("HeadEnqueuedAt = %v", p.HeadEnqueuedAt)
	}
	// Peek keeps its contract: the same records as PeekBatch.
	items, err := q.Peek(bg, 10, 1<<20)
	b, _ := q.PeekBatch(bg, 10, 1<<20)
	if err != nil || len(items) != len(b.Items) || items[0].ID != b.Items[0].ID {
		t.Fatalf("Peek/PeekBatch disagree: %d vs %d (%v)", len(items), len(b.Items), err)
	}
}
