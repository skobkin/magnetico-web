// Package crawlqueue is the crawler's durable local queue: a SQLite database
// in WAL mode with synchronous=FULL (docs/specs/crawler-ingest-delivery-v1.md
// §4).
//
// Guarantees:
//   - Append returns nil only after the row is committed with a full fsync, so
//     every acknowledged record survives SIGKILL and power loss.
//   - Appends are group-committed by a single writer goroutine (≤ 64 records
//     or ≤ 50 ms per commit); Ack/Quarantine run on the same goroutine.
//   - The byte quota (queue + quarantine payload bytes) is tracked in memory
//     by that single writer; the process is exclusive through flock.
//   - Append never blocks for space: when the quota or the database is full it
//     returns ErrQueueFull immediately.
//   - Startup performs only SQLite's normal WAL recovery, no integrity scan;
//     see Check for the on-demand diagnostic.
//   - Corruption and I/O errors are fatal: the queue refuses further work and
//     reports the error through Failed/Err so the process can exit non-zero.
//     Nothing is repaired or deleted automatically.
package crawlqueue

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
)

const (
	// SchemaVersion is stored in PRAGMA user_version.
	SchemaVersion = 1

	dbFileName   = "queue.db"
	lockFileName = "LOCK"

	// FormatJSONv1 is the payload format of record v1 (the only one).
	FormatJSONv1 = 1

	defaultMaxBytes         = 1 << 30
	defaultGroupRecords     = 64
	defaultJournalSizeLimit = 64 << 20
	defaultCheckpointEvery  = 30 * time.Second
	defaultVacuumThreshold  = 32 << 20
	defaultCacheKiB         = 8 << 10
	walWarnBytes            = 256 << 20
	logThrottle             = 10 * time.Minute
	vacuumChunkBytes        = 16 << 20
	busyTimeoutMillis       = 5000
	maxPeekRecordsCap       = 100_000
)

var (
	// ErrQueueFull reports that the byte quota or the database file limit
	// would be exceeded. Nothing was written.
	ErrQueueFull = errors.New("crawlqueue: queue is full")
	// ErrClosed is returned by operations on a closed queue.
	ErrClosed = errors.New("crawlqueue: queue is closed")
	// ErrLocked reports that another process holds the queue directory.
	ErrLocked = errors.New("crawlqueue: queue directory is locked by another process")
	// ErrSchemaVersion reports an unknown or inconsistent on-disk schema.
	ErrSchemaVersion = errors.New("crawlqueue: unsupported queue schema version")
	// ErrFatal wraps unrecoverable database errors (I/O, corruption).
	ErrFatal = errors.New("crawlqueue: fatal database error")
)

// Options configures Open. Zero values select the documented defaults.
type Options struct {
	Dir string
	// MaxBytes bounds SUM(payload_bytes) of queue + quarantine. Default 1 GiB.
	MaxBytes int64
	// GroupMaxRecords (default 64) bounds one group commit. GroupMaxDelay is
	// an optional linger: how long the writer waits for more appends before
	// committing a group that is not yet full. The default 0 means no linger:
	// the writer commits what is already waiting, so groups still form
	// naturally while the previous commit's fsync is in progress, and a lone
	// append is not delayed. The spec's bound is "≤ 64 records or ≤ 50 ms".
	GroupMaxRecords int
	GroupMaxDelay   time.Duration
	// JournalSizeLimit is SQLite's journal_size_limit. It is NOT a ceiling:
	// it only truncates the WAL after a completed checkpoint.
	JournalSizeLimit int64
	// MaxPageCount sets PRAGMA max_page_count (0 = SQLite default). It makes
	// the "database is full" path testable and can cap the file size.
	MaxPageCount int64
	// CheckpointInterval is the period of the maintenance pass (WAL
	// checkpoint, incremental vacuum, WAL size accounting). Default 30 s.
	CheckpointInterval time.Duration
	// VacuumThreshold is the free-list size in bytes above which free pages
	// are returned to the filesystem. Default 32 MiB.
	VacuumThreshold int64
	// Logf receives throttled operational messages. Default log.Printf.
	Logf func(format string, args ...any)

	hooks hooks // test hooks
	// walWarn overrides the WAL size warning threshold (tests only).
	walWarn int64
}

type hooks struct {
	// checkpointErr, when set, replaces the result of a checkpoint attempt.
	checkpointErr func() error
	// beforeOp, when set, runs in the writer goroutine before each operation
	// is handled; tests use it to hold an operation in flight.
	beforeOp func()
}

func (o *Options) applyDefaults() {
	if o.MaxBytes <= 0 {
		o.MaxBytes = defaultMaxBytes
	}
	if o.GroupMaxRecords <= 0 {
		o.GroupMaxRecords = defaultGroupRecords
	}
	if o.JournalSizeLimit <= 0 {
		o.JournalSizeLimit = defaultJournalSizeLimit
	}
	if o.CheckpointInterval <= 0 {
		o.CheckpointInterval = defaultCheckpointEvery
	}
	if o.VacuumThreshold <= 0 {
		o.VacuumThreshold = defaultVacuumThreshold
	}
	if o.walWarn <= 0 {
		o.walWarn = walWarnBytes
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
}

// Item is a queued record as returned by Peek.
type Item struct {
	ID         int64
	InfoHash   [20]byte
	Format     int
	Payload    []byte
	EnqueuedAt time.Time
}

// Stats is a snapshot of the queue counters.
type Stats struct {
	QueueRecords      int64
	QueueBytes        int64
	QuarantineRecords int64
	QuarantineBytes   int64
	MaxBytes          int64
	WALBytes          int64 // size of the -wal file at the last maintenance pass / Stats call

	CheckpointRuns       int64
	CheckpointIncomplete int64 // PASSIVE checkpoints that could not finish (readers/pinned frames)
	CheckpointFailures   int64
	VacuumRuns           int64
	FullRejections       int64 // appends refused with ErrQueueFull

	// Group-commit instrumentation (append commits only).
	AppendCommits     int64 // committed group transactions
	AppendedRecords   int64 // records inserted by those commits
	AppendCommitNanos int64 // total wall time spent in BEGIN..COMMIT of those commits
	MaxGroupRecords   int64 // largest group committed so far
}

type opKind int

const (
	opAppend opKind = iota
	opAppendQuarantined
	opAck
	opQuarantine
)

type op struct {
	kind     opKind
	hash     [20]byte
	format   int
	payload  []byte
	reason   string
	status   int
	ids      []int64
	resp     chan result
	enqueued time.Time
}

type result struct {
	appended bool
	err      error
}

// Queue is the durable local queue. All methods are safe for concurrent use.
type Queue struct {
	opts Options
	dir  string
	lock *os.File
	wdb  *sql.DB // single writer connection
	rdb  *sql.DB // short read transactions

	ops         chan *op
	maintainReq chan chan struct{}
	stop        chan struct{}
	done        chan struct{}
	// dbCtx bounds every writer database operation. It is cancelled only by
	// CloseWithin, after the graceful window, to interrupt in-flight
	// statements (an interrupted transaction rolls back; committed rows are
	// unaffected).
	dbCtx    context.Context
	dbCancel context.CancelFunc
	// abandoned is set when CloseWithin gave up on a writer that did not
	// terminate: the database handles and the directory lock are then
	// deliberately left open for the process exit to release.
	abandoned atomic.Bool
	stopOnce  sync.Once
	closeMu   sync.Mutex
	closeErr  error
	closed    bool // handles closed and lock released; guarded by closeMu

	failedCh   chan struct{}
	failedOnce sync.Once
	failedErr  atomic.Pointer[error]

	// Counters. Written by the writer goroutine (and Open), read anywhere.
	queueRecords atomic.Int64
	queueBytes   atomic.Int64
	quarRecords  atomic.Int64
	quarBytes    atomic.Int64
	walBytes     atomic.Int64
	cpRuns       atomic.Int64
	cpIncomplete atomic.Int64
	cpFailures   atomic.Int64
	vacRuns      atomic.Int64
	fullRejects  atomic.Int64
	commits      atomic.Int64
	commitRecs   atomic.Int64
	commitNanos  atomic.Int64
	maxGroup     atomic.Int64

	lastWarn map[string]time.Time // writer goroutine only
}

func dsn(path string, extra url.Values) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+strconv.Itoa(busyTimeoutMillis)+")")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(OFF)")
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	return "file:" + filepath.ToSlash(path) + "?" + q.Encode()
}

// Open creates or opens the queue in opts.Dir. It takes an exclusive lock on
// the directory; a second opener gets ErrLocked.
func Open(opts Options) (*Queue, error) {
	opts.applyDefaults()
	if opts.Dir == "" {
		return nil, errors.New("crawlqueue: empty queue directory")
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("crawlqueue: create queue directory: %w", err)
	}
	lock, err := lockDir(filepath.Join(opts.Dir, lockFileName))
	if err != nil {
		return nil, err
	}

	dbCtx, dbCancel := context.WithCancel(context.Background())
	q := &Queue{
		dbCtx:       dbCtx,
		dbCancel:    dbCancel,
		opts:        opts,
		dir:         opts.Dir,
		lock:        lock,
		ops:         make(chan *op, 1024),
		maintainReq: make(chan chan struct{}),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		failedCh:    make(chan struct{}),
		lastWarn:    map[string]time.Time{},
	}
	if err := q.open(); err != nil {
		dbCancel()
		q.closeDBs()
		unlockDir(lock)
		return nil, err
	}
	go q.writer()
	return q, nil
}

func (q *Queue) open() error {
	path := filepath.Join(q.dir, dbFileName)

	wextra := url.Values{}
	wextra.Add("_txlock", "immediate")
	wextra.Add("_pragma", "journal_size_limit("+strconv.FormatInt(q.opts.JournalSizeLimit, 10)+")")
	wextra.Add("_pragma", "cache_size(-"+strconv.Itoa(defaultCacheKiB)+")")
	wdb, err := sql.Open("sqlite", dsn(path, wextra))
	if err != nil {
		return fmt.Errorf("crawlqueue: open database: %w", err)
	}
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	wdb.SetConnMaxLifetime(0)
	q.wdb = wdb

	ctx := context.Background()
	if err := q.initSchema(ctx); err != nil {
		return err
	}
	if q.opts.MaxPageCount > 0 {
		if _, err := wdb.ExecContext(ctx, "PRAGMA max_page_count="+strconv.FormatInt(q.opts.MaxPageCount, 10)); err != nil {
			return fmt.Errorf("crawlqueue: set max_page_count: %w", err)
		}
	}

	rextra := url.Values{}
	rextra.Add("_pragma", "query_only(1)")
	rdb, err := sql.Open("sqlite", dsn(path, rextra))
	if err != nil {
		return fmt.Errorf("crawlqueue: open read connection: %w", err)
	}
	rdb.SetMaxOpenConns(4)
	q.rdb = rdb

	return q.loadCounters(ctx)
}

const schemaSQL = `
CREATE TABLE queue(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  info_hash BLOB NOT NULL UNIQUE,
  format INTEGER NOT NULL,
  payload_bytes INTEGER NOT NULL,
  enqueued_at INTEGER NOT NULL,
  payload BLOB NOT NULL
);
CREATE TABLE quarantine(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  info_hash BLOB NOT NULL,
  format INTEGER NOT NULL,
  payload_bytes INTEGER NOT NULL,
  reason_code TEXT NOT NULL,
  http_status INTEGER,
  quarantined_at INTEGER NOT NULL,
  payload BLOB NOT NULL
);`

// The payload column is last on purpose: SQLite reads a record's columns in
// order, so scans that only need payload_bytes (counters at open, Peek size
// planning) never follow the payload's overflow-page chain.

func (q *Queue) initSchema(ctx context.Context) error {
	conn, err := q.wdb.Conn(ctx)
	if err != nil {
		return classify(fmt.Errorf("crawlqueue: open database: %w", err))
	}
	defer conn.Close()

	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return classify(fmt.Errorf("crawlqueue: read schema version: %w", err))
	}
	var tables int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&tables); err != nil {
		return classify(fmt.Errorf("crawlqueue: read schema: %w", err))
	}

	switch {
	case version == 0 && tables == 0:
		// auto_vacuum only takes effect before the first table exists.
		if _, err := conn.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
			return fmt.Errorf("crawlqueue: set auto_vacuum: %w", err)
		}
		if err := setWAL(ctx, conn); err != nil {
			return err
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("crawlqueue: init schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("crawlqueue: init schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version="+strconv.Itoa(SchemaVersion)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("crawlqueue: init schema: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("crawlqueue: init schema: %w", err)
		}
		return nil
	case version == SchemaVersion:
		return setWAL(ctx, conn)
	default:
		return fmt.Errorf("%w: user_version=%d, tables=%d (this build supports %d)",
			ErrSchemaVersion, version, tables, SchemaVersion)
	}
}

func setWAL(ctx context.Context, conn *sql.Conn) error {
	var mode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return classify(fmt.Errorf("crawlqueue: enable WAL: %w", err))
	}
	if mode != "wal" {
		return fmt.Errorf("crawlqueue: journal_mode is %q, WAL is required (is the filesystem local?)", mode)
	}
	return nil
}

func (q *Queue) loadCounters(ctx context.Context) error {
	var n, b int64
	if err := q.rdb.QueryRowContext(ctx,
		"SELECT count(*), COALESCE(sum(payload_bytes),0) FROM queue").Scan(&n, &b); err != nil {
		return classify(fmt.Errorf("crawlqueue: count queue: %w", err))
	}
	q.queueRecords.Store(n)
	q.queueBytes.Store(b)
	if err := q.rdb.QueryRowContext(ctx,
		"SELECT count(*), COALESCE(sum(payload_bytes),0) FROM quarantine").Scan(&n, &b); err != nil {
		return classify(fmt.Errorf("crawlqueue: count quarantine: %w", err))
	}
	q.quarRecords.Store(n)
	q.quarBytes.Store(b)
	return nil
}

// ---------------------------------------------------------------- errors

// SQLite primary result codes the queue cares about.
const (
	sqliteIOErr   = 10
	sqliteCorrupt = 11
	sqliteFull    = 13
	sqliteNotADB  = 26
)

func sqliteCode(err error) int {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() & 0xff
	}
	return 0
}

func isFull(err error) bool { return sqliteCode(err) == sqliteFull }

func isFatalCode(err error) bool {
	switch sqliteCode(err) {
	case sqliteIOErr, sqliteCorrupt, sqliteNotADB:
		return true
	}
	return false
}

// classify wraps fatal database errors in ErrFatal and leaves others as is.
func classify(err error) error {
	if err != nil && isFatalCode(err) && !errors.Is(err, ErrFatal) {
		return fmt.Errorf("%w: %w", ErrFatal, err)
	}
	return err
}

// fail records the first fatal error. Subsequent operations return it.
func (q *Queue) fail(err error) error {
	err = classify(err)
	if errors.Is(err, ErrFatal) {
		q.failedOnce.Do(func() {
			q.failedErr.Store(&err)
			close(q.failedCh)
		})
	}
	return err
}

// Failed is closed after an unrecoverable database error. The process should
// exit non-zero; committed data stays on disk.
func (q *Queue) Failed() <-chan struct{} { return q.failedCh }

// Err returns the fatal error once Failed is closed, otherwise nil.
func (q *Queue) Err() error {
	if p := q.failedErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (q *Queue) usable() error {
	if err := q.Err(); err != nil {
		return err
	}
	select {
	case <-q.done:
		return ErrClosed
	case <-q.stop:
		return ErrClosed
	default:
		return nil
	}
}

// -------------------------------------------------------------- public API

// Append durably enqueues one record. It returns appended=false (and nil) if
// a record with the same info hash is already queued. A nil error means the
// row is committed and fsynced. If ctx ends while waiting, the record may
// still be committed. The payload is copied before the handoff, so the caller
// may reuse it as soon as Append returns, whatever the outcome. A duplicate of
// an already queued hash is reported as appended=false even when the queue is
// at its byte quota.
func (q *Queue) Append(ctx context.Context, infoHash [20]byte, format int, payload []byte) (appended bool, err error) {
	r, err := q.submit(ctx, &op{kind: opAppend, hash: infoHash, format: format, payload: bytes.Clone(payload)})
	return r.appended, err
}

// AppendQuarantined stores a record directly in quarantine (for example
// record_too_large at enqueue time). It counts toward the byte quota.
func (q *Queue) AppendQuarantined(ctx context.Context, infoHash [20]byte, format int, payload []byte, reason string) error {
	_, err := q.submit(ctx, &op{kind: opAppendQuarantined, hash: infoHash, format: format, payload: bytes.Clone(payload), reason: reason})
	return err
}

// Ack deletes delivered records. Unknown ids are ignored.
func (q *Queue) Ack(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := q.submit(ctx, &op{kind: opAck, ids: slices.Clone(ids)})
	return err
}

// Quarantine moves records from the queue to quarantine in one transaction,
// keeping the payload and recording the reason and HTTP status (0 = none).
// Unknown ids are ignored. Quarantine rows are never deleted by this package.
func (q *Queue) Quarantine(ctx context.Context, ids []int64, reason string, httpStatus int) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := q.submit(ctx, &op{kind: opQuarantine, ids: slices.Clone(ids), reason: reason, status: httpStatus})
	return err
}

func (q *Queue) submit(ctx context.Context, o *op) (result, error) {
	if err := q.usable(); err != nil {
		return result{}, err
	}
	o.resp = make(chan result, 1)
	o.enqueued = time.Now()
	select {
	case q.ops <- o:
	case <-q.done:
		return result{}, ErrClosed
	case <-ctx.Done():
		return result{}, ctx.Err()
	}
	select {
	case r := <-o.resp:
		return r, r.err
	case <-q.done:
		select {
		case r := <-o.resp:
			return r, r.err
		default:
			return result{}, ErrClosed
		}
	case <-ctx.Done():
		return result{}, ctx.Err()
	}
}

// Peek returns the oldest records (FIFO by id): at most maxRecords and at
// most maxBytes of payload, but always at least one record when the queue is
// not empty (so an oversized record cannot wedge the head). It uses one short
// read transaction and never holds it across other work.
func (q *Queue) Peek(ctx context.Context, maxRecords int, maxBytes int64) ([]Item, error) {
	if err := q.usable(); err != nil {
		return nil, err
	}
	if maxRecords <= 0 {
		return nil, nil
	}
	if maxRecords > maxPeekRecordsCap {
		maxRecords = maxPeekRecordsCap
	}
	tx, err := q.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, q.fail(fmt.Errorf("crawlqueue: peek: %w", err))
	}
	defer tx.Rollback() //nolint:errcheck // read-only

	rows, err := tx.QueryContext(ctx,
		"SELECT id, payload_bytes FROM queue ORDER BY id LIMIT ?", maxRecords)
	if err != nil {
		return nil, q.fail(fmt.Errorf("crawlqueue: peek: %w", err))
	}
	var firstID, lastID int64
	var total int64
	n := 0
	for rows.Next() {
		var id, sz int64
		if err := rows.Scan(&id, &sz); err != nil {
			rows.Close()
			return nil, q.fail(fmt.Errorf("crawlqueue: peek: %w", err))
		}
		if n > 0 && total+sz > maxBytes {
			break
		}
		if n == 0 {
			firstID = id
		}
		lastID = id
		total += sz
		n++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, q.fail(fmt.Errorf("crawlqueue: peek: %w", err))
	}
	rows.Close()
	if n == 0 {
		return nil, nil
	}

	// The chosen ids are a contiguous prefix of the id order, so the id range
	// selects exactly them (same snapshot).
	prows, err := tx.QueryContext(ctx,
		"SELECT id, info_hash, format, enqueued_at, payload FROM queue WHERE id BETWEEN ? AND ? ORDER BY id",
		firstID, lastID)
	if err != nil {
		return nil, q.fail(fmt.Errorf("crawlqueue: peek: %w", err))
	}
	defer prows.Close()
	items := make([]Item, 0, n)
	for prows.Next() {
		var it Item
		var hash []byte
		var at int64
		if err := prows.Scan(&it.ID, &hash, &it.Format, &at, &it.Payload); err != nil {
			return nil, q.fail(fmt.Errorf("crawlqueue: peek: %w", err))
		}
		if len(hash) != 20 {
			return nil, q.fail(fmt.Errorf("%w: queue row %d has a %d-byte info hash", ErrFatal, it.ID, len(hash)))
		}
		copy(it.InfoHash[:], hash)
		it.EnqueuedAt = time.Unix(0, at)
		items = append(items, it)
	}
	if err := prows.Err(); err != nil {
		return nil, q.fail(fmt.Errorf("crawlqueue: peek: %w", err))
	}
	return items, nil
}

// Stats returns a snapshot of the counters.
func (q *Queue) Stats() Stats {
	q.refreshWALSize()
	return Stats{
		QueueRecords:         q.queueRecords.Load(),
		QueueBytes:           q.queueBytes.Load(),
		QuarantineRecords:    q.quarRecords.Load(),
		QuarantineBytes:      q.quarBytes.Load(),
		MaxBytes:             q.opts.MaxBytes,
		WALBytes:             q.walBytes.Load(),
		CheckpointRuns:       q.cpRuns.Load(),
		CheckpointIncomplete: q.cpIncomplete.Load(),
		CheckpointFailures:   q.cpFailures.Load(),
		VacuumRuns:           q.vacRuns.Load(),
		FullRejections:       q.fullRejects.Load(),
		AppendCommits:        q.commits.Load(),
		AppendedRecords:      q.commitRecs.Load(),
		AppendCommitNanos:    q.commitNanos.Load(),
		MaxGroupRecords:      q.maxGroup.Load(),
	}
}

// ErrCloseTimeout reports that CloseWithin could not stop the writer: it is
// blocked in a database or file-system operation that did not return even
// after the statements were interrupted. The database handles and the
// directory lock are intentionally NOT released (an abandoned writer could
// still touch them); every committed record is safe on disk and SQLite WAL
// recovery runs at the next open. The caller must terminate the process.
var ErrCloseTimeout = errors.New("crawlqueue: the writer did not stop within the shutdown grace period")

// Abandoned reports whether CloseWithin gave up on the writer.
func (q *Queue) Abandoned() bool { return q.abandoned.Load() }

// CloseWithin is the bounded variant of Close for process shutdown. It
// stops accepting new work and waits up to grace for the writer to finish
// the operation it is executing; then it cancels the in-flight statements
// and waits up to grace/2 more (minimum 100 ms). If the writer is stuck
// beyond that — for example in an uninterruptible fsync — it returns
// ErrCloseTimeout WITHOUT closing the database handles or releasing the
// directory lock, because the abandoned writer may still use them. Records
// committed earlier are never affected. SQLite's busy timeout is not a bound
// on file-system I/O and is not relied upon here.
func (q *Queue) CloseWithin(grace time.Duration) error {
	if grace <= 0 {
		grace = time.Second
	}
	if !q.closeMu.TryLock() {
		// Another Close is in progress (and may be waiting unboundedly):
		// do not join it, just bound our own wait.
		select {
		case <-q.done:
			return nil
		case <-time.After(grace):
			return ErrCloseTimeout
		}
	}
	defer q.closeMu.Unlock()
	if q.closed {
		return q.closeErr
	}
	q.stopOnce.Do(func() { close(q.stop) })

	wait := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-q.done:
			return true
		case <-t.C:
			return false
		}
	}
	if !wait(grace) {
		q.dbCancel() // interrupt the statement the writer is stuck in
		second := grace / 2
		if second < 100*time.Millisecond {
			second = 100 * time.Millisecond
		}
		if !wait(second) {
			q.abandoned.Store(true)
			return ErrCloseTimeout
		}
	}
	return q.finishClose()
}

// Close stops the writer (committed work is never affected), attempts a final
// WAL checkpoint and releases the directory lock. It is idempotent.
func (q *Queue) Close() error {
	q.closeMu.Lock()
	defer q.closeMu.Unlock()
	if q.closed {
		return q.closeErr
	}
	q.stopOnce.Do(func() { close(q.stop) })
	<-q.done
	return q.finishClose()
}

// finishClose runs after the writer has terminated (q.done is closed): final
// checkpoint, handle close, lock release. Callers hold closeMu.
func (q *Queue) finishClose() error {
	if q.Err() == nil && q.dbCtx.Err() == nil {
		// Best-effort: leave a small WAL behind.
		_, _ = q.wdb.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	}
	q.dbCancel()
	q.closeErr = q.closeDBs()
	unlockDir(q.lock)
	q.closed = true
	return q.closeErr
}

func (q *Queue) closeDBs() error {
	var err error
	if q.rdb != nil {
		err = errors.Join(err, q.rdb.Close())
	}
	if q.wdb != nil {
		err = errors.Join(err, q.wdb.Close())
	}
	return err
}

func (q *Queue) refreshWALSize() {
	if fi, err := os.Stat(filepath.Join(q.dir, dbFileName+"-wal")); err == nil {
		q.walBytes.Store(fi.Size())
	} else if errors.Is(err, os.ErrNotExist) {
		q.walBytes.Store(0)
	}
}
