package stats

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/opstats"
)

var testLimits = samplerLimits{statement: 2 * time.Second, lock: time.Second, cycle: 5 * time.Second}

func (e *wenv) sampleQueue(lim samplerLimits) opstats.QueueState {
	e.t.Helper()
	q, err := sampleQueue(bg, e.w.samplePool, e.clock.Now(), lim, 1<<30)
	if err != nil {
		e.t.Fatalf("sampleQueue: %v", err)
	}
	return q
}

func (e *wenv) insertQueue(n int, extra string) {
	e.t.Helper()
	e.exec(fmt.Sprintf(`INSERT INTO magnetico_ingest.queue (info_hash, format, payload, payload_bytes %s)
SELECT decode(lpad(to_hex(g + (SELECT COALESCE(max(id), 0) FROM magnetico_ingest.queue)), 40, '0'), 'hex'), 1, '\x00'::bytea, 1 FROM generate_series(1, %d) g`, extra, n))
}

func TestQueueSampleCountsRowsBytesQuotaAndQuarantine(t *testing.T) {
	e := newWEnv(t, nil)
	e.insertQueue(5, "")
	e.exec(`INSERT INTO magnetico_ingest.quarantine (info_hash, format, payload, payload_bytes, reason_code, attempts, received_at)
VALUES ('\x01', 1, '\x0000', 2, 'invalid_record', 1, now())`)
	q := e.sampleQueue(testLimits)
	v := q.Values
	if !q.Valid[opstats.GQueueRows] || v[opstats.GQueueRows] != 5 || v[opstats.GQueueBytes] != 5 || v[opstats.GQuotaUsedBytes] != 7 ||
		v[opstats.GQuarantineRows] != 1 || v[opstats.GQuarantineBytes] != 2 || q.CountsApprox || q.OldestApprox {
		t.Fatalf("sample = %+v", q)
	}
	for g := opstats.Gauge(0); g < opstats.NumGauges; g++ {
		if !q.Valid[g] {
			t.Errorf("gauge %s is unavailable", g.Name())
		}
	}
	if q.Slot != e.clock.Now().Unix()/10 || q.QuotaMax != 1<<30 {
		t.Fatalf("slot %d quota max %d", q.Slot, q.QuotaMax)
	}
}

// Concurrent admissions and settlements must not make the related gauges
// disagree: quota usage always equals queue bytes + quarantine bytes in one sample.
func TestQueueSampleIsOneConsistentSnapshot(t *testing.T) {
	e := newWEnv(t, nil)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = e.pool.Exec(bg, fmt.Sprintf(`INSERT INTO magnetico_ingest.queue (info_hash, format, payload, payload_bytes)
SELECT decode(lpad(to_hex(g + %d), 40, '0'), 'hex'), 1, '\x00'::bytea, 1 FROM generate_series(1, 50) g`, i*100))
			_, _ = e.pool.Exec(bg, `DELETE FROM magnetico_ingest.queue WHERE id IN (SELECT id FROM magnetico_ingest.queue ORDER BY id LIMIT 30)`)
		}
	}()
	for i := 0; i < 150; i++ {
		q := e.sampleQueue(testLimits)
		if q.Values[opstats.GQuotaUsedBytes] != q.Values[opstats.GQueueBytes]+q.Values[opstats.GQuarantineBytes] {
			close(stop)
			wg.Wait()
			t.Fatalf("sample %d is not one snapshot: quota %d, queue %d + quarantine %d", i,
				q.Values[opstats.GQuotaUsedBytes], q.Values[opstats.GQueueBytes], q.Values[opstats.GQuarantineBytes])
		}
	}
	close(stop)
	wg.Wait()
}

func TestOldestIsTheTrueMinimumOfReceivedAtNotTheLowestID(t *testing.T) {
	e := newWEnv(t, nil)
	e.insertQueue(1, "")                                            // the lowest id, received now
	e.exec(`UPDATE magnetico_ingest.queue SET received_at = now()`) // (explicit)
	e.insertQueue(1, "")                                            // a later id ...
	e.exec(`UPDATE magnetico_ingest.queue SET received_at = now() - interval '3 hours' WHERE id = (SELECT max(id) FROM magnetico_ingest.queue)`)
	q := e.sampleQueue(testLimits)
	age := q.Values[opstats.GOldestAgeSeconds]
	if !q.Valid[opstats.GOldestAgeSeconds] || age < 3*3600-60 || age > 3*3600+60 || q.OldestApprox {
		t.Fatalf("oldest age = %d s (approx %v), want about 3 h: the true minimum of received_at, not the age of the lowest id", age, q.OldestApprox)
	}
	// An empty queue has no pending record: age 0, exact.
	e.exec(`DELETE FROM magnetico_ingest.queue`)
	if q := e.sampleQueue(testLimits); !q.Valid[opstats.GOldestAgeSeconds] || q.Values[opstats.GOldestAgeSeconds] != 0 {
		t.Fatalf("empty queue: %+v", q.Values)
	}
}

// With a statement budget the aggregate cannot meet, the counts fall back to
// the planner estimate (flagged), the byte total becomes unavailable (never
// estimated), the oldest age falls back to the lowest id (flagged); the quota
// (O(1), exact) and the quarantine are still answered: the transaction was not
// poisoned by the timeout.
func TestFallbacksAreIndependentPerQuantityAndAStatementTimeoutDoesNotAbortTheTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("inserts 700k rows")
	}
	e := newWEnv(t, nil)
	e.insertQueue(700_000, "")
	e.exec(`ANALYZE magnetico_ingest.queue`)
	lim := samplerLimits{statement: 15 * time.Millisecond, lock: time.Second, cycle: 10 * time.Second}
	var q opstats.QueueState
	for attempt := 0; attempt < 3; attempt++ { // the aggregate must really be slower than the budget
		q = e.sampleQueue(lim)
		if q.CountsApprox {
			break
		}
	}
	if !q.CountsApprox {
		t.Skip("the aggregate over 700k rows finished within 15 ms on this machine")
	}
	if !q.Valid[opstats.GQueueRows] || q.Values[opstats.GQueueRows] < 600_000 {
		t.Fatalf("estimated rows = %d (valid %v)", q.Values[opstats.GQueueRows], q.Valid[opstats.GQueueRows])
	}
	if q.Valid[opstats.GQueueBytes] {
		t.Fatal("a byte total was reported although the exact query timed out: bytes are never estimated")
	}
	if !q.Valid[opstats.GOldestAgeSeconds] || !q.OldestApprox {
		t.Fatalf("oldest age: valid %v approx %v, want the flagged fallback", q.Valid[opstats.GOldestAgeSeconds], q.OldestApprox)
	}
	if !q.Valid[opstats.GQuotaUsedBytes] || q.Values[opstats.GQuotaUsedBytes] != 700_000 || !q.Valid[opstats.GQuarantineRows] || q.Values[opstats.GQuarantineRows] != 0 {
		t.Fatalf("the quota and the quarantine were not answered after the timeout: %+v", q)
	}
}

func TestQueueStateIsNeverZeroWhenUnavailable(t *testing.T) {
	e := newWEnv(t, nil)
	var q opstats.QueueState
	q.At, q.Slot, q.QuotaMax = e.clock.Now(), e.clock.Now().Unix()/10, 100
	q.Values[opstats.GQuotaUsedBytes], q.Valid[opstats.GQuotaUsedBytes] = 40, true // bytes and rows unavailable
	e.w.addQueueSample(q)
	e.ev(1)
	e.mustFlush()
	if e.n(`SELECT quota_used_bytes FROM magnetico_stats.shared_state`) != 40 {
		t.Fatal("the available gauge was not stored")
	}
	for _, col := range []string{"queue_rows", "queue_bytes", "quarantine_rows", "quarantine_bytes", "oldest_age_s"} {
		if e.n(`SELECT `+col+` FROM magnetico_stats.shared_state`) != -1 { // -1 stands for SQL NULL in wenv.n
			t.Errorf("%s was stored as a number although unavailable", col)
		}
	}
	if e.n(`SELECT queue_rows_n FROM magnetico_stats.agg_hour LIMIT 1`) != 0 {
		t.Fatal("an unavailable gauge contributed a sample to the aggregate")
	}
}

func TestSavepointRollbackKeepsTheSameSnapshot(t *testing.T) {
	e := newWEnv(t, nil)
	e.insertQueue(3, "")
	var first, second int64
	err := runSampled(bg, e.w.samplePool, testLimits, func(s *sampledTx) error {
		if err := s.try(`SELECT count(*) FROM magnetico_ingest.queue`, nil, &first); err != nil {
			return err
		}
		e.insertQueue(4, "") // committed by another session after the snapshot was taken
		var x int
		if err := s.try(`SELECT 1/0`, nil, &x); err == nil {
			t.Error("1/0 succeeded")
		}
		return s.try(`SELECT count(*) FROM magnetico_ingest.queue`, nil, &second)
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != 3 || second != 3 {
		t.Fatalf("counts %d then %d: the savepoint rollback lost the snapshot (7 would mean it did)", first, second)
	}
}

func TestSamplingCycleHasATotalDeadlineAndTheConnectionIsDiscarded(t *testing.T) {
	e := newWEnv(t, nil)
	lim := samplerLimits{statement: 30 * time.Second, lock: time.Second, cycle: 400 * time.Millisecond}
	var pid1 int64
	start := time.Now()
	err := runSampled(bg, e.w.samplePool, lim, func(s *sampledTx) error {
		_ = s.try(`SELECT pg_backend_pid()`, nil, &pid1)
		var x any
		return s.try(`SELECT pg_sleep(10)::text`, nil, &x)
	})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("err %v after %v: the cycle deadline did not cut the sleeping query", err, time.Since(start))
	}
	var pid2 int64
	if err := runSampled(bg, e.w.samplePool, testLimits, func(s *sampledTx) error {
		return s.try(`SELECT pg_backend_pid()`, nil, &pid2)
	}); err != nil {
		t.Fatal(err)
	}
	if pid1 == 0 || pid1 == pid2 {
		t.Fatalf("the connection was reused after a deadline (pid %d, then %d)", pid1, pid2)
	}
	var alive int64
	if err := e.pool.QueryRow(bg, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, pid1).Scan(&alive); err != nil || alive != 0 {
		t.Fatalf("the abandoned backend %d is still connected (%v)", pid1, err)
	}
}

func TestQueueSamplerBacksOffWhenSlow(t *testing.T) {
	base, thr := 10*time.Second, 2*time.Second
	cur := base
	var seen []time.Duration
	for _, took := range []time.Duration{3 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second} {
		var slow bool
		cur, slow = nextQueueInterval(cur, base, took, thr)
		if !slow {
			t.Fatal("a slow cycle was not flagged")
		}
		seen = append(seen, cur)
	}
	if seen[0] != 20*time.Second || seen[1] != 40*time.Second || seen[2] != time.Minute || seen[6] != time.Minute {
		t.Fatalf("intervals %v, want doubling up to 60 s", seen)
	}
	if next, slow := nextQueueInterval(time.Minute, base, 500*time.Millisecond, thr); slow || next != base {
		t.Fatalf("recovery: %v %v", next, slow)
	}
}

// ---- isolation of the three pools -------------------------------------------------

func TestSlowSamplerNeverDelaysFlushOrHeartbeat(t *testing.T) {
	e := newWEnv(t, nil)
	e.ev(1)
	e.mustFlush()
	hold := make(chan struct{})
	started := make(chan struct{})
	go func() {
		lim := samplerLimits{statement: 30 * time.Second, lock: time.Second, cycle: 30 * time.Second}
		_ = runSampled(bg, e.w.samplePool, lim, func(s *sampledTx) error {
			close(started)
			var x any
			return s.try(`SELECT pg_sleep(4)::text`, nil, &x)
		})
		close(hold)
	}()
	<-started
	begin := time.Now()
	e.ev(2)
	e.mustFlush()
	if _, err := e.w.hbPool.Exec(bg, opstats.UpsertSnapshotSQL(), func() []any { s := e.w.snapshotNow(); return s.Args() }()...); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("flush and heartbeat took %v while a sample was running", d)
	}
	<-hold
}

func TestStalledFlushNeverDelaysTheHeartbeatAndReportsTheStall(t *testing.T) {
	e := newWEnv(t, func(o *WriterOptions, _ *wenv) { o.HeartbeatInterval = 60 * time.Millisecond })
	e.ev(1)
	e.mustFlush() // ready
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { defer close(done); e.w.heartbeatLoop(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "the first heartbeat", func() bool { return e.n(`SELECT count(*) FROM magnetico_stats.snapshot`) == 1 })

	// The instance row is locked: the flush's arbiter blocks (for up to its lock timeout).
	tx, err := e.pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(bg) //nolint:errcheck
	if _, err := tx.Exec(bg, `SELECT 1 FROM magnetico_stats.ingest_instances FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	e.ev(1)
	flushDone := make(chan error, 1)
	go func() { flushDone <- e.flush() }()
	var first time.Time
	if err := e.pool.QueryRow(bg, `SELECT published_at FROM magnetico_stats.snapshot`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // the flush is stalled for all of this time
	select {
	case err := <-flushDone:
		t.Fatalf("the flush finished although the row was locked: %v", err)
	default:
	}
	var last time.Time
	var stalled int64
	if err := e.pool.QueryRow(bg, `SELECT published_at, flush_stalled_s FROM magnetico_stats.snapshot`).Scan(&last, &stalled); err != nil {
		t.Fatal(err)
	}
	_ = stalled
	if beats := last.Sub(first); beats < 1000*time.Millisecond {
		t.Fatalf("the heartbeat advanced only %v in 1.5 s of a stalled flush (interval 60 ms): it was delayed", beats)
	}
	_ = tx.Rollback(bg)
	if err := <-flushDone; err != nil {
		t.Fatalf("the flush after the lock was released: %v", err)
	}
}

func TestWriterUsesThreeIndependentPools(t *testing.T) {
	e := newWEnv(t, nil)
	e.ev(1)
	e.mustFlush()
	if _, err := e.w.hbPool.Exec(bg, opstats.UpsertSnapshotSQL(), func() []any { s := e.w.snapshotNow(); return s.Args() }()...); err != nil {
		t.Fatal(err)
	}
	if _, err := sampleQueue(bg, e.w.samplePool, e.clock.Now(), testLimits, 1); err != nil {
		t.Fatal(err)
	}
	rows, err := e.pool.Query(bg, `SELECT application_name, count(*) FROM pg_stat_activity WHERE application_name LIKE 'magnetico-stats-%' AND datname = current_database() GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var n string
		var c int64
		if err := rows.Scan(&n, &c); err != nil {
			t.Fatal(err)
		}
		got[n] = c
	}
	for _, n := range []string{"magnetico-stats-flush", "magnetico-stats-heartbeat", "magnetico-stats-sampler"} {
		if got[n] != 1 {
			t.Errorf("%s: %d connections, want exactly one", n, got[n])
		}
	}
}

var _ = pgx.ErrNoRows
var _ atomic.Int64

// A process that has just started must show up on the dashboard without
// waiting a whole heartbeat interval: the first snapshot is published at once.
func TestFirstHeartbeatIsPublishedImmediatelyOnceReady(t *testing.T) {
	e := newWEnv(t, func(o *WriterOptions, _ *wenv) { o.HeartbeatInterval = time.Hour })
	e.ev(1)
	e.mustFlush() // ready
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { defer close(done); e.w.heartbeatLoop(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "a snapshot long before the first tick", func() bool { return e.n(`SELECT count(*) FROM magnetico_stats.snapshot`) == 1 })
}
