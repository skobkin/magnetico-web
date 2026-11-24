package opstats

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func apply(t testing.TB, pool *pgxpool.Pool, d *Delta) ApplyResult {
	t.Helper()
	var res ApplyResult
	err := pgx.BeginFunc(bg, pool, func(tx pgx.Tx) error {
		var err error
		res, err = Apply(bg, tx, d)
		return err
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return res
}

func hourDelta(h time.Time, rec, ack int64, mask int64) HourDelta {
	d := HourDelta{Hour: h, Coverage: mask}
	d.Counters[CRecordsReceived], d.Counters[CRecordsAcknowledged] = rec, ack
	return d
}

func scanInt(t testing.TB, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var v *int64
	if err := pool.QueryRow(bg, sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if v == nil {
		return -1
	}
	return *v
}

var base = time.Date(2026, 5, 31, 22, 0, 0, 0, time.UTC) // two hours before a month boundary

func TestApplyWritesHourDayMonthFromOneDelta(t *testing.T) {
	_, pool := newDB(t, true)
	d := &Delta{Hours: []HourDelta{
		hourDelta(base, 10, 8, 0b111),                 // 22:00 May 31
		hourDelta(base.Add(time.Hour), 5, 5, 0b11),    // 23:00 May 31
		hourDelta(base.Add(2*time.Hour), 7, 6, 1<<59), // 00:00 June 1
	}}
	apply(t, pool, d)
	for _, tc := range []struct {
		table, bucket string
		received      int64
		covered       string
		want          int64
	}{
		{"agg_hour", "2026-05-31T22:00:00Z", 10, "covered_mask", 0b111},
		{"agg_hour", "2026-06-01T00:00:00Z", 7, "covered_mask", 1 << 59},
		{"agg_day", "2026-05-31T00:00:00Z", 15, "covered_minutes", 5},
		{"agg_day", "2026-06-01T00:00:00Z", 7, "covered_minutes", 1},
		{"agg_month", "2026-05-01T00:00:00Z", 15, "covered_minutes", 5},
		{"agg_month", "2026-06-01T00:00:00Z", 7, "covered_minutes", 1},
	} {
		if got := scanInt(t, pool, `SELECT records_received FROM magnetico_stats.`+tc.table+` WHERE bucket_start = $1`, tc.bucket); got != tc.received {
			t.Errorf("%s %s: received = %d, want %d", tc.table, tc.bucket, got, tc.received)
		}
		if got := scanInt(t, pool, `SELECT `+tc.covered+` FROM magnetico_stats.`+tc.table+` WHERE bucket_start = $1`, tc.bucket); got != tc.want {
			t.Errorf("%s %s: %s = %d, want %d", tc.table, tc.bucket, tc.covered, got, tc.want)
		}
	}
}

func TestApplyIsAdditiveAndSaturates(t *testing.T) {
	_, pool := newDB(t, true)
	for i := 0; i < 3; i++ {
		apply(t, pool, &Delta{Hours: []HourDelta{hourDelta(base, 10, 8, 0)}})
	}
	if got := scanInt(t, pool, `SELECT records_received FROM magnetico_stats.agg_day WHERE bucket_start = '2026-05-31T00:00:00Z'`); got != 30 {
		t.Fatalf("after three batches: %d, want 30", got)
	}
	big := hourDelta(base, Saturation-5, 0, 0)
	apply(t, pool, &Delta{Hours: []HourDelta{big}})
	apply(t, pool, &Delta{Hours: []HourDelta{big}})
	if got := scanInt(t, pool, `SELECT records_received FROM magnetico_stats.agg_hour WHERE bucket_start = $1`, base); got != Saturation {
		t.Fatalf("saturation in SQL: %d, want %d", got, Saturation)
	}
}

func TestWeightedMeansUseSumAndCount(t *testing.T) {
	_, pool := newDB(t, true)
	a, b := hourDelta(base, 0, 0, 0), hourDelta(base, 0, 0, 0)
	a.Counters[CProcessingMsSum], a.Counters[CProcessingMsCount] = 100, 1 // one slow record
	b.Counters[CProcessingMsSum], b.Counters[CProcessingMsCount] = 90, 9  // nine fast ones
	apply(t, pool, &Delta{Hours: []HourDelta{a}})
	apply(t, pool, &Delta{Hours: []HourDelta{b}})
	sum := scanInt(t, pool, `SELECT processing_ms_sum FROM magnetico_stats.agg_hour`)
	n := scanInt(t, pool, `SELECT processing_ms_count FROM magnetico_stats.agg_hour`)
	if sum != 190 || n != 10 || float64(sum)/float64(n) != 19 {
		t.Fatalf("sum %d count %d: the weighted mean is Σsum ÷ Σcount = 19, not the mean of means 50", sum, n)
	}
}

func queueSample(at time.Time, slot int64, rows, bytes int64) QueueState {
	q := QueueState{At: at, Slot: slot, QuotaMax: 1 << 30}
	q.Values[GQueueRows], q.Valid[GQueueRows] = rows, true
	q.Values[GQueueBytes], q.Valid[GQueueBytes] = bytes, true
	q.Values[GQuotaUsedBytes], q.Valid[GQuotaUsedBytes] = bytes, true
	return q
}

func TestGaugeMinMaxLastAndLastAtGuard(t *testing.T) {
	_, pool := newDB(t, true)
	var d Delta
	d.AddQueueSample(queueSample(base.Add(10*time.Second), base.Add(10*time.Second).Unix()/10, 5, 500))
	d.AddQueueSample(queueSample(base.Add(20*time.Second), base.Add(20*time.Second).Unix()/10, 9, 900))
	d.AddQueueSample(queueSample(base.Add(30*time.Second), base.Add(30*time.Second).Unix()/10, 2, 200))
	apply(t, pool, &d)
	row := func() (sum, n, mn, mx, last int64) {
		if err := pool.QueryRow(bg, `SELECT queue_rows_sum, queue_rows_n, queue_rows_min, queue_rows_max, queue_rows_last
FROM magnetico_stats.agg_hour WHERE bucket_start = $1`, base).Scan(&sum, &n, &mn, &mx, &last); err != nil {
			t.Fatal(err)
		}
		return
	}
	if sum, n, mn, mx, last := row(); sum != 16 || n != 3 || mn != 2 || mx != 9 || last != 2 {
		t.Fatalf("after one batch: sum=%d n=%d min=%d max=%d last=%d", sum, n, mn, mx, last)
	}
	// A delayed batch with an OLDER sample (a later slot is already applied, so a
	// batch with earlier slots is skipped as an overlap; within the hour the
	// last_at guard must still keep the newest value for samples of one batch).
	var d2 Delta
	d2.AddQueueSample(queueSample(base.Add(5*time.Second), 0, 100, 1))
	d2.Hours[0].GaugeLastAt = base.Add(5 * time.Second)
	d2.QueueFirstSlot, d2.QueueLastSlot = base.Add(40*time.Second).Unix()/10, base.Add(40*time.Second).Unix()/10 // a fresh slot: applied
	apply(t, pool, &d2)
	if _, _, mn, mx, last := row(); last != 2 || mx != 100 || mn != 2 {
		t.Fatalf("an older sample replaced the last value: min=%d max=%d last=%d", mn, mx, last)
	}
}

func TestSharedGaugesAreNeverSummedAcrossInstancesAndOverlapIsSkipped(t *testing.T) {
	_, pool := newDB(t, true)
	slot := base.Unix() / 10
	mk := func(slots ...int64) *Delta {
		var d Delta
		for _, s := range slots {
			d.AddQueueSample(queueSample(time.Unix(s*10, 0).UTC(), s, s%7, s))
		}
		d.Hours = append(d.Hours, hourDelta(base, 3, 3, 1)) // event counters ride along
		return &d
	}
	// Instance A samples slots 1–3; instance B (after a handover) overlaps at slot 3.
	if r := apply(t, pool, mk(slot+1, slot+2, slot+3)); !r.QueueApplied || r.QueueSkipped {
		t.Fatalf("first batch: %+v", r)
	}
	if r := apply(t, pool, mk(slot+3, slot+4)); r.QueueApplied || !r.QueueSkipped {
		t.Fatalf("an overlapping batch was applied: %+v", r)
	}
	n := scanInt(t, pool, `SELECT queue_rows_n FROM magnetico_stats.agg_hour WHERE bucket_start = $1`, base)
	if n != 3 {
		t.Fatalf("gauge samples counted %d times, want 3 (the overlap must not be summed)", n)
	}
	// The event counters of the skipped batch were still applied.
	if got := scanInt(t, pool, `SELECT records_received FROM magnetico_stats.agg_hour WHERE bucket_start = $1`, base); got != 6 {
		t.Fatalf("event counters = %d, want 6 (both batches)", got)
	}
	// A batch strictly after the marker is applied; shared_state holds ONE row, the newest sample.
	if r := apply(t, pool, mk(slot+5, slot+6)); !r.QueueApplied {
		t.Fatalf("a later batch was skipped: %+v", r)
	}
	if got := scanInt(t, pool, `SELECT queue_rows_n FROM magnetico_stats.agg_hour WHERE bucket_start = $1`, base); got != 5 {
		t.Fatalf("gauge n = %d, want 5", got)
	}
	if rows := scanInt(t, pool, `SELECT count(*) FROM magnetico_stats.shared_state`); rows != 1 {
		t.Fatalf("shared_state rows = %d", rows)
	}
	if last := scanInt(t, pool, `SELECT gauge_last_slot FROM magnetico_stats.shared_state`); last != slot+6 {
		t.Fatalf("gauge_last_slot = %d, want %d", last, slot+6)
	}
	if got := scanInt(t, pool, `SELECT queue_bytes FROM magnetico_stats.shared_state`); got != slot+6 {
		t.Fatalf("shared_state.queue_bytes = %d, want the newest sample's value %d", got, slot+6)
	}
}

func TestCoverageIsTheUnionOfInstancesNeverTheSum(t *testing.T) {
	_, pool := newDB(t, true)
	full := hourDelta(base, 0, 0, FullHourMask)
	// Two instances recording the same hour and the same minutes, several times.
	for i := 0; i < 4; i++ {
		apply(t, pool, &Delta{Hours: []HourDelta{full}})
	}
	if got := scanInt(t, pool, `SELECT covered_mask FROM magnetico_stats.agg_hour`); BitCount(got) != 60 {
		t.Fatalf("hour coverage %d minutes, want 60", BitCount(got))
	}
	if got := scanInt(t, pool, `SELECT covered_minutes FROM magnetico_stats.agg_day`); got != 60 {
		t.Fatalf("day coverage %d, want 60 (never more than the minutes that exist)", got)
	}
	if got := scanInt(t, pool, `SELECT covered_minutes FROM magnetico_stats.agg_month`); got != 60 {
		t.Fatalf("month coverage %d", got)
	}
	// Every hour of a day, twice by two instances: at most 1,440.
	var d Delta
	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	for h := 0; h < 24; h++ {
		d.Hours = append(d.Hours, hourDelta(day.Add(time.Duration(h)*time.Hour), 0, 0, FullHourMask))
	}
	apply(t, pool, &d)
	apply(t, pool, &d)
	if got := scanInt(t, pool, `SELECT covered_minutes FROM magnetico_stats.agg_day WHERE bucket_start = $1`, day); got != 1440 {
		t.Fatalf("day coverage %d, want exactly 1440", got)
	}
}

func TestDayCoverageAddsOnlyNewBits(t *testing.T) {
	_, pool := newDB(t, true)
	apply(t, pool, &Delta{Hours: []HourDelta{hourDelta(base, 0, 0, 0b1111)}})             // minutes 0-3
	apply(t, pool, &Delta{Hours: []HourDelta{hourDelta(base, 0, 0, 0b11110000)}})         // minutes 4-7: all new
	apply(t, pool, &Delta{Hours: []HourDelta{hourDelta(base, 0, 0, 0b00111100)}})         // 2-5: nothing new
	apply(t, pool, &Delta{Hours: []HourDelta{hourDelta(base, 0, 0, 0b1_0000_0000_0000)}}) // minute 12: one new
	if got := scanInt(t, pool, `SELECT covered_minutes FROM magnetico_stats.agg_day`); got != 9 {
		t.Fatalf("day coverage %d, want 9 (8 + 1 new bit)", got)
	}
}

// TestLockOrderHourDayMonthAvoidsDeadlock: concurrent writers whose batches span
// the same hours, days and months in different subsets never deadlock and the
// counters add up exactly.
func TestLockOrderHourDayMonthAvoidsDeadlock(t *testing.T) {
	_, pool := newDB(t, true)
	start := time.Date(2026, 5, 30, 20, 0, 0, 0, time.UTC) // spans three days and two months
	const hours = 36
	var wg sync.WaitGroup
	var mu sync.Mutex
	var expected int64
	errs := make(chan error, 64)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 7))
			for i := 0; i < 25; i++ {
				var d Delta
				for h := 0; h < hours; h++ {
					if rng.IntN(3) == 0 {
						d.Hours = append(d.Hours, hourDelta(start.Add(time.Duration(h)*time.Hour), 1, 1, 1<<uint(rng.IntN(60))))
					}
				}
				if len(d.Hours) == 0 {
					continue
				}
				err := pgx.BeginFunc(bg, pool, func(tx pgx.Tx) error { _, err := Apply(bg, tx, &d); return err })
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				expected += int64(len(d.Hours))
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a concurrent Apply failed (deadlock?): %v", err)
	}
	for _, table := range []string{"agg_hour", "agg_day", "agg_month"} {
		if got := scanInt(t, pool, `SELECT sum(records_received) FROM magnetico_stats.`+table); got != expected {
			t.Errorf("%s: Σ received = %d, want %d", table, got, expected)
		}
	}
}

func crawler(id string, first, last time.Time, ip string, submit time.Time, recv int64) CrawlerDelta {
	c := CrawlerDelta{ID: id, FirstSeen: first, LastSeen: last, LastSubmit: submit, RecordsReceived: recv}
	if ip != "" {
		c.LastIP = netip.MustParseAddr(ip)
	}
	return c
}

func activityRow(t testing.TB, pool *pgxpool.Pool, id string) (first, last time.Time, ip *string, submit *time.Time, recv int64) {
	t.Helper()
	if err := pool.QueryRow(bg, `SELECT first_seen_at, last_seen_at, host(last_ip), last_submit_at, records_received
FROM magnetico_stats.crawler_activity WHERE crawler_id = $1`, id).Scan(&first, &last, &ip, &submit, &recv); err != nil {
		t.Fatal(err)
	}
	return
}

const cid = "11111111-1111-4111-8111-111111111111"

func TestCrawlerActivityMergeIsTimestampAware(t *testing.T) {
	_, pool := newDB(t, true)
	t10, t11, t12 := base.Add(10*time.Minute), base.Add(11*time.Minute), base.Add(12*time.Minute)
	apply(t, pool, &Delta{Crawlers: []CrawlerDelta{crawler(cid, t11, t12, "192.0.2.12", t12, 5)}})
	// A delayed batch with older sightings never overwrites the newer address.
	apply(t, pool, &Delta{Crawlers: []CrawlerDelta{crawler(cid, t10, t11, "192.0.2.11", t11, 3)}})
	first, last, ip, submit, recv := activityRow(t, pool, cid)
	if !first.Equal(t10) || !last.Equal(t12) || ip == nil || *ip != "192.0.2.12" || submit == nil || !submit.Equal(t12) || recv != 8 {
		t.Fatalf("first=%v last=%v ip=%v submit=%v recv=%d", first, last, ip, submit, recv)
	}
	// An exact tie keeps the stored address.
	apply(t, pool, &Delta{Crawlers: []CrawlerDelta{crawler(cid, t12, t12, "192.0.2.99", t12, 0)}})
	if _, _, ip, _, _ = activityRow(t, pool, cid); *ip != "192.0.2.12" {
		t.Fatalf("a tie replaced the stored address: %s", *ip)
	}
	// A newer sighting replaces it.
	t13 := base.Add(13 * time.Minute)
	apply(t, pool, &Delta{Crawlers: []CrawlerDelta{crawler(cid, t13, t13, "192.0.2.13", time.Time{}, 1)}})
	if _, last, ip, _, _ = activityRow(t, pool, cid); *ip != "192.0.2.13" || !last.Equal(t13) {
		t.Fatalf("a newer sighting did not replace the address: %s %v", *ip, last)
	}
}

func TestLastSubmitAtIsMergedIndependentlyOfLastSeenAt(t *testing.T) {
	_, pool := newDB(t, true)
	t10, t11, t12, t13 := base.Add(10*time.Minute), base.Add(11*time.Minute), base.Add(12*time.Minute), base.Add(13*time.Minute)
	apply(t, pool, &Delta{Crawlers: []CrawlerDelta{crawler(cid, t10, t10, "192.0.2.1", t10, 1)}}) // submitted at 10
	// An existence-only batch with a newer last_seen: no submit timestamp at all.
	apply(t, pool, &Delta{Crawlers: []CrawlerDelta{crawler(cid, t12, t12, "192.0.2.2", time.Time{}, 0)}})
	if _, last, _, submit, _ := activityRow(t, pool, cid); !last.Equal(t12) || submit == nil || !submit.Equal(t10) {
		t.Fatalf("an existence-only batch erased or moved last_submit_at: last=%v submit=%v", last, submit)
	}
	// A delayed batch with an older submit never moves it back.
	apply(t, pool, &Delta{Crawlers: []CrawlerDelta{crawler(cid, t10.Add(-time.Minute), t10.Add(-time.Minute), "192.0.2.3", t10.Add(-time.Minute), 0)}})
	if _, _, _, submit, _ := activityRow(t, pool, cid); !submit.Equal(t10) {
		t.Fatalf("an older submit moved last_submit_at back to %v", submit)
	}
	// A newer submit with an OLDER last_seen advances last_submit_at and leaves last_seen and the address.
	apply(t, pool, &Delta{Crawlers: []CrawlerDelta{crawler(cid, t11, t11, "192.0.2.4", t13, 0)}})
	_, last, ip, submit, _ := activityRow(t, pool, cid)
	if !submit.Equal(t13) || !last.Equal(t12) || *ip != "192.0.2.2" {
		t.Fatalf("submit=%v last=%v ip=%s, want submit 13, last 12 and the address of 12", submit, last, *ip)
	}
	// A crawler that never submitted has a NULL last_submit_at.
	other := "22222222-2222-4222-8222-222222222222"
	apply(t, pool, &Delta{Crawlers: []CrawlerDelta{crawler(other, t10, t10, "", time.Time{}, 0)}})
	if _, _, ip, submit, _ := activityRow(t, pool, other); submit != nil || ip != nil {
		t.Fatalf("expected NULL submit and ip, got %v %v", submit, ip)
	}
}

func TestUpsertSizeIsBoundedByTheCap(t *testing.T) {
	_, pool := newDB(t, true)
	var d Delta
	for i := 0; i < MaxCrawlersPerDrain; i++ {
		d.Crawlers = append(d.Crawlers, crawler(fmt.Sprintf("00000000-0000-4000-8000-%012d", i), base, base, "", time.Time{}, 1))
	}
	start := time.Now()
	apply(t, pool, &d)
	if n := scanInt(t, pool, `SELECT count(*) FROM magnetico_stats.crawler_activity`); n != MaxCrawlersPerDrain {
		t.Fatalf("%d rows", n)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("a full-cap batch took %v", time.Since(start))
	}
	// Merging more crawlers than the cap stays at the cap and counts the overflow.
	var a, b Delta
	for i := 0; i < MaxCrawlersPerDrain; i++ {
		a.Crawlers = append(a.Crawlers, crawler(fmt.Sprintf("00000000-0000-4000-8000-%012d", i), base, base, "", time.Time{}, 1))
		b.Crawlers = append(b.Crawlers, crawler(fmt.Sprintf("00000000-0000-4000-9000-%012d", i), base, base, "", time.Time{}, 1))
	}
	Merge(&a, &b)
	if len(a.Crawlers) != MaxCrawlersPerDrain || a.CrawlersOverflow != MaxCrawlersPerDrain {
		t.Fatalf("merged %d crawlers, overflow %d", len(a.Crawlers), a.CrawlersOverflow)
	}
}

func storageSample(at time.Time, slot int64, total, est int64, reconciled bool) StorageState {
	s := StorageState{At: at, Slot: slot, Reconciled: reconciled}
	for m := Measure(0); m < NumMeasures; m++ {
		s.Valid[m] = true
		s.Values[m] = total / 10
	}
	s.Values[MMagneticodTotal], s.Values[MIndexedTorrents] = total, est
	return s
}

func TestStorageUpsertKeepsLastMaxSumAndTheRatioOfSums(t *testing.T) {
	_, pool := newDB(t, true)
	var d Delta
	d.AddStorageSample(storageSample(base.Add(1*time.Minute), 1, 1000, 10, true))
	d.AddStorageSample(storageSample(base.Add(6*time.Minute), 2, 3000, 20, true))
	apply(t, pool, &d)
	var d2 Delta
	d2.AddStorageSample(storageSample(base.Add(11*time.Minute), 3, 2000, 0, false)) // unknown torrent count: no ratio contribution
	apply(t, pool, &d2)
	var last, mx, sum, n, bytes, torrents, samples, mism int64
	if err := pool.QueryRow(bg, `SELECT magneticod_total_bytes_last, magneticod_total_bytes_max, magneticod_total_bytes_sum, magneticod_total_bytes_n,
bpt_bytes_sum, bpt_torrents_sum, samples, reconciliation_mismatches FROM magnetico_stats.storage_day`).Scan(&last, &mx, &sum, &n, &bytes, &torrents, &samples, &mism); err != nil {
		t.Fatal(err)
	}
	if last != 2000 || mx != 3000 || sum != 6000 || n != 3 || bytes != 4000 || torrents != 30 || samples != 3 || mism != 1 {
		t.Fatalf("last=%d max=%d sum=%d n=%d bpt=%d/%d samples=%d mismatches=%d", last, mx, sum, n, bytes, torrents, samples, mism)
	}
	if got := float64(bytes) / float64(torrents); got != 4000.0/30 {
		t.Fatalf("ratio of sums %v", got)
	}
}

func TestMergeBoundsHoursAndKeepsTimestampAwareState(t *testing.T) {
	var dst, src Delta
	for h := 0; h < MaxLiveHours; h++ {
		dst.Hours = append(dst.Hours, hourDelta(base.Add(time.Duration(h)*time.Hour), 1, 0, 0))
	}
	src.Hours = append(src.Hours, hourDelta(base.Add(100*time.Hour), 1, 0, 0), hourDelta(base.Add(101*time.Hour), 1, 0, 0))
	src.InFlightAtDrain = 7
	Merge(&dst, &src)
	if len(dst.Hours) != MaxLiveHours || dst.HoursDropped != 2 || !dst.Hours[0].Hour.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("%d hours, dropped %d, oldest %v", len(dst.Hours), dst.HoursDropped, dst.Hours[0].Hour)
	}
	if dst.InFlightAtDrain != 7 {
		t.Fatalf("in flight = %d, want the newest drain's value", dst.InFlightAtDrain)
	}
	// Gauge merge keeps the newest last value regardless of merge order.
	var a, b Delta
	a.AddQueueSample(queueSample(base.Add(50*time.Second), 5, 50, 50))
	b.AddQueueSample(queueSample(base.Add(10*time.Second), 1, 10, 10))
	Merge(&a, &b)
	if a.Hours[0].Gauges[GQueueRows].Last != 50 || a.Hours[0].Gauges[GQueueRows].N != 2 || a.LatestQueue.Values[GQueueRows] != 50 ||
		a.QueueFirstSlot != 1 || a.QueueLastSlot != 5 {
		t.Fatalf("gauge merge: %+v latest=%+v slots=%d..%d", a.Hours[0].Gauges[GQueueRows], a.LatestQueue.Values[GQueueRows], a.QueueFirstSlot, a.QueueLastSlot)
	}
}

var _ = context.Background
