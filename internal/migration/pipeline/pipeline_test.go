package pipeline

// Pipeline unit tests (no database): frontier ordering and gaps, the
// processed-record batch cap, oversized solo admission, the byte semaphore,
// the error taxonomy, cancellation settling and ambiguous commit surfacing.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
)

// --- fixtures ------------------------------------------------------------

func mkInfoHash(id int32) []byte {
	ih := make([]byte, 20)
	for i := range ih {
		ih[i] = byte(int32(i) + id)
	}
	return ih
}

// mkRec builds a valid record: name "t<id>", total = sum of sizes.
func mkRec(id int32, pathsSizes ...[2]int) *Record {
	rec := &Record{
		SourceID:     id,
		InfoHash:     mkInfoHash(id),
		Name:         fmt.Sprintf("t%d", id),
		DiscoveredOn: 1700000000,
	}
	for _, ps := range pathsSizes {
		rec.Files = append(rec.Files, FileEntry{
			Path:         fmt.Sprintf("%s%0*d", string(rune('a'+id%26)), ps[0]-1, 0),
			Size:         int64(ps[1]),
			SourceFileID: id,
		})
	}
	for _, f := range rec.Files {
		rec.TotalSize += f.Size
	}
	return rec
}

func mkFail(id int32, code string) *Failure {
	return &Failure{
		SourceID: int64(id),
		InfoHash: mkInfoHash(id),
		Stage:    StageRead,
		Code:     code,
	}
}

type fakeSource struct {
	chunks [][]Job
	i      int
}

func (f *fakeSource) Next(context.Context) ([]Job, bool, error) {
	if f.i >= len(f.chunks) {
		return nil, false, nil
	}
	c := f.chunks[f.i]
	f.i++
	return c, true, nil
}

type recordingCommitter struct {
	mu      sync.Mutex
	batches []*Batch
	hook    func(b *Batch)
	fail    map[int64]error // LastSourceID → injected failure
}

func (rc *recordingCommitter) Commit(_ context.Context, b *Batch) error {
	rc.mu.Lock()
	rc.batches = append(rc.batches, b)
	rc.mu.Unlock()
	if rc.hook != nil {
		rc.hook(b)
	}
	if rc.fail != nil {
		if err, has := rc.fail[b.LastSourceID]; has {
			return err
		}
	}
	return nil
}

func (rc *recordingCommitter) all() []*Batch {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]*Batch(nil), rc.batches...)
}

func testConfig(workers, batch, inflight int64) Config {
	return Config{
		Workers:          int(workers),
		BatchSize:        int(batch),
		MaxInflightBytes: inflight,
		Profile:          codec.DefaultProfile(),
		Limits:           codec.DefaultLimits(),
		VerifyDecode:     true,
		SettleTimeout:    5 * time.Second,
	}
}

// --- ordering, gaps, counters --------------------------------------------

func TestFrontierOrderingCountersAndCaps(t *testing.T) {
	t.Parallel()

	// Source IDs 1,2,3,4,5 (5 invalid → failure), 6 skipped (gap), 7..10.
	chunk := []Job{
		{Record: mkRec(1, [2]int{4, 1})},
		{Record: mkRec(2, [2]int{4, 1}, [2]int{4, 2})}, // duplicate-ish, valid
		{Record: mkRec(3, [2]int{4, 5})},
		{Failure: mkFail(4, CodeMissingFiles)},
		{Record: mkRec(5, [2]int{4, 1})},
		{Record: mkRec(7, [2]int{4, 1})},
		{Record: mkRec(8, [2]int{4, 1})},
		{Record: mkRec(9, [2]int{4, 1})},
		{Record: mkRec(10, [2]int{4, 1})},
	}
	rc := &recordingCommitter{}
	res, err := Run(context.Background(), &fakeSource{chunks: [][]Job{chunk}},
		testConfig(4, 3, 1<<20), rc)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if res.Scanned != 9 || res.Successes != 8 || res.Errors != 1 {
		t.Fatalf("result counters = %+v, want scanned 9 / success 8 / error 1", res)
	}
	if res.LastSourceID != 10 {
		t.Fatalf("LastSourceID = %d, want 10 (gap at 6 must not stall the frontier)", res.LastSourceID)
	}
	batches := rc.all()
	var processed int
	var lastID int64
	for i, b := range batches {
		if b.Processed() > 3 {
			t.Fatalf("batch %d carries %d records, cap is 3", i, b.Processed())
		}
		if b.LastSourceID <= lastID {
			t.Fatalf("batch %d checkpoint %d not advancing (previous %d)", i, b.LastSourceID, lastID)
		}
		lastID = b.LastSourceID
		processed += b.Processed()
	}
	if processed != 9 {
		t.Fatalf("committed %d records, want 9", processed)
	}
	// The failure and successes may share a batch — both count toward the
	// cap; ordering within dispatch order is preserved.
	for _, b := range batches {
		for i := 1; i < len(b.Successes); i++ {
			if b.Successes[i].Record.SourceID < b.Successes[i-1].Record.SourceID {
				t.Fatalf("batch successes out of dispatch order: %v", b.Successes)
			}
		}
	}
}

func TestInvalidConfigRejected(t *testing.T) {
	t.Parallel()

	for _, cfg := range []Config{
		{Workers: 0, BatchSize: 1, MaxInflightBytes: 1},
		{Workers: 1, BatchSize: 0, MaxInflightBytes: 1},
		{Workers: 1, BatchSize: 1, MaxInflightBytes: 0},
	} {
		cfg.Profile, cfg.Limits = codec.DefaultProfile(), codec.DefaultLimits()
		if _, err := Run(context.Background(), &fakeSource{}, cfg, &recordingCommitter{}); err == nil {
			t.Fatalf("Run(%+v) must reject invalid config", cfg)
		}
	}
}

// --- batch cap counts failures and successes alike ------------------------

func TestBatchCapCoversAllProcessedRecords(t *testing.T) {
	t.Parallel()

	// 10 records, half invalid: each batch of 3 must carry at most 3
	// processed records regardless of kind.
	chunk := make([]Job, 0, 10)
	for id := int32(1); id <= 10; id++ {
		if id%2 == 0 {
			chunk = append(chunk, Job{Failure: mkFail(id, CodeSourceIntegrity)})
		} else {
			chunk = append(chunk, Job{Record: mkRec(id, [2]int{4, 1})})
		}
	}
	rc := &recordingCommitter{}
	res, err := Run(context.Background(), &fakeSource{chunks: [][]Job{chunk}},
		testConfig(3, 3, 1<<20), rc)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Scanned != 10 || res.Successes != 5 || res.Errors != 5 {
		t.Fatalf("counters = %+v", res)
	}
	for i, b := range rc.all() {
		if b.Processed() > 3 {
			t.Fatalf("batch %d carries %d processed records, cap is 3", i, b.Processed())
		}
	}
}

// --- oversized solo admission (directive ④) --------------------------------

func TestOversizedRecordsAdmitWithoutDeadlock(t *testing.T) {
	t.Parallel()

	// Capacity 8 bytes: two records with ~100-byte paths are oversized and
	// must proceed through the solo slot while small records flow through
	// the semaphore — the run must complete, never deadlock.
	oversized1 := mkRec(100, [2]int{100, 1})
	oversized2 := mkRec(101, [2]int{100, 1})
	chunk := []Job{{Record: oversized1}, {Record: oversized2}}
	for id := int32(1); id <= 40; id++ {
		chunk = append(chunk, Job{Record: mkRec(id, [2]int{3, 1})})
	}

	done := make(chan struct{})
	var res Result
	var err error
	go func() {
		defer close(done)
		res, err = Run(context.Background(), &fakeSource{chunks: [][]Job{chunk}},
			testConfig(4, 5, 8), &recordingCommitter{})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run() deadlocked on oversized records")
	}
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Scanned != 42 || res.Successes != 42 {
		t.Fatalf("counters = %+v, want 42/42", res)
	}
}

// --- semaphore ------------------------------------------------------------

func TestWeightsAcquireReleaseAndSolo(t *testing.T) {
	t.Parallel()

	w := newWeights(16)

	_, rel1, err := w.acquire(context.Background(), 12)
	if err != nil {
		t.Fatalf("acquire(12) error = %v", err)
	}

	// 8 does not fit while 12 is held: the acquire blocks until cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err = w.acquire(ctx, 8)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked acquire error = %v, want deadline exceeded", err)
	}

	// An oversized request bypasses the full counter via the solo slot.
	solo, relSolo, err := w.acquire(context.Background(), 100)
	if err != nil || !solo {
		t.Fatalf("solo acquire = (%v, %v), want solo=true, nil error", solo, err)
	}
	// Only one solo at a time.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, _, err := w.acquire(ctx2, 100); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second solo acquire error = %v, want deadline exceeded", err)
	}
	relSolo()

	// After releasing the 12, the 8 fits.
	rel1()
	_, rel8, err := w.acquire(context.Background(), 8)
	if err != nil {
		t.Fatalf("acquire(8) after release error = %v", err)
	}
	rel8()

	// Zero-weight acquire always succeeds.
	_, rel0, err := w.acquire(context.Background(), 0)
	if err != nil {
		t.Fatalf("acquire(0) error = %v", err)
	}
	rel0()
}

// --- error taxonomy ---------------------------------------------------------

func TestProcessorErrorTaxonomy(t *testing.T) {
	t.Parallel()

	p := newProcessor(codec.DefaultLimits(), codec.DefaultProfile(), 2, true)

	// Sum mismatch → invalid_metadata at the validate stage.
	bad := mkRec(1, [2]int{4, 5})
	bad.TotalSize = 99
	_, fail := p.process(bad)
	if fail == nil || fail.Stage != StageValidate || fail.Code != CodeInvalidMetadata {
		t.Fatalf("sum mismatch failure = %+v", fail)
	}
	if _, has := fail.Details["reason"]; !has {
		t.Fatalf("invalid_metadata details must carry a reason: %+v", fail)
	}

	// Aggregate path bytes over a small limit → codec_limit with the
	// structured limit detail.
	small := codec.DefaultLimits()
	small.MaxAggregatePathBytes = 16
	pSmall := newProcessor(small, codec.DefaultProfile(), 1, true)
	big := mkRec(2, [2]int{10, 1}, [2]int{10, 1})
	_, fail = pSmall.process(big)
	if fail == nil || fail.Stage != StageEncode || fail.Code != CodeCodecLimit {
		t.Fatalf("aggregate-path failure = %+v", fail)
	}
	if fail.Details["limit"] != codec.LimitAggregatePathBytes {
		t.Fatalf("codec_limit details = %+v, want limit %q", fail.Details, codec.LimitAggregatePathBytes)
	}

	// Typical single file → no blob; atypical single file → blob.
	typical := &Record{
		SourceID:     3,
		InfoHash:     mkInfoHash(3),
		Name:         "t3",
		TotalSize:    7,
		DiscoveredOn: 1700000000,
		Files:        []FileEntry{{Path: "t3", Size: 7}},
	}
	success, fail := p.process(typical)
	if fail != nil || success == nil || success.Blob != nil {
		t.Fatalf("typical single = (%+v, %+v), want success without blob", success, fail)
	}
	atypical := &Record{
		SourceID:     4,
		InfoHash:     mkInfoHash(4),
		Name:         "other",
		TotalSize:    7,
		DiscoveredOn: 1700000000,
		Files:        []FileEntry{{Path: "f", Size: 7}},
	}
	success, fail = p.process(atypical)
	if fail != nil || success == nil || success.Blob == nil {
		t.Fatalf("atypical single = (%+v, %+v), want success with blob", success, fail)
	}
}

func TestEqualCanonicalWithDuplicates(t *testing.T) {
	t.Parallel()

	a := codec.File{Path: "a", Size: 1}
	b := codec.File{Path: "b", Size: 2}
	// decoded is canonically sorted (codec.Decode contract); source order
	// is arbitrary until the internal sort.
	if !equalCanonical([]codec.File{a, b}, []codec.File{b, a}) {
		t.Fatal("equal sets in any source order must compare equal")
	}
	if equalCanonical([]codec.File{a, a}, []codec.File{a}) {
		t.Fatal("different lengths must not compare equal")
	}
	if equalCanonical([]codec.File{a, a}, []codec.File{a, b}) {
		t.Fatal("duplicate vs distinct must not compare equal")
	}
	if !equalCanonical([]codec.File{a, a}, []codec.File{a, a}) {
		t.Fatal("identical duplicates must compare equal")
	}
}

// --- cancellation and ambiguous commits -------------------------------------

func TestCancelSettlesContiguousPrefix(t *testing.T) {
	t.Parallel()

	chunk := make([]Job, 0, 100)
	for id := int32(1); id <= 100; id++ {
		chunk = append(chunk, Job{Record: mkRec(id, [2]int{4, 1})})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rc := &recordingCommitter{}
	rc.hook = func(b *Batch) {
		if b.LastSourceID >= 10 {
			cancel() // cancel after the first full batch committed
		}
	}

	res, err := Run(ctx, &fakeSource{chunks: [][]Job{chunk}}, testConfig(4, 10, 1<<20), rc)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Run() error = %v, want ErrInterrupted", err)
	}
	batches := rc.all()
	if len(batches) == 0 {
		t.Fatal("interrupted run must settle at least the completed first batch")
	}
	var lastID int64
	committed := 0
	for i, b := range batches {
		if b.Processed() > 10 {
			t.Fatalf("batch %d over the cap", i)
		}
		if b.LastSourceID <= lastID {
			t.Fatalf("checkpoints not monotonic: %d after %d", b.LastSourceID, lastID)
		}
		lastID = b.LastSourceID
		committed += b.Processed()
	}
	if res.LastSourceID != lastID || res.Scanned != int64(committed) {
		t.Fatalf("result = %+v, but committed %d records through %d", res, committed, lastID)
	}
	if committed >= 100 {
		t.Fatalf("run committed everything (%d) despite cancellation", committed)
	}
}

func TestCommitFailureSurfacesCommitError(t *testing.T) {
	t.Parallel()

	chunk := make([]Job, 0, 20)
	for id := int32(1); id <= 20; id++ {
		chunk = append(chunk, Job{Record: mkRec(id, [2]int{4, 1})})
	}
	// Batch sizes are scheduling-dependent, so fail EVERY checkpoint: the
	// first attempted commit must surface as a CommitError and stop the run.
	fail := map[int64]error{}
	for id := int64(1); id <= 20; id++ {
		fail[id] = errors.New("connection reset while committing")
	}
	rc := &recordingCommitter{fail: fail}
	_, err := Run(context.Background(), &fakeSource{chunks: [][]Job{chunk}},
		testConfig(2, 10, 1<<20), rc)
	var ce *CommitError
	if !errors.As(err, &ce) {
		t.Fatalf("Run() error = %v, want CommitError", err)
	}
	if ce.LastSourceID < 1 || ce.LastSourceID > 20 || ce.Processed < 1 {
		t.Fatalf("CommitError = %+v, out of range", ce)
	}
	if len(rc.all()) != 1 {
		t.Fatalf("a failed first commit must stop the run, attempted %d batches", len(rc.all()))
	}
	if !errors.Is(err, ce.Err) {
		t.Fatalf("CommitError must unwrap to the cause, got %v", err)
	}
}

func TestSourceReadFailureIsFatalAfterSettling(t *testing.T) {
	t.Parallel()

	good := []Job{{Record: mkRec(1, [2]int{4, 1})}}
	src := &failingSource{chunks: [][]Job{good}}
	rc := &recordingCommitter{}
	res, err := Run(context.Background(), src, testConfig(2, 10, 1<<20), rc)
	if err == nil || errors.Is(err, ErrInterrupted) {
		t.Fatalf("Run() error = %v, want a fatal read error", err)
	}
	if res.Scanned != 1 {
		t.Fatalf("settled prefix must stay committed: %+v", res)
	}
	if len(rc.all()) != 1 {
		t.Fatalf("expected exactly one committed batch, got %d", len(rc.all()))
	}
}

type failingSource struct {
	chunks [][]Job
	i      int
}

func (f *failingSource) Next(context.Context) ([]Job, bool, error) {
	if f.i >= len(f.chunks) {
		return nil, false, errors.New("source connection lost")
	}
	c := f.chunks[f.i]
	f.i++
	return c, true, nil
}
