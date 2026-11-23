package filecache

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/filetree"
	"github.com/skobkin/magnetico-web/internal/web/gate"
	"github.com/skobkin/magnetico-web/persistence"
)

// ---- fake source ---------------------------------------------------------------

type stored struct {
	t       persistence.Torrent
	blob    []byte
	hasBlob bool
	ver     string
}

type fakeSource struct {
	mu     sync.Mutex
	rows   map[int32]*stored
	limits codec.Limits

	// Optional hooks, called without the lock held.
	onHeader func(ctx context.Context, id int32) error
	onBlob   func(ctx context.Context, id int32, ver string) error

	identities, headers, blobs atomic.Int64
}

func newFake() *fakeSource {
	return &fakeSource{rows: map[int32]*stored{}, limits: codec.DefaultLimits()}
}

func mkFiles(prefix string, n int) []codec.File {
	fs := make([]codec.File, n)
	for i := range fs {
		fs[i] = codec.File{Path: fmt.Sprintf("%s/d%02d/f%04d", prefix, i%7, i), Size: int64(i + 1)}
	}
	return fs
}

// put stores a torrent whose list is files, at version ver.
func (f *fakeSource) put(t *testing.T, id int32, files []codec.File, ver string) {
	t.Helper()
	blob, _, err := codec.Encode(files, codec.ProfileCompact, f.limits)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, x := range files {
		sum += x.Size
	}
	f.mu.Lock()
	f.rows[id] = &stored{
		t:    persistence.Torrent{ID: id, InfoHash: make([]byte, 20), Name: fmt.Sprintf("t%d", id), TotalSize: sum, FileCount: int32(len(files))},
		blob: blob, hasBlob: true, ver: ver,
	}
	f.mu.Unlock()
}

func (f *fakeSource) Limits() codec.Limits { return f.limits }

func (f *fakeSource) FileIdentity(ctx context.Context, id int32) (persistence.FileIdentity, error) {
	f.identities.Add(1)
	if err := ctx.Err(); err != nil {
		return persistence.FileIdentity{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.rows[id]
	if s == nil {
		return persistence.FileIdentity{}, persistence.ErrTorrentNotFound
	}
	return persistence.FileIdentity{Torrent: s.t, HasBlob: s.hasBlob, Version: s.ver}, nil
}

func (f *fakeSource) FileHeader(ctx context.Context, id int32) ([]byte, int, error) {
	f.headers.Add(1)
	if f.onHeader != nil {
		if err := f.onHeader(ctx, id); err != nil {
			return nil, 0, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.rows[id]
	if s == nil || !s.hasBlob {
		return nil, 0, persistence.ErrIntegrityMissingBlob
	}
	return s.blob[:min(len(s.blob), codec.PeekHeaderLen)], len(s.blob), nil
}

func (f *fakeSource) FileBlob(ctx context.Context, id int32, ver string) ([]byte, error) {
	f.blobs.Add(1)
	if f.onBlob != nil {
		if err := f.onBlob(ctx, id, ver); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.rows[id]
	if s == nil || !s.hasBlob || s.ver != ver {
		return nil, persistence.ErrBlobChanged
	}
	return append([]byte(nil), s.blob...), nil
}

func newCache(t *testing.T, src *fakeSource, cfg Config) *Cache {
	t.Helper()
	c, err := New(src, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return c
}

func bg() context.Context { return context.Background() }

// wait polls cond for up to 5 s (a deterministic condition, not a delay).
func wait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 5000; i++ {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---- tests --------------------------------------------------------------------

func TestMaxWeightFitsDefaultCapacity(t *testing.T) {
	l := codec.DefaultLimits()
	w := MaxWeight(l)
	if w > DecodeCapacity {
		t.Fatalf("MaxWeight(default limits) = %d > DecodeCapacity %d", w, DecodeCapacity)
	}
	t.Logf("MaxWeight(default limits) = %d bytes = %.1f MiB of %d MiB", w, float64(w)/(1<<20), DecodeCapacity>>20)
	// Every component at its limit: 32 MiB stored + 64 MiB declared + 64 MiB
	// paths + 60 B × 250,000 files.
	want := int64(32<<20) + 64<<20 + 64<<20 + 60*250_000
	if w != want {
		t.Fatalf("MaxWeight = %d, want %d", w, want)
	}
	if _, err := New(newFake(), Config{DecodeCapacity: 100 << 20}); err == nil {
		t.Fatal("New must reject a decode capacity below the largest admissible weight")
	}
}

func TestHundredNavigationsOneDecode(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 300), "v1")
	c := newCache(t, src, Config{})
	var first filetree.Index
	for i := 0; i < 100; i++ {
		r, err := c.Get(bg(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if r.Hit {
				t.Fatal("first request cannot be a hit")
			}
			first = r.Index
		} else if !r.Hit || r.Index != first {
			t.Fatalf("request %d: hit=%v, same index=%v", i, r.Hit, r.Index == first)
		}
	}
	s := c.Stats()
	if s.Builds != 1 || s.Hits != 99 || s.Misses != 1 || s.Entries != 1 {
		t.Fatalf("stats %+v", s)
	}
	if src.blobs.Load() != 1 || src.headers.Load() != 1 {
		t.Fatalf("blob reads %d, header reads %d, want 1 and 1", src.blobs.Load(), src.headers.Load())
	}
	if src.identities.Load() != 100 {
		t.Fatalf("identity revalidations = %d, want 100 (one per request)", src.identities.Load())
	}
}

func TestSimultaneousRequestsSameTorrentOneBuild(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 300), "v1")
	release := make(chan struct{})
	var inBlob atomic.Int64
	src.onBlob = func(ctx context.Context, _ int32, _ string) error {
		inBlob.Add(1)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := newCache(t, src, Config{})
	const n = 24
	results := make(chan Result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.Get(bg(), 1)
			if err != nil {
				t.Error(err)
				return
			}
			results <- r
		}()
	}
	wait(t, "all requests to wait on the build", func() bool { s := c.Stats(); return s.Misses == n && inBlob.Load() == 1 })
	close(release)
	wg.Wait()
	close(results)
	var idx filetree.Index
	for r := range results {
		if idx == nil {
			idx = r.Index
		}
		if r.Index != idx {
			t.Fatal("waiters received different indexes")
		}
	}
	s := c.Stats()
	if s.Builds != 1 || s.Coalesced != n-1 {
		t.Fatalf("builds %d coalesced %d, want 1 and %d", s.Builds, s.Coalesced, n-1)
	}
	if src.blobs.Load() != 1 {
		t.Fatalf("blob fetched %d times", src.blobs.Load())
	}
}

func TestDifferentTorrentsBuildIndependently(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 50), "v1")
	src.put(t, 2, mkFiles("b", 50), "v1")
	var inBlob atomic.Int64
	both := make(chan struct{})
	src.onBlob = func(ctx context.Context, _ int32, _ string) error {
		if inBlob.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both: // both builds are in the blob phase at once
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := newCache(t, src, Config{})
	var wg sync.WaitGroup
	for _, id := range []int32{1, 2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(bg(), id); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if s := c.Stats(); s.Builds != 2 || s.Entries != 2 {
		t.Fatalf("stats %+v", s)
	}
}

func TestAllWaitersCancelBeforeDecodeCancelsTheWork(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 50), "v1")
	entered := make(chan struct{})
	var sawCancel atomic.Bool
	src.onBlob = func(ctx context.Context, _ int32, _ string) error {
		close(entered)
		<-ctx.Done()
		sawCancel.Store(true)
		return ctx.Err()
	}
	c := newCache(t, src, Config{})

	ctx1, cancel1 := context.WithCancel(bg())
	ctx2, cancel2 := context.WithCancel(bg())
	errs := make(chan error, 2)
	go func() { _, err := c.Get(ctx1, 1); errs <- err }()
	<-entered
	go func() { _, err := c.Get(ctx2, 1); errs <- err }()
	wait(t, "second waiter to join", func() bool { return c.Stats().Coalesced == 1 })

	cancel1() // one waiter leaves: the work must continue for the other
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if sawCancel.Load() {
		t.Fatal("the build was cancelled while a waiter remained")
	}
	cancel2() // the last waiter leaves
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("second waiter: %v", err)
	}
	wait(t, "the abandoned build to be cancelled", func() bool { return sawCancel.Load() && c.Stats().Flights == 0 && c.Gate().InUse() == 0 })
	if s := c.Stats(); s.Builds != 0 || s.Entries != 0 {
		t.Fatalf("an abandoned build must not decode or cache: %+v", s)
	}
	if c.Gate().InUse() != 0 {
		t.Fatalf("gate still holds %d", c.Gate().InUse())
	}

	// A new request after the abandonment starts a fresh build that succeeds.
	src.onBlob = nil
	if _, err := c.Get(bg(), 1); err != nil {
		t.Fatal(err)
	}
	if s := c.Stats(); s.Builds != 1 || s.Entries != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestDecodeCompletesAfterRequesterLeaves(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 200), "v1")
	c := newCache(t, src, Config{})
	atDecode := make(chan struct{})
	proceed := make(chan struct{})
	c.onDecode = func() { close(atDecode); <-proceed }

	ctx, cancel := context.WithCancel(bg())
	errc := make(chan error, 1)
	go func() { _, err := c.Get(ctx, 1); errc <- err }()
	<-atDecode // the decode is committed
	cancel()   // the only requester disconnects
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("requester: %v", err)
	}
	close(proceed)
	wait(t, "the committed build to finish and cache", func() bool { s := c.Stats(); return s.Entries == 1 && s.Flights == 0 })
	if s := c.Stats(); s.Builds != 1 || s.BuildErrors != 0 {
		t.Fatalf("stats %+v", s)
	}
	r, err := c.Get(bg(), 1)
	if err != nil || !r.Hit {
		t.Fatalf("the finished decode must serve the next request: hit=%v err=%v", r.Hit, err)
	}
	if c.Stats().Builds != 1 || c.Gate().InUse() != 0 {
		t.Fatal("no second decode, gate released")
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 20), "v1")
	boom := errors.New("storage hiccup")
	var fail atomic.Bool
	fail.Store(true)
	src.onBlob = func(context.Context, int32, string) error {
		if fail.Load() {
			return boom
		}
		return nil
	}
	c := newCache(t, src, Config{})
	if _, err := c.Get(bg(), 1); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if s := c.Stats(); s.Entries != 0 || s.BuildErrors != 1 {
		t.Fatalf("stats %+v", s)
	}
	fail.Store(false)
	if r, err := c.Get(bg(), 1); err != nil || r.Hit {
		t.Fatalf("after the failure a fresh build must run: hit=%v err=%v", r.Hit, err)
	}
	if c.Gate().InUse() != 0 {
		t.Fatal("gate leaked")
	}
}

func TestBlobChangedDuringFetchRetriesAndNeverServesTheOldVersion(t *testing.T) {
	src := newFake()
	old, newer := mkFiles("old", 30), mkFiles("new", 40)
	src.put(t, 1, old, "v1")
	var swapped atomic.Bool
	src.onBlob = func(_ context.Context, id int32, _ string) error {
		if swapped.CompareAndSwap(false, true) {
			src.put(t, id, newer, "v2") // replaced between identity and blob fetch
		}
		return nil
	}
	c := newCache(t, src, Config{})
	r, err := c.Get(bg(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Index.Files() != len(newer) {
		t.Fatalf("served %d files, want the new version's %d", r.Index.Files(), len(newer))
	}
	if r.Torrent.FileCount != int32(len(newer)) {
		t.Fatalf("metadata belongs to another version: %+v", r.Torrent)
	}
	if s := c.Stats(); s.Retries != 1 || s.Builds != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestBlobChurnIsBounded(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 10), "v1")
	var n atomic.Int64
	src.onBlob = func(_ context.Context, id int32, _ string) error {
		src.put(t, id, mkFiles("a", 10), fmt.Sprintf("v%d", n.Add(1)+1)) // always changes
		return nil
	}
	c := newCache(t, src, Config{})
	_, err := c.Get(bg(), 1)
	if !errors.Is(err, persistence.ErrBlobChanged) {
		t.Fatalf("got %v", err)
	}
	if got := src.blobs.Load(); got != maxAttempts {
		t.Fatalf("blob fetched %d times, want exactly %d (bounded retry)", got, maxAttempts)
	}
	if c.Stats().Entries != 0 {
		t.Fatal("nothing may be cached")
	}
}

func TestLimitAndCorruptionAreNotRetried(t *testing.T) {
	for name, hook := range map[string]func(*fakeSource){
		"oversized-blob": func(s *fakeSource) {
			s.onBlob = func(context.Context, int32, string) error {
				return fmt.Errorf("%w: %w", persistence.ErrCorruptBlob, &codec.LimitError{Limit: codec.LimitStoredBytes, Actual: 2, Allowed: 1})
			}
		},
		"count-mismatch": func(s *fakeSource) {
			s.mu.Lock()
			s.rows[1].t.FileCount++ // the row disagrees with the blob
			s.mu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			src := newFake()
			src.put(t, 1, mkFiles("a", 10), "v1")
			hook(src)
			c := newCache(t, src, Config{})
			_, err := c.Get(bg(), 1)
			if !errors.Is(err, persistence.ErrCorruptBlob) {
				t.Fatalf("got %v", err)
			}
			if s := c.Stats(); s.Retries != 0 || s.Entries != 0 {
				t.Fatalf("stats %+v", s)
			}
			if src.identities.Load() != 1 {
				t.Fatalf("identity looked up %d times; non-retryable errors must not retry", src.identities.Load())
			}
		})
	}
}

func TestMissingBlobAndSingleFile(t *testing.T) {
	src := newFake()
	src.mu.Lock()
	src.rows[1] = &stored{t: persistence.Torrent{ID: 1, Name: "solo.iso", TotalSize: 7, FileCount: 1}}
	src.rows[2] = &stored{t: persistence.Torrent{ID: 2, Name: "multi", TotalSize: 7, FileCount: 3}}
	src.mu.Unlock()
	c := newCache(t, src, Config{})
	r, err := c.Get(bg(), 1)
	if err != nil || !r.Single || r.Index.Files() != 1 {
		t.Fatalf("single-file torrent: %+v %v", r, err)
	}
	if c.Stats().Entries != 0 || src.headers.Load() != 0 {
		t.Fatal("a single-file torrent is neither cached nor decoded")
	}
	if _, err := c.Get(bg(), 2); !errors.Is(err, persistence.ErrIntegrityMissingBlob) {
		t.Fatalf("multi-file torrent without a blob: %v", err)
	}
	if _, err := c.Get(bg(), 3); !errors.Is(err, persistence.ErrTorrentNotFound) {
		t.Fatalf("unknown torrent: %v", err)
	}
}

func TestReplacementOnIdentityChange(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 10), "v1")
	c := newCache(t, src, Config{})
	if _, err := c.Get(bg(), 1); err != nil {
		t.Fatal(err)
	}
	src.put(t, 1, mkFiles("b", 25), "v2")
	r, err := c.Get(bg(), 1)
	if err != nil || r.Hit || r.Index.Files() != 25 {
		t.Fatalf("replacement: hit=%v files=%d err=%v", r.Hit, r.Index.Files(), err)
	}
	if s := c.Stats(); s.Replaced != 1 || s.Builds != 2 || s.Entries != 1 {
		t.Fatalf("stats %+v", s)
	}
	// Same blob version, changed row metadata (count): also a different identity.
	src.mu.Lock()
	src.rows[1].t.TotalSize++
	src.mu.Unlock()
	if _, err := c.Get(bg(), 1); err == nil {
		t.Fatal("a row whose total no longer matches the blob must not be served from a stale entry")
	}
}

func TestExpiration(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 10), "v1")
	now := time.Unix(1_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c := newCache(t, src, Config{Now: clock, MaxAge: 10 * time.Minute})
	if _, err := c.Get(bg(), 1); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(10 * time.Minute) // exactly the age: still fresh
	mu.Unlock()
	if r, _ := c.Get(bg(), 1); !r.Hit {
		t.Fatal("an entry exactly at MaxAge is still valid")
	}
	mu.Lock()
	now = now.Add(time.Second)
	mu.Unlock()
	r, err := c.Get(bg(), 1)
	if err != nil || r.Hit {
		t.Fatalf("expired entry must rebuild: hit=%v err=%v", r.Hit, err)
	}
	if s := c.Stats(); s.Expired != 1 || s.Builds != 2 {
		t.Fatalf("stats %+v", s)
	}
}

func TestLRUEvictionByBytesReleasesTheIndex(t *testing.T) {
	src := newFake()
	for id := int32(1); id <= 3; id++ {
		src.put(t, id, mkFiles(fmt.Sprintf("p%d", id), 400), "v1")
	}
	probe := newCache(t, src, Config{})
	r, err := probe.Get(bg(), 1)
	if err != nil {
		t.Fatal(err)
	}
	cost := int64(r.Index.Bytes()) + entryOverhead

	c := newCache(t, src, Config{Budget: 2*cost + cost/2}) // room for two entries
	var wp weak.Pointer[filetree.Arena]
	for _, id := range []int32{1, 2} {
		r, err := c.Get(bg(), id)
		if err != nil {
			t.Fatal(err)
		}
		if id == 1 {
			wp = weak.Make(r.Index.(*filetree.Arena))
		}
	}
	if _, err := c.Get(bg(), 1); err != nil { // touch 1: 2 is now least recently used
		t.Fatal(err)
	}
	if _, err := c.Get(bg(), 3); err != nil {
		t.Fatal(err)
	}
	s := c.Stats()
	if s.Entries != 2 || s.Evictions != 1 || s.Bytes > 2*cost+cost/2 {
		t.Fatalf("stats %+v", s)
	}
	if r, _ := c.Get(bg(), 2); r.Hit {
		t.Fatal("torrent 2 should have been evicted (least recently used)")
	}
	if r, _ := c.Get(bg(), 3); !r.Hit {
		t.Fatal("torrent 3 should still be cached")
	}
	_ = wp
}

func TestEvictedIndexIsReleased(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 400), "v1")
	src.put(t, 2, mkFiles("b", 400), "v1")
	probe := newCache(t, src, Config{})
	r, err := probe.Get(bg(), 1)
	if err != nil {
		t.Fatal(err)
	}
	cost := int64(r.Index.Bytes()) + entryOverhead

	c := newCache(t, src, Config{Budget: cost + cost/2}) // one entry
	var wp weak.Pointer[filetree.Arena]
	func() {
		r, err := c.Get(bg(), 1)
		if err != nil {
			t.Fatal(err)
		}
		wp = weak.Make(r.Index.(*filetree.Arena))
	}()
	if _, err := c.Get(bg(), 2); err != nil { // evicts 1
		t.Fatal(err)
	}
	for i := 0; i < 5 && wp.Value() != nil; i++ {
		runtime.GC()
	}
	if wp.Value() != nil {
		t.Fatal("the evicted index is still reachable: the cache keeps a reference")
	}
}

func TestOversizeEntryServedButNotRetained(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 400), "v1")
	c := newCache(t, src, Config{Budget: 1024}) // smaller than any index
	for i := 0; i < 2; i++ {
		r, err := c.Get(bg(), 1)
		if err != nil || r.Index.Files() != 400 {
			t.Fatalf("oversize entry must still be served: %v", err)
		}
	}
	if s := c.Stats(); s.Entries != 0 || s.Bytes != 0 || s.Oversize != 2 || s.Builds != 2 {
		t.Fatalf("stats %+v: repeated decodes of an oversize entry must be explicit", s)
	}
}

func TestDecodeGateSaturationIsBusyNotAnError500(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 100), "v1")
	src.put(t, 2, mkFiles("b", 100), "v1")
	hdr, n, _ := src.FileHeader(bg(), 1)
	_, declared, err := codec.PeekDecodedSize(hdr, n, src.limits)
	if err != nil {
		t.Fatal(err)
	}
	w := Weight(src.limits, n, declared, 100)
	// The decode capacity must exceed the default-limits MaxWeight (New checks),
	// so saturate it with a held heavy permit instead.
	c := newCache(t, src, Config{QueueWait: 50 * time.Millisecond, MaxWaiters: 1})
	heavy, err := c.Gate().Acquire(bg(), c.Gate().Capacity()-w+1) // leaves w-1 bytes
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.Get(bg(), 1); !errors.Is(err, gate.ErrBusy) {
		t.Fatalf("got %v, want gate.ErrBusy", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("the queue wait is not bounded")
	}
	if src.blobs.Load() != 0 {
		t.Fatal("admission must precede the whole-blob read")
	}
	heavy()
	if _, err := c.Get(bg(), 1); err != nil {
		t.Fatalf("after the gate drained: %v", err)
	}
	if c.Gate().InUse() != 0 {
		t.Fatal("gate leaked")
	}
}

func TestNoGateHeldWhileWaitingForConnectionFreeWork(t *testing.T) {
	// The identity and header statements run before the gate; a build blocked
	// in the header phase holds no permit.
	src := newFake()
	src.put(t, 1, mkFiles("a", 10), "v1")
	entered := make(chan struct{})
	src.onHeader = func(ctx context.Context, _ int32) error { close(entered); <-ctx.Done(); return ctx.Err() }
	c := newCache(t, src, Config{})
	ctx, cancel := context.WithCancel(bg())
	done := make(chan struct{})
	go func() { _, _ = c.Get(ctx, 1); close(done) }()
	<-entered
	if c.Gate().InUse() != 0 {
		t.Fatal("a build holds decode capacity before admission")
	}
	cancel()
	<-done
}

func TestBuildPanicReleasesEverything(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 10), "v1")
	var boom atomic.Bool
	boom.Store(true)
	src.onBlob = func(context.Context, int32, string) error {
		if boom.Load() {
			panic("synthetic")
		}
		return nil
	}
	c := newCache(t, src, Config{})
	if _, err := c.Get(bg(), 1); !errors.Is(err, ErrBuildPanic) {
		t.Fatalf("got %v", err)
	}
	if c.Gate().InUse() != 0 || c.Stats().Flights != 0 {
		t.Fatalf("panic leaked: gate %d flights %d", c.Gate().InUse(), c.Stats().Flights)
	}
	boom.Store(false)
	if _, err := c.Get(bg(), 1); err != nil {
		t.Fatalf("cache unusable after a recovered panic: %v", err)
	}
}

func TestCloseCancelsDatabasePhasesAndWaits(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 10), "v1")
	entered := make(chan struct{})
	var touchedAfterClose atomic.Bool
	var closed atomic.Bool
	src.onBlob = func(ctx context.Context, _ int32, _ string) error {
		close(entered)
		<-ctx.Done()
		if closed.Load() {
			touchedAfterClose.Store(true) // would be a use after shutdown
		}
		return ctx.Err()
	}
	c, err := New(src, Config{})
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { _, err := c.Get(bg(), 1); errc <- err }()
	<-entered
	cctx, cancel := context.WithTimeout(bg(), 5*time.Second)
	defer cancel()
	if err := c.Close(cctx); err != nil {
		t.Fatal(err)
	}
	closed.Store(true)
	if err := <-errc; err == nil {
		t.Fatal("the waiter must fail when the cache closes")
	}
	if touchedAfterClose.Load() {
		t.Fatal("a build touched the source after Close returned")
	}
	if _, err := c.Get(bg(), 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close: %v", err)
	}
	if c.Gate().InUse() != 0 {
		t.Fatal("gate leaked on close")
	}
}

func TestCloseTimesOutOnADecodeButDoesNotTouchTheSource(t *testing.T) {
	src := newFake()
	src.put(t, 1, mkFiles("a", 10), "v1")
	c, _ := New(src, Config{})
	atDecode := make(chan struct{})
	proceed := make(chan struct{})
	c.onDecode = func() { close(atDecode); <-proceed }
	go func() { _, _ = c.Get(bg(), 1) }()
	<-atDecode
	cctx, cancel := context.WithTimeout(bg(), 30*time.Millisecond)
	defer cancel()
	if err := c.Close(cctx); err == nil {
		t.Fatal("Close must report builds still running")
	}
	blobs := src.blobs.Load()
	close(proceed)
	wait(t, "the decode to finish", func() bool { return c.Gate().InUse() == 0 })
	if src.blobs.Load() != blobs || src.headers.Load() > 1 {
		t.Fatal("source used after Close")
	}
	if c.Stats().Entries != 0 {
		t.Fatal("a closed cache must not retain results")
	}
}

func TestConcurrentStress(t *testing.T) {
	src := newFake()
	const torrents = 8
	for id := int32(1); id <= torrents; id++ {
		src.put(t, id, mkFiles(fmt.Sprintf("t%d", id), 200+int(id)), "v1")
	}
	probe := newCache(t, src, Config{})
	r, _ := probe.Get(bg(), 1)
	cost := int64(r.Index.Bytes()) + entryOverhead
	c := newCache(t, src, Config{Budget: 3 * cost}) // forces churn
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				id := int32((g+i)%torrents + 1)
				ctx, cancel := context.WithCancel(bg())
				if (g+i)%5 == 0 {
					cancel() // some requests give up immediately
				}
				res, err := c.Get(ctx, id)
				cancel()
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
					continue
				}
				if res.Torrent.ID != id || res.Index.Files() != 200+int(id) {
					t.Errorf("torrent %d got index of %d files", id, res.Index.Files())
				}
			}
		}()
	}
	wg.Wait()
	wait(t, "flights to drain", func() bool { return c.Stats().Flights == 0 })
	if c.Gate().InUse() != 0 || c.Gate().Waiting() != 0 {
		t.Fatalf("gate in use %d waiting %d", c.Gate().InUse(), c.Gate().Waiting())
	}
	if s := c.Stats(); s.Bytes > 3*cost {
		t.Fatalf("budget exceeded: %+v", s)
	}
}
