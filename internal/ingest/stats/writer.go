package stats

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/opstats"
)

// Owner says whether this process owns the queue consumer; only the owner
// samples the database-wide (shared) state.
type Owner interface{ Owns() bool }

// WriterOptions configures a Writer. Zero values select the defaults.
type WriterOptions struct {
	// DSN is the database of the statistics (the ingest database).
	DSN string
	// FlushDSN, when set, is used by the flush pool instead of DSN (tests route
	// it through a faulty proxy).
	FlushDSN string
	Logger   *log.Logger
	// QuotaMaxBytes is the configured queue quota, published beside the usage.
	QuotaMaxBytes int64
	// Clock defaults to time.Now.
	Clock func() time.Time

	FlushInterval     time.Duration // 60 s
	HeartbeatInterval time.Duration // 10 s
	QueueInterval     time.Duration // 10 s
	StorageInterval   time.Duration // 5 min
	AttemptTimeout    time.Duration // 30 s per flush attempt
	BatchMaxAge       time.Duration // 24 h: an older sealed batch is abandoned
	PruneInterval     time.Duration // 10 min
}

func (o *WriterOptions) defaults() {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	for _, p := range []struct {
		d *time.Duration
		v time.Duration
	}{{&o.FlushInterval, time.Minute}, {&o.HeartbeatInterval, 10 * time.Second}, {&o.QueueInterval, 10 * time.Second},
		{&o.StorageInterval, 5 * time.Minute}, {&o.AttemptTimeout, 30 * time.Second}, {&o.BatchMaxAge, 24 * time.Hour},
		{&o.PruneInterval, 10 * time.Minute}} {
		if *p.d <= 0 {
			*p.d = p.v
		}
	}
}

// WriterCounters are the writer's status counters (published by the heartbeat).
type WriterCounters struct {
	FlushesOK, FlushesFailed int64
	HoursDropped             int64
	SequenceAnomalies        int64
	BatchesDroppedUnresolved int64
	ResolvedCommitted        int64
	GaugeBatchesSkipped      int64
	CrawlersOverflow         int64
	ClockAnomaly             bool
}

// WriterStatus is a snapshot of the writer's state.
type WriterStatus struct {
	Ready         bool
	LastError     string
	DBSeq         int64
	NextSeq       int64
	SealedPending bool
	SealedAbandon bool
	CarryHours    int
	Counters      WriterCounters
}

// batch is one sealed, numbered unit of persistence. It is immutable from its
// first attempt; the conditional sequence update is the single arbiter of
// whether it has been applied.
type batch struct {
	seq, prev int64
	delta     opstats.Delta
	inDoubt   bool
	abandoned bool // a tombstone: the delta was discarded, the sequence number is kept
	sealedAt  time.Time
	// For a tombstone: what is lost if the arbiter shows the batch was never applied.
	lostHours int64
}

// Writer persists a Recorder periodically. It owns three one-connection pools
// (flush, heartbeat, sampler) so that a stalled flush can never delay a
// heartbeat or a sample. Nothing it does can reach an admission decision.
type Writer struct {
	rec   *Recorder
	owner Owner
	opt   WriterOptions
	log   *log.Logger

	instanceID string
	started    time.Time

	flushPool, hbPool, samplePool *pgxpool.Pool

	mu       sync.Mutex
	pending  opstats.Delta // gauge and storage samples awaiting the next flush
	counters WriterCounters
	lastErr  string
	lastOK   time.Time
	ready    bool
	status   struct {
		dbSeq, nextSeq int64
		sealed         bool
		abandoned      bool
		carryHours     int
	}

	// flush-goroutine state (only touched by the flush path, serialised by flushMu)
	flushMu       sync.Mutex
	carry         opstats.Delta
	sealed        *batch
	dbSeq         int64
	nextSeq       int64
	lastPrune     time.Time
	failures      int
	nextAttemptAt time.Time
	lastLogged    map[string]string // topic → last logged state
}

// NewWriter builds a writer; call Run to start it. No connection is opened
// until the first use.
func NewWriter(rec *Recorder, owner Owner, opt WriterOptions) (*Writer, error) {
	opt.defaults()
	w := &Writer{rec: rec, owner: owner, opt: opt, log: opt.Logger, instanceID: opstats.NewInstanceID(), nextSeq: 1}
	w.started = opt.Clock()
	flushDSN := opt.FlushDSN
	if flushDSN == "" {
		flushDSN = opt.DSN
	}
	var err error
	if w.flushPool, err = newPool(flushDSN, "magnetico-stats-flush", nil); err != nil {
		return nil, err
	}
	if w.hbPool, err = newPool(opt.DSN, "magnetico-stats-heartbeat", map[string]string{"statement_timeout": "5000", "lock_timeout": "1000"}); err != nil {
		w.flushPool.Close()
		return nil, err
	}
	if w.samplePool, err = newPool(opt.DSN, "magnetico-stats-sampler", nil); err != nil {
		w.flushPool.Close()
		w.hbPool.Close()
		return nil, err
	}
	w.status.nextSeq = 1
	return w, nil
}

func newPool(dsn, app string, params map[string]string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("statistics: %w", err)
	}
	cfg.MaxConns, cfg.MinConns = 1, 0
	cfg.ConnConfig.RuntimeParams["application_name"] = app
	for k, v := range params {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	return pgxpool.NewWithConfig(context.Background(), cfg)
}

// InstanceID is the identity of this process in the statistics.
func (w *Writer) InstanceID() string { return w.instanceID }

// Status returns a snapshot of the writer's state.
func (w *Writer) Status() WriterStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WriterStatus{Ready: w.ready, LastError: w.lastErr, DBSeq: w.status.dbSeq, NextSeq: w.status.nextSeq,
		SealedPending: w.status.sealed, SealedAbandon: w.status.abandoned, CarryHours: w.status.carryHours, Counters: w.counters}
}

func (w *Writer) now() time.Time { return w.opt.Clock() }

// Run starts the flush, heartbeat and sampler goroutines and blocks until ctx
// is done and they have returned. It never returns an error: a failing
// statistics database is logged (once per distinct condition) and retried.
func (w *Writer) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, loop := range []func(context.Context){w.flushLoop, w.heartbeatLoop, w.queueSamplerLoop, w.storageSamplerLoop} {
		wg.Add(1)
		go func() { defer wg.Done(); loop(ctx) }()
	}
	wg.Wait()
}

// Close makes a last bounded flush (ctx bounds it) and closes the pools. Call
// it after Run returned. A pool whose connection is stuck is abandoned rather
// than waited for.
func (w *Writer) Close(ctx context.Context) {
	if w.Status().Ready {
		// One pass settles at most one sealed batch: after a failed flush the
		// newer events wait in the carry behind it, so drain until nothing is
		// left (or a pass fails, or the context ends).
		for pass := 0; pass < 4 && ctx.Err() == nil; pass++ {
			if err := w.flushOnce(ctx, true); err != nil || w.drained() {
				break
			}
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.flushPool.Close()
		w.hbPool.Close()
		w.samplePool.Close()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		w.log.Printf("statistics: a database connection did not close in time; abandoned")
	}
}

// drained reports that no batch is sealed and nothing waits in the carry.
func (w *Writer) drained() bool {
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	return w.sealed == nil && w.carry.IsEmpty()
}

// logChange logs the state of a topic ("flush", "heartbeat", …) only when it
// changes: a persistent failure is one line, not one per retry.
func (w *Writer) logChange(topic, state, format string, args ...any) {
	w.mu.Lock()
	if w.lastLogged == nil {
		w.lastLogged = map[string]string{}
	}
	same := w.lastLogged[topic] == state
	w.lastLogged[topic] = state
	w.mu.Unlock()
	if !same {
		w.log.Printf(format, args...)
	}
}

// addQueueSample and addStorageSample hand a sample to the next flush.
func (w *Writer) addQueueSample(q opstats.QueueState) {
	w.mu.Lock()
	w.pending.AddQueueSample(q)
	w.mu.Unlock()
}

func (w *Writer) addStorageSample(s opstats.StorageState) {
	w.mu.Lock()
	w.pending.AddStorageSample(s)
	w.mu.Unlock()
}

// ---- the flush ---------------------------------------------------------------

func (w *Writer) flushLoop(ctx context.Context) {
	t := time.NewTicker(w.opt.FlushInterval)
	defer t.Stop()
	for {
		if err := w.flushOnce(ctx, false); err != nil && ctx.Err() == nil {
			w.logChange("flush", errClass(err), "statistics: persisting failed (%s); measurements stay buffered in memory and the retry continues", safeErr(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// errMissingPrivileges names the one start-up condition whose message we own.
var errMissingPrivileges = errors.New("the ingest role lacks privileges on the statistics schema")

// safeErr is what a log line may say about a failure: our own schema and
// privilege messages (names only), otherwise the class. A driver error carries
// the host, user and database of the connection, which the ingest log never
// prints (the convention of the whole service).
func safeErr(err error) string {
	if errors.Is(err, opstats.ErrSchemaMissing) || errors.Is(err, opstats.ErrSchemaInvalid) || errors.Is(err, errMissingPrivileges) {
		return err.Error()
	}
	return errClass(err)
}

func errClass(err error) string {
	switch {
	case errors.Is(err, opstats.ErrSchemaMissing):
		return "schema-missing"
	case errors.Is(err, opstats.ErrSchemaInvalid):
		return "schema-invalid"
	}
	// The class must not depend on how the driver wrapped the failure (a prepare
	// error and an execute error carry the same SQLSTATE): a repeated condition
	// is one state and is logged once.
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "57014":
		// query_canceled: the server answered the cancellation of a statement that outran its
		// deadline before the client's own context expired. The same condition as the
		// context deadline below; two classes would log one outage twice.
		return "timeout"
	case errors.As(err, &pgErr):
		return "pg:" + pgErr.Code
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return fmt.Sprintf("%T", err)
}

// prepare verifies the schema and the privileges and registers this instance.
func (w *Writer) prepare(ctx context.Context) error {
	if err := opstats.Verify(ctx, w.flushPool); err != nil {
		return err
	}
	missing, err := opstats.MissingPrivileges(ctx, w.flushPool, opstats.RoleIngest)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %v", errMissingPrivileges, missing)
	}
	if err := w.ensureInstance(ctx, 0); err != nil {
		return err
	}
	w.mu.Lock()
	w.ready = true
	w.mu.Unlock()
	return nil
}

// ensureInstance creates this instance's sequence row at seq (a no-op when it exists).
func (w *Writer) ensureInstance(ctx context.Context, seq int64) error {
	_, err := w.flushPool.Exec(ctx, `INSERT INTO magnetico_stats.ingest_instances (instance_id, started_at, last_flush_seq, heartbeat_at)
VALUES ($1, $2, $3, now()) ON CONFLICT DO NOTHING`, w.instanceID, w.started, seq)
	return err
}

// collect drains the recorder and the pending samples into the carry.
func (w *Writer) collect() {
	d := w.rec.Drain()
	w.mu.Lock()
	opstats.Merge(&d, &w.pending)
	w.pending = opstats.Delta{}
	w.mu.Unlock()
	opstats.Merge(&w.carry, &d)

	w.mu.Lock()
	w.counters.HoursDropped += w.carry.HoursDropped
	w.counters.CrawlersOverflow += w.carry.CrawlersOverflow
	w.counters.ClockAnomaly = w.counters.ClockAnomaly || w.carry.ClockAnomaly
	w.carry.HoursDropped, w.carry.CrawlersOverflow = 0, 0
	w.mu.Unlock()
}

// flushOnce collects and makes one attempt to persist. final skips the retry
// back-off (the shutdown flush).
func (w *Writer) flushOnce(ctx context.Context, final bool) error {
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	w.collect()
	defer w.publishState()

	if !w.Status().Ready {
		if err := w.prepare(ctx); err != nil {
			w.bumpFailure(err)
			return err
		}
		w.failures, w.nextAttemptAt = 0, time.Time{} // a recovered schema is used at once, not after the old back-off
	}
	// One attempt applies the sealed batch; what piled up behind it during an outage (the
	// carry) is sealed and applied right after, so a recovered database is caught up by the
	// first successful tick and not one batch per tick. Bounded: a long outage buffers at
	// most 26 hourly deltas, and a failure stops the loop at once.
	for applied := 0; applied < maxBatchesPerFlush; applied++ {
		if w.sealed == nil && !w.carry.IsEmpty() {
			w.sealed = &batch{seq: w.nextSeq, prev: w.dbSeq, delta: w.carry, sealedAt: w.now()}
			w.nextSeq++
			w.carry = opstats.Delta{}
		}
		if w.sealed == nil {
			break
		}
		if !w.sealed.abandoned && w.now().Sub(w.sealed.sealedAt) > w.opt.BatchMaxAge {
			w.abandon(w.sealed)
		}
		if !final && w.now().Before(w.nextAttemptAt) {
			return nil
		}
		if err := w.attempt(ctx); err != nil {
			w.bumpFailure(err)
			return err
		}
		w.failures = 0
	}
	w.markOK()
	return nil
}

// maxBatchesPerFlush bounds the batches one flush applies in a row (the sealed batch, then
// what waited behind it).
const maxBatchesPerFlush = 4

func (w *Writer) markOK() {
	w.mu.Lock()
	w.lastOK = w.now()
	w.lastErr = ""
	w.mu.Unlock()
}

// retryDelay is the pause after the failures-th consecutive failed attempt: 5 s,
// doubling, and never more than a quarter of the flush interval. An attempt has its
// own deadline (30 s) and the delay is counted from its end, so a longer delay would
// reach past the next tick and that tick would be skipped (the effective retry cadence
// would double); with the cap, deadline + delay stays below the interval.
func retryDelay(failures int, interval time.Duration) time.Duration {
	return min(5*time.Second<<min(max(failures-1, 0), 4), interval/4)
}

func (w *Writer) bumpFailure(err error) {
	w.failures++
	d := retryDelay(w.failures, w.opt.FlushInterval)
	w.nextAttemptAt = w.now().Add(d)
	w.mu.Lock()
	w.counters.FlushesFailed++
	w.lastErr = err.Error()
	if errors.Is(err, opstats.ErrSchemaMissing) || errors.Is(err, opstats.ErrSchemaInvalid) {
		w.ready = false
	}
	w.mu.Unlock()
}

func (w *Writer) publishState() {
	w.mu.Lock()
	w.status.dbSeq, w.status.nextSeq = w.dbSeq, w.nextSeq
	w.status.sealed = w.sealed != nil
	w.status.abandoned = w.sealed != nil && w.sealed.abandoned
	w.status.carryHours = len(w.carry.Hours)
	w.mu.Unlock()
}

// abandon turns an aged batch into a tombstone: its delta is discarded but its
// sequence number is kept, so the next batch meets no gap and the arbiter
// still decides whether the old delta was applied.
func (w *Writer) abandon(b *batch) {
	b.abandoned = true
	b.lostHours = int64(len(b.delta.Hours))
	b.delta = opstats.Delta{}
}

// errRetry means a concurrent transaction rolled back; try again.
var errRetry = errors.New("statistics: sequence arbiter saw a concurrent rollback")

// attempt makes one attempt to settle the sealed batch.
func (w *Writer) attempt(ctx context.Context) error {
	for round := 0; round < 3; round++ {
		err := w.attemptOnce(ctx)
		if errors.Is(err, errRetry) {
			continue
		}
		return err
	}
	return errRetry
}

func (w *Writer) attemptOnce(ctx context.Context) error {
	b := w.sealed
	actx, cancel := context.WithTimeout(ctx, w.opt.AttemptTimeout)
	defer cancel()
	tx, err := w.flushPool.BeginTx(actx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = tx.Rollback(rctx)
			rcancel()
		}
	}()
	if _, err := tx.Exec(actx, `SET LOCAL lock_timeout = '5s'; SET LOCAL statement_timeout = '10s'; SET LOCAL idle_in_transaction_session_timeout = '15s'`); err != nil {
		return err
	}
	// The arbiter: its affected-row count is read before any delta is applied.
	tag, err := tx.Exec(actx, `UPDATE magnetico_stats.ingest_instances SET last_flush_seq = $1, last_flush_at = now(), heartbeat_at = now()
WHERE instance_id = $2 AND last_flush_seq = $3`, b.seq, w.instanceID, b.prev)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		var res opstats.ApplyResult
		if !b.abandoned {
			b.delta.GuardRef = w.now() // the writer's clock now: a sample from a clock far ahead of it is not applied
			if res, err = opstats.Apply(actx, tx, &b.delta); err != nil {
				return err
			}
		}
		if err := tx.Commit(actx); err != nil {
			committed = true // the connection is gone or the outcome unknown: do not roll back
			b.inDoubt = true
			return err
		}
		committed = true
		w.mu.Lock()
		w.counters.FlushesOK++
		if res.QueueSkipped || res.StorageSkipped {
			w.counters.GaugeBatchesSkipped++
		}
		if b.abandoned {
			w.counters.HoursDropped += b.lostHours // the arbiter proved it was never applied
		}
		w.mu.Unlock()
		w.dbSeq = b.seq
		w.sealed = nil
		w.maybePrune(ctx)
		return nil
	}

	// No row matched: no delta is applied. Read the sequence to see why.
	var v int64
	err = tx.QueryRow(actx, `SELECT last_flush_seq FROM magnetico_stats.ingest_instances WHERE instance_id = $1`, w.instanceID).Scan(&v)
	missing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !missing {
		return err
	}
	_ = tx.Rollback(actx)
	committed = true // rolled back explicitly
	switch {
	case !missing && v == b.seq: // the batch (or its tombstone) committed already
		w.mu.Lock()
		w.counters.ResolvedCommitted++
		w.mu.Unlock()
		w.dbSeq = v
		w.sealed = nil
		return nil
	case !missing && v == b.prev: // a concurrent transaction rolled back
		return errRetry
	case missing && !b.inDoubt && !b.abandoned:
		// The row was removed (manual action, pruned after a long outage): recreate it
		// where this writer expects it and try once more.
		if err := w.ensureInstance(actx, b.prev); err != nil {
			return err
		}
		return errRetry
	case missing: // cannot tell whether it committed: never risk a double count
		if err := w.ensureInstance(actx, b.seq); err != nil {
			return err
		}
		w.dropUnresolved(b, true)
		w.dbSeq = b.seq
		return nil
	default: // a foreign write or corruption
		w.log.Printf("statistics: the sequence of this instance in the database is %d, expected %d or %d; the batch is dropped and the writer resynchronised", v, b.prev, b.seq)
		w.mu.Lock()
		w.counters.SequenceAnomalies++
		w.mu.Unlock()
		w.dropUnresolved(b, false)
		w.dbSeq = v
		w.nextSeq = max(w.nextSeq, v+1)
		return nil
	}
}

// dropUnresolved discards a batch whose fate cannot be determined or applied.
func (w *Writer) dropUnresolved(b *batch, unresolved bool) {
	w.mu.Lock()
	if unresolved {
		w.counters.BatchesDroppedUnresolved++
	}
	w.counters.HoursDropped += int64(len(b.delta.Hours)) + b.lostHours
	w.mu.Unlock()
	w.sealed = nil
}

// maybePrune deletes the rows of instances that disappeared. It runs after a
// successful flush, at most every PruneInterval, never touches this instance
// and skips rows locked by an in-flight flush.
func (w *Writer) maybePrune(ctx context.Context) {
	if w.now().Sub(w.lastPrune) < w.opt.PruneInterval {
		return
	}
	w.lastPrune = w.now()
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _ = w.flushPool.Exec(pctx, `DELETE FROM magnetico_stats.ingest_instances WHERE instance_id IN (
  SELECT instance_id FROM magnetico_stats.ingest_instances
  WHERE greatest(COALESCE(last_flush_at, started_at), COALESCE(heartbeat_at, started_at), started_at) < now() - interval '30 days'
    AND instance_id <> $1 FOR UPDATE SKIP LOCKED)`, w.instanceID)
	_, _ = w.flushPool.Exec(pctx, `DELETE FROM magnetico_stats.snapshot WHERE instance_id IN (
  SELECT instance_id FROM magnetico_stats.snapshot WHERE published_at < now() - interval '24 hours'
    AND instance_id <> $1 FOR UPDATE SKIP LOCKED)`, w.instanceID)
}
