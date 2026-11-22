package pgqueue

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/ingest/record"
	"github.com/skobkin/magnetico-web/persistence"
)

// consumerLockKey is the session advisory-lock key of the single consumer.
// It must stay a positive int64 (see ownsLockSQL).
const consumerLockKey int64 = 7881644658832750187

// Quarantine reason codes.
const (
	ReasonInvalidRecord  = "invalid_record"
	ReasonInvalidMeta    = "invalid_metadata"
	ReasonMaxAttempts    = "max_attempts"
	maxAttempts          = 10
	consumerBatch        = 32
	defaultIdlePoll      = 500 * time.Millisecond
	defaultStandbyRetry  = 10 * time.Second
	defaultBackoffMin    = time.Second
	defaultBackoffMax    = 60 * time.Second
	maxRowBackoffSeconds = 300
)

// Store is the persistence seam of the consumer; persistence.Store satisfies it.
type Store interface {
	AddTorrent(ctx context.Context, meta persistence.Torrent, files []persistence.File) (bool, int32, error)
}

// ConsumerOptions configures a Consumer. Zero values select the defaults.
type ConsumerOptions struct {
	// OnStored is called after a record was stored (created or already
	// present) and its queue row deleted; the ingest service uses it to
	// promote the existence cache.
	OnStored func(infoHash [20]byte)
	// Limits are the record decoding limits (default codec.DefaultLimits).
	Limits codec.Limits

	IdlePoll     time.Duration
	StandbyRetry time.Duration
	BackoffMin   time.Duration
	BackoffMax   time.Duration

	// Hooks for tests; never set in production.
	beforeDelete func(infoHash [20]byte) // after AddTorrent, before DELETE
}

// ConsumerStats are the consumer counters.
type ConsumerStats struct {
	Stored       uint64 // records AddTorrent created
	Already      uint64 // records already present
	Quarantined  uint64
	Retried      uint64 // per-row attempt increments
	StorageErrs  uint64 // global backoffs
	Acquisitions uint64 // times the advisory lock was acquired
	LockLosses   uint64 // times ownership was lost
}

// Consumer is the single sequential queue consumer (spec §6.4). Ownership is
// a session-level advisory lock on a dedicated pinned connection that never
// comes from a pool.
type Consumer struct {
	q     *Queue
	store Store
	o     ConsumerOptions

	wake chan struct{}

	owned atomic.Bool

	stored, already, quarantined, retried, storageErrs, acquisitions, lockLosses atomic.Uint64
}

// NewConsumer builds a consumer; call Run to start it.
func NewConsumer(q *Queue, store Store, o ConsumerOptions) *Consumer {
	if o.IdlePoll <= 0 {
		o.IdlePoll = defaultIdlePoll
	}
	if o.StandbyRetry <= 0 {
		o.StandbyRetry = defaultStandbyRetry
	}
	if o.BackoffMin <= 0 {
		o.BackoffMin = defaultBackoffMin
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = defaultBackoffMax
	}
	if o.Limits == (codec.Limits{}) {
		o.Limits = codec.DefaultLimits()
	}
	return &Consumer{q: q, store: store, o: o, wake: make(chan struct{}, 1)}
}

// Wake nudges an idle consumer (called after a successful admission).
func (c *Consumer) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Owns reports whether this consumer currently holds the advisory lock.
func (c *Consumer) Owns() bool { return c.owned.Load() }

// Stats returns the counters.
func (c *Consumer) Stats() ConsumerStats {
	return ConsumerStats{
		Stored: c.stored.Load(), Already: c.already.Load(), Quarantined: c.quarantined.Load(),
		Retried: c.retried.Load(), StorageErrs: c.storageErrs.Load(),
		Acquisitions: c.acquisitions.Load(), LockLosses: c.lockLosses.Load(),
	}
}

// Run drives the consumer until ctx is cancelled: acquire the lock (standby
// otherwise), process while ownership verifies, and on any loss stop
// processing, discard the connection and reacquire from scratch. On
// graceful stop it finishes the current record, unlocks and closes. It
// returns nil on cancellation.
func (c *Consumer) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		conn, ok := c.acquire(ctx)
		if !ok {
			if !sleep(ctx, c.o.StandbyRetry) {
				return nil
			}
			continue
		}
		lost := c.processOwned(ctx, conn)
		c.owned.Store(false)
		if lost {
			c.lockLosses.Add(1)
			conn.Close(context.Background()) // drops the session lock if it still exists
			if !sleep(ctx, c.o.BackoffMin) {
				return nil
			}
			continue
		}
		// Graceful: explicit unlock, then close (closing releases it anyway).
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, consumerLockKey)
		cancel()
		conn.Close(context.Background())
	}
	return nil
}

// acquire opens the pinned connection and tries the session lock.
func (c *Consumer) acquire(ctx context.Context) (*pgx.Conn, bool) {
	conn, err := pgx.Connect(ctx, c.q.dsn)
	if err != nil {
		return nil, false
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, consumerLockKey).Scan(&got); err != nil || !got {
		conn.Close(context.Background())
		return nil, false
	}
	c.owned.Store(true)
	c.acquisitions.Add(1)
	return conn, true
}

// ownsLockSQL proves, on the pinned connection itself, that it is alive and
// that its backend holds the advisory lock (classid/objid are the high/low
// 32 bits of the positive int64 key).
const ownsLockSQL = `
SELECT count(*) FROM pg_locks
WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid() AND objsubid = 1
  AND ((classid::bigint << 32) | objid::bigint) = $1`

func (c *Consumer) verifyOwnership(ctx context.Context, conn *pgx.Conn) bool {
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var n int64
	if err := conn.QueryRow(vctx, ownsLockSQL, consumerLockKey).Scan(&n); err != nil {
		return false
	}
	return n == 1
}

// processOwned works the queue while ownership holds. It returns true if
// ownership was lost (caller must drop the connection and reacquire), false
// on graceful cancellation.
func (c *Consumer) processOwned(ctx context.Context, conn *pgx.Conn) (lost bool) {
	backoff := c.o.BackoffMin
	for ctx.Err() == nil {
		if !c.verifyOwnership(ctx, conn) {
			if ctx.Err() != nil {
				return false
			}
			return true
		}
		ids, err := c.claimIDs(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			c.storageErrs.Add(1)
			if !sleep(ctx, backoff) {
				return false
			}
			backoff = nextBackoff(backoff, c.o.BackoffMax)
			continue
		}
		if len(ids) == 0 {
			select {
			case <-ctx.Done():
				return false
			case <-c.wake:
			case <-time.After(c.o.IdlePoll):
			}
			continue
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return false
			}
			// Ownership is re-proven before every record: at most one
			// record's idempotent work can run after a loss.
			if !c.verifyOwnership(ctx, conn) {
				return ctx.Err() == nil
			}
			err := c.processRow(ctx, id)
			if err == nil {
				backoff = c.o.BackoffMin
				continue
			}
			if ctx.Err() != nil {
				return false
			}
			// Global storage backoff: the row stays untouched.
			c.storageErrs.Add(1)
			if !sleep(ctx, backoff) {
				return false
			}
			backoff = nextBackoff(backoff, c.o.BackoffMax)
			break
		}
	}
	return false
}

func nextBackoff(cur, max time.Duration) time.Duration {
	cur *= 2
	if cur > max {
		return max
	}
	return cur
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// claimIDs returns the ids of due rows (ids only, so memory stays bounded
// to one payload at a time).
func (c *Consumer) claimIDs(ctx context.Context) ([]int64, error) {
	rows, err := c.q.pool.Query(ctx, `
SELECT id FROM magnetico_ingest.queue WHERE next_attempt_at <= now() ORDER BY id LIMIT $1`, consumerBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// processRow handles one row. A non-nil error means a storage-level failure
// (global backoff, row untouched); every other outcome is settled in the
// database and returns nil.
func (c *Consumer) processRow(ctx context.Context, id int64) error {
	var (
		hash     []byte
		payload  []byte
		attempts int32
	)
	err := c.q.pool.QueryRow(ctx,
		`SELECT info_hash, payload, attempts FROM magnetico_ingest.queue WHERE id = $1`, id).
		Scan(&hash, &payload, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already settled
	}
	if err != nil {
		return err
	}

	rec, err := record.Decode(payload, c.o.Limits)
	if err != nil {
		return c.quarantineRow(ctx, id, ReasonInvalidRecord, "record decode failed")
	}
	created, _, err := c.store.AddTorrent(ctx, rec.Torrent(), rec.Files)
	switch {
	case err == nil:
	case errors.Is(err, persistence.ErrInvalidMetadata):
		return c.quarantineRow(ctx, id, ReasonInvalidMeta, "metadata rejected by storage validation")
	case errors.Is(err, persistence.ErrStorageIO), ctx.Err() != nil:
		return err
	default:
		return c.retryRow(ctx, id, attempts)
	}
	if c.o.beforeDelete != nil {
		c.o.beforeDelete(rec.InfoHash)
	}
	if _, err := c.q.pool.Exec(ctx, `DELETE FROM magnetico_ingest.queue WHERE id = $1`, id); err != nil {
		return err // reprocessed later; AddTorrent is idempotent
	}
	if created {
		c.stored.Add(1)
	} else {
		c.already.Add(1)
	}
	if c.o.OnStored != nil {
		c.o.OnStored(rec.InfoHash)
	}
	return nil
}

// retryRow records an unclassified failure: attempts++ with exponential
// backoff via next_attempt_at; the tenth attempt quarantines the row.
func (c *Consumer) retryRow(ctx context.Context, id int64, attempts int32) error {
	attempts++
	if attempts >= maxAttempts {
		// Record the final attempt so the quarantine row carries the true count.
		if _, err := c.q.pool.Exec(ctx, `UPDATE magnetico_ingest.queue SET attempts = $2 WHERE id = $1`, id, attempts); err != nil {
			return err
		}
		return c.quarantineRow(ctx, id, ReasonMaxAttempts, "attempt limit reached")
	}
	delay := 1 << uint(attempts)
	if delay > maxRowBackoffSeconds {
		delay = maxRowBackoffSeconds
	}
	_, err := c.q.pool.Exec(ctx, `
UPDATE magnetico_ingest.queue
SET attempts = $2, next_attempt_at = now() + make_interval(secs => $3), last_error_code = 'store_error'
WHERE id = $1`, id, attempts, delay)
	if err != nil {
		return err
	}
	c.retried.Add(1)
	return nil
}

// quarantineRow moves a row to quarantine (DELETE + INSERT, one statement and
// one transaction); usage nets to zero.
func (c *Consumer) quarantineRow(ctx context.Context, id int64, reason, detail string) error {
	_, err := c.q.pool.Exec(ctx, `
WITH d AS (
    DELETE FROM magnetico_ingest.queue WHERE id = $1
    RETURNING info_hash, format, payload, payload_bytes, received_at, attempts
)
INSERT INTO magnetico_ingest.quarantine
    (info_hash, format, payload, payload_bytes, reason_code, detail, attempts, received_at)
SELECT info_hash, format, payload, payload_bytes, $2, $3, attempts, received_at FROM d`,
		id, reason, detail)
	if err != nil {
		return err
	}
	c.quarantined.Add(1)
	return nil
}
