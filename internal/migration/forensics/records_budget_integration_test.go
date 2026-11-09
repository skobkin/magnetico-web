package forensics

// Owner review round 1 tests (findings 1 and 2):
//
//   - the record-batch blob budget: blobs are fetched ONE record at a time,
//     inside the target snapshot, immediately before the record's block is
//     emitted — never prefetched into a per-batch map (64 records × a 64 MiB
//     cap would be a ~4 GiB batch). A pgx tracer + an event-recording sink
//     writer share one timeline so the fetch/emit interleaving is directly
//     observable.
//   - over-cap blobs under --include-blobs are rejected from the
//     octet_length probe BEFORE any blob byte is fetched.
//   - the target snapshot is REPEATABLE READ READ ONLY: a concurrent target
//     commit between the batch's statements is invisible to the batch.
//   - --max-records beyond the selection batch is a typed cap error (never a
//     zero-length slice panic) and an exact-fit selection succeeds.
//
// Fixtures stay small: KiB-scale blobs only.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// timeline records blob-fetch queries (via a pgx tracer) and emitted events
// (via the sink's writer) in one shared sequence.
type timeline struct {
	mu   sync.Mutex
	rows []string
}

func (t *timeline) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rows = append(t.rows, s)
}

func (t *timeline) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.rows...)
}

func (t *timeline) indexOf(prefix string) int {
	for i, r := range t.rows {
		if strings.HasPrefix(r, prefix) {
			return i
		}
	}
	return -1
}

func (t *timeline) count(prefix string) int {
	n := 0
	for _, r := range t.rows {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// queryTracer records the two queries the records pass makes against the
// target: the batch metadata scan and each individual blob fetch (with its
// torrent_id argument).
type queryTracer struct{ tl *timeline }

func (q *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	sql := strings.Join(strings.Fields(data.SQL), " ")
	switch {
	case strings.Contains(sql, "SELECT data FROM magneticod.torrent_file_blobs"):
		id := "?"
		if len(data.Args) > 0 {
			id = fmt.Sprint(data.Args[0])
		}
		q.tl.add("blobfetch:" + id)
	case strings.Contains(sql, "octet_length"):
		q.tl.add("metadatascan")
	}
	return ctx
}

func (q *queryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// eventRecorder wraps the sink's writer: it decodes each emitted JSON line,
// appends it to the timeline, runs an optional per-line hook (the
// repeatable-read test performs its concurrent UPDATE from here, mid-batch)
// and passes the bytes through to the underlying writer.
type eventRecorder struct {
	under  io.Writer
	tl     *timeline
	onLine func(map[string]any)
	line   []byte
}

func (w *eventRecorder) Write(p []byte) (int, error) {
	w.line = append(w.line, p...)
	for {
		idx := bytes.IndexByte(w.line, '\n')
		if idx < 0 {
			break
		}
		var m map[string]any
		if err := json.Unmarshal(w.line[:idx+1], &m); err == nil && m["type"] != nil {
			w.tl.add(fmt.Sprintf("event:%v:%v", m["type"], m["source_id"]))
			if w.onLine != nil {
				w.onLine(m)
			}
		}
		w.line = w.line[idx+1:]
	}
	return w.under.Write(p)
}

func tracedForensicsPool(t *testing.T, dsn string, tr pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pool parse: %v", err)
	}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedBudgetTarget builds a v2 registry + production schema and selects the
// given ids through record-scope errors, on a pool traced into tl.
func seedBudgetTarget(t *testing.T, tl *timeline, ids []int64) (*pgxpool.Pool, string, string) {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	pool := tracedForensicsPool(t, dsn, &queryTracer{tl: tl})
	runID := seedV2Forensics(t, pool)
	recordsExec(t, dsn, recordsTargetDDL)
	for _, id := range ids {
		if _, err := pool.Exec(context.Background(), `INSERT INTO magnetico_migration.errors
            (run_id, scope, source_id, stage, category, code)
            VALUES ($1::uuid, 'record', $2, 'validate', 'integrity', 'invalid_metadata')`,
			runID, id); err != nil {
			t.Fatal(err)
		}
	}
	return pool, dsn, runID
}

// driveExpandBatch runs one selection batch directly against a traced pool,
// emitting through an event-recording sink. Returns the sink's full output.
func driveExpandBatch(t *testing.T, pool *pgxpool.Pool, srcDSN string, tl *timeline,
	opts ExportOptions, ids []int64, onLine func(map[string]any),
) ([]byte, error) {
	t.Helper()
	src, err := original.Open(context.Background(), srcDSN)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer src.Close()
	f := &Forensics{
		Pool:     pool,
		Progress: &state.Progress{RunID: "00000000-0000-4000-8000-000000000000"},
		Class:    state.SchemaV2,
	}
	var buf bytes.Buffer
	sink := newJSONLSink(&eventRecorder{under: &buf, tl: tl, onLine: onLine}, opts.Limits)
	err = expandBatch(context.Background(), f, src, sink, opts, ids)
	return buf.Bytes(), err
}

// TestRecordsBlobFetchStreamsPerRecord pins the batch memory budget: exactly
// one blob-fetch query per blob-bearing record, each immediately before its
// own blob emission — record 1's blob (all chunks included) is fully emitted
// before record 2's bytes are fetched, so peak retention is a single blob
// plus one chunk regardless of batch size. The pre-review code prefetched
// every blob into a map before ANY emission; this timeline fails there.
func TestRecordsBlobFetchStreamsPerRecord(t *testing.T) {
	t.Parallel()
	tl := &timeline{}
	srcDSN := seedRecordSource(t)
	pool, _, _ := seedBudgetTarget(t, tl, []int64{1, 2})
	blob1 := encodeTestBlob(t, []codec.File{{Path: "a", Size: 10}})
	blob2 := encodeTestBlob(t, []codec.File{{Path: "b", Size: 20}})
	seedTargetRecord(t, pool, recordTarget{id: 1, name: "one", totalSize: 10, fileCount: 1, blob: blob1})
	seedTargetRecord(t, pool, recordTarget{id: 2, name: "two", totalSize: 20, fileCount: 1, blob: blob2})

	_, err := driveExpandBatch(t, pool, srcDSN, tl, ExportOptions{
		IncludeBlobs: true,
		Limits:       DefaultLimits(),
	}, []int64{1, 2}, nil)
	if err != nil {
		t.Fatalf("expandBatch error = %v", err)
	}

	if got := tl.count("blobfetch:"); got != 2 {
		t.Fatalf("%d blob-fetch queries on the timeline, want one per blob-bearing record:\n%v",
			got, tl.snapshot())
	}
	q1, q2 := tl.indexOf("blobfetch:1"), tl.indexOf("blobfetch:2")
	e1, e2 := tl.indexOf("event:target_blob:1"), tl.indexOf("event:target_blob:2")
	for _, idx := range []int{q1, q2, e1, e2} {
		if idx < 0 {
			t.Fatalf("timeline missing a required entry:\n%v", tl.snapshot())
		}
	}
	if !(q1 < e1 && e1 < q2 && q2 < e2) {
		t.Fatalf("fetch/emit interleaving broken: blobfetch1@%d blob1@%d blobfetch2@%d blob2@%d\n%v",
			q1, e1, q2, e2, tl.snapshot())
	}
	if tl.count("metadatascan") != 1 {
		t.Fatalf("metadata scan count = %d, want 1", tl.count("metadatascan"))
	}
}

// TestRecordsOverCapBlobRejectedPrefetch: under --include-blobs a blob over
// --max-blob-bytes fails the export during the metadata scan — BEFORE any
// blob byte is fetched (zero blob-fetch queries, zero blob events).
func TestRecordsOverCapBlobRejectedPrefetch(t *testing.T) {
	t.Parallel()
	tl := &timeline{}
	srcDSN := seedRecordSource(t)
	pool, _, _ := seedBudgetTarget(t, tl, []int64{1, 2})
	seedTargetRecord(t, pool, recordTarget{id: 1, name: "ok", totalSize: 10, fileCount: 1,
		blob: encodeTestBlob(t, []codec.File{{Path: "a", Size: 10}})})
	seedTargetRecord(t, pool, recordTarget{id: 2, name: "over", totalSize: 20, fileCount: 1,
		blob: bytes.Repeat([]byte{0xab}, 64<<10)}) // 64 KiB, over the 32 KiB cap

	limits := DefaultLimits()
	limits.MaxBlobBytes = 32 << 10
	_, err := driveExpandBatch(t, pool, srcDSN, tl, ExportOptions{
		IncludeBlobs: true,
		Limits:       limits,
	}, []int64{1, 2}, nil)
	if !errors.Is(err, ErrExportCapExceeded) || !strings.Contains(err.Error(), "--max-blob-bytes") {
		t.Fatalf("error = %v, want the max-blob-bytes hard error", err)
	}
	if got := tl.count("blobfetch:"); got != 0 {
		t.Fatalf("%d blob-fetch queries ran before the rejection — over-cap blobs must be "+
			"refused from the probe, not after a fetch:\n%v", got, tl.snapshot())
	}
	if tl.count("event:") != 0 {
		t.Fatalf("events emitted past the rejection:\n%v", tl.snapshot())
	}
}

// TestRecordsTargetSnapshotRepeatableRead (finding 1, isolation half): the
// batch's target transaction is REPEATABLE READ READ ONLY — a concurrent
// target commit DURING the batch's emission is invisible to the batch's
// statements. The hook updates record 2's blob bytes right after record 2's
// source_record is emitted and before its blob is fetched; the export must
// still deliver the ORIGINAL bytes (sha256 + lossless round-trip against the
// probe-time stored_bytes). Under READ COMMITTED the fetch would return the
// new bytes and the assertions would fail.
func TestRecordsTargetSnapshotRepeatableRead(t *testing.T) {
	t.Parallel()
	tl := &timeline{}
	srcDSN := seedRecordSource(t)
	pool, dsn, _ := seedBudgetTarget(t, tl, []int64{2})
	blob := encodeTestBlob(t, []codec.File{{Path: "keep", Size: 20}})
	seedTargetRecord(t, pool, recordTarget{id: 2, name: "two", totalSize: 20, fileCount: 1, blob: blob})
	replacement := bytes.Repeat([]byte{0xcd}, 8<<10) // different length AND content

	updated := false
	out, err := driveExpandBatch(t, pool, srcDSN, tl, ExportOptions{
		IncludeBlobs: true,
		Limits:       DefaultLimits(),
	}, []int64{2}, func(m map[string]any) {
		if updated || m["type"] != TypeSourceRecord || fmt.Sprint(m["source_id"]) != "2" {
			return
		}
		updated = true
		ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
		defer cancel()
		conn, cerr := pgx.Connect(ctx, dsn)
		if cerr != nil {
			t.Errorf("concurrent-update connect: %v", cerr)
			return
		}
		defer conn.Close(context.Background())
		if _, uerr := conn.Exec(ctx,
			`UPDATE magneticod.torrent_file_blobs SET data = $1 WHERE torrent_id = 2`,
			replacement); uerr != nil {
			t.Errorf("concurrent update: %v", uerr)
		}
	})
	if err != nil {
		t.Fatalf("expandBatch error = %v", err)
	}
	if !updated {
		t.Fatal("the mid-batch concurrent update never ran — the test proves nothing")
	}

	// The exported blob must be the SNAPSHOT's bytes, not the concurrent
	// commit's.
	var blobEv map[string]any
	var rebuilt []byte
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		switch fmt.Sprint(m["type"]) {
		case TypeTargetBlob:
			if fmt.Sprint(m["source_id"]) == "2" {
				blobEv = m
			}
		case TypeTargetBlobChunk:
			raw, derr := base64.StdEncoding.DecodeString(fmt.Sprint(m["data_base64"]))
			if derr != nil {
				t.Fatalf("chunk base64: %v", derr)
			}
			rebuilt = append(rebuilt, raw...)
		}
	}
	if blobEv == nil || len(rebuilt) == 0 {
		t.Fatalf("blob events missing (blob=%v chunks=%d bytes)", blobEv, len(rebuilt))
	}
	sum := sha256.Sum256(blob)
	if fmt.Sprint(blobEv["sha256"]) != hex.EncodeToString(sum[:]) {
		t.Fatalf("exported blob sha256 = %v — the concurrent commit leaked into the batch snapshot",
			blobEv["sha256"])
	}
	if !bytes.Equal(rebuilt, blob) {
		t.Fatalf("round-trip delivered %d bytes, want the original %d (concurrent commit leaked)",
			len(rebuilt), len(blob))
	}
}

// TestRecordsMaxRecordsOverrunIsCapError (finding 2): 65 selected IDs with
// --max-records=64 — the old code sliced the next page to zero elements and
// indexed take[len-1], panicking. Now: a typed cap error, no output. The
// exact-fit boundary (65 selected, cap 65) must SUCCEED across the batch
// split.
func TestRecordsMaxRecordsOverrunIsCapError(t *testing.T) {
	t.Parallel()
	srcDSN := seedRecordSource(t)
	ids := make([]int64, 65)
	for i := range ids {
		ids[i] = int64(i) + 1
	}
	_, dsn := seedRecordsTarget(t, ids)

	t.Run("one past the cap is a hard error", func(t *testing.T) {
		limits := DefaultLimits()
		limits.MaxRecords = 64
		out, err := runRecordsExport(t, dsn, srcDSN, ExportOptions{Limits: limits})
		if !errors.Is(err, ErrExportCapExceeded) || !strings.Contains(err.Error(), "--max-records") {
			t.Fatalf("error = %v, want the max-records cap (never a panic)", err)
		}
		assertNoOutput(t, filepath.Dir(out), filepath.Base(out))
	})

	t.Run("exact fit across the batch split succeeds", func(t *testing.T) {
		limits := DefaultLimits()
		limits.MaxRecords = 65
		out, err := runRecordsExport(t, dsn, srcDSN, ExportOptions{Limits: limits})
		if err != nil {
			t.Fatalf("exact-fit export error = %v — the cap must not fire at the boundary", err)
		}
		want := make(map[int64]recordExpectation, len(ids))
		for _, id := range ids {
			want[id] = recordExpectation{srcStatus: "absent", tgtStatus: "absent"}
		}
		assertRecordBlocks(t, jsonLines(t, out), want)
	})
}
