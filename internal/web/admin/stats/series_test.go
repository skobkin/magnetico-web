package stats

import (
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/opstats"
)

var seriesDay = time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)

func dayBuckets() []time.Time {
	var b []time.Time
	for h := 0; h < 24; h++ {
		b = append(b, seriesDay.Add(time.Duration(h)*time.Hour))
	}
	return b
}

func gauge(sum, n, min, max int64) opstats.GaugeAgg {
	return opstats.GaugeAgg{Sum: sum, N: n, Min: min, Max: max, Last: max, Valid: true}
}

func hasNote(s Series, i int, note string) bool {
	for _, n := range s.Notes[i] {
		if n == note {
			return true
		}
	}
	return false
}

// TestQuotaUtilisationHistoryDoesNotDependOnTheCurrentQuota (Task 09A): each
// observation was stored with the quota in force when it was taken; the chart
// reads that, so reconfiguring the quota later cannot rewrite old buckets.
func TestQuotaUtilisationHistoryDoesNotDependOnTheCurrentQuota(t *testing.T) {
	rows := map[time.Time]*bucketRow{}
	b := dayBuckets()
	// hour 1: two samples at 25 and 75 percent of a quota that was later changed; hour 2: one at 50 percent
	r1, r2, r3 := &bucketRow{start: b[1], covered: 60}, &bucketRow{start: b[2], covered: 60}, &bucketRow{start: b[3], covered: 60}
	r1.g[opstats.GQuotaUtilisationPPM] = gauge(250_000+750_000, 2, 250_000, 750_000)
	r2.g[opstats.GQuotaUtilisationPPM] = gauge(500_000, 1, 500_000, 500_000)
	// hour 3 sampled with no quota configured: bytes are known but no utilisation was recorded
	r3.g[opstats.GQuotaUsedBytes] = gauge(1<<30, 1, 1<<30, 1<<30)
	rows[b[1]], rows[b[2]], rows[b[3]] = r1, r2, r3
	s := buildSeries(Query{Chart: ChartQueue, Granularity: GranDay}, b, rows, nil, seriesDay.Add(48*time.Hour))
	avg, peak := lineByKey(t, s, "quota"), lineByKey(t, s, "quota_peak")
	if v := avg.Values[1]; v == nil || *v != 0.5 {
		t.Fatalf("hour 1 average = %v, want 0.5 (mean of 25 and 75 percent)", v)
	}
	if v := peak.Values[1]; v == nil || *v != 0.75 {
		t.Fatalf("hour 1 peak = %v, want 0.75", v)
	}
	if v := avg.Values[2]; v == nil || *v != 0.5 {
		t.Fatalf("hour 2 average = %v", v)
	}
	if avg.Values[3] != nil || peak.Values[3] != nil {
		t.Fatal("a bucket sampled with no quota configured must show no utilisation, not one computed from today's quota")
	}
	if avg.Values[0] != nil {
		t.Fatal("a bucket with no row is a gap")
	}
}

func TestApproximateCountsAreMarkedPerBucketInTheHistory(t *testing.T) {
	b := dayBuckets()
	rows := map[time.Time]*bucketRow{}
	exact, estimated, mixed := &bucketRow{start: b[1], covered: 60}, &bucketRow{start: b[2], covered: 60}, &bucketRow{start: b[3], covered: 60}
	for _, r := range []*bucketRow{exact, estimated, mixed} {
		r.g[opstats.GQueueRows] = gauge(30, 3, 10, 10)
		r.g[opstats.GQuarantineRows] = gauge(3, 3, 1, 1)
	}
	exact.g[opstats.GQueueRowsApprox], exact.g[opstats.GQuarantineRowsApprox] = gauge(0, 3, 0, 0), gauge(0, 3, 0, 0)
	estimated.g[opstats.GQueueRowsApprox], estimated.g[opstats.GQuarantineRowsApprox] = gauge(3, 3, 1, 1), gauge(0, 3, 0, 0)
	mixed.g[opstats.GQueueRowsApprox], mixed.g[opstats.GQuarantineRowsApprox] = gauge(1, 3, 0, 1), gauge(0, 3, 0, 0) // one of three samples estimated
	rows[b[1]], rows[b[2]], rows[b[3]] = exact, estimated, mixed
	s := buildSeries(Query{Chart: ChartQueue, Granularity: GranDay}, b, rows, nil, seriesDay.Add(48*time.Hour))
	if hasNote(s, 1, NoteApproximateCounts) {
		t.Error("an exact bucket was marked approximate")
	}
	if !hasNote(s, 2, NoteApproximateCounts) || !hasNote(s, 3, NoteApproximateCounts) {
		t.Errorf("a bucket with any estimated sample must be marked: %v %v", s.Notes[2], s.Notes[3])
	}
	if len(s.Notes) != len(b) {
		t.Fatalf("notes cover %d of %d buckets", len(s.Notes), len(b))
	}
}

func storageRowWith(start time.Time, total int64, missing ...opstats.Measure) *storageRow {
	r := &storageRow{start: start, samples: 1}
	for m := opstats.Measure(0); m < opstats.NumMeasures; m++ {
		v := total / 10
		r.last[m] = &v
	}
	t := total
	r.last[opstats.MMagneticodTotal] = &t
	for _, m := range missing {
		r.last[m] = nil
	}
	return r
}

// TestStorageBreakdownIsNeverDrawnFromIncompleteOrUnreconciledSamples keeps
// missing values missing and surfaces reconciliation outcomes: a bucket whose
// newest sample lacks a category draws no stack (a partial stack would look like
// a smaller total), and a mismatch or an unevaluated check is a visible note.
func TestStorageBreakdownIsNeverDrawnFromIncompleteOrUnreconciledSamples(t *testing.T) {
	b := dayBuckets()
	srows := map[time.Time]*storageRow{}
	ok := storageRowWith(b[1], 1000)
	incomplete := storageRowWith(b[2], 1000, opstats.MBlobsTable)
	mismatch := storageRowWith(b[3], 1000)
	mismatch.mismatches = 1
	unevaluated := storageRowWith(b[4], 1000, opstats.MBlobsTable)
	unevaluated.unevaluated = 1
	srows[b[1]], srows[b[2]], srows[b[3]], srows[b[4]] = ok, incomplete, mismatch, unevaluated
	s := buildSeries(Query{Chart: ChartStorage, Granularity: GranDay}, b, nil, srows, seriesDay.Add(48*time.Hour))

	stack := []string{"torrents_table", "blobs_table", "indexes", "other"}
	for _, k := range stack {
		if v := lineByKey(t, s, k).Values[1]; v == nil {
			t.Errorf("the complete bucket lost %s", k)
		}
	}
	for _, i := range []int{2, 4} {
		for _, k := range stack {
			if lineByKey(t, s, k).Values[i] != nil {
				t.Errorf("bucket %d draws %s although its newest sample lacks a category", i, k)
			}
		}
		if !hasNote(s, i, NoteIncompleteBreakdown) {
			t.Errorf("bucket %d: no incomplete-breakdown note: %v", i, s.Notes[i])
		}
		if v := lineByKey(t, s, "magneticod_total").Values[i]; v == nil {
			t.Errorf("bucket %d: the total of the same sample must still be drawn", i)
		}
	}
	if !hasNote(s, 3, NoteReconciliationMismatch) || hasNote(s, 1, NoteReconciliationMismatch) {
		t.Errorf("mismatch note: bucket3=%v bucket1=%v", s.Notes[3], s.Notes[1])
	}
	if !hasNote(s, 4, NoteReconciliationNotEvaluated) || hasNote(s, 3, NoteReconciliationNotEvaluated) {
		t.Errorf("not-evaluated note: bucket4=%v bucket3=%v", s.Notes[4], s.Notes[3])
	}
	if hasNote(s, 4, NoteReconciliationMismatch) {
		t.Error("an unavailable measure must not be reported as an inconsistent sample")
	}
}
