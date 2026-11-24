package pgqueue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/persistence"
)

var (
	// ErrQueueFull reports that admitting the batch would exceed the byte
	// quota. Nothing was written; the condition is retryable.
	ErrQueueFull = errors.New("pgqueue: the ingest queue byte quota is exhausted")

	// ErrImportIncomplete reports an in-progress or incomplete
	// `migrate torrents` import: ingest must not insert before it finishes.
	ErrImportIncomplete = errors.New(
		"pgqueue: a `migrate torrents` import is in progress or incomplete; finish it before starting ingest")

	// ErrNonDurableDatabase reports fsync=off without the explicit override.
	ErrNonDurableDatabase = errors.New(
		"pgqueue: the PostgreSQL server runs with fsync=off; refusing to acknowledge records as durable " +
			"(set MAGNETICO_INGEST_ALLOW_NON_DURABLE_DB=true only for development and tests)")
)

const (
	// DefaultMaxBytes is the default quota of queue+quarantine payload bytes.
	DefaultMaxBytes int64 = 2 << 30
	// DefaultPoolSize is the queue pool size (spec §6.5).
	DefaultPoolSize = 4
)

// Options configures Open.
type Options struct {
	// MaxBytes limits queue+quarantine payload bytes (default DefaultMaxBytes).
	MaxBytes int64
	// PoolSize is the queue pool size (default DefaultPoolSize).
	PoolSize int32
	// AllowNonDurable permits fsync=off (development and tests only).
	AllowNonDurable bool
}

// Item is one record to admit: the exact record JSON plus its info hash.
type Item struct {
	InfoHash [20]byte
	Payload  []byte
}

// AdmitResult describes a committed admission.
type AdmitResult struct {
	Inserted   int // rows newly queued
	Duplicates int // rows already pending (ON CONFLICT DO NOTHING)
}

// Queue is the PostgreSQL ingest queue.
type Queue struct {
	pool *pgxpool.Pool
	opts Options
	dsn  string
}

// Open connects the queue pool and verifies, read-only, the schema version,
// fsync, and that no import is in progress. It never creates anything.
func Open(ctx context.Context, dsn string, opts Options) (*Queue, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.PoolSize <= 0 {
		opts.PoolSize = DefaultPoolSize
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	cfg.MaxConns = opts.PoolSize
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	q := &Queue{pool: pool, opts: opts, dsn: dsn}
	if err := q.verify(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return q, nil
}

func (q *Queue) verify(ctx context.Context) error {
	if err := q.pool.Ping(ctx); err != nil {
		return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	if err := verifyVersion(ctx, q.pool); err != nil {
		if errors.Is(err, ErrSchemaMissing) {
			return err
		}
		// A missing schema surfaces as to_regclass NULL (handled); anything
		// else is a storage failure.
		return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	if !q.opts.AllowNonDurable {
		var fsync string
		if err := q.pool.QueryRow(ctx, `SHOW fsync`).Scan(&fsync); err != nil {
			return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
		}
		if strings.EqualFold(fsync, "off") {
			return ErrNonDurableDatabase
		}
	}
	// The progress table exists only after a `migrate torrents` run. Check
	// for it first: referencing a missing relation fails at planning time.
	var progressTable *string
	if err := q.pool.QueryRow(ctx, `SELECT to_regclass('magnetico_migration.progress')::text`).Scan(&progressTable); err != nil {
		return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	var incomplete int64
	if progressTable != nil {
		if err := q.pool.QueryRow(ctx, `
SELECT count(*) FROM magnetico_migration.progress
WHERE phase NOT IN ('import_complete', 'import_complete_with_errors')`).Scan(&incomplete); err != nil {
			return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
		}
	}
	if incomplete > 0 {
		return ErrImportIncomplete
	}
	return nil
}

// Close closes the queue pool. The caller must have stopped the consumer
// and any admission first.
func (q *Queue) Close() { q.pool.Close() }

// Pool exposes the queue pool for diagnostics and tests.
func (q *Queue) Pool() *pgxpool.Pool { return q.pool }

// Admit durably queues every item or none. It returns only after the
// transaction committed with synchronous_commit=on, so a nil error means
// each item is either queued or was already pending. Rows are sorted by
// info hash so concurrent batches take the unique-index locks in one order;
// the trigger's usage-row lock is always the last lock taken. A batch that
// would exceed the quota is rolled back and returns ErrQueueFull. A batch
// consisting only of duplicates writes nothing and is never refused by the
// quota (idempotent redelivery at a full queue).
func (q *Queue) Admit(ctx context.Context, items []Item) (AdmitResult, error) {
	if len(items) == 0 {
		return AdmitResult{}, nil
	}
	sorted := make([]Item, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		return string(sorted[i].InfoHash[:]) < string(sorted[j].InfoHash[:])
	})
	hashes := make([][]byte, len(sorted))
	formats := make([]int16, len(sorted))
	payloads := make([][]byte, len(sorted))
	sizes := make([]int32, len(sorted))
	for i := range sorted {
		h := sorted[i].InfoHash
		hashes[i] = h[:]
		formats[i] = FormatRecordV1
		payloads[i] = sorted[i].Payload
		sizes[i] = int32(len(sorted[i].Payload))
	}

	var res AdmitResult
	tx, err := q.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AdmitResult{}, storageErr(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		return AdmitResult{}, storageErr(err)
	}
	// Duplicate input hashes inside one batch would make ON CONFLICT touch
	// the same row twice; collapse them (first wins) before the insert.
	tag, err := tx.Exec(ctx, `
INSERT INTO magnetico_ingest.queue (info_hash, format, payload, payload_bytes)
SELECT DISTINCT ON (h) h, f, p, s
FROM unnest($1::bytea[], $2::smallint[], $3::bytea[], $4::int[]) WITH ORDINALITY AS t(h, f, p, s, ord)
ORDER BY h, ord
ON CONFLICT (info_hash) DO NOTHING`, hashes, formats, payloads, sizes)
	if err != nil {
		return AdmitResult{}, storageErr(err)
	}
	res.Inserted = int(tag.RowsAffected())
	res.Duplicates = len(items) - res.Inserted

	if res.Inserted > 0 {
		// Our trigger already updated (and locked) the usage row, so this
		// read is the serialised, committed-plus-ours total.
		var used int64
		if err := tx.QueryRow(ctx, `SELECT used_bytes FROM magnetico_ingest.usage WHERE id = 1`).Scan(&used); err != nil {
			return AdmitResult{}, storageErr(err)
		}
		if used > q.opts.MaxBytes {
			return AdmitResult{}, ErrQueueFull // deferred Rollback discards everything
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AdmitResult{}, storageErr(err)
	}
	return res, nil
}

func storageErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
}

// Stats is a point-in-time view of the queue.
type Stats struct {
	QueueRows       int64
	QueueBytes      int64
	QuarantineRows  int64
	QuarantineBytes int64
	UsedBytes       int64 // usage.used_bytes
}

// Stats reads the counts and the accounted usage.
func (q *Queue) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := q.pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM magnetico_ingest.queue),
       (SELECT COALESCE(sum(payload_bytes), 0) FROM magnetico_ingest.queue),
       (SELECT count(*) FROM magnetico_ingest.quarantine),
       (SELECT COALESCE(sum(payload_bytes), 0) FROM magnetico_ingest.quarantine),
       (SELECT used_bytes FROM magnetico_ingest.usage WHERE id = 1)`).
		Scan(&s.QueueRows, &s.QueueBytes, &s.QuarantineRows, &s.QuarantineBytes, &s.UsedBytes)
	if err != nil {
		return Stats{}, storageErr(err)
	}
	return s, nil
}

// UsageConsistent reports whether usage.used_bytes equals the table sums
// (read-only startup check; a mismatch is a warning, never a refusal).
func (s Stats) UsageConsistent() bool {
	return s.UsedBytes == s.QueueBytes+s.QuarantineBytes
}
