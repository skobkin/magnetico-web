package trackers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

type fakeSource struct {
	mu    sync.Mutex
	urls  []string
	err   error
	calls atomic.Int64
	gate  chan struct{} // when set, URLs blocks until it is closed
}

func (f *fakeSource) URLs(ctx context.Context) ([]string, error) {
	f.calls.Add(1)
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]string(nil), f.urls...), nil
}

func (f *fakeSource) set(urls []string, err error) {
	f.mu.Lock()
	f.urls, f.err = urls, err
	f.mu.Unlock()
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newCache(t *testing.T, src Source, ttl time.Duration) (*Cache, *clock, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	c := New(src, ttl, slog.New(slog.NewTextHandler(&logs, nil)))
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	c.now = clk.Now
	return c, clk, &logs
}

func run(t *testing.T, c *Cache) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestLoadFailsStartupWhenTheListCannotBeRead(t *testing.T) {
	src := &fakeSource{err: errors.New("db down")}
	c, _, _ := newCache(t, src, time.Minute)
	if err := c.Load(context.Background()); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("err = %v", err)
	}
	if s := c.Snapshot(); len(s.URLs) != 0 {
		t.Fatal("snapshot not empty after a failed load")
	}
	src.set([]string{"udp://a:1/announce"}, nil)
	if err := c.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.URLs(); len(got) != 1 || got[0] != "udp://a:1/announce" {
		t.Fatalf("urls = %v", got)
	}
}

func TestStaleSnapshotIsServedWhileOneBackgroundReloadRuns(t *testing.T) {
	src := &fakeSource{urls: []string{"udp://a:1/announce"}}
	c, clk, _ := newCache(t, src, time.Minute)
	if err := c.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	run(t, c)
	first := c.Snapshot()
	if src.calls.Load() != 1 {
		t.Fatalf("calls = %d", src.calls.Load())
	}
	// Fresh: no reload however often it is read.
	for i := 0; i < 50; i++ {
		c.Snapshot()
	}
	time.Sleep(30 * time.Millisecond)
	if src.calls.Load() != 1 {
		t.Fatal("a fresh snapshot triggered a reload")
	}
	// Stale: readers get the old list at once; exactly one reload happens even
	// under concurrent readers, and the new list appears afterwards.
	src.set([]string{"udp://b:1/announce", "udp://a:1/announce"}, nil)
	src.gate = make(chan struct{})
	clk.Advance(2 * time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s := c.Snapshot(); s.Version != first.Version {
				t.Error("a stale read returned a new snapshot synchronously")
			}
		}()
	}
	wg.Wait()
	waitFor(t, "reload started", func() bool { return src.calls.Load() == 2 })
	time.Sleep(30 * time.Millisecond)
	if src.calls.Load() != 2 {
		t.Fatalf("calls = %d, want a single flight", src.calls.Load())
	}
	close(src.gate)
	waitFor(t, "new snapshot", func() bool { return c.Snapshot().Version == first.Version+1 })
	if got := c.URLs(); got[0] != "udp://b:1/announce" || len(got) != 2 {
		t.Fatalf("urls = %v", got)
	}
}

func TestFailedReloadKeepsTheLastGoodSnapshotAndRetriesAtTTLCadence(t *testing.T) {
	src := &fakeSource{urls: []string{"udp://a:1/announce", "udp://b:1/announce"}}
	c, clk, logs := newCache(t, src, time.Minute)
	c.Load(context.Background())
	run(t, c)

	src.set(nil, errors.New("connection refused to secret-host"))
	clk.Advance(2 * time.Minute)
	c.Snapshot()
	waitFor(t, "failure counted", func() bool { return c.Failures.Load() == 1 })
	if got := c.URLs(); len(got) != 2 {
		t.Fatalf("the snapshot was replaced on error: %v", got)
	}
	// No hammering: reads right after the failure do not reload again.
	calls := src.calls.Load()
	for i := 0; i < 100; i++ {
		c.Snapshot()
	}
	time.Sleep(30 * time.Millisecond)
	if src.calls.Load() != calls {
		t.Fatal("reload attempted again before the TTL")
	}
	// After another TTL it retries and, now succeeding, recovers.
	src.set([]string{"udp://c:1/announce"}, nil)
	clk.Advance(2 * time.Minute)
	c.Snapshot()
	waitFor(t, "recovery", func() bool { return len(c.URLs()) == 1 && c.URLs()[0] == "udp://c:1/announce" })
	if strings.Contains(logs.String(), "secret-host") {
		t.Fatal("the log carries the driver's error text")
	}
	if !strings.Contains(logs.String(), "keeping the last good snapshot") {
		t.Fatalf("failure not logged: %s", logs.String())
	}
}

func TestSuccessfulEmptyListReplacesButAFailureNeverDoes(t *testing.T) {
	src := &fakeSource{urls: []string{"udp://a:1/announce"}}
	c, clk, _ := newCache(t, src, time.Minute)
	c.Load(context.Background())
	run(t, c)
	src.set([]string{}, nil) // the operator removed every tracker
	clk.Advance(2 * time.Minute)
	c.Snapshot()
	waitFor(t, "empty list adopted", func() bool { return len(c.URLs()) == 0 })
}

func TestInvalidateReloadsNow(t *testing.T) {
	src := &fakeSource{urls: []string{"udp://a:1/announce"}}
	c, _, _ := newCache(t, src, time.Hour)
	c.Load(context.Background())
	run(t, c)
	src.set([]string{"udp://z:1/announce"}, nil)
	c.Invalidate()
	waitFor(t, "invalidated reload", func() bool { u := c.URLs(); return len(u) == 1 && u[0] == "udp://z:1/announce" })
}

func TestSnapshotNeverBlocksOnTheSource(t *testing.T) {
	src := &fakeSource{urls: []string{"udp://a:1/announce"}}
	c, clk, _ := newCache(t, src, time.Minute)
	c.Load(context.Background())
	// No Run: nothing consumes the reload requests, yet reads stay instant.
	src.gate = make(chan struct{})
	clk.Advance(time.Hour)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			c.Snapshot()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Snapshot blocked")
	}
}

func TestReaderOrdersByPositionThenIDAndSeedIsServedVerbatim(t *testing.T) {
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	ctx := context.Background()
	if err := persistence.InitSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	if _, err := webschema.Init(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	r := NewReader(pool)
	got, err := r.URLs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 12 || got[0] != "udp://tracker.opentrackr.org:1337/announce" || got[11] != "udp://tracker.torrent.eu.org:451/announce" {
		t.Fatalf("seed = %v", got)
	}
	// Reordering is honoured: move the last one to position 1 (positions may
	// tie only through ids).
	if _, err := pool.Exec(ctx, `UPDATE magnetico_web.trackers SET position = position + 1 WHERE position >= 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE magnetico_web.trackers SET position = 1 WHERE url = 'udp://tracker.torrent.eu.org:451/announce'`); err != nil {
		t.Fatal(err)
	}
	got, _ = r.URLs(ctx)
	if got[0] != "udp://tracker.torrent.eu.org:451/announce" || got[1] != "udp://tracker.opentrackr.org:1337/announce" {
		t.Fatalf("after reorder = %v", got)
	}
}

func TestMagnetURIMatchesTheLegacyForm(t *testing.T) {
	const h = "0123456789abcdef0123456789abcdef01234567"
	trackers := []string{"udp://tracker.example1.tld:1337/announce", "http://t.example.org:80/announce"}
	name := "Some torrent name"
	if got, want := MagnetURI(h, &name, nil), "magnet:?xt=urn:btih:"+h+"&dn=Some+torrent+name"; got != want {
		t.Fatalf("with name = %s", got)
	}
	if got, want := MagnetURI(h, nil, trackers), "magnet:?xt=urn:btih:"+h+"&tr=udp%3A%2F%2Ftracker.example1.tld%3A1337%2Fannounce&tr=http%3A%2F%2Ft.example.org%3A80%2Fannounce"; got != want {
		t.Fatalf("short link = %s", got)
	}
	special := "name/with&specials"
	if got := MagnetURI(h, &special, nil); got != "magnet:?xt=urn:btih:"+h+"&dn=name%2Fwith%26specials" {
		t.Fatalf("specials = %s", got)
	}
	// Order: xt, dn, then the trackers in the given order.
	full := MagnetURI(h, &name, trackers)
	if !(strings.Index(full, "xt=") < strings.Index(full, "&dn=") && strings.Index(full, "&dn=") < strings.Index(full, "&tr=udp")) {
		t.Fatalf("parameter order: %s", full)
	}
	if strings.Contains(MagnetURI(h, nil, nil), "dn=") {
		t.Fatal("dn present without a name")
	}
	// An empty name is still a name (dn= is written), unlike nil.
	empty := ""
	if !strings.Contains(MagnetURI(h, &empty, nil), "&dn=") {
		t.Fatal("empty name dropped")
	}
}

func TestValidInfoHash(t *testing.T) {
	for in, want := range map[string]string{
		"0123456789ABCDEF0123456789abcdef01234567": "0123456789abcdef0123456789abcdef01234567",
		"0123456789abcdef0123456789abcdef01234567": "0123456789abcdef0123456789abcdef01234567",
	} {
		if got, ok := ValidInfoHash(in); !ok || got != want {
			t.Errorf("%q = %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 40), strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("a", 39) + " ", strings.Repeat("a", 38) + "%2", "０123456789abcdef0123456789abcdef0123456"} {
		if _, ok := ValidInfoHash(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if InfoHashHex([]byte{0x01, 0xab}) != "01ab" {
		t.Fatal("InfoHashHex")
	}
}
