package stats

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skobkin/magnetico-web/internal/ingest/crawlers"
	"github.com/skobkin/magnetico-web/internal/opstats"
)

func (e *wenv) ev(n int) { e.rec.SubmitAcked(cidA, n, 0, 1) }

func proxied(t *testing.T) (*wenv, *faultProxy) {
	t.Helper()
	var p *faultProxy
	e := newWEnv(t, func(o *WriterOptions, e *wenv) {
		p = newProxy(t, e.dsn)
		o.FlushDSN = p.dsn(e.dsn)
	})
	return e, p
}

func TestFlushWritesHourDayMonthFromOneBatch(t *testing.T) {
	e := newWEnv(t, nil)
	e.ev(3)
	e.clock.Set(time.Date(2026, 11, 25, 11, 15, 0, 0, time.UTC))
	e.ev(2)
	e.mustFlush()
	for table, want := range map[string]int64{"agg_hour": 2, "agg_day": 1, "agg_month": 1} {
		if got := e.n(`SELECT count(*) FROM magnetico_stats.` + table); got != want {
			t.Errorf("%s rows = %d, want %d", table, got, want)
		}
	}
	if e.total("records_enqueued") != 5 || e.n(`SELECT records_enqueued FROM magnetico_stats.agg_day`) != 5 || e.n(`SELECT records_enqueued FROM magnetico_stats.agg_month`) != 5 {
		t.Fatal("hour, day and month totals differ from the 5 recorded events")
	}
	if e.seq() != 1 || e.n(`SELECT count(*) FROM magnetico_stats.crawler_activity`) != 1 {
		t.Fatalf("seq %d", e.seq())
	}
}

func TestFlushIsAdditiveAcrossRestartsInTheSameBucket(t *testing.T) {
	e := newWEnv(t, nil)
	e.ev(3)
	e.mustFlush()
	// A restarted process: a new instance id and a fresh recorder, the same bucket.
	rec2 := New(e.clock.Now)
	w2, err := NewWriter(rec2, e.owner, WriterOptions{DSN: e.dsn, Clock: e.clock.Now, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w2.flushPool.Close(); w2.hbPool.Close(); w2.samplePool.Close() })
	rec2.SubmitAcked(cidA, 4, 0, 1)
	if err := w2.flushOnce(bg, true); err != nil {
		t.Fatal(err)
	}
	if e.total("records_enqueued") != 7 || e.n(`SELECT count(*) FROM magnetico_stats.agg_hour`) != 1 || e.n(`SELECT count(*) FROM magnetico_stats.ingest_instances`) != 2 {
		t.Fatalf("total %d", e.total("records_enqueued"))
	}
}

func TestTwoWritersProduceTheSumOfBoth(t *testing.T) {
	e := newWEnv(t, nil)
	rec2 := New(e.clock.Now)
	w2, err := NewWriter(rec2, e.owner, WriterOptions{DSN: e.dsn, Clock: e.clock.Now, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w2.flushPool.Close(); w2.hbPool.Close(); w2.samplePool.Close() })
	done := make(chan error, 2)
	for i := 0; i < 20; i++ {
		e.ev(1)
		rec2.SubmitAcked(cidB, 2, 0, 1)
		go func() { done <- e.flush() }()
		go func() { done <- w2.flushOnce(bg, true) }()
		for j := 0; j < 2; j++ {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := e.total("records_enqueued"); got != 60 {
		t.Fatalf("total %d, want 60", got)
	}
	if e.n(`SELECT count(*) FROM magnetico_stats.crawler_activity`) != 2 {
		t.Fatal("activity rows")
	}
}

func TestSealedBatchIsNeverMutatedAfterTheFirstAttemptAndNewBatchWaitsForIt(t *testing.T) {
	e, p := proxied(t)
	e.ev(1)
	e.mustFlush() // ready, seq 1
	p.setMode("down")
	e.ev(5)
	if err := e.flush(); err == nil {
		t.Fatal("a flush through a dead proxy succeeded")
	}
	b := e.w.sealed
	if b == nil || b.seq != 2 || b.prev != 1 || b.delta.Hours[0].Counters[opstats.CRecordsEnqueued] != 5 {
		t.Fatalf("sealed = %+v", b)
	}
	for i := 0; i < 3; i++ { // newer observations of the very same hour never enter the sealed batch
		e.ev(7)
		_ = e.flush()
		if e.w.sealed != b || b.delta.Hours[0].Counters[opstats.CRecordsEnqueued] != 5 {
			t.Fatalf("the sealed batch was mutated or replaced: %+v", e.w.sealed)
		}
	}
	if e.w.carry.Hours[0].Counters[opstats.CRecordsEnqueued] != 21 || e.w.nextSeq != 3 {
		t.Fatalf("carry %d, nextSeq %d (the carry holds the newer events; no new batch is sealed before the old one resolved)",
			e.w.carry.Hours[0].Counters[opstats.CRecordsEnqueued], e.w.nextSeq)
	}
	p.setMode("")
	e.mustFlush() // seq 2 resolves
	if e.seq() != 2 || e.total("records_enqueued") != 6 {
		t.Fatalf("after resolving seq 2: seq %d total %d", e.seq(), e.total("records_enqueued"))
	}
	e.mustFlush() // now the carry is sealed as seq 3
	if e.seq() != 3 || e.total("records_enqueued") != 27 {
		t.Fatalf("after seq 3: seq %d total %d, want 27 (1+5+21)", e.seq(), e.total("records_enqueued"))
	}
}

func TestAmbiguousCommitAfterCommitDoesNotDoubleCount(t *testing.T) {
	e, p := proxied(t)
	e.ev(1)
	e.mustFlush()
	p.setMode("cut-after-commit")
	e.ev(4)
	if err := e.flush(); err == nil {
		t.Fatal("expected an error: the reply was lost")
	}
	if !e.w.sealed.inDoubt {
		t.Fatal("a lost COMMIT reply must mark the batch in doubt")
	}
	if e.total("records_enqueued") != 5 {
		t.Fatalf("the commit did not take effect on the server (total %d); the test setup is wrong", e.total("records_enqueued"))
	}
	p.setMode("")
	e.mustFlush() // the arbiter sees last_flush_seq == seq
	if e.total("records_enqueued") != 5 || e.w.sealed != nil || e.w.counters.ResolvedCommitted != 1 {
		t.Fatalf("double count or unresolved: total %d sealed %v resolved %d", e.total("records_enqueued"), e.w.sealed, e.w.counters.ResolvedCommitted)
	}
}

func TestAmbiguousCommitBeforeCommitIsRetriedOnce(t *testing.T) {
	e, p := proxied(t)
	e.ev(1)
	e.mustFlush()
	p.setMode("cut-before-commit")
	e.ev(4)
	if err := e.flush(); err == nil {
		t.Fatal("expected an error")
	}
	if e.total("records_enqueued") != 1 {
		t.Fatalf("a cut before COMMIT must not commit (total %d)", e.total("records_enqueued"))
	}
	p.setMode("")
	e.mustFlush()
	if e.total("records_enqueued") != 5 || e.w.sealed != nil || e.w.counters.ResolvedCommitted != 0 {
		t.Fatalf("total %d sealed %v resolved %d", e.total("records_enqueued"), e.w.sealed, e.w.counters.ResolvedCommitted)
	}
	e.mustFlush()
	if e.total("records_enqueued") != 5 {
		t.Fatalf("a further flush changed the totals: %d", e.total("records_enqueued"))
	}
}

// An old backend holds the instance row (it ran the arbiter and applied the delta)
// when the retry arrives: the retry waits on the row lock, then matches zero rows.
func TestRetryRacingAStillRunningCommitWaitsOnTheRowLockAndAppliesNothing(t *testing.T) {
	e := newWEnv(t, nil)
	e.ev(1)
	e.mustFlush()
	e.ev(6)
	e.w.collect()
	e.w.sealed = &batch{seq: e.w.nextSeq, prev: e.w.dbSeq, delta: e.w.carry, sealedAt: e.clock.Now(), inDoubt: true}
	e.w.nextSeq++
	e.w.carry = opstats.Delta{}
	b := e.w.sealed

	old, err := pgx.Connect(bg, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close(bg)
	tx, err := old.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(bg, `UPDATE magnetico_stats.ingest_instances SET last_flush_seq = $1 WHERE instance_id = $2 AND last_flush_seq = $3`, b.seq, e.w.instanceID, b.prev)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("old backend arbiter: %v %v", tag, err)
	}
	if _, err := opstats.Apply(bg, tx, &b.delta); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.w.attempt(bg) }()
	select {
	case err := <-done:
		t.Fatalf("the retry did not wait for the old backend's row lock (%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(bg); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("retry: %v", err)
	}
	if e.total("records_enqueued") != 7 || e.w.counters.ResolvedCommitted != 1 || e.w.sealed != nil {
		t.Fatalf("total %d resolved %d: the retry applied the batch a second time", e.total("records_enqueued"), e.w.counters.ResolvedCommitted)
	}
}

func TestMissingInstanceRowNeverInDoubtIsRecreatedAndRetried(t *testing.T) {
	e := newWEnv(t, nil)
	e.ev(1)
	e.mustFlush()
	e.exec(`DELETE FROM magnetico_stats.ingest_instances`)
	e.ev(4)
	e.mustFlush()
	if e.total("records_enqueued") != 5 || e.seq() != 2 || e.w.counters.BatchesDroppedUnresolved != 0 {
		t.Fatalf("total %d seq %d", e.total("records_enqueued"), e.seq())
	}
}

func TestMissingInstanceRowInDoubtDropsTheBatchAndCountsIt(t *testing.T) {
	e := newWEnv(t, nil)
	e.ev(1)
	e.mustFlush()
	e.ev(4)
	e.w.collect()
	e.w.sealed = &batch{seq: e.w.nextSeq, prev: e.w.dbSeq, delta: e.w.carry, sealedAt: e.clock.Now(), inDoubt: true}
	e.w.nextSeq++
	e.w.carry = opstats.Delta{}
	e.exec(`DELETE FROM magnetico_stats.ingest_instances`)
	e.mustFlush()
	if e.total("records_enqueued") != 1 || e.w.counters.BatchesDroppedUnresolved != 1 || e.w.sealed != nil {
		t.Fatalf("total %d dropped %d: an unresolvable batch must be dropped, never applied", e.total("records_enqueued"), e.w.counters.BatchesDroppedUnresolved)
	}
	if e.seq() != 2 || e.w.dbSeq != 2 {
		t.Fatalf("the row must be recreated at the batch's sequence: db %d writer %d", e.seq(), e.w.dbSeq)
	}
	e.ev(9)
	e.mustFlush() // no gap: seq 3 follows
	if e.total("records_enqueued") != 10 || e.seq() != 3 {
		t.Fatalf("after recovery: total %d seq %d", e.total("records_enqueued"), e.seq())
	}
}

func TestUnexpectedSequenceValuesDropResyncAndCountAndNeverReuseANumber(t *testing.T) {
	for _, foreign := range []int64{0, 7, 1000} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			e := newWEnv(t, nil)
			for i := 0; i < 4; i++ { // the writer's sequence is 4
				e.ev(1)
				e.mustFlush()
			}
			e.exec(`UPDATE magnetico_stats.ingest_instances SET last_flush_seq = $1`, foreign)
			before := e.total("records_enqueued")
			maxSeq := e.w.nextSeq
			e.ev(50)
			e.mustFlush()
			if e.total("records_enqueued") != before {
				t.Fatalf("a batch was applied despite a foreign sequence value (%d → %d)", before, e.total("records_enqueued"))
			}
			if e.w.counters.SequenceAnomalies != 1 || e.w.dbSeq != foreign || e.w.nextSeq < maxSeq || e.w.nextSeq <= foreign {
				t.Fatalf("anomalies %d dbSeq %d nextSeq %d (was %d)", e.w.counters.SequenceAnomalies, e.w.dbSeq, e.w.nextSeq, maxSeq)
			}
			used := e.w.nextSeq
			e.ev(3)
			e.mustFlush()
			if e.total("records_enqueued") != before+3 || e.seq() != used {
				t.Fatalf("after the resync: total %d, db seq %d, want %d and %d", e.total("records_enqueued"), e.seq(), before+3, used)
			}
			if e.w.nextSeq != used+1 {
				t.Fatalf("a sequence number was reused or skipped: nextSeq %d", e.w.nextSeq)
			}
		})
	}
}

func TestAbandonedBatchKeepsItsSequenceAsATombstoneAndTheNextBatchHasNoGap(t *testing.T) {
	e, p := proxied(t)
	e.ev(1)
	e.mustFlush()
	p.setMode("down")
	e.ev(5)
	_ = e.flush() // sealed seq 2 fails
	e.clock.Advance(25 * time.Hour)
	e.ev(3) // after the outage: new events, a new hour
	_ = e.flush()
	b := e.w.sealed
	if b == nil || !b.abandoned || b.seq != 2 || len(b.delta.Hours) != 0 {
		t.Fatalf("sealed = %+v, want the tombstone of seq 2 with its delta discarded", b)
	}
	if e.w.counters.HoursDropped != 0 {
		t.Fatalf("the loss was counted (%d) before the outcome was known", e.w.counters.HoursDropped)
	}
	p.setMode("")
	e.mustFlush() // the tombstone resolves first: skipped
	if e.seq() != 2 || e.w.counters.HoursDropped != 1 || e.w.sealed != nil {
		t.Fatalf("tombstone: db seq %d dropped hours %d", e.seq(), e.w.counters.HoursDropped)
	}
	e.mustFlush() // seq 3 follows without a gap
	if e.seq() != 3 || e.total("records_enqueued") != 4 {
		t.Fatalf("after the tombstone: seq %d total %d (1 + 3; the 5 are lost, never double counted)", e.seq(), e.total("records_enqueued"))
	}
}

func TestAbandonedInDoubtBatchThatCommittedIsCountedOnceNeverTwice(t *testing.T) {
	e, p := proxied(t)
	e.ev(1)
	e.mustFlush()
	p.setMode("cut-after-commit")
	e.ev(5)
	_ = e.flush() // committed on the server, reply lost: in doubt
	e.clock.Advance(25 * time.Hour)
	p.setMode("down")
	_ = e.flush() // aged out during the outage: abandoned
	if !e.w.sealed.abandoned {
		t.Fatal("expected a tombstone")
	}
	p.setMode("")
	e.mustFlush()
	if e.total("records_enqueued") != 6 || e.w.counters.ResolvedCommitted != 1 || e.w.counters.HoursDropped != 0 {
		t.Fatalf("total %d resolved %d dropped %d: the committed delta must stay counted once and must not be reported as dropped",
			e.total("records_enqueued"), e.w.counters.ResolvedCommitted, e.w.counters.HoursDropped)
	}
}

func TestTombstoneSkipsAndAnOldBackendThatRunsLaterAppliesNothing(t *testing.T) {
	e := newWEnv(t, nil)
	e.ev(1)
	e.mustFlush() // seq 1
	e.ev(5)
	e.w.collect()
	e.w.sealed = &batch{seq: 2, prev: 1, delta: e.w.carry, sealedAt: e.clock.Now(), inDoubt: true}
	e.w.nextSeq = 3
	e.w.carry = opstats.Delta{}
	e.w.abandon(e.w.sealed)
	e.mustFlush() // the tombstone claims seq 2
	if e.seq() != 2 {
		t.Fatalf("seq %d", e.seq())
	}
	// The old backend finally runs its step 2a: the arbiter matches no row, so it applies nothing.
	tag, err := e.pool.Exec(bg, `UPDATE magnetico_stats.ingest_instances SET last_flush_seq = 2 WHERE instance_id = $1 AND last_flush_seq = 1`, e.w.instanceID)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("a late old backend still matched a row: %v %v", tag, err)
	}
	if e.total("records_enqueued") != 1 {
		t.Fatal("the discarded delta was applied")
	}
}

func TestOutageBuffersAtMost26HoursAndCountsDrops(t *testing.T) {
	e, p := proxied(t)
	e.ev(1)
	e.mustFlush()
	p.setMode("down")
	for h := 0; h < 40; h++ {
		e.clock.Advance(time.Hour)
		e.ev(1)
		_ = e.flush()
		if got := len(e.w.carry.Hours); got > opstats.MaxLiveHours {
			t.Fatalf("hour %d: %d hours buffered, bound %d", h, got, opstats.MaxLiveHours)
		}
	}
	if e.w.counters.HoursDropped == 0 {
		t.Fatal("hours were dropped without being counted")
	}
	p.setMode("")
	for i := 0; i < 5; i++ {
		e.mustFlush()
	}
	// Whatever was persisted is exactly one event per hour: nothing doubled.
	if got := e.n(`SELECT max(records_enqueued) FROM magnetico_stats.agg_hour`); got != 1 {
		t.Fatalf("an hour holds %d events, want 1", got)
	}
	if rows := e.n(`SELECT count(*) FROM magnetico_stats.agg_hour`); rows < 20 || rows > 41 {
		t.Fatalf("%d hour rows persisted after the outage", rows)
	}
}

func TestPruneNeverTouchesALiveWritersRowOrALockedRow(t *testing.T) {
	e := newWEnv(t, nil)
	e.exec(`INSERT INTO magnetico_stats.ingest_instances (instance_id, started_at, last_flush_seq, last_flush_at, heartbeat_at)
VALUES ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', now() - interval '40 days', 3, now() - interval '40 days', now() - interval '40 days'),
       ('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', now() - interval '40 days', 3, now() - interval '40 days', now() - interval '40 days'),
       ('cccccccc-cccc-4ccc-8ccc-cccccccccccc', now() - interval '40 days', 3, now() - interval '40 days', now() - interval '2 days')`)
	e.exec(`INSERT INTO magnetico_stats.snapshot (instance_id, started_at, published_at) VALUES
       ('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', now() - interval '3 days', now() - interval '2 days'),
       ('cccccccc-cccc-4ccc-8ccc-cccccccccccc', now() - interval '1 hour', now() - interval '10 seconds')`)
	// A row locked by a flush in flight is skipped, not waited for and not deleted.
	tx, err := e.pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(bg, `SELECT 1 FROM magnetico_stats.ingest_instances WHERE instance_id = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	e.ev(1)
	e.mustFlush() // registers this instance and prunes
	_ = tx.Rollback(bg)
	left := e.n(`SELECT count(*) FROM magnetico_stats.ingest_instances`)
	if got := e.n(`SELECT count(*) FROM magnetico_stats.ingest_instances WHERE instance_id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'`); got != 0 {
		t.Fatal("a dead instance past the grace period survived")
	}
	if e.n(`SELECT count(*) FROM magnetico_stats.ingest_instances WHERE instance_id = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'`) != 1 {
		t.Fatal("a locked row was deleted")
	}
	if e.n(`SELECT count(*) FROM magnetico_stats.ingest_instances WHERE instance_id = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc'`) != 1 {
		t.Fatal("a row with a recent heartbeat was deleted")
	}
	if e.seq() != 1 || left != 3 {
		t.Fatalf("this instance's own row: seq %d, rows left %d", e.seq(), left)
	}
	if e.n(`SELECT count(*) FROM magnetico_stats.snapshot WHERE instance_id = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'`) != 0 ||
		e.n(`SELECT count(*) FROM magnetico_stats.snapshot WHERE instance_id = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc'`) != 1 {
		t.Fatal("snapshot pruning (older than 24 h only)")
	}
}

// ---- event-time attribution through the writer -------------------------------

func TestTickNeverNeedsToSpanAnHourBoundary(t *testing.T) {
	for _, boundary := range []time.Time{
		time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
		time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC),
	} {
		e := newWEnv(t, nil)
		e.clock.Set(boundary.Add(-5 * time.Second))
		e.ev(1)
		e.clock.Set(boundary.Add(30 * time.Second)) // the tick that fires late
		e.ev(2)
		e.clock.Set(boundary.Add(45 * time.Second))
		e.mustFlush()
		if got := e.n(`SELECT records_enqueued FROM magnetico_stats.agg_hour WHERE bucket_start = $1`, boundary.Add(-time.Hour)); got != 1 {
			t.Errorf("%v: the hour before the boundary holds %d, want 1", boundary, got)
		}
		if got := e.n(`SELECT records_enqueued FROM magnetico_stats.agg_hour WHERE bucket_start = $1`, boundary); got != 2 {
			t.Errorf("%v: the hour after holds %d, want 2", boundary, got)
		}
		day := opstats.DayOf(boundary)
		prevDay := opstats.DayOf(boundary.Add(-time.Hour))
		if !day.Equal(prevDay) {
			if e.n(`SELECT records_enqueued FROM magnetico_stats.agg_day WHERE bucket_start = $1`, prevDay) != 1 || e.n(`SELECT records_enqueued FROM magnetico_stats.agg_day WHERE bucket_start = $1`, day) != 2 {
				t.Errorf("%v: day rows are wrong", boundary)
			}
		}
		month, prevMonth := opstats.MonthOf(boundary), opstats.MonthOf(boundary.Add(-time.Hour))
		if !month.Equal(prevMonth) {
			if e.n(`SELECT records_enqueued FROM magnetico_stats.agg_month WHERE bucket_start = $1`, prevMonth) != 1 || e.n(`SELECT records_enqueued FROM magnetico_stats.agg_month WHERE bucket_start = $1`, month) != 2 {
				t.Errorf("%v: month rows are wrong", boundary)
			}
		}
	}
}

func TestBlockedWriterDoesNotMisattributeEvents(t *testing.T) {
	e, p := proxied(t)
	e.ev(1)
	e.mustFlush()
	p.setMode("down")
	for h := 0; h < 5; h++ { // five hours of events while the writer cannot persist
		e.clock.Advance(time.Hour)
		e.ev(h + 1)
		_ = e.flush()
	}
	p.setMode("")
	for i := 0; i < 8; i++ {
		e.mustFlush()
	}
	start := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	for h := 0; h < 5; h++ {
		if got := e.n(`SELECT records_enqueued FROM magnetico_stats.agg_hour WHERE bucket_start = $1`, start.Add(time.Duration(h+1)*time.Hour)); got != int64(h+1) {
			t.Errorf("hour +%d holds %d, want %d", h+1, got, h+1)
		}
	}
}

func TestCrawlerActivitySurvivesRotationAndDisable(t *testing.T) {
	e := newWEnv(t, nil)
	store := crawlers.New(e.pool)
	c, err := store.Add(bg, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	e.rec.SubmitValidated(c.ID, netipAddr("192.0.2.5"), 3)
	e.rec.SubmitAcked(c.ID, 3, 0, 9)
	e.mustFlush()
	if _, err := store.RotateToken(bg, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(bg, "alpha", false); err != nil {
		t.Fatal(err)
	}
	e.exec(`DELETE FROM magnetico_ingest.crawlers`) // even the registry row may go: no foreign key
	if e.n(`SELECT records_enqueued FROM magnetico_stats.crawler_activity WHERE crawler_id = $1`, c.ID) != 3 {
		t.Fatal("the crawler's historical statistics did not survive rotation, disable and removal")
	}
	e.rec.SubmitAcked(c.ID, 2, 0, 1)
	e.mustFlush()
	if e.n(`SELECT records_enqueued FROM magnetico_stats.crawler_activity WHERE crawler_id = $1`, c.ID) != 5 {
		t.Fatal("activity no longer accumulates")
	}
}

// ---- who samples ---------------------------------------------------------------

func TestOnlyTheConsumerOwnerSamplesSharedGaugesAndFailoverLeavesAGap(t *testing.T) {
	e := newWEnv(t, func(o *WriterOptions, _ *wenv) { o.QueueInterval = 20 * time.Millisecond })
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { defer close(done); e.w.queueSamplerLoop(ctx) }()
	defer func() { cancel(); <-done }()
	count := func() int64 {
		e.w.mu.Lock()
		defer e.w.mu.Unlock()
		if len(e.w.pending.Hours) == 0 {
			return 0
		}
		return e.w.pending.Hours[0].Gauges[opstats.GQuotaUsedBytes].N
	}
	time.Sleep(250 * time.Millisecond)
	if count() != 0 {
		t.Fatalf("a non-owner sampled %d times", count())
	}
	e.owner.v.Store(true)
	waitFor(t, "samples from the owner", func() bool { return count() >= 3 })
	e.owner.v.Store(false) // failover: the new owner is another process
	time.Sleep(60 * time.Millisecond)
	n := count()
	time.Sleep(250 * time.Millisecond)
	if count() != n {
		t.Fatalf("sampling continued after ownership was lost (%d → %d)", n, count())
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The state key of a repeated failure must not depend on how the driver wrapped
// it: the same SQLSTATE through a prepare error and through an execute error is
// one condition and is logged once.
func TestErrClassIsStableAcrossWrappers(t *testing.T) {
	pg := &pgconn.PgError{Code: "42P01"}
	wrapped := fmt.Errorf("prepare: %w", pg)
	if a, b := errClass(pg), errClass(wrapped); a != b || a == "" {
		t.Fatalf("classes differ: %q vs %q", a, b)
	}
	if errClass(&pgconn.PgError{Code: "42501"}) == errClass(pg) {
		t.Fatal("different SQLSTATEs share a class")
	}
	if errClass(context.DeadlineExceeded) != errClass(fmt.Errorf("x: %w", context.DeadlineExceeded)) {
		t.Fatal("a deadline is classified by its wrapper")
	}
}

// A flush that failed earlier leaves a sealed batch and, behind it, the newer
// events in the carry. The final flush of a shutdown must persist both: one
// attempt would settle the sealed batch and silently drop everything after it.
func TestCloseDrainsTheCarryBehindAPendingSealedBatch(t *testing.T) {
	e, p := proxied(t)
	e.ev(1)
	e.mustFlush() // ready, seq 1
	p.setMode("down")
	e.ev(5)
	if err := e.flush(); err == nil {
		t.Fatal("a flush through a dead proxy succeeded")
	}
	e.ev(7) // newer events wait in the carry behind the sealed batch
	p.setMode("")
	e.w.Close(bg)
	if got := e.total("records_enqueued"); got != 13 {
		t.Fatalf("persisted %d after Close, want 13 (1 + 5 sealed + 7 carried): the shutdown lost events", got)
	}
}

// The ingest log never prints what a driver error carries (host, user and
// database of the connection); our own schema and privilege messages are kept.
func TestLogLinesNeverCarryDriverErrorDetails(t *testing.T) {
	driver := fmt.Errorf("failed to connect to `host=db.internal.example user=magnetico_rt database=prod`: dial error")
	if got := safeErr(driver); strings.Contains(got, "db.internal.example") || strings.Contains(got, "magnetico_rt") || got == "" {
		t.Fatalf("safeErr leaks the connection details: %q", got)
	}
	own := fmt.Errorf("%w: [magnetico_stats.snapshot]", errMissingPrivileges)
	if got := safeErr(own); !strings.Contains(got, "magnetico_stats.snapshot") {
		t.Fatalf("our own privilege message was lost: %q", got)
	}
}
