//go:build loadtest

package cli

// Task 09A: the "statistics tables locked" sabotage at a PRODUCTION-LIKE rate.
//
// The Task 09 qualification saturated the ingest with closed-loop submitters
// (tens of thousands of records per second) and reported the locked-tables
// sabotage as not met (54 % of the disabled baseline, 55 % for a control holding an
// unrelated idle transaction): a method artifact of the saturating load, and a
// result that stays on record in docs/wip/09-statistics-admin-report.md. The owner
// decided that the artificial rate is no reason to optimise. This test measures the
// same sabotage at the rate a real deployment sees, with the controls, and adds the
// figures that explain it: admission latency, throughput, queue depth, backlog
// recovery, the age of the holding transaction, dead tuples and vacuum activity.
//
//	conditions (alternated, MAGNETICO_STATS_QUALIFY_ROUNDS rounds, default 2):
//	  control            statistics disabled, nothing else
//	  control-idle-tx    statistics disabled, an idle transaction holding an
//	                     unrelated table lock open (the same kind of transaction)
//	  enabled            statistics enabled, nothing else
//	  enabled-locked     statistics enabled, every statistics table the writer needs
//	                     locked ACCESS EXCLUSIVE by an open transaction
//
// MAGNETICO_STATS_OUT collects the JSON lines; MAGNETICO_STATS_QUALIFY_SCALE scales the
// durations; MAGNETICO_STATS_RATE overrides the 200 records/s.

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type sabotageRateResult struct {
	Cond            string  `json:"cond"`
	Round           int     `json:"round"`
	TargetRate      float64 `json:"target_records_per_s"`
	AdmittedPerS    float64 `json:"admitted_records_per_s"`
	AdmitP50Ms      float64 `json:"admission_p50_ms"`
	AdmitP95Ms      float64 `json:"admission_p95_ms"`
	AdmitP99Ms      float64 `json:"admission_p99_ms"`
	Refused         int64   `json:"records_refused_or_failed"`
	QueueMax        int64   `json:"queue_depth_max"`
	QueueEnd        int64   `json:"queue_depth_at_end_of_load"`
	DrainSeconds    float64 `json:"backlog_drain_s"`
	TxAgeMaxSeconds float64 `json:"holding_transaction_age_max_s"`
	XminAgeMax      int64   `json:"xmin_age_max_xids"`
	DeadQueueMax    int64   `json:"queue_dead_tuples_max"`
	DeadUsageMax    int64   `json:"usage_dead_tuples_max"`
	Autovacuums     int64   `json:"queue_autovacuums_during_load"`
	StatsCatchUpS   float64 `json:"statistics_catch_up_after_release_s"`
	StatsConserved  string  `json:"statistics_conservation"`
	IngestWarnings  int     `json:"ingest_warning_lines"`
}

func TestQualifySabotageAtProductionRate(t *testing.T) {
	requireOut(t)
	rounds := 2
	if v := os.Getenv("MAGNETICO_STATS_QUALIFY_ROUNDS"); v != "" {
		rounds, _ = strconv.Atoi(v)
	}
	rate := 200.0
	if v := os.Getenv("MAGNETICO_STATS_RATE"); v != "" {
		rate, _ = strconv.ParseFloat(v, 64)
	}
	idleTx := func(t *testing.T, s *statsWorld) *pgx.Conn {
		c, err := pgx.Connect(context.Background(), s.dsn)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{`BEGIN`, `SELECT txid_current()`, `LOCK TABLE magnetico_web.settings IN ACCESS EXCLUSIVE MODE`} {
			if _, err := c.Exec(context.Background(), q); err != nil {
				t.Fatal(err)
			}
		}
		return c
	}
	lockStats := func(t *testing.T, s *statsWorld) *pgx.Conn {
		c, err := pgx.Connect(context.Background(), s.dsn)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{`BEGIN`, `SELECT txid_current()`,
			`LOCK TABLE magnetico_stats.agg_hour, magnetico_stats.ingest_instances, magnetico_stats.snapshot, magnetico_stats.crawler_activity, magnetico_stats.shared_state IN ACCESS EXCLUSIVE MODE`} {
			if _, err := c.Exec(context.Background(), q); err != nil {
				t.Fatal(err)
			}
		}
		return c
	}
	conds := []struct {
		name  string
		env   []string
		apply func(*testing.T, *statsWorld) *pgx.Conn
	}{
		{"control", []string{"MAGNETICO_INGEST_STATS_DISABLED=true"}, nil},
		{"control-idle-tx", []string{"MAGNETICO_INGEST_STATS_DISABLED=true"}, idleTx},
		{"enabled", nil, nil},
		{"enabled-locked", nil, lockStats},
	}
	var all []sabotageRateResult
	for round := 1; round <= rounds; round++ {
		for k := range conds {
			c := conds[(k+round-1)%len(conds)] // alternate the order between rounds
			t.Run(fmt.Sprintf("%s/%d", c.name, round), func(t *testing.T) {
				r := sabotageRateRun(t, c.name, round, rate, c.env, c.apply)
				qualOut(t, "sabotage_rate", r)
				all = append(all, r)
			})
		}
	}
	// summary: median admitted rate and p95 per condition, relative to the control
	by := map[string][]sabotageRateResult{}
	for _, r := range all {
		by[r.Cond] = append(by[r.Cond], r)
	}
	med := func(cond string, f func(sabotageRateResult) float64) float64 {
		var v []float64
		for _, r := range by[cond] {
			v = append(v, f(r))
		}
		return median(v)
	}
	sum := map[string]any{}
	for _, c := range conds {
		sum[c.name] = map[string]float64{
			"admitted_rate_vs_control": med(c.name, func(r sabotageRateResult) float64 { return r.AdmittedPerS }) / med("control", func(r sabotageRateResult) float64 { return r.AdmittedPerS }),
			"admission_p95_ms":         med(c.name, func(r sabotageRateResult) float64 { return r.AdmitP95Ms }),
			"admission_p99_ms":         med(c.name, func(r sabotageRateResult) float64 { return r.AdmitP99Ms }),
			"queue_depth_max":          med(c.name, func(r sabotageRateResult) float64 { return float64(r.QueueMax) }),
		}
	}
	qualOut(t, "sabotage_rate_summary", sum)
	// The criterion of the plan, now at a production-like rate: with the statistics
	// locked, admission throughput stays at or above 90 % of the baseline and nothing
	// is refused because of the statistics.
	locked := med("enabled-locked", func(r sabotageRateResult) float64 { return r.AdmittedPerS })
	base := med("control", func(r sabotageRateResult) float64 { return r.AdmittedPerS })
	if locked < 0.9*base {
		t.Errorf("admitted rate with the statistics locked %.1f rec/s is %.0f %% of the control %.1f rec/s (criterion: at least 90 %%)", locked, 100*locked/base, base)
	}
	for _, r := range by["enabled-locked"] {
		if r.Refused != 0 {
			t.Errorf("round %d: %d records were refused or failed while the statistics were locked", r.Round, r.Refused)
		}
	}
}

func sabotageRateRun(t *testing.T, cond string, round int, rate float64, env []string, apply func(*testing.T, *statsWorld) *pgx.Conn) sabotageRateResult {
	s := newStatsWorld(t)
	tok := s.crawler("load")
	s.startIngest(env...)
	res := sabotageRateResult{Cond: cond, Round: round, TargetRate: rate}

	const batchSize, workers = 50, 4
	interval := time.Duration(float64(time.Second) * batchSize * workers / rate)
	sb := &submitter{addr: s.addr, token: tok}
	runFor := func(d time.Duration, collect bool) []float64 {
		var mu sync.Mutex
		var lat []float64
		var wg sync.WaitGroup
		end := time.Now().Add(d)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				time.Sleep(time.Duration(w) * interval / workers) // spread the workers
				next := time.Now()
				for next.Before(end) {
					t0 := time.Now()
					sb.batch(batchSize)
					if collect {
						mu.Lock()
						lat = append(lat, float64(time.Since(t0).Microseconds())/1000)
						mu.Unlock()
					}
					next = next.Add(interval) // open loop at a fixed pace
					if d := time.Until(next); d > 0 {
						time.Sleep(d)
					}
				}
			}(w)
		}
		wg.Wait()
		return lat
	}
	runFor(scaled(30*time.Second), false) // warm-up: connections, caches
	sb.ok.Store(0)
	sb.unavailable.Store(0)
	sb.other.Store(0)
	sb.transport.Store(0)

	var holder *pgx.Conn
	if apply != nil {
		holder = apply(t, s)
		t.Cleanup(func() { _ = holder.Close(context.Background()) })
	}
	window := scaled(5 * time.Minute)
	mon, err := pgx.Connect(context.Background(), s.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer mon.Close(context.Background())
	scalar := func(sql string) int64 {
		var n int64
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := mon.QueryRow(ctx, sql).Scan(&n); err != nil {
			return -1
		}
		return n
	}
	av0 := scalar(`SELECT COALESCE(autovacuum_count, 0) FROM pg_stat_user_tables WHERE schemaname = 'magnetico_ingest' AND relname = 'queue'`)
	stopMon := make(chan struct{})
	var monWG sync.WaitGroup
	monWG.Add(1)
	go func() {
		defer monWG.Done()
		for {
			select {
			case <-stopMon:
				return
			case <-time.After(scaledMin(15*time.Second, time.Second)):
			}
			res.QueueMax = max(res.QueueMax, scalar(`SELECT count(*) FROM magnetico_ingest.queue`))
			res.DeadQueueMax = max(res.DeadQueueMax, scalar(`SELECT COALESCE(n_dead_tup, 0) FROM pg_stat_user_tables WHERE schemaname = 'magnetico_ingest' AND relname = 'queue'`))
			res.DeadUsageMax = max(res.DeadUsageMax, scalar(`SELECT COALESCE(n_dead_tup, 0) FROM pg_stat_user_tables WHERE schemaname = 'magnetico_ingest' AND relname = 'usage'`))
			res.XminAgeMax = max(res.XminAgeMax, scalar(`SELECT COALESCE(max(age(backend_xmin)), 0) FROM pg_stat_activity WHERE datname = current_database()`))
			if age := scalar(`SELECT COALESCE(max(EXTRACT(EPOCH FROM now() - xact_start)), 0)::bigint FROM pg_stat_activity WHERE datname = current_database() AND state = 'idle in transaction'`); float64(age) > res.TxAgeMaxSeconds {
				res.TxAgeMaxSeconds = float64(age)
			}
		}
	}()
	begin := time.Now()
	lat := runFor(window, true)
	took := time.Since(begin)
	close(stopMon)
	monWG.Wait()
	slices.Sort(lat)
	res.AdmitP50Ms, res.AdmitP95Ms, res.AdmitP99Ms = percentile(lat, .5), percentile(lat, .95), percentile(lat, .99)
	res.AdmittedPerS = float64(sb.ok.Load()) / took.Seconds()
	res.Refused = sb.unavailable.Load() + sb.other.Load() + sb.transport.Load()
	res.QueueEnd = scalar(`SELECT count(*) FROM magnetico_ingest.queue`)
	res.QueueMax = max(res.QueueMax, res.QueueEnd)
	res.Autovacuums = scalar(`SELECT COALESCE(autovacuum_count, 0) FROM pg_stat_user_tables WHERE schemaname = 'magnetico_ingest' AND relname = 'queue'`) - av0

	// backlog recovery: the producers stopped, the consumer works the queue down
	drainStart := time.Now()
	for scalar(`SELECT count(*) FROM magnetico_ingest.queue`) > 0 && time.Since(drainStart) < 10*time.Minute {
		time.Sleep(time.Second)
	}
	res.DrainSeconds = time.Since(drainStart).Seconds()

	// release the holder; with the statistics enabled they catch up and conserve
	if holder != nil {
		_ = holder.Close(context.Background())
	}
	if env == nil || len(env) == 0 { // statistics enabled
		// The heartbeat table is locked too, so its stalled-seconds field cannot tell: the
		// statistics have caught up when the persisted acknowledged total equals what the
		// producers saw acknowledged (the load has stopped and the queue is drained).
		release := time.Now()
		caught := false
		for time.Since(release) < 5*time.Minute { // recorded, not fatal: the admission figures above must survive
			if int64(s.sum("records_acknowledged")) >= sb.ok.Load() {
				caught = true
				break
			}
			time.Sleep(time.Second)
		}
		res.StatsCatchUpS = time.Since(release).Seconds()
		if !caught {
			t.Errorf("the persisted statistics did not catch up within 5 minutes after the release")
		}
		s.stopIngest()
		rec, ack, ref, enq, dup := s.sum("records_received"), s.sum("records_acknowledged"), s.sum("records_refused"), s.sum("records_enqueued"), s.sum("admission_duplicates")
		ok := rec-ack-ref == 0 && ack == enq+dup && int64(ack) >= sb.ok.Load()
		res.StatsConserved = fmt.Sprintf("received %d acknowledged %d refused %d enqueued %d duplicates %d (client saw %d acknowledged): ok=%v", rec, ack, ref, enq, dup, sb.ok.Load(), ok)
		if !ok {
			t.Errorf("statistics are not conserved after the release: %s", res.StatsConserved)
		}
	} else {
		s.stopIngest()
	}
	for _, l := range strings.Split(s.ing.out.String(), "\n") {
		if strings.Contains(l, "level=WARN") || strings.Contains(l, "level=ERROR") {
			res.IngestWarnings++
		}
	}
	t.Logf("%s #%d: %.1f rec/s admitted (target %.0f), p95 %.1f ms, queue max %d, drain %.0f s, tx age max %.0f s, dead tuples queue/usage %d/%d, autovacuums %d, stats catch-up %.0f s",
		cond, round, res.AdmittedPerS, rate, res.AdmitP95Ms, res.QueueMax, res.DrainSeconds, res.TxAgeMaxSeconds, res.DeadQueueMax, res.DeadUsageMax, res.Autovacuums, res.StatsCatchUpS)
	return res
}

func scaledMin(d, floor time.Duration) time.Duration { return max(scaled(d), floor) }
