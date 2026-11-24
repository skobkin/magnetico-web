package crawler

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/crawlqueue"
	"github.com/skobkin/magnetico-web/internal/ingest/crawlerauth"
	"github.com/skobkin/magnetico-web/internal/ingest/existence"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/ingest/record"
	"github.com/skobkin/magnetico-web/internal/ingest/server"
	"github.com/skobkin/magnetico-web/persistence"
)

const token = "0123456789abcdef0123456789abcdef-test-token"

var (
	bg     = context.Background()
	limits = codec.DefaultLimits()
)

func hashN(n int) [20]byte {
	var h [20]byte
	h[0], h[1], h[19] = byte(n>>8), byte(n), 0x7E
	return h
}

func goodPayload(t testing.TB, n int) []byte {
	t.Helper()
	b, err := record.Encode(record.Record{
		InfoHash: hashN(n), Name: fmt.Sprintf("torrent-%d", n), TotalSize: 5, DiscoveredOn: 1700000000,
		Files: []persistence.File{{Path: "f", Size: 5}},
	}, limits)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func badPayload(n int) []byte { // strictly invalid: rejected as invalid_record
	return []byte(fmt.Sprintf(`{"v":1,"info_hash":"%s"}`, hex.EncodeToString(func() []byte { h := hashN(n); return h[:] }())))
}

func unsupportedPayload(t testing.TB, n int) []byte {
	return []byte(strings.Replace(string(goodPayload(t, n)), `"v":1`, `"v":2`, 1))
}

// fakeIngest is a real ingest HTTP handler (internal/ingest/server) over an
// in-memory admitter, so the crawler is tested against the true contract.
type fakeIngest struct {
	mu       sync.Mutex
	stored   map[[20]byte]bool
	requests []int // records per submit request
}

func (f *fakeIngest) Admit(_ context.Context, items []pgqueue.Item) (pgqueue.AdmitResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, len(items))
	for _, it := range items {
		f.stored[it.InfoHash] = true
	}
	return pgqueue.AdmitResult{Inserted: len(items)}, nil
}

func (f *fakeIngest) has(n int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stored[hashN(n)]
}

func (f *fakeIngest) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stored)
}

type rig struct {
	q      *crawlqueue.Queue
	dir    string
	ingest *fakeIngest
	ts     *httptest.Server
	client *Client
	sender *Sender
	cnt    *Counters
	logs   []string
	logMu  sync.Mutex
	// override, when non-nil, answers instead of the real handler.
	override http.HandlerFunc
	sleeps   []time.Duration
}

type missingExist struct{}

func (missingExist) Check(_ context.Context, h [][20]byte) existence.Result {
	return existence.Result{Missing: h}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{ingest: &fakeIngest{stored: map[[20]byte]bool{}}, cnt: &Counters{}}
	r.dir = t.TempDir()
	q, err := crawlqueue.Open(crawlqueue.Options{Dir: r.dir, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	r.q = q
	t.Cleanup(func() { _ = q.Close() })

	srv, err := server.New(server.Config{Auth: rigAuth{}, Queue: r.ingest, Exist: missingExist{}})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	r.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.override != nil {
			r.override(w, req)
			return
		}
		h.ServeHTTP(w, req)
	}))
	t.Cleanup(r.ts.Close)
	r.client, err = NewClient(r.ts.URL, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.sender = NewSender(q, r.client, r.cnt, func(f string, a ...any) {
		r.logMu.Lock()
		r.logs = append(r.logs, fmt.Sprintf(f, a...))
		r.logMu.Unlock()
	})
	r.sender.Sleep = func(_ context.Context, d time.Duration) bool { r.sleeps = append(r.sleeps, d); return true }
	r.sender.Jitter = func(max time.Duration) time.Duration { return max }
	return r
}

func (r *rig) logged() string {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	return strings.Join(r.logs, "\n")
}

func (r *rig) add(t *testing.T, n int, payload []byte) {
	t.Helper()
	if ok, err := r.q.Append(bg, hashN(n), record.Version, payload); err != nil || !ok {
		t.Fatalf("append %d: %v %v", n, ok, err)
	}
}

// step peeks one batch and delivers it.
func (r *rig) step(t *testing.T) (outcome, int) {
	t.Helper()
	items, err := r.q.Peek(bg, SendMaxRecords, SendMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		return outcomeProgress, 0
	}
	return r.sender.deliver(bg, items), len(items)
}

// drain delivers until the queue is empty or a non-progress outcome occurs.
func (r *rig) drain(t *testing.T) outcome {
	t.Helper()
	for i := 0; i < 200; i++ {
		o, n := r.step(t)
		if n == 0 {
			return outcomeProgress
		}
		if o != outcomeProgress {
			return o
		}
	}
	t.Fatal("drain did not converge")
	return outcomeStop
}

// rigAuth accepts exactly the rig's token and answers with a fixed identity,
// standing in for the registry-backed authenticator.
type rigAuth struct{}

func (rigAuth) Authenticate(_ context.Context, bearer string) (crawlerauth.Identity, error) {
	if bearer != token {
		return crawlerauth.Identity{}, crawlerauth.ErrUnauthorized
	}
	return crawlerauth.Identity{CrawlerID: "00000000-0000-4000-8000-000000000001", Name: "rig"}, nil
}
