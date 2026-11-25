package stats

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/opstats"
)

const (
	queueSlotSeconds   = 10
	storageSlotSeconds = 300
)

// samplerLimits are the per-statement and per-cycle bounds (fields so tests can shorten them).
type samplerLimits struct {
	statement time.Duration
	lock      time.Duration
	cycle     time.Duration
}

var queueLimits = samplerLimits{statement: 2 * time.Second, lock: time.Second, cycle: 5 * time.Second}

// queueSamplerLoop samples the queue every QueueInterval, only while this
// process owns the queue consumer. A cycle slower than 2 s doubles the
// interval up to 60 s.
func (w *Writer) queueSamplerLoop(ctx context.Context) {
	interval := w.opt.QueueInterval
	for {
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if w.owner == nil || !w.owner.Owns() {
			interval = w.opt.QueueInterval
			continue
		}
		start := time.Now()
		q, err := sampleQueue(ctx, w.samplePool, w.now(), queueLimits, w.opt.QuotaMaxBytes)
		if err != nil {
			if ctx.Err() == nil {
				w.logChange("queue-sample", errClass(err), "statistics: sampling the queue failed (%s); the previous state stays and shows its age", safeErr(err))
			}
		} else {
			w.logChange("queue-sample", "ok", "statistics: queue sampling works")
			var slow bool
			interval, slow = nextQueueInterval(interval, w.opt.QueueInterval, time.Since(start), queueLimits.statement)
			q.Backoff = slow
			w.addQueueSample(q)
		}
	}
}

// nextQueueInterval widens the sampling interval (doubling, up to 60 s) after a
// cycle slower than threshold and restores the base interval after a fast one.
func nextQueueInterval(cur, base, took, threshold time.Duration) (next time.Duration, slow bool) {
	if took > threshold {
		return min(cur*2, time.Minute), true
	}
	return base, false
}

// sampledTx runs queries inside one REPEATABLE READ, READ ONLY transaction with
// a cycle deadline. Each query runs in its own SAVEPOINT: in PostgreSQL a
// statement or lock timeout aborts the whole transaction, but ROLLBACK TO
// SAVEPOINT revives it and keeps the snapshot, so a failed query does not
// poison the rest and fallbacks run in the same snapshot. When the cycle
// deadline fires the connection is closed and discarded instead of reused.
type sampledTx struct {
	tx     pgx.Tx
	lim    samplerLimits
	cycle  context.Context
	broken bool
}

func runSampled(ctx context.Context, pool *pgxpool.Pool, lim samplerLimits, fn func(s *sampledTx) error) error {
	cycle, cancel := context.WithTimeout(ctx, lim.cycle)
	defer cancel()
	conn, err := pool.Acquire(cycle)
	if err != nil {
		return err
	}
	tx, err := conn.BeginTx(cycle, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		conn.Release()
		return err
	}
	s := &sampledTx{tx: tx, lim: lim, cycle: cycle}
	ferr := fn(s)
	rctx, rcancel := context.WithTimeout(context.Background(), time.Second)
	defer rcancel()
	if rerr := tx.Rollback(rctx); rerr != nil || s.broken || cycle.Err() != nil {
		// The connection may be in an unknown state: close it, never reuse it.
		c := conn.Hijack()
		_ = c.Close(rctx)
	} else {
		conn.Release()
	}
	return ferr
}

// try runs one query in a savepoint. A failure (statement or lock timeout,
// permission, anything) rolls back to the savepoint and is returned; the
// transaction stays usable unless the connection itself is broken.
func (s *sampledTx) try(query string, args []any, dest ...any) error {
	if s.cycle.Err() != nil {
		s.broken = true
		return s.cycle.Err()
	}
	sp, err := s.tx.Begin(s.cycle)
	if err != nil {
		s.broken = true
		return err
	}
	set := fmt.Sprintf("SET LOCAL statement_timeout = %d; SET LOCAL lock_timeout = %d", s.lim.statement.Milliseconds(), s.lim.lock.Milliseconds())
	var qerr error
	if _, qerr = sp.Exec(s.cycle, set); qerr == nil {
		qerr = sp.QueryRow(s.cycle, query, args...).Scan(dest...)
	}
	if qerr != nil {
		rctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if rerr := sp.Rollback(rctx); rerr != nil {
			s.broken = true
		}
		return qerr
	}
	if err := sp.Commit(s.cycle); err != nil {
		s.broken = true
		return err
	}
	return nil
}

// sampleQueue takes one consistent sample of the queue, the quarantine and the
// quota. Fallbacks are independent per quantity: counts may fall back to the
// planner estimate (flagged), byte totals never do, and the oldest age falls
// back to the age of the lowest id (flagged). Nothing is ever reported as zero
// when it is unavailable.
func sampleQueue(ctx context.Context, pool *pgxpool.Pool, at time.Time, lim samplerLimits, quotaMax int64) (opstats.QueueState, error) {
	q := opstats.QueueState{At: at, Slot: at.Unix() / queueSlotSeconds, QuotaMax: quotaMax}
	err := runSampled(ctx, pool, lim, func(s *sampledTx) error {
		var used int64
		// The O(1) read first: it fixes the snapshot of all related gauges.
		if err := s.try(`SELECT used_bytes FROM magnetico_ingest.usage WHERE id = 1`, nil, &used); err != nil {
			return err
		}
		q.Values[opstats.GQuotaUsedBytes], q.Valid[opstats.GQuotaUsedBytes] = used, true
		// The utilisation is stored per observation with the quota in force now.
		if ppm, ok := utilisationPPM(used, quotaMax); ok {
			q.Values[opstats.GQuotaUtilisationPPM], q.Valid[opstats.GQuotaUtilisationPPM] = ppm, true
		}

		var rows, bytes, age int64
		if err := s.try(`SELECT count(*), COALESCE(sum(payload_bytes), 0)::bigint,
COALESCE(EXTRACT(EPOCH FROM now() - min(received_at)), 0)::bigint FROM magnetico_ingest.queue`, nil, &rows, &bytes, &age); err == nil {
			q.Values[opstats.GQueueRows], q.Valid[opstats.GQueueRows] = rows, true
			q.Values[opstats.GQueueBytes], q.Valid[opstats.GQueueBytes] = bytes, true
			q.Values[opstats.GOldestAgeSeconds], q.Valid[opstats.GOldestAgeSeconds] = max(age, 0), true
			flag(&q, opstats.GQueueRowsApprox, false)
			flag(&q, opstats.GOldestAgeApprox, false)
		} else if s.cycle.Err() == nil {
			var est *int64
			if err := s.try(`SELECT CASE WHEN reltuples >= 0 THEN reltuples::bigint END FROM pg_class WHERE oid = 'magnetico_ingest.queue'::regclass`, nil, &est); err == nil && est != nil {
				q.Values[opstats.GQueueRows], q.Valid[opstats.GQueueRows] = *est, true
				flag(&q, opstats.GQueueRowsApprox, true)
			}
			// Bytes have no estimate: they stay unavailable.
			var lowest *int64
			if err := s.try(`SELECT COALESCE((SELECT EXTRACT(EPOCH FROM now() - received_at)::bigint FROM magnetico_ingest.queue ORDER BY id LIMIT 1), 0)`, nil, &lowest); err == nil && lowest != nil {
				q.Values[opstats.GOldestAgeSeconds], q.Valid[opstats.GOldestAgeSeconds] = max(*lowest, 0), true
				flag(&q, opstats.GOldestAgeApprox, true)
			}
		}
		var qrows, qbytes int64
		if err := s.try(`SELECT count(*), COALESCE(sum(payload_bytes), 0)::bigint FROM magnetico_ingest.quarantine`, nil, &qrows, &qbytes); err == nil {
			q.Values[opstats.GQuarantineRows], q.Valid[opstats.GQuarantineRows] = qrows, true
			q.Values[opstats.GQuarantineBytes], q.Valid[opstats.GQuarantineBytes] = qbytes, true
			flag(&q, opstats.GQuarantineRowsApprox, false)
		} else if s.cycle.Err() == nil {
			var est *int64
			if err := s.try(`SELECT CASE WHEN reltuples >= 0 THEN reltuples::bigint END FROM pg_class WHERE oid = 'magnetico_ingest.quarantine'::regclass`, nil, &est); err == nil && est != nil {
				q.Values[opstats.GQuarantineRows], q.Valid[opstats.GQuarantineRows] = *est, true
				flag(&q, opstats.GQuarantineRowsApprox, true)
			}
		}
		return nil
	})
	if err != nil {
		return opstats.QueueState{}, errors.New("queue sample failed")
	}
	return q, nil
}

// flag records, as a 0/1 gauge of this sample, whether a quantity is an estimate.
func flag(q *opstats.QueueState, g opstats.Gauge, estimated bool) {
	q.Values[g], q.Valid[g] = 0, true
	if estimated {
		q.Values[g] = 1
	}
}

// utilisationPPM is used ÷ quota in parts per million, computed with a 128-bit
// intermediate so that a usage of terabytes cannot overflow, and saturating at
// the largest int64. Unavailable when no quota is configured.
func utilisationPPM(used, quota int64) (int64, bool) {
	if quota <= 0 {
		return 0, false
	}
	if used <= 0 {
		return 0, true
	}
	hi, lo := bits.Mul64(uint64(used), 1_000_000)
	if hi >= uint64(quota) { // the quotient does not fit in 64 bits
		return math.MaxInt64, true
	}
	q, _ := bits.Div64(hi, lo, uint64(quota))
	if q > math.MaxInt64 {
		return math.MaxInt64, true
	}
	return int64(q), true
}
