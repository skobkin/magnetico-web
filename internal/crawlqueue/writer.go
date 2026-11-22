package crawlqueue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// writer is the only goroutine that mutates the database and the byte
// counters. Appends are coalesced into group commits; every other operation
// is executed on its own right away.
func (q *Queue) writer() {
	defer close(q.done)
	ticker := time.NewTicker(q.opts.CheckpointInterval)
	defer ticker.Stop()

	for {
		select {
		case o := <-q.ops:
			q.handle(o)
		case <-ticker.C:
			q.maintain()
		case done := <-q.maintainReq:
			q.maintain()
			close(done)
		case <-q.stop:
			q.drainClosed()
			return
		}
	}
}

// drainClosed answers queued but unprocessed operations with ErrClosed.
func (q *Queue) drainClosed() {
	for {
		select {
		case o := <-q.ops:
			o.resp <- result{err: ErrClosed}
		default:
			return
		}
	}
}

func (q *Queue) handle(o *op) {
	if h := q.opts.hooks.beforeOp; h != nil {
		h()
	}
	if err := q.Err(); err != nil {
		o.resp <- result{err: err}
		return
	}
	switch o.kind {
	case opAppend:
		q.handleAppends(o)
	case opAppendQuarantined:
		q.doAppendQuarantined(o)
	case opAck:
		q.doAck(o)
	case opQuarantine:
		q.doQuarantine(o)
	}
}

// handleAppends gathers further appends until the group is full, nothing more
// is waiting (no linger) or the optional linger expires, commits them in one
// transaction and then runs a non-append
// operation that interrupted the gathering, if any.
func (q *Queue) handleAppends(first *op) {
	batch := []*op{first}
	var pending *op

	var timeout <-chan time.Time
	if q.opts.GroupMaxDelay > 0 {
		timer := time.NewTimer(q.opts.GroupMaxDelay)
		defer timer.Stop()
		timeout = timer.C
	}
gather:
	for len(batch) < q.opts.GroupMaxRecords {
		if timeout == nil {
			// No linger: take only what is already waiting.
			select {
			case o := <-q.ops:
				if o.kind != opAppend {
					pending = o
					break gather
				}
				batch = append(batch, o)
			default:
				break gather
			}
			continue
		}
		select {
		case o := <-q.ops:
			if o.kind != opAppend {
				pending = o
				break gather
			}
			batch = append(batch, o)
		case <-timeout:
			break gather
		case <-q.stop:
			break gather
		}
	}
	q.commitAppends(batch)
	if pending != nil {
		q.handle(pending)
	}
}

func (q *Queue) commitAppends(batch []*op) {
	ctx := q.dbCtx
	res := make([]result, len(batch))
	failAll := func(err error) {
		for i := range batch {
			batch[i].resp <- result{err: err}
		}
	}

	started := time.Now()
	tx, err := q.wdb.BeginTx(ctx, nil)
	if err != nil {
		failAll(q.writeErr("begin", err))
		return
	}
	curBytes := q.queueBytes.Load() + q.quarBytes.Load()
	var addedRecords, addedBytes int64
	for i, o := range batch {
		size := int64(len(o.payload))
		if curBytes+size > q.opts.MaxBytes {
			// Over quota. A hash that is already queued is still an
			// idempotent no-op, so look only on this (rare) path; a normal
			// append pays no extra query.
			var one int
			err := tx.QueryRowContext(ctx, "SELECT 1 FROM queue WHERE info_hash = ?", o.hash[:]).Scan(&one)
			switch {
			case err == nil:
				// duplicate: appended=false, no error
			case errors.Is(err, sql.ErrNoRows):
				res[i].err = ErrQueueFull
			default:
				_ = tx.Rollback()
				failAll(q.writeErr("append lookup", err))
				return
			}
			continue
		}
		r, err := tx.ExecContext(ctx,
			"INSERT INTO queue(info_hash, format, payload_bytes, enqueued_at, payload) VALUES(?,?,?,?,?) "+
				"ON CONFLICT(info_hash) DO NOTHING",
			o.hash[:], o.format, size, o.enqueued.UnixNano(), o.payload)
		if err != nil {
			_ = tx.Rollback()
			failAll(q.writeErr("append", err))
			return
		}
		if n, _ := r.RowsAffected(); n == 1 {
			res[i].appended = true
			curBytes += size
			addedRecords++
			addedBytes += size
		}
	}
	if err := tx.Commit(); err != nil {
		failAll(q.writeErr("commit", err))
		return
	}
	q.queueRecords.Add(addedRecords)
	q.queueBytes.Add(addedBytes)
	q.commits.Add(1)
	q.commitRecs.Add(addedRecords)
	q.commitNanos.Add(int64(time.Since(started)))
	if n := int64(len(batch)); n > q.maxGroup.Load() {
		q.maxGroup.Store(n)
	}
	for i, o := range batch {
		if errors.Is(res[i].err, ErrQueueFull) {
			q.fullRejects.Add(1)
		}
		o.resp <- res[i]
	}
}

// writeErr maps a write failure: SQLITE_FULL becomes ErrQueueFull, fatal
// codes poison the queue, anything else is returned wrapped.
func (q *Queue) writeErr(what string, err error) error {
	if isFull(err) {
		q.fullRejects.Add(1)
		return fmt.Errorf("%w: %s: %w", ErrQueueFull, what, err)
	}
	return q.fail(fmt.Errorf("crawlqueue: %s: %w", what, err))
}

func (q *Queue) doAppendQuarantined(o *op) {
	ctx := q.dbCtx
	size := int64(len(o.payload))
	if q.queueBytes.Load()+q.quarBytes.Load()+size > q.opts.MaxBytes {
		q.fullRejects.Add(1)
		o.resp <- result{err: ErrQueueFull}
		return
	}
	tx, err := q.wdb.BeginTx(ctx, nil)
	if err != nil {
		o.resp <- result{err: q.writeErr("begin", err)}
		return
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO quarantine(info_hash, format, payload_bytes, reason_code, http_status, quarantined_at, payload) "+
			"VALUES(?,?,?,?,NULL,?,?)",
		o.hash[:], o.format, size, o.reason, time.Now().UnixNano(), o.payload); err != nil {
		_ = tx.Rollback()
		o.resp <- result{err: q.writeErr("quarantine append", err)}
		return
	}
	if err := tx.Commit(); err != nil {
		o.resp <- result{err: q.writeErr("commit", err)}
		return
	}
	q.quarRecords.Add(1)
	q.quarBytes.Add(size)
	o.resp <- result{appended: true}
}

func (q *Queue) doAck(o *op) {
	ctx := q.dbCtx
	tx, err := q.wdb.BeginTx(ctx, nil)
	if err != nil {
		o.resp <- result{err: q.writeErr("begin", err)}
		return
	}
	var removed, freed int64
	for _, id := range o.ids {
		var sz int64
		err := tx.QueryRowContext(ctx,
			"DELETE FROM queue WHERE id=? RETURNING payload_bytes", id).Scan(&sz)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			_ = tx.Rollback()
			o.resp <- result{err: q.writeErr("ack", err)}
			return
		}
		removed++
		freed += sz
	}
	if err := tx.Commit(); err != nil {
		o.resp <- result{err: q.writeErr("commit", err)}
		return
	}
	q.queueRecords.Add(-removed)
	q.queueBytes.Add(-freed)
	o.resp <- result{}
}

func (q *Queue) doQuarantine(o *op) {
	ctx := q.dbCtx
	tx, err := q.wdb.BeginTx(ctx, nil)
	if err != nil {
		o.resp <- result{err: q.writeErr("begin", err)}
		return
	}
	var status any
	if o.status != 0 {
		status = o.status
	}
	now := time.Now().UnixNano()
	var moved, bytes int64
	for _, id := range o.ids {
		var sz int64
		err := tx.QueryRowContext(ctx, "SELECT payload_bytes FROM queue WHERE id=?", id).Scan(&sz)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			_ = tx.Rollback()
			o.resp <- result{err: q.writeErr("quarantine", err)}
			return
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO quarantine(info_hash, format, payload_bytes, reason_code, http_status, quarantined_at, payload) "+
				"SELECT info_hash, format, payload_bytes, ?, ?, ?, payload FROM queue WHERE id=?",
			o.reason, status, now, id); err != nil {
			_ = tx.Rollback()
			o.resp <- result{err: q.writeErr("quarantine", err)}
			return
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM queue WHERE id=?", id); err != nil {
			_ = tx.Rollback()
			o.resp <- result{err: q.writeErr("quarantine", err)}
			return
		}
		moved++
		bytes += sz
	}
	if err := tx.Commit(); err != nil {
		o.resp <- result{err: q.writeErr("commit", err)}
		return
	}
	// Bytes stay inside the quota: only the table changes.
	q.queueRecords.Add(-moved)
	q.queueBytes.Add(-bytes)
	q.quarRecords.Add(moved)
	q.quarBytes.Add(bytes)
	o.resp <- result{}
}

// ------------------------------------------------------------ maintenance

// warn logs a message at most once per logThrottle per key.
func (q *Queue) warn(key, format string, args ...any) {
	now := time.Now()
	if last, ok := q.lastWarn[key]; ok && now.Sub(last) < logThrottle {
		return
	}
	q.lastWarn[key] = now
	q.opts.Logf(format, args...)
}

// maintain runs on the writer goroutine: a WAL checkpoint (PASSIVE, then
// TRUNCATE when nothing pins the WAL), incremental vacuum and WAL size
// accounting. Failures are counted, logged (throttled) and retried at the
// next pass; they never stop the writer.
func (q *Queue) maintain() {
	if q.Err() != nil {
		return
	}
	ctx := q.dbCtx
	q.cpRuns.Add(1)

	var cpErr error
	if h := q.opts.hooks.checkpointErr; h != nil {
		cpErr = h()
	}
	if cpErr == nil {
		var busy, logFrames, checkpointed int64
		err := q.wdb.QueryRowContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &logFrames, &checkpointed)
		switch {
		case err != nil:
			cpErr = err
		case busy != 0 || checkpointed < logFrames:
			// A reader pins part of the WAL: not an error, but the WAL cannot
			// be reset yet.
			q.cpIncomplete.Add(1)
		default:
			// Everything is in the database file and no reader pins the WAL:
			// resetting it is cheap and lets journal_size_limit truncate it.
			if _, err := q.wdb.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				cpErr = err
			}
		}
	}
	if cpErr != nil {
		if isFatalCode(cpErr) {
			q.fail(fmt.Errorf("crawlqueue: checkpoint: %w", cpErr))
			return
		}
		q.cpFailures.Add(1)
		q.warn("checkpoint", "crawl queue: WAL checkpoint failed (will retry): %v", cpErr)
	}

	q.refreshWALSize()
	if w := q.walBytes.Load(); w > q.opts.walWarn {
		q.warn("wal-size", "crawl queue: WAL file is %d MiB (> %d MiB): a long-lived reader or failing "+
			"checkpoint is preventing reclamation; provision disk headroom beyond the payload quota",
			w>>20, q.opts.walWarn>>20)
	}

	q.vacuum(ctx)
}

func (q *Queue) vacuum(ctx context.Context) {
	var free, pageSize int64
	if err := q.wdb.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&free); err != nil {
		return
	}
	if err := q.wdb.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil || pageSize <= 0 {
		return
	}
	if free*pageSize < q.opts.VacuumThreshold {
		return
	}
	pages := vacuumChunkBytes / pageSize
	if pages < 1 {
		pages = 1
	}
	if _, err := q.wdb.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", pages)); err != nil {
		if isFatalCode(err) {
			q.fail(fmt.Errorf("crawlqueue: incremental vacuum: %w", err))
			return
		}
		q.warn("vacuum", "crawl queue: incremental vacuum failed: %v", err)
		return
	}
	q.vacRuns.Add(1)
}

// Maintain runs one maintenance pass synchronously on the writer goroutine.
// It exists for tests and diagnostics; the queue also runs it periodically.
func (q *Queue) Maintain(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case q.maintainReq <- done:
	case <-q.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return q.Err()
	case <-q.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}
