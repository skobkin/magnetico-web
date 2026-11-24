package crawler

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/ingest/existence"
	"github.com/skobkin/magnetico-web/internal/ingest/server"
)

// batchExist records the size and time of every existence request.
type batchExist struct {
	mu    sync.Mutex
	sizes []int
	times []time.Time
}

func (b *batchExist) Exists(_ context.Context, h [][20]byte) (ExistsResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sizes = append(b.sizes, len(h))
	b.times = append(b.times, time.Now())
	return ExistsResult{Missing: h}, nil
}

func (b *batchExist) snapshot() ([]int, []time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int(nil), b.sizes...), append([]time.Time(nil), b.times...)
}

func startPrecheck(t *testing.T, linger time.Duration, ex Exister) *Prechecker {
	t.Helper()
	p := NewPrechecker(ex, &fakeSink{}, gate(true), &Counters{}, nil)
	p.Linger = linger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return p
}

func TestPrecheckBatchOf1024SerialisesBelowTheServerLimits(t *testing.T) {
	if PrecheckBatch != 1024 || PrecheckLinger != time.Second {
		t.Fatalf("PrecheckBatch=%d PrecheckLinger=%v, want 1024 and 1s", PrecheckBatch, PrecheckLinger)
	}
	if PrecheckBatch > server.MaxExistsHashes {
		t.Fatalf("a batch of %d exceeds the server maximum %d", PrecheckBatch, server.MaxExistsHashes)
	}
	// The real client against the real handler: the request body of 1,024
	// hashes stays below MaxExistsBodyBytes and the response below what the client reads.
	var bodyLen int
	var respLen int
	srv, err := server.New(server.Config{Auth: rigAuth{}, Queue: &fakeIngest{}, Exist: allExist{}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyLen = len(b)
		rec := httptest.NewRecorder()
		r.Body = io.NopCloser(bytes.NewReader(b))
		srv.Handler().ServeHTTP(rec, r)
		respLen = rec.Body.Len()
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	}))
	defer ts.Close()
	c, err := NewClient(ts.URL, token, nil)
	if err != nil {
		t.Fatal(err)
	}
	hashes := make([][20]byte, PrecheckBatch)
	for i := range hashes {
		hashes[i] = hashN(i + 1)
	}
	res, err := c.Exists(context.Background(), hashes)
	if err != nil || len(res.Exists) != PrecheckBatch {
		t.Fatalf("Exists of %d hashes: %d exist, err %v", PrecheckBatch, len(res.Exists), err)
	}
	if bodyLen >= server.MaxExistsBodyBytes {
		t.Fatalf("request body %d bytes, limit %d", bodyLen, server.MaxExistsBodyBytes)
	}
	if respLen >= maxResponseRead {
		t.Fatalf("response %d bytes, the client reads at most %d", respLen, maxResponseRead)
	}
	t.Logf("1,024 hashes: request %d bytes (limit %d), response %d bytes (read limit %d)", bodyLen, server.MaxExistsBodyBytes, respLen, maxResponseRead)
}

type allExist struct{}

func (allExist) Check(_ context.Context, h [][20]byte) existence.Result {
	return existence.Result{Exists: h}
}

func TestPrecheckDispatchesAtBatchSizeOrLingerAndNeverEmpty(t *testing.T) {
	// Idle: no request at all, however long the linger would be.
	ex := &batchExist{}
	p := startPrecheck(t, 100*time.Millisecond, ex)
	time.Sleep(400 * time.Millisecond)
	if sizes, _ := ex.snapshot(); len(sizes) != 0 {
		t.Fatalf("an idle prechecker sent %v", sizes)
	}
	// A partial batch goes after the linger of its first element.
	start := time.Now()
	for i := 1; i <= 3; i++ {
		p.Offer(&fakeRes{h: hashN(i)})
	}
	waitUntil(t, "the lingering batch", 3*time.Second, func() bool { s, _ := ex.snapshot(); return len(s) == 1 })
	if s, tm := ex.snapshot(); s[0] != 3 || tm[0].Sub(start) < 80*time.Millisecond {
		t.Fatalf("batch %v at +%v, want 3 hashes after the linger", s, tm[0].Sub(start))
	}
	// A full batch goes at once, long before a 1-hour linger.
	ex2 := &batchExist{}
	p2 := startPrecheck(t, time.Hour, ex2)
	t0 := time.Now()
	for i := 1; i <= PrecheckBatch; i++ {
		p2.Offer(&fakeRes{h: hashN(i)})
	}
	waitUntil(t, "the full batch", 3*time.Second, func() bool { s, _ := ex2.snapshot(); return len(s) == 1 })
	if s, tm := ex2.snapshot(); s[0] != PrecheckBatch || tm[0].Sub(t0) > time.Second {
		t.Fatalf("full batch %v after %v", s, tm[0].Sub(t0))
	}
}

func TestPrecheckSparseTrafficOneRequestPerLinger(t *testing.T) {
	ex := &batchExist{}
	p := startPrecheck(t, 200*time.Millisecond, ex)
	start := time.Now()
	for i := 1; time.Since(start) < time.Second; i++ {
		p.Offer(&fakeRes{h: hashN(i)})
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	sizes, _ := ex.snapshot()
	if len(sizes) < 3 || len(sizes) > 7 {
		t.Fatalf("%d requests in ~1 s with a 200 ms linger: %v", len(sizes), sizes)
	}
	for _, n := range sizes {
		if n == 0 {
			t.Fatal("an empty request was sent")
		}
	}
}

// --- transport -----------------------------------------------------------

func TestConsecutiveRequestsReuseOneConnection(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	newConns, requests := 0, 0
	call := func(do func(ctx context.Context)) {
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			GotConn: func(i httptrace.GotConnInfo) {
				mu.Lock()
				defer mu.Unlock()
				requests++
				if !i.Reused {
					newConns++
				}
			},
		})
		do(ctx)
	}
	for i := 1; i <= 20; i++ {
		call(func(ctx context.Context) {
			if i%2 == 0 {
				if _, err := r.client.Exists(ctx, [][20]byte{hashN(i)}); err != nil {
					t.Fatal(err)
				}
				return
			}
			if res := r.client.Submit(ctx, [][]byte{goodPayload(t, i)}); res.Kind != SubmitAccepted {
				t.Fatalf("submit: %+v", res)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 20 || newConns != 1 {
		t.Fatalf("%d connections for %d requests, want 1 (bodies drained, keep-alive honoured)", newConns, requests)
	}
}

func TestTLSNegotiatesHTTP2WithTheDefaultTransport(t *testing.T) {
	tr := defaultTransport()
	// Go enables HTTP/2 automatically only while none of these is customised.
	if tr.TLSClientConfig != nil || tr.DialContext != nil || tr.DialTLSContext != nil || tr.Dial != nil || tr.DialTLS != nil {
		t.Fatal("the default transport customises TLS or dialing, which would silently disable HTTP/2")
	}
	// A real TLS server with HTTP/2: a clone of the default transport that only adds the
	// test CA (and so must force HTTP/2) negotiates h2.
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"proto": r.Proto})
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	clone := tr.Clone()
	clone.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	clone.ForceAttemptHTTP2 = true
	resp, err := (&http.Client{Transport: clone}).Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("negotiated HTTP/%d, want HTTP/2", resp.ProtoMajor)
	}
	// The default configuration negotiates h2 against a server that offers it:
	// after the first use the transport has registered the h2 protocol.
	d := defaultTransport()
	_, _ = (&http.Client{Transport: d, Timeout: time.Second}).Get(ts.URL) // fails on the untrusted test CA; initialisation is what matters
	if d.TLSNextProto["h2"] == nil {
		t.Fatal("the default transport did not register HTTP/2 (ALPN h2)")
	}
}
