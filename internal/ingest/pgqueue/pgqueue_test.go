package pgqueue

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
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/ingest/record"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

var limits = codec.DefaultLimits()

func hashOf(n int) [20]byte {
	var h [20]byte
	h[0], h[1], h[2], h[3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	h[19] = 0xAB
	return h
}

// payloadFor returns a valid record v1 payload whose name pads it to about
// `pad` extra bytes.
func payloadFor(t testing.TB, n, pad int) Item {
	t.Helper()
	h := hashOf(n)
	rec := record.Record{
		InfoHash: h, Name: fmt.Sprintf("t%d-%s", n, strings.Repeat("x", pad)),
		TotalSize: 10, DiscoveredOn: 1700000000,
		Files: []persistence.File{{Path: "a.bin", Size: 10}},
	}
	b, err := record.Encode(rec, limits)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return Item{InfoHash: h, Payload: b}
}

// env is a disposable database with the 0.8.0 and ingest schemas.
type env struct {
	dsn   string
	store persistence.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	ctx := context.Background()
	if err := persistence.InitSchema(ctx, dsn); err != nil {
		t.Fatalf("persistence.InitSchema: %v", err)
	}
	if err := InitSchema(ctx, dsn); err != nil {
		t.Fatalf("pgqueue.InitSchema: %v", err)
	}
	store, err := persistence.MakeDatabase(ctx, dsn)
	if err != nil {
		t.Fatalf("MakeDatabase: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &env{dsn: dsn, store: store}
}

func (e *env) open(t *testing.T, o Options) *Queue {
	t.Helper()
	q, err := Open(context.Background(), e.dsn, o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(q.Close)
	return q
}

func mustStats(t *testing.T, q *Queue) Stats {
	t.Helper()
	s, err := q.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	return s
}

func torrentCount(t *testing.T, e *env) int {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM magneticod.torrents`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- schema -----------------------------------------------------------

func TestInitSchemaRequiresVerifiedMagneticodSchema(t *testing.T) {
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	err := InitSchema(context.Background(), dsn)
	if err == nil {
		t.Fatal("InitSchema on a database without the 0.8.0 schema must fail")
	}
	// Nothing was created.
	conn, cerr := pgx.Connect(context.Background(), dsn)
	if cerr != nil {
		t.Fatal(cerr)
	}
	defer conn.Close(context.Background())
	var exists bool
	if err := conn.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname='magnetico_ingest')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("a refused ingest-schema-init created the schema")
	}
}

func TestInitSchemaIdempotentAndIsolated(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := InitSchema(ctx, e.dsn); err != nil {
			t.Fatalf("InitSchema #%d: %v", i, err)
		}
	}
	// Persistence startup verification and schema-init are unaffected by the
	// presence of magnetico_ingest (and the reverse order works too).
	s, err := persistence.MakeDatabase(ctx, e.dsn)
	if err != nil {
		t.Fatalf("MakeDatabase with magnetico_ingest present: %v", err)
	}
	_ = s.Close()
	if err := persistence.InitSchema(ctx, e.dsn); err != nil {
		t.Fatalf("persistence.InitSchema with magnetico_ingest present: %v", err)
	}
}

func TestOpenVerifiesSchemaVersion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO magnetico_ingest.ingest_version VALUES (2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, e.dsn, Options{}); !errors.Is(err, ErrSchemaMissing) {
		t.Fatalf("Open with extra version row: %v, want ErrSchemaMissing", err)
	}
	// And a database without the schema at all.
	bare := pgtest.IntegrationCluster(t).TestDatabase(t)
	if _, err := Open(ctx, bare, Options{}); !errors.Is(err, ErrSchemaMissing) {
		t.Fatalf("Open without schema: %v, want ErrSchemaMissing", err)
	}
}

func TestOpenRefusesIncompleteImport(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// Simulate a migration progress table with a non-complete run.
	for _, stmt := range []string{
		`CREATE SCHEMA magnetico_migration`,
		`CREATE TABLE magnetico_migration.progress (run_id UUID PRIMARY KEY, phase TEXT NOT NULL)`,
		`INSERT INTO magnetico_migration.progress VALUES (gen_random_uuid(), 'importing')`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(ctx, e.dsn, Options{}); !errors.Is(err, ErrImportIncomplete) {
		t.Fatalf("Open during import: %v, want ErrImportIncomplete", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE magnetico_migration.progress SET phase='import_complete'`); err != nil {
		t.Fatal(err)
	}
	q, err := Open(ctx, e.dsn, Options{})
	if err != nil {
		t.Fatalf("Open after completed import: %v", err)
	}
	q.Close()
}

func TestOpenRefusesFsyncOff(t *testing.T) {
	c := pgtest.IntegrationCluster(t)
	if c.External {
		t.Skip("will not change fsync on an external server")
	}
	e := newEnv(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, c.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	setFsync := func(v string) {
		if _, err := admin.Exec(ctx, "ALTER SYSTEM SET fsync = "+v); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
			t.Fatal(err)
		}
	}
	setFsync("off")
	t.Cleanup(func() {
		c2, err := pgx.Connect(context.Background(), c.AdminDSN)
		if err != nil {
			t.Errorf("restore fsync: %v", err)
			return
		}
		defer c2.Close(context.Background())
		_, _ = c2.Exec(context.Background(), "ALTER SYSTEM RESET fsync")
		_, _ = c2.Exec(context.Background(), "SELECT pg_reload_conf()")
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := Open(ctx, e.dsn, Options{})
		if errors.Is(err, ErrNonDurableDatabase) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Open with fsync=off: %v, want ErrNonDurableDatabase", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	q, err := Open(ctx, e.dsn, Options{AllowNonDurable: true})
	if err != nil {
		t.Fatalf("Open with the override: %v", err)
	}
	q.Close()
}

func TestUsageTriggersGuardMutation(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	ctx := context.Background()
	if _, err := q.Admit(ctx, []Item{payloadFor(t, 1, 10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.pool.Exec(ctx, `UPDATE magnetico_ingest.queue SET payload_bytes = 1`); err == nil {
		t.Fatal("payload_bytes update must be rejected")
	}
	if _, err := q.pool.Exec(ctx, `TRUNCATE magnetico_ingest.queue`); err == nil {
		t.Fatal("TRUNCATE must be rejected")
	}
	// Operator deletes keep the accounting exact.
	if _, err := q.pool.Exec(ctx, `DELETE FROM magnetico_ingest.queue`); err != nil {
		t.Fatal(err)
	}
	if s := mustStats(t, q); s.UsedBytes != 0 || !s.UsageConsistent() {
		t.Fatalf("after operator delete: %+v", s)
	}
}

// --- admission ---------------------------------------------------------

func TestAdmitDurableDuplicatesAndAllOrNothing(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	ctx := context.Background()

	a, b := payloadFor(t, 1, 100), payloadFor(t, 2, 100)
	res, err := q.Admit(ctx, []Item{b, a, a}) // unsorted, with an in-batch duplicate
	if err != nil || res.Inserted != 2 || res.Duplicates != 1 {
		t.Fatalf("Admit = %+v, %v", res, err)
	}
	res, err = q.Admit(ctx, []Item{a, b})
	if err != nil || res.Inserted != 0 || res.Duplicates != 2 {
		t.Fatalf("redelivery = %+v, %v", res, err)
	}
	s := mustStats(t, q)
	if s.QueueRows != 2 || !s.UsageConsistent() || s.UsedBytes != int64(len(a.Payload)+len(b.Payload)) {
		t.Fatalf("stats = %+v", s)
	}

	// Contents survive a restart of the queue handle.
	q.Close()
	q2 := e.open(t, Options{})
	if s2 := mustStats(t, q2); s2.QueueRows != 2 {
		t.Fatalf("after reopen: %+v", s2)
	}
}

func TestAdmitRollsBackWholeBatchOnFailure(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	ctx := context.Background()
	good := payloadFor(t, 1, 10)
	// A row violating the table CHECK cannot happen through Admit's sizes;
	// force a failure with an unusable context cancelled mid-flight instead.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := q.Admit(cctx, []Item{good}); err == nil {
		t.Fatal("cancelled admission must fail")
	}
	if s := mustStats(t, q); s.QueueRows != 0 || s.UsedBytes != 0 {
		t.Fatalf("a failed admission left rows: %+v", s)
	}
}

func TestQuotaRefusesAndDuplicateStaysIdempotentWhenFull(t *testing.T) {
	e := newEnv(t)
	first := payloadFor(t, 1, 1000)
	q := e.open(t, Options{MaxBytes: int64(len(first.Payload)) + 100})
	ctx := context.Background()
	if _, err := q.Admit(ctx, []Item{first}); err != nil {
		t.Fatal(err)
	}
	// New record exceeds the quota: nothing written, retryable error.
	if _, err := q.Admit(ctx, []Item{payloadFor(t, 2, 1000)}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("over quota: %v, want ErrQueueFull", err)
	}
	// A mixed batch (duplicate + new over quota) is refused as a whole.
	if _, err := q.Admit(ctx, []Item{first, payloadFor(t, 3, 1000)}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("mixed over quota: %v, want ErrQueueFull", err)
	}
	if s := mustStats(t, q); s.QueueRows != 1 {
		t.Fatalf("refused batches left rows: %+v", s)
	}
	// Redelivery of a pending hash at full capacity succeeds.
	res, err := q.Admit(ctx, []Item{first})
	if err != nil || res.Inserted != 0 || res.Duplicates != 1 {
		t.Fatalf("duplicate at full: %+v, %v", res, err)
	}
}

// TestQuotaConcurrencyRegression submits many overlapping batches whose
// total exceeds the quota. used_bytes must never exceed the limit, must
// equal the table sums after every kind of write path, and surplus requests
// must be refused with ErrQueueFull; no deadlock may occur.
func TestQuotaConcurrencyRegression(t *testing.T) {
	e := newEnv(t)
	const (
		payloadPad = 2000
		workers    = 16
		batches    = 12
		perBatch   = 5
		distinct   = 120
	)
	probe := payloadFor(t, 0, payloadPad)
	limit := int64(len(probe.Payload)) * 60 // room for about 60 of ~120 distinct hashes
	q := e.open(t, Options{MaxBytes: limit, PoolSize: 8})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var full, ok atomic.Int64
	var overLimit atomic.Bool
	stop := make(chan struct{})
	var mon sync.WaitGroup
	mon.Add(1)
	go func() { // monitor: committed used_bytes never above the limit
		defer mon.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			var used int64
			if err := q.pool.QueryRow(ctx, `SELECT used_bytes FROM magnetico_ingest.usage`).Scan(&used); err == nil && used > limit {
				overLimit.Store(true)
			}
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for b := 0; b < batches; b++ {
				var items []Item
				for i := 0; i < perBatch; i++ {
					// Overlapping hashes across workers: duplicates and disjoint mix.
					items = append(items, payloadFor(t, (w*7+b*perBatch+i)%distinct, payloadPad))
				}
				_, err := q.Admit(ctx, items)
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ErrQueueFull):
					full.Add(1)
				default:
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	mon.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("unexpected admission error (deadlock/timeout?): %v", err)
	}
	if overLimit.Load() {
		t.Fatal("used_bytes exceeded the limit at some point")
	}
	if full.Load() == 0 || ok.Load() == 0 {
		t.Fatalf("expected both successes and queue_full refusals, got ok=%d full=%d", ok.Load(), full.Load())
	}
	s := mustStats(t, q)
	if s.UsedBytes > limit || !s.UsageConsistent() {
		t.Fatalf("after admissions: %+v (limit %d)", s, limit)
	}

	// Acknowledge (delete) half, quarantine some, operator-delete some.
	rows, err := q.pool.Query(ctx, `SELECT id FROM magnetico_ingest.queue ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) < 6 {
		t.Fatalf("too few rows to exercise paths: %d", len(ids))
	}
	c := NewConsumer(q, e.store, ConsumerOptions{})
	var wg2 sync.WaitGroup
	for i, id := range ids {
		wg2.Add(1)
		go func(i int, id int64) {
			defer wg2.Done()
			switch i % 3 {
			case 0: // acknowledgement
				if _, err := q.pool.Exec(ctx, `DELETE FROM magnetico_ingest.queue WHERE id=$1`, id); err != nil {
					t.Errorf("ack: %v", err)
				}
			case 1: // queue -> quarantine transfer
				if err := c.quarantineRow(ctx, id, "test", "x"); err != nil {
					t.Errorf("quarantine: %v", err)
				}
			}
		}(i, id)
	}
	wg2.Wait()
	s = mustStats(t, q)
	if !s.UsageConsistent() || s.QuarantineRows == 0 {
		t.Fatalf("after ack+quarantine: %+v", s)
	}
	if _, err := q.pool.Exec(ctx, `DELETE FROM magnetico_ingest.quarantine`); err != nil {
		t.Fatal(err)
	}
	s = mustStats(t, q)
	if !s.UsageConsistent() || s.QuarantineRows != 0 {
		t.Fatalf("after operator delete: %+v", s)
	}
}

// --- consumer ----------------------------------------------------------

func runConsumer(t *testing.T, c *Consumer) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func fastOpts(o ConsumerOptions) ConsumerOptions {
	o.IdlePoll = 20 * time.Millisecond
	o.StandbyRetry = 50 * time.Millisecond
	o.BackoffMin = 20 * time.Millisecond
	o.BackoffMax = 100 * time.Millisecond
	return o
}

func TestConsumerStoresAndAcknowledges(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	var stored sync.Map
	c := NewConsumer(q, e.store, fastOpts(ConsumerOptions{OnStored: func(h [20]byte) { stored.Store(h, true) }}))
	items := []Item{payloadFor(t, 1, 10), payloadFor(t, 2, 10), payloadFor(t, 3, 10)}
	if _, err := q.Admit(context.Background(), items); err != nil {
		t.Fatal(err)
	}
	runConsumer(t, c)
	waitFor(t, "queue drain", func() bool { return mustStats(t, q).QueueRows == 0 })
	if n := torrentCount(t, e); n != 3 {
		t.Fatalf("torrents = %d, want 3", n)
	}
	if s := mustStats(t, q); s.UsedBytes != 0 || !s.UsageConsistent() {
		t.Fatalf("stats after drain: %+v", s)
	}
	for _, it := range items {
		if _, ok := stored.Load(it.InfoHash); !ok {
			t.Errorf("OnStored not called for %x", it.InfoHash)
		}
	}
}

// TestConsumerCrashAfterAddTorrentIsIdempotent simulates a crash between
// AddTorrent and the queue DELETE: the row is processed again by the next
// consumer, which must leave exactly one torrents row.
func TestConsumerCrashAfterAddTorrentIsIdempotent(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	item := payloadFor(t, 7, 10)
	if _, err := q.Admit(context.Background(), []Item{item}); err != nil {
		t.Fatal(err)
	}
	// The hook fails after AddTorrent committed and before the DELETE and
	// stops the consumer, like a process death at that point.
	c1 := NewConsumer(q, e.store, fastOpts(ConsumerOptions{beforeDelete: func([20]byte) error { cancel(); return errors.New("simulated crash") }}))
	done := make(chan struct{})
	go func() { defer close(done); _ = c1.Run(ctx) }()
	<-done
	if n := torrentCount(t, e); n != 1 {
		t.Fatalf("after crash torrents = %d, want 1", n)
	}
	if s := mustStats(t, q); s.QueueRows != 1 {
		t.Fatalf("row must remain after the crash: %+v", s)
	}
	c2 := NewConsumer(q, e.store, fastOpts(ConsumerOptions{}))
	runConsumer(t, c2)
	waitFor(t, "reprocess", func() bool { return mustStats(t, q).QueueRows == 0 })
	if n := torrentCount(t, e); n != 1 {
		t.Fatalf("after reprocess torrents = %d, want exactly 1", n)
	}
	if st := c2.Stats(); st.Already != 1 || st.Stored != 0 {
		t.Fatalf("stats = %+v, want the reprocessed row counted as already present", st)
	}
}

// fakeStore classifies by info hash for error-path tests.
type fakeStore struct {
	inner persistence.Store
	fail  func(h [20]byte) error
	calls atomic.Int64
}

func (f *fakeStore) AddTorrent(ctx context.Context, m persistence.Torrent, files []persistence.File) (bool, int32, error) {
	f.calls.Add(1)
	var h [20]byte
	copy(h[:], m.InfoHash)
	if err := f.fail(h); err != nil {
		return false, 0, err
	}
	return f.inner.AddTorrent(ctx, m, files)
}

func TestConsumerQuarantinesInvalidMetadataAndPoisonDoesNotBlock(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	bad, good := payloadFor(t, 1, 10), payloadFor(t, 2, 10)
	fs := &fakeStore{inner: e.store, fail: func(h [20]byte) error {
		if h == bad.InfoHash {
			return fmt.Errorf("%w: test", persistence.ErrInvalidMetadata)
		}
		return nil
	}}
	// Also an undecodable payload straight in the table.
	if _, err := q.Admit(context.Background(), []Item{bad, good}); err != nil {
		t.Fatal(err)
	}
	junk := hashOf(99)
	if _, err := q.pool.Exec(context.Background(),
		`INSERT INTO magnetico_ingest.queue (info_hash, format, payload, payload_bytes) VALUES ($1, 1, '\x7b7d', 2)`, junk[:]); err != nil {
		t.Fatal(err)
	}
	c := NewConsumer(q, fs, fastOpts(ConsumerOptions{}))
	runConsumer(t, c)
	waitFor(t, "drain", func() bool { return mustStats(t, q).QueueRows == 0 })
	s := mustStats(t, q)
	if s.QuarantineRows != 2 || !s.UsageConsistent() {
		t.Fatalf("stats = %+v, want 2 quarantined", s)
	}
	if n := torrentCount(t, e); n != 1 {
		t.Fatalf("torrents = %d, want only the good record", n)
	}
	rows, err := q.pool.Query(context.Background(), `SELECT reason_code, payload_bytes FROM magnetico_ingest.quarantine ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	reasons := map[string]int{}
	for rows.Next() {
		var r string
		var b int
		if err := rows.Scan(&r, &b); err != nil {
			t.Fatal(err)
		}
		reasons[r]++
	}
	if reasons[ReasonInvalidMeta] != 1 || reasons[ReasonInvalidRecord] != 1 {
		t.Fatalf("reasons = %v", reasons)
	}
}

func TestConsumerRetriesThenQuarantinesAfterMaxAttempts(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	item := payloadFor(t, 5, 10)
	fs := &fakeStore{inner: e.store, fail: func([20]byte) error { return errors.New("unclassified") }}
	if _, err := q.Admit(context.Background(), []Item{item}); err != nil {
		t.Fatal(err)
	}
	c := NewConsumer(q, fs, fastOpts(ConsumerOptions{}))
	runConsumer(t, c)
	ctx := context.Background()
	waitFor(t, "max_attempts quarantine", func() bool {
		// Collapse the per-row backoff so the test does not wait minutes.
		_, _ = q.pool.Exec(ctx, `UPDATE magnetico_ingest.queue SET next_attempt_at = now()`)
		return mustStats(t, q).QuarantineRows == 1
	})
	var reason string
	var attempts int
	if err := q.pool.QueryRow(ctx, `SELECT reason_code, attempts FROM magnetico_ingest.quarantine`).Scan(&reason, &attempts); err != nil {
		t.Fatal(err)
	}
	if reason != ReasonMaxAttempts || attempts != maxAttempts {
		t.Fatalf("reason=%s attempts=%d", reason, attempts)
	}
	if got := fs.calls.Load(); got != maxAttempts {
		t.Fatalf("AddTorrent called %d times, want %d", got, maxAttempts)
	}
}

func TestConsumerStorageErrorBacksOffWithoutTouchingRow(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	item := payloadFor(t, 6, 10)
	var failing atomic.Bool
	failing.Store(true)
	fs := &fakeStore{inner: e.store, fail: func([20]byte) error {
		if failing.Load() {
			return fmt.Errorf("%w: down", persistence.ErrStorageIO)
		}
		return nil
	}}
	if _, err := q.Admit(context.Background(), []Item{item}); err != nil {
		t.Fatal(err)
	}
	c := NewConsumer(q, fs, fastOpts(ConsumerOptions{}))
	runConsumer(t, c)
	waitFor(t, "storage errors", func() bool { return c.Stats().StorageErrs >= 2 })
	var attempts int
	if err := q.pool.QueryRow(context.Background(), `SELECT attempts FROM magnetico_ingest.queue`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("attempts = %d, %v; storage errors must not consume attempts", attempts, err)
	}
	failing.Store(false)
	waitFor(t, "recovery", func() bool { return mustStats(t, q).QueueRows == 0 })
	if n := torrentCount(t, e); n != 1 {
		t.Fatalf("torrents = %d", n)
	}
}

// advisoryHolders lists the backend pids holding the consumer lock.
func advisoryHolders(t *testing.T, e *env) []int32 {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	rows, err := pool.Query(context.Background(), `
SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted AND objsubid=1
  AND ((classid::bigint << 32) | objid::bigint) = $1`, consumerLockKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var pids []int32
	for rows.Next() {
		var p int32
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		pids = append(pids, p)
	}
	return pids
}

func TestConsumerSingleOwnerAndTakeoverAfterBackendKill(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	var n atomic.Int64
	count := func([20]byte) { n.Add(1) }
	c1 := NewConsumer(q, e.store, fastOpts(ConsumerOptions{OnStored: count}))
	c2 := NewConsumer(q, e.store, fastOpts(ConsumerOptions{OnStored: count}))
	stop1, stop2 := runConsumer(t, c1), runConsumer(t, c2)
	defer stop1()
	defer stop2()
	waitFor(t, "an owner", func() bool { return c1.Owns() || c2.Owns() })
	time.Sleep(300 * time.Millisecond) // the standby retries meanwhile
	if c1.Owns() && c2.Owns() {
		t.Fatal("two consumers own the lock")
	}
	pids := advisoryHolders(t, e)
	if len(pids) != 1 {
		t.Fatalf("advisory lock holders = %v, want exactly one session", pids)
	}
	owner, standby := c1, c2
	if c2.Owns() {
		owner, standby = c2, c1
	}

	// The lock is a session lock on a dedicated connection: killing that
	// backend (not any pooled query) must make the owner stop.
	admin, err := pgx.Connect(context.Background(), e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, pids[0]); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "owner to notice the loss", func() bool { return owner.Stats().LockLosses >= 1 })
	// Either consumer may win the race to reacquire; exactly one must.
	waitFor(t, "takeover", func() bool { return owner.Owns() != standby.Owns() })
	waitFor(t, "a single new lock holder", func() bool { return len(advisoryHolders(t, e)) == 1 })

	items := []Item{payloadFor(t, 10, 10), payloadFor(t, 11, 10), payloadFor(t, 12, 10)}
	if _, err := q.Admit(context.Background(), items); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "drain after takeover", func() bool { return mustStats(t, q).QueueRows == 0 })
	if n := torrentCount(t, e); n != 3 {
		t.Fatalf("torrents = %d, want 3 (nothing lost or duplicated)", n)
	}
	if got := n.Load(); got != 3 {
		t.Fatalf("OnStored count = %d, want 3", got)
	}
}

func TestConsumerGracefulShutdownReleasesLock(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	c := NewConsumer(q, e.store, fastOpts(ConsumerOptions{}))
	stop := runConsumer(t, c)
	waitFor(t, "ownership", c.Owns)
	if len(advisoryHolders(t, e)) != 1 {
		t.Fatal("expected one lock holder while running")
	}
	stop()
	if c.Owns() {
		t.Fatal("still owns after shutdown")
	}
	waitFor(t, "lock released", func() bool { return len(advisoryHolders(t, e)) == 0 })
}

func TestConsumerOwnershipCheckFailsClosed(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	c := NewConsumer(q, e.store, fastOpts(ConsumerOptions{}))
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if c.verifyOwnership(ctx, conn) {
		t.Fatal("a connection that never took the lock must not verify as owner")
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, consumerLockKey); err != nil {
		t.Fatal(err)
	}
	if !c.verifyOwnership(ctx, conn) {
		t.Fatal("holder must verify")
	}
	// A pool query must never be accepted as ownership: take the lock
	// through the pool on whatever connection it picks and verify on a
	// different, lock-less connection.
	other, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	if c.verifyOwnership(ctx, other) {
		t.Fatal("a different session must not verify as owner")
	}
	conn.Close(ctx)
	if c.verifyOwnership(ctx, conn) {
		t.Fatal("a closed connection must fail closed")
	}
}
