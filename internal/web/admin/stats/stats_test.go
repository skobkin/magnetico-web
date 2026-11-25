package stats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/ingest/crawlers"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/opstats"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/web/admin/access"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

var bg = context.Background()

var admin = access.Grant(1, "root")

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ---- the cache ------------------------------------------------------------------

func TestSnapshotCacheCoalescesConcurrentRefreshes(t *testing.T) {
	c := newCache[struct{}, int](10*time.Second, 1, time.Now)
	var calls atomic.Int64
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _, _, err := c.get(bg, struct{}{}, func(ctx context.Context) (int, error) {
				calls.Add(1)
				<-release
				return 42, nil
			})
			if err != nil || v != 42 {
				t.Errorf("%d %v", v, err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d loads for 100 concurrent callers, want 1", calls.Load())
	}
}

func TestSnapshotCacheServesLastKnownGoodOnFailureAndMarksItStale(t *testing.T) {
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := newCache[string, int](10*time.Second, 4, clk.now)
	fail := false
	load := func(ctx context.Context) (int, error) {
		if fail {
			return 0, errors.New("database down")
		}
		return 7, nil
	}
	v, at, stale, err := c.get(bg, "k", load)
	if v != 7 || stale || err != nil {
		t.Fatalf("%d %v %v", v, stale, err)
	}
	fail = true
	clk.advance(11 * time.Second)
	v2, at2, stale2, err := c.get(bg, "k", load)
	if err != nil || v2 != 7 || !stale2 || !at2.Equal(at) {
		t.Fatalf("on failure: value %d stale %v fetchedAt %v (was %v) err %v; want the old value, marked stale, with its original FetchedAt", v2, stale2, at2, at, err)
	}
	if _, _, _, err := c.get(bg, "never-loaded", load); err == nil {
		t.Fatal("a failure with no last known good must be an error")
	}
	fail = false
	if _, _, stale3, _ := c.get(bg, "k", load); stale3 {
		t.Fatal("still stale after recovery")
	}
}

func TestSeriesCacheIsBoundedAndLRU(t *testing.T) {
	c := newCache[int, int](time.Hour, 3, time.Now)
	var loads atomic.Int64
	get := func(k int) {
		_, _, _, _ = c.get(bg, k, func(ctx context.Context) (int, error) { loads.Add(1); return k, nil })
	}
	for k := 1; k <= 3; k++ {
		get(k)
	}
	get(1) // 1 is now the most recently used
	get(4) // evicts 2, the least recently used
	if c.len() != 3 {
		t.Fatalf("len = %d, want the bound 3", c.len())
	}
	before := loads.Load()
	get(1)
	get(3)
	get(4)
	if loads.Load() != before {
		t.Fatal("a recently used entry was evicted")
	}
	get(2)
	if loads.Load() != before+1 {
		t.Fatal("the least recently used entry was not evicted")
	}
}

// ---- freshness and rates (pure) --------------------------------------------------

func TestFreshnessClassificationBoundaries(t *testing.T) {
	s := time.Second
	for _, tc := range []struct {
		name string
		f    func(time.Duration) State
		age  time.Duration
		want State
	}{
		{"ingest 29s", ClassifyIngest, 29 * s, StateUp}, {"ingest 30s", ClassifyIngest, 30 * s, StateStale},
		{"ingest 4m59s", ClassifyIngest, 299 * s, StateStale}, {"ingest 5m", ClassifyIngest, 300 * s, StateUnavailable},
		{"queue 90s", ClassifyQueue, 90 * s, StateUp}, {"queue 91s", ClassifyQueue, 91 * s, StateStale},
		{"queue 14m59s", ClassifyQueue, 899 * s, StateStale}, {"queue 15m", ClassifyQueue, 900 * s, StateUnavailable},
		{"storage 15m", ClassifyStorage, 900 * s, StateUp}, {"storage 15m1s", ClassifyStorage, 901 * s, StateStale},
		{"storage 59m59s", ClassifyStorage, 3599 * s, StateStale}, {"storage 1h", ClassifyStorage, 3600 * s, StateUnavailable},
		{"queue 20s from the future", ClassifyQueue, -20 * s, StateUp}, {"queue a day from the future", ClassifyQueue, -86400 * s, StateUnavailable},
		{"storage a day from the future", ClassifyStorage, -86400 * s, StateUnavailable}, {"ingest 5m from the future", ClassifyIngest, -300 * s, StateUnavailable},
	} {
		if got := tc.f(tc.age); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func inst(id string, state State, windowEndAge time.Duration, covered [3]int, counts [3]int64) Instance {
	in := Instance{ID: id, State: state, windowEndAge: windowEndAge, covered: covered}
	for w := 0; w < 3; w++ {
		in.counts[w][0] = counts[w]
	}
	return in
}

func TestMultiInstanceRatesFollowTheD8Rules(t *testing.T) {
	a := inst("a", StateUp, 2*time.Second, [3]int{60, 900, 3600}, [3]int64{120, 900, 3600}) // 2/s, 1/s, 1/s
	b := inst("b", StateUp, 3*time.Second, [3]int{60, 900, 3600}, [3]int64{60, 1800, 7200}) // 1/s, 2/s, 2/s
	r := computeRates([]Instance{a, b})
	want := [3]float64{3, 3 * 60, 3 * 3600} // per second, per minute, per hour
	for w := 0; w < 3; w++ {
		v := r.Windows[w].Values[0]
		if !v.Valid || v.Value != want[w] || r.Windows[w].Included != 2 {
			t.Errorf("window %d: %+v included %d, want %v", w, v, r.Windows[w].Included, want[w])
		}
	}
	if r.Windows[0].Unit != "per second" || r.Windows[1].Unit != "per minute" || r.Windows[2].Unit != "per hour" {
		t.Fatalf("units: %v %v %v", r.Windows[0].Unit, r.Windows[1].Unit, r.Windows[2].Unit)
	}
	// A stale instance is listed, not summed.
	st := inst("c", StateStale, 40*time.Second, [3]int{60, 900, 3600}, [3]int64{600, 600, 600})
	r = computeRates([]Instance{a, st})
	if r.Windows[0].Values[0].Value != 2 || len(r.Windows[0].Excluded) != 1 || r.Windows[0].Excluded[0].InstanceID != "c" {
		t.Fatalf("stale instance: %+v", r.Windows[0])
	}
	// No compatible instance: unavailable, not zero.
	r = computeRates([]Instance{st})
	if r.Windows[0].Values[0].Valid {
		t.Fatal("a rate was shown with no fresh instance")
	}
}

func TestRollingRateUsesCoveredNotNominalSeconds(t *testing.T) {
	// A process that has run for 40 s saw 80 events in the 60 s window: 2/s, not 80/60.
	r := computeRates([]Instance{inst("a", StateUp, time.Second, [3]int{40, 40, 40}, [3]int64{80, 80, 80})})
	if got := r.Windows[0].Values[0].Value; got != 2 {
		t.Fatalf("rate = %v, want 2 per second", got)
	}
}

func TestInstancesBelowHalfTheWindowAreExcluded(t *testing.T) {
	r := computeRates([]Instance{
		inst("short", StateUp, time.Second, [3]int{29, 29, 29}, [3]int64{58, 58, 58}),
		inst("ok", StateUp, time.Second, [3]int{30, 450, 1800}, [3]int64{60, 900, 3600}),
	})
	if r.Windows[0].Included != 1 || r.Windows[0].Excluded[0].InstanceID != "short" {
		t.Fatalf("60 s window: %+v", r.Windows[0])
	}
	if r.Windows[1].Included != 1 || r.Windows[2].Included != 1 || len(r.Windows[2].Excluded) != 1 {
		t.Fatalf("longer windows: %+v %+v", r.Windows[1], r.Windows[2])
	}
}

func TestIncompatibleWindowEndsAreExcluded(t *testing.T) {
	r := computeRates([]Instance{
		inst("new", StateUp, 2*time.Second, [3]int{60, 900, 3600}, [3]int64{60, 900, 3600}),
		inst("lagging", StateUp, 31*time.Second, [3]int{60, 900, 3600}, [3]int64{60, 900, 3600}), // 29 s behind the newest: still compatible
		inst("late", StateUp, 33*time.Second, [3]int{60, 900, 3600}, [3]int64{60, 900, 3600}),    // 31 s behind: excluded
	})
	if r.Windows[0].Included != 2 || len(r.Windows[0].Excluded) != 1 || r.Windows[0].Excluded[0].InstanceID != "late" {
		t.Fatalf("window ends: %+v", r.Windows[0])
	}
}

// ---- database-backed -------------------------------------------------------------

type env struct {
	t    *testing.T
	dsn  string
	pool *pgxpool.Pool
	svc  *Service
	clk  *clock
	qs   atomic.Int64
}

type countingDB struct {
	db opstats.DB
	n  *atomic.Int64
}

func (c countingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n.Add(1)
	return c.db.Query(ctx, sql, args...)
}
func (c countingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.n.Add(1)
	return c.db.QueryRow(ctx, sql, args...)
}

type flags struct{ stats, ingest, web bool }

func newEnv(t *testing.T, f flags) *env {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := persistence.InitSchema(bg, dsn); err != nil {
		t.Fatal(err)
	}
	if f.ingest {
		if err := pgqueue.InitSchema(bg, dsn); err != nil {
			t.Fatal(err)
		}
	}
	if f.stats {
		if err := opstats.InitSchema(bg, dsn); err != nil {
			t.Fatal(err)
		}
	}
	if f.web {
		if _, err := webschema.Init(bg, dsn); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := pgxpool.New(bg, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	e := &env{t: t, dsn: dsn, pool: pool, clk: &clock{t: time.Now().UTC()}}
	e.svc = NewService(countingDB{pool, &e.qs}, WithClock(e.clk.now))
	return e
}

func (e *env) apply(d *opstats.Delta) {
	e.t.Helper()
	if err := pgx.BeginFunc(bg, e.pool, func(tx pgx.Tx) error { _, err := opstats.Apply(bg, tx, d); return err }); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(bg, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func hour(day string, h int) time.Time {
	t, _ := time.Parse("2006-01-02", day)
	return t.Add(time.Duration(h) * time.Hour)
}

func hd(h time.Time, mutate func(*opstats.HourDelta)) opstats.HourDelta {
	d := opstats.HourDelta{Hour: h, Coverage: opstats.FullHourMask}
	if mutate != nil {
		mutate(&d)
	}
	return d
}

func (e *env) series(chart, gran, period string) (Series, error) {
	s, _, err := e.svc.Series(bg, admin, Query{Chart: chart, Granularity: gran, Period: period})
	return s, err
}

func TestSeriesReturnsNullForMissingBucketsAndZeroForMeasuredZero(t *testing.T) {
	e := newEnv(t, flags{stats: true})
	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	e.apply(&opstats.Delta{Hours: []opstats.HourDelta{
		hd(hour(day, 10), nil), // observed, zero events: a measured zero
		hd(hour(day, 12), func(h *opstats.HourDelta) { h.Counters[opstats.CRecordsReceived] = 9 }),
	}})
	s, err := e.series(ChartThroughput, GranDay, day)
	if err != nil {
		t.Fatal(err)
	}
	recv := s.Lines[0]
	if recv.Key != "received" || len(recv.Values) != 24 {
		t.Fatalf("line %q with %d values", recv.Key, len(recv.Values))
	}
	if recv.Values[10] == nil || *recv.Values[10] != 0 {
		t.Fatal("hour 10 was measured as zero and must be 0, not a gap")
	}
	if recv.Values[11] != nil {
		t.Fatal("hour 11 has no row: it must be a gap (null), never 0")
	}
	if recv.Values[12] == nil || *recv.Values[12] != 9 {
		t.Fatalf("hour 12 = %v", recv.Values[12])
	}
	if s.Partial[10] || s.Partial[12] {
		t.Fatal("fully covered past hours must not be partial")
	}
}

func TestThroughputSeriesCarriesEnqueuedBesideReceivedAcknowledgedAndSettled(t *testing.T) {
	e := newEnv(t, flags{stats: true})
	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	e.apply(&opstats.Delta{Hours: []opstats.HourDelta{hd(hour(day, 3), func(h *opstats.HourDelta) {
		h.Counters[opstats.CRecordsReceived], h.Counters[opstats.CRecordsAcknowledged], h.Counters[opstats.CRecordsEnqueued] = 10, 9, 7
		h.Counters[opstats.CSettledStoredNew], h.Counters[opstats.CSettledStoredExisting], h.Counters[opstats.CSettledQuarantined] = 4, 2, 1
	})}})
	s, err := e.series(ChartThroughput, GranDay, day)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, l := range s.Lines {
		if v := l.Values[3]; v != nil {
			got[l.Key] = *v
		}
	}
	want := map[string]float64{"received": 10, "acknowledged": 9, "enqueued": 7, "settled": 7}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (all four series must be present: %v)", k, got[k], v, got)
		}
	}
}

func TestSeriesRatiosAreWeighted(t *testing.T) {
	e := newEnv(t, flags{stats: true})
	day := time.Now().UTC().AddDate(0, 0, -1)
	d := day.Format("2006-01-02")
	e.apply(&opstats.Delta{Hours: []opstats.HourDelta{
		hd(hour(d, 1), func(h *opstats.HourDelta) {
			h.Counters[opstats.CExistsHashes], h.Counters[opstats.CExistsRequests] = 100, 1
		}),
		hd(hour(d, 2), func(h *opstats.HourDelta) {
			h.Counters[opstats.CExistsHashes], h.Counters[opstats.CExistsRequests] = 100, 99
		}),
	}})
	// Hourly: 100 and ~1.01 hashes per request.
	s, _ := e.series(ChartTraffic, GranDay, d)
	hpr := lineByKey(t, s, "hashes_per_request")
	if *hpr.Values[1] != 100 || *hpr.Values[2] < 1.0 || *hpr.Values[2] > 1.02 {
		t.Fatalf("hourly ratios: %v %v", *hpr.Values[1], *hpr.Values[2])
	}
	// Daily: Σ hashes ÷ Σ requests = 200/100 = 2, NOT the mean of the hourly ratios (≈ 50.5).
	m, err := e.series(ChartTraffic, GranMonth, day.Format("2006-01"))
	if err != nil {
		t.Fatal(err)
	}
	dayIdx := day.Day() - 1
	if v := lineByKey(t, m, "hashes_per_request").Values[dayIdx]; v == nil || *v != 2 {
		t.Fatalf("daily ratio = %v, want the ratio of sums 2", v)
	}
	// A zero denominator is a gap, not zero.
	if hour3 := lineByKey(t, s, "records_per_request").Values[1]; hour3 != nil {
		t.Fatalf("a ratio with no requests = %v, want a gap", *hour3)
	}
	// Bytes per torrent: ratio of sums across samples.
	var st opstats.Delta
	st.AddStorageSample(storageSample(hour(d, 5).Add(time.Minute), 1, 1000, 10))
	st.AddStorageSample(storageSample(hour(d, 5).Add(6*time.Minute), 2, 3000, 20))
	e.apply(&st)
	ss, err := e.series(ChartStorage, GranDay, d)
	if err != nil {
		t.Fatal(err)
	}
	if v := lineByKey(t, ss, "bytes_per_torrent").Values[5]; v == nil || *v != 4000.0/30 {
		t.Fatalf("bytes per torrent = %v, want 4000/30 (ratio of sums), not the mean of 100 and 150", v)
	}
}

func storageSample(at time.Time, slot int64, total, est int64) opstats.StorageState {
	s := opstats.StorageState{At: at, Slot: slot, Reconciliation: opstats.ReconOK}
	for m := opstats.Measure(0); m < opstats.NumMeasures; m++ {
		s.Valid[m], s.Values[m] = true, total/10
	}
	s.Values[opstats.MMagneticodTotal], s.Values[opstats.MIndexedTorrents] = total, est
	return s
}

func lineByKey(t *testing.T, s Series, key string) Line {
	t.Helper()
	for _, l := range s.Lines {
		if l.Key == key {
			return l
		}
	}
	t.Fatalf("no line %q in %d lines", key, len(s.Lines))
	return Line{}
}

func TestSeriesRejectsOutOfRangePeriods(t *testing.T) {
	e := newEnv(t, flags{stats: true})
	now := time.Now().UTC()
	e.apply(&opstats.Delta{Hours: []opstats.HourDelta{hd(now.Truncate(time.Hour).Add(-2*time.Hour), nil)}})
	for _, tc := range []struct {
		name string
		q    Query
		err  error
	}{
		{"unknown chart", Query{"nope", GranDay, ""}, ErrBadQuery},
		{"unknown granularity", Query{ChartQueue, "week", ""}, ErrBadQuery},
		{"malformed day", Query{ChartQueue, GranDay, "2026-13-45"}, ErrBadQuery},
		{"day with trailing text", Query{ChartQueue, GranDay, "2026-11-25x"}, ErrBadQuery},
		{"month for a day query", Query{ChartQueue, GranDay, "2026-11"}, ErrBadQuery},
		{"huge year", Query{ChartQueue, GranYear, "9999"}, ErrBadQuery},
		{"year 1999", Query{ChartQueue, GranYear, "1999"}, ErrBadQuery},
		{"a future day", Query{ChartQueue, GranDay, now.AddDate(0, 0, 3).Format("2006-01-02")}, ErrPeriodOutOfRange},
		{"before the first data", Query{ChartQueue, GranDay, now.AddDate(0, 0, -10).Format("2006-01-02")}, ErrPeriodOutOfRange},
		{"a negative-looking period", Query{ChartQueue, GranMonth, "-2026-1"}, ErrBadQuery},
	} {
		if _, _, err := e.svc.Series(bg, admin, tc.q); !errors.Is(err, tc.err) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.err)
		}
	}
	for _, q := range []Query{{ChartQueue, GranDay, ""}, {ChartQueue, GranMonth, ""}, {ChartQueue, GranYear, now.Format("2006")}} {
		if _, _, err := e.svc.Series(bg, admin, q); err != nil {
			t.Errorf("%+v: %v", q, err)
		}
	}
	// Navigation: only neighbours that exist.
	s, _ := e.series(ChartQueue, GranDay, now.Format("2006-01-02"))
	if s.Next != "" {
		t.Errorf("the current period has a next: %q", s.Next)
	}
}

func TestCoverageNeverExceedsBucketLength(t *testing.T) {
	e := newEnv(t, flags{stats: true})
	d := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	full := opstats.Delta{Hours: []opstats.HourDelta{hd(hour(d, 4), nil)}}
	for i := 0; i < 5; i++ { // five instances covering the very same minutes
		e.apply(&full)
	}
	rows, err := e.svc.r.aggRows(bg, GranDay, hour(d, 0), hour(d, 24))
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[hour(d, 4)].covered; got != 60 {
		t.Fatalf("hour coverage = %d minutes, want 60", got)
	}
	m, _ := e.svc.r.aggRows(bg, GranMonth, hour(d, 0), hour(d, 24))
	if got := m[hour(d, 0)].covered; got != 60 {
		t.Fatalf("day coverage = %d minutes, want 60 (the union, never the sum)", got)
	}
}

func TestUnavailableIsNeverZero(t *testing.T) {
	e := newEnv(t, flags{stats: true})
	// A shared row whose queue was sampled long ago, with values: they must not be shown.
	e.exec(`UPDATE magnetico_stats.shared_state SET queue_sampled_at = now() - interval '20 minutes', queue_rows = 99, queue_bytes = 99, quota_used_bytes = 5,
 quota_max_bytes = 100, storage_sampled_at = now() - interval '2 hours', storage_magneticod_total_bytes = 12345`)
	o, f, err := e.svc.Overview(bg, admin)
	if err != nil {
		t.Fatal(err)
	}
	if o.Queue.State != StateUnavailable || o.Queue.Rows.Valid || o.Queue.Bytes.Valid || o.Queue.Utilisation.Valid {
		t.Fatalf("an expired queue sample was shown: %+v", o.Queue)
	}
	if o.Storage.State != StateUnavailable || o.Storage.Magneticod.Valid {
		t.Fatalf("an expired storage sample was shown: %+v", o.Storage)
	}
	if f.Queue != StateUnavailable || f.Storage != StateUnavailable || f.Ingest != StateUnavailable {
		t.Fatalf("freshness %+v", f)
	}
	// Fresh sample, one gauge NULL: that gauge is unavailable, the others show.
	e.exec(`UPDATE magnetico_stats.shared_state SET queue_sampled_at = now() - interval '5 seconds', queue_rows = 7, queue_bytes = NULL, oldest_age_s = NULL`)
	e.clk.advance(time.Minute) // expire the 10 s cache
	o, _, _ = e.svc.Overview(bg, admin)
	if !o.Queue.Rows.Valid || o.Queue.Rows.Value != 7 || o.Queue.Bytes.Valid || o.Queue.OldestAge.Valid {
		t.Fatalf("queue view: %+v", o.Queue)
	}
	if !o.Queue.Utilisation.Valid || o.Queue.Utilisation.Value != 0.05 {
		t.Fatalf("utilisation %+v", o.Queue.Utilisation)
	}
}

func TestFreshnessClocksAreIndependent(t *testing.T) {
	e := newEnv(t, flags{stats: true})
	id := opstats.NewInstanceID()
	e.exec(`INSERT INTO magnetico_stats.snapshot (instance_id, started_at, published_at, window_end, w60_covered_s, w60_records_received)
VALUES ($1, now(), now(), now(), 60, 120)`, id)
	e.exec(`UPDATE magnetico_stats.shared_state SET queue_sampled_at = now() - interval '10 minutes', queue_rows = 1`) // queue stale, ingest up
	_, f, err := e.svc.Overview(bg, admin)
	if err != nil {
		t.Fatal(err)
	}
	if f.Ingest != StateUp || f.Queue != StateStale || f.Storage != StateUnavailable {
		t.Fatalf("freshness %+v: a failing queue sample must leave the instance up", f)
	}
}

func TestSharedStateIsReadOnceAndNeverSummed(t *testing.T) {
	e := newEnv(t, flags{stats: true})
	for i := 0; i < 2; i++ {
		e.exec(`INSERT INTO magnetico_stats.snapshot (instance_id, started_at, published_at, window_end, w60_covered_s, w60_records_received, total_records_received)
VALUES ($1, now(), now(), now(), 60, 60, 500)`, opstats.NewInstanceID())
	}
	e.exec(`UPDATE magnetico_stats.shared_state SET queue_sampled_at = now(), queue_rows = 40, queue_bytes = 4000, quota_used_bytes = 4000`)
	o, _, err := e.svc.Overview(bg, admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Instances) != 2 || !o.Queue.Rows.Valid || o.Queue.Rows.Value != 40 {
		t.Fatalf("queue rows %+v with two instances: the shared state must be read once, not summed (80)", o.Queue.Rows)
	}
	if o.Rates.Windows[0].Values[0].Value != 2 { // 1/s per instance: event rates DO sum
		t.Fatalf("event rate %v, want 2 per second", o.Rates.Windows[0].Values[0].Value)
	}
}

func TestCacheNeverStoresPerUserData(t *testing.T) {
	e := newEnv(t, flags{stats: true})
	if _, _, err := e.svc.Overview(bg, access.Grant(1, "alice")); err != nil {
		t.Fatal(err)
	}
	n := e.qs.Load()
	if _, _, err := e.svc.Overview(bg, access.Grant(2, "bob")); err != nil {
		t.Fatal(err)
	}
	if e.qs.Load() != n {
		t.Fatal("a second administrator caused another query: the entry is keyed by data, not by user, and is shared")
	}
	// An invalid Principal is refused before any query or cache access.
	before := e.qs.Load()
	for name, p := range map[string]access.Principal{"zero": {}, "forged zero value": access.Principal{}} {
		if _, _, err := e.svc.Overview(bg, p); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s: %v", name, err)
		}
		if _, _, err := e.svc.Series(bg, p, Query{Chart: ChartQueue, Granularity: GranDay}); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s series: %v", name, err)
		}
		if _, _, err := e.svc.Crawlers(bg, p, Cursor{}); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s crawlers: %v", name, err)
		}
		if _, err := e.svc.IngestStatus(bg, p); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s ingest status: %v", name, err)
		}
	}
	if e.qs.Load() != before {
		t.Fatal("a refused Principal reached the database")
	}
}

func TestUsersCountsAreOptional(t *testing.T) {
	e := newEnv(t, flags{stats: true}) // no web schema
	o, _, err := e.svc.Overview(bg, admin)
	if err != nil {
		t.Fatal(err)
	}
	if o.Users.Total.Valid {
		t.Fatal("user counts shown without a web schema")
	}
	e2 := newEnv(t, flags{stats: true, web: true})
	e2.exec(`INSERT INTO magnetico_web.users (username, role) VALUES ('a', 'admin'), ('b', 'user'), ('c', 'user')`)
	e2.exec(`UPDATE magnetico_web.users SET active = false WHERE username = 'c'`)
	o, _, err = e2.svc.Overview(bg, admin)
	if err != nil || o.Users.Total.Value != 3 || o.Users.Active.Value != 2 || o.Users.Admins.Value != 1 {
		t.Fatalf("%+v %v", o.Users, err)
	}
}

func TestOverviewWithoutTheStatsSchemaSaysSo(t *testing.T) {
	e := newEnv(t, flags{})
	o, f, err := e.svc.Overview(bg, admin)
	if err != nil || o.Schema != StateUninitialised || f.Ingest != StateUninitialised {
		t.Fatalf("%+v %+v %v", o.Schema, f, err)
	}
	s, err := e.series(ChartThroughput, GranDay, "")
	if err != nil || len(s.Buckets) != 24 {
		t.Fatalf("series without the schema: %v %d", err, len(s.Buckets))
	}
	// An incompatible schema is "unavailable", not an error and not data.
	e2 := newEnv(t, flags{stats: true})
	e2.exec(`ALTER TABLE magnetico_stats.snapshot ADD COLUMN junk text`)
	o, _, err = e2.svc.Overview(bg, admin)
	if err != nil || o.Schema != StateUnavailable {
		t.Fatalf("incompatible schema: %v %v", o.Schema, err)
	}
}

// ---- crawlers -----------------------------------------------------------------------

func TestCrawlersListIncludesNeverConnectedAndDisabledCrawlers(t *testing.T) {
	e := newEnv(t, flags{stats: true, ingest: true})
	store := crawlers.New(e.pool)
	var ids []string
	for _, n := range []string{"alpha", "Bravo", "charlie"} {
		c, err := store.Add(bg, n)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	if err := store.SetEnabled(bg, "charlie", false); err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO magnetico_stats.crawler_activity (crawler_id, first_seen_at, last_seen_at, last_ip, last_submit_at, records_received, records_enqueued)
VALUES ($1, now() - interval '1 day', now(), '192.0.2.9', now(), 100, 80)`, ids[0])
	page, _, err := e.svc.Crawlers(bg, admin, Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Ingest.State != IngestOK || !page.StatsAvailable || len(page.Items) != 3 {
		t.Fatalf("page: %+v", page)
	}
	if page.Items[0].Name != "alpha" || page.Items[1].Name != "Bravo" || page.Items[2].Name != "charlie" {
		t.Fatalf("order: %v %v %v", page.Items[0].Name, page.Items[1].Name, page.Items[2].Name)
	}
	if a := page.Items[0].Activity; a == nil || a.Received != 100 || a.Enqueued != 80 || a.LastIP != "192.0.2.9" || a.LastSubmit == nil {
		t.Fatalf("activity: %+v", page.Items[0].Activity)
	}
	if page.Items[1].Activity != nil || page.Items[2].Activity != nil {
		t.Fatal("a crawler that never connected has activity")
	}
	if page.Items[2].Enabled || page.Items[2].DisabledAt == nil {
		t.Fatalf("the disabled crawler: %+v", page.Items[2])
	}
}

func TestCrawlersQueryIsBoundedForAnyRegisteredCount(t *testing.T) {
	e := newEnv(t, flags{stats: true, ingest: true})
	e.exec(`INSERT INTO magnetico_ingest.crawlers (crawler_id, name, token_hash)
SELECT gen_random_uuid(), 'c' || lpad(g::text, 6, '0'), decode(md5(g::text) || md5(g::text || 'x'), 'hex') FROM generate_series(1, 10000) g`)
	e.exec(`ANALYZE magnetico_ingest.crawlers`)
	seen := map[string]bool{}
	var cur Cursor
	pages := 0
	for {
		page, _, err := e.svc.Crawlers(bg, admin, cur)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > CrawlerPageSize {
			t.Fatalf("a page of %d rows", len(page.Items))
		}
		for _, it := range page.Items {
			if seen[it.Name] {
				t.Fatalf("%s listed twice: the keyset is not stable", it.Name)
			}
			seen[it.Name] = true
		}
		pages++
		if page.Next == nil || pages >= 400 {
			break
		}
		cur = *page.Next
	}
	if len(seen) != 10000 || pages != 200 {
		t.Fatalf("%d crawlers in %d pages", len(seen), pages)
	}
	// The plan is an index scan with no sort: independent of the registered count.
	rows, err := e.pool.Query(bg, `EXPLAIN (FORMAT TEXT) `+strings.Replace(strings.Replace(crawlersSQL(true), "$1", "'c005000'", 1), "$2", "51", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var l string
		_ = rows.Scan(&l)
		plan.WriteString(l + "\n")
	}
	if !strings.Contains(plan.String(), "Index") || strings.Contains(plan.String(), "Sort") {
		t.Fatalf("the page query is not an index scan without a sort:\n%s", plan.String())
	}
}

func TestCrawlersWorkWithoutTheStatsSchema(t *testing.T) {
	e := newEnv(t, flags{ingest: true})
	if _, err := crawlers.New(e.pool).Add(bg, "alpha"); err != nil {
		t.Fatal(err)
	}
	page, _, err := e.svc.Crawlers(bg, admin, Cursor{})
	if err != nil || page.StatsAvailable || len(page.Items) != 1 || page.Items[0].Activity != nil {
		t.Fatalf("%+v %v: the registry must be listed without statistics", page, err)
	}
}

func TestIngestProbeClassifiesMissingIncompatibleAndUnprivileged(t *testing.T) {
	// missing
	e := newEnv(t, flags{stats: true})
	st, err := e.svc.IngestStatus(bg, admin)
	if err != nil || st.State != IngestMissing {
		t.Fatalf("missing: %+v %v", st, err)
	}
	if page, _, err := e.svc.Crawlers(bg, admin, Cursor{}); err != nil || page.Ingest.State != IngestMissing || len(page.Items) != 0 {
		t.Fatalf("crawlers page over a missing ingest schema: %+v %v", page, err)
	}
	// incompatible: an older development schema without crawlers
	e2 := newEnv(t, flags{stats: true, ingest: true})
	e2.exec(`DROP TABLE magnetico_ingest.crawlers`)
	if st, _ := e2.svc.IngestStatus(bg, admin); st.State != IngestIncompatible || !strings.Contains(strings.Join(st.Missing, ","), "crawlers") {
		t.Fatalf("incompatible: %+v", st)
	}
	// privileges: a role that may use the schema but has no column grants, then the documented grants
	e3 := newEnv(t, flags{stats: true, ingest: true})
	role := "web_" + pgtest.RandomSuffix()
	cl := pgtest.IntegrationCluster(t)
	e3.exec(`CREATE ROLE ` + pgtest.QuoteIdent(role) + ` LOGIN`)
	t.Cleanup(func() {
		if c, err := pgx.Connect(bg, e3.dsn); err == nil {
			_, _ = c.Exec(bg, `DROP OWNED BY `+pgtest.QuoteIdent(role))
			c.Close(bg)
		}
		if c, err := pgx.Connect(bg, cl.AdminDSN); err == nil {
			_, _ = c.Exec(bg, `DROP ROLE IF EXISTS `+pgtest.QuoteIdent(role))
			c.Close(bg)
		}
	})
	rolePool := func() *Service {
		cfg, _ := pgxpool.ParseConfig(e3.dsn)
		cfg.ConnConfig.User = role
		p, err := pgxpool.NewWithConfig(bg, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return NewService(p)
	}
	svc := rolePool()
	if st, _ := svc.IngestStatus(bg, admin); st.State != IngestPrivileges || st.Missing[0] != "magnetico_ingest: USAGE" {
		t.Fatalf("no usage: %+v", st)
	}
	e3.exec(`GRANT USAGE ON SCHEMA magnetico_ingest, magnetico_stats TO ` + pgtest.QuoteIdent(role))
	e3.exec(`GRANT SELECT ON ALL TABLES IN SCHEMA magnetico_stats TO ` + pgtest.QuoteIdent(role))
	svc = rolePool()
	st, _ = svc.IngestStatus(bg, admin)
	if st.State != IngestPrivileges || len(st.Missing) != len(crawlerColumns)+len(quarantineColumns) {
		t.Fatalf("no column grants: %+v", st)
	}
	for _, m := range st.Missing {
		if strings.Contains(m, "token_hash") || strings.Contains(m, "(payload)") || strings.Contains(m, "(info_hash)") {
			t.Fatalf("the probe asks for a forbidden column: %s", m)
		}
	}
	e3.exec(`GRANT SELECT (crawler_id, name, enabled, created_at, token_rotated_at, disabled_at) ON magnetico_ingest.crawlers TO ` + pgtest.QuoteIdent(role))
	e3.exec(`GRANT SELECT (id, reason_code, detail, attempts, payload_bytes, received_at, quarantined_at) ON magnetico_ingest.quarantine TO ` + pgtest.QuoteIdent(role))
	svc = rolePool()
	if st, _ := svc.IngestStatus(bg, admin); st.State != IngestOK {
		t.Fatalf("with the documented grants: %+v", st)
	}
	if _, err := crawlers.New(e3.pool).Add(bg, "alpha"); err != nil {
		t.Fatal(err)
	}
	page, _, err := svc.Crawlers(bg, admin, Cursor{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Name != "alpha" {
		t.Fatalf("crawlers through the restricted role: %+v %v", page, err)
	}
}

func TestColdAndCachedLatencyBudget(t *testing.T) {
	e := newEnv(t, flags{stats: true, web: true})
	e.exec(`INSERT INTO magnetico_stats.snapshot (instance_id, started_at, published_at, window_end, w60_covered_s) VALUES ($1, now(), now(), now(), 60)`, opstats.NewInstanceID())
	cold := time.Now()
	if _, _, err := e.svc.Overview(bg, admin); err != nil {
		t.Fatal(err)
	}
	coldD := time.Since(cold)
	warm := time.Now()
	for i := 0; i < 100; i++ {
		if _, _, err := e.svc.Overview(bg, admin); err != nil {
			t.Fatal(err)
		}
	}
	warmD := time.Since(warm) / 100
	t.Logf("cold overview %v, cached %v", coldD, warmD)
	if coldD > 2*time.Second || warmD > 5*time.Millisecond {
		t.Fatalf("latency budget exceeded: cold %v cached %v", coldD, warmD)
	}
}

var _ = fmt.Sprint
