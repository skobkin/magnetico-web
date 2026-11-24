package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/ingest/cache"
	"github.com/skobkin/magnetico-web/internal/ingest/crawlerauth"
	"github.com/skobkin/magnetico-web/internal/ingest/existence"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/ingest/record"
	"github.com/skobkin/magnetico-web/persistence"
)

const testToken = "0123456789abcdef0123456789abcdef-secret-token"

// fakeAuth maps tokens to identities; behaviour for unknown tokens is chosen
// by err (default ErrUnauthorized).
type fakeAuth struct {
	ids map[string]crawlerauth.Identity
	err error // returned instead of looking the token up
}

func (f fakeAuth) Authenticate(_ context.Context, bearer string) (crawlerauth.Identity, error) {
	if f.err != nil {
		return crawlerauth.Identity{}, f.err
	}
	if id, ok := f.ids[bearer]; ok {
		return id, nil
	}
	return crawlerauth.Identity{}, crawlerauth.ErrUnauthorized
}

var testIdentity = crawlerauth.Identity{CrawlerID: "11111111-1111-4111-8111-111111111111", Name: "test-crawler"}

func okAuth() fakeAuth {
	return fakeAuth{ids: map[string]crawlerauth.Identity{testToken: testIdentity}}
}

var limits = codec.DefaultLimits()

type fakeQueue struct {
	mu       sync.Mutex
	admitted [][]pgqueue.Item
	err      error
	gate     chan struct{} // when set, Admit blocks until closed
	entered  chan struct{}
}

func (f *fakeQueue) Admit(ctx context.Context, items []pgqueue.Item) (pgqueue.AdmitResult, error) {
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return pgqueue.AdmitResult{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return pgqueue.AdmitResult{}, f.err
	}
	f.admitted = append(f.admitted, items)
	return pgqueue.AdmitResult{Inserted: len(items)}, nil
}

func (f *fakeQueue) batches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.admitted)
}

type fakeChecker struct {
	fn func(hashes [][20]byte) existence.Result
}

func (f fakeChecker) Check(_ context.Context, h [][20]byte) existence.Result { return f.fn(h) }

type env struct {
	srv   *Server
	ts    *httptest.Server
	q     *fakeQueue
	logs  *bytes.Buffer
	woken atomic.Int64
}

func newEnv(t *testing.T, mutate func(*Config)) *env {
	t.Helper()
	e := &env{q: &fakeQueue{}, logs: &bytes.Buffer{}}
	cfg := Config{
		Auth: okAuth(), Queue: e.q,
		Exist:      fakeChecker{fn: func(h [][20]byte) existence.Result { return existence.Result{Missing: h} }},
		Logger:     log.New(&syncWriter{w: e.logs}, "", 0),
		OnAdmitted: func() { e.woken.Add(1) },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	e.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (e *env) post(t *testing.T, path, token, ctype string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+APIPrefix+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *env) json(t *testing.T, path string, body string) (*http.Response, []byte) {
	return e.post(t, path, testToken, "application/json", []byte(body))
}

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var eb errorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("response is not an ingest error body: %q", body)
	}
	return eb.Error.Code
}

func hashN(n int) [20]byte {
	var h [20]byte
	h[0], h[1], h[19] = byte(n>>8), byte(n), 0x5A
	return h
}

func recJSON(t testing.TB, n int) string {
	t.Helper()
	b, err := record.Encode(record.Record{
		InfoHash: hashN(n), Name: fmt.Sprintf("name-%d", n), TotalSize: 5, DiscoveredOn: 1700000000,
		Files: []persistence.File{{Path: "f", Size: 5}},
	}, limits)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func records(items ...string) string { return `{"records":[` + strings.Join(items, ",") + `]}` }

// --- authentication ----------------------------------------------------

func TestNewRequiresAnAuthenticator(t *testing.T) {
	if _, err := New(Config{Queue: &fakeQueue{}, Exist: fakeChecker{}}); err == nil {
		t.Fatal("New without an authenticator must fail: there is no unauthenticated mode")
	}
}

func TestAuthenticationAndNoTokenLeak(t *testing.T) {
	e := newEnv(t, nil)
	body := records(recJSON(t, 1))
	for name, tok := range map[string]string{"missing": "", "wrong": "wrong-token-wrong-token-wrong-token-xx", "prefix": testToken[:len(testToken)-1], "extended": testToken + "x"} {
		for _, path := range []string{"/torrents", "/torrents/exists"} {
			resp, b := e.post(t, path, tok, "application/json", []byte(body))
			if resp.StatusCode != http.StatusUnauthorized || errCode(t, b) != "unauthorized" {
				t.Errorf("%s token on %s: %d %s", name, path, resp.StatusCode, b)
			}
			if strings.Contains(string(b), testToken) {
				t.Errorf("response leaks the token")
			}
		}
	}
	if e.q.batches() != 0 {
		t.Fatal("unauthenticated request reached the queue")
	}
	// A wrong scheme is not accepted either.
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+APIPrefix+"/torrents", strings.NewReader(body))
	req.Header.Set("Authorization", "Basic "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Basic scheme: %d", resp.StatusCode)
	}
	// Valid request, then inspect logs for the token.
	if resp, b := e.json(t, "/torrents", body); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("valid request: %d %s", resp.StatusCode, b)
	}
	if strings.Contains(e.logs.String(), "secret-token") {
		t.Fatal("token appeared in the log")
	}
}

func TestMethodAndContentType(t *testing.T) {
	e := newEnv(t, nil)
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+APIPrefix+"/torrents", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
		t.Fatalf("GET: %d allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		resp, b := e.post(t, "/torrents", testToken, ct, []byte(records(recJSON(t, 1))))
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("content-type %q: %d %s", ct, resp.StatusCode, b)
		}
	}
	// A charset parameter is fine.
	if resp, b := e.post(t, "/torrents", testToken, "application/json; charset=utf-8", []byte(records(recJSON(t, 1)))); resp.StatusCode != 202 {
		t.Fatalf("charset param: %d %s", resp.StatusCode, b)
	}
}

// --- submit ------------------------------------------------------------

func TestSubmitAccepted(t *testing.T) {
	e := newEnv(t, nil)
	resp, b := e.json(t, "/torrents", records(recJSON(t, 1), recJSON(t, 2), recJSON(t, 3)))
	if resp.StatusCode != http.StatusAccepted || strings.TrimSpace(string(b)) != `{"accepted":3}` {
		t.Fatalf("got %d %q, want 202 {\"accepted\":3}", resp.StatusCode, b)
	}
	if e.q.batches() != 1 || len(e.q.admitted[0]) != 3 || e.woken.Load() != 1 {
		t.Fatalf("admitted=%d woken=%d", e.q.batches(), e.woken.Load())
	}
	// The stored payload is the exact record JSON.
	if string(e.q.admitted[0][0].Payload) != recJSON(t, 1) {
		t.Fatalf("payload differs from the record bytes: %s", e.q.admitted[0][0].Payload)
	}
	if hex.EncodeToString(e.q.admitted[0][1].InfoHash[:]) != hex.EncodeToString(func() []byte { h := hashN(2); return h[:] }()) {
		t.Fatal("wrong info hash")
	}
}

func TestSubmitEnvelopeErrors(t *testing.T) {
	e := newEnv(t, nil)
	var tooMany []string
	for i := 0; i <= MaxSubmitRecords; i++ {
		tooMany = append(tooMany, recJSON(t, i))
	}
	cases := map[string]string{
		"not json":      `not json`,
		"empty object":  `{}`,
		"null records":  `{"records":null}`,
		"empty records": `{"records":[]}`,
		"unknown field": `{"records":[` + recJSON(t, 1) + `],"extra":1}`,
		"trailing data": records(recJSON(t, 1)) + `{}`,
		"too many":      records(tooMany...),
		"not array":     `{"records":"x"}`,
	}
	for name, body := range cases {
		resp, b := e.json(t, "/torrents", body)
		if resp.StatusCode != 400 || errCode(t, b) != "invalid_request" {
			t.Errorf("%s: %d %s", name, resp.StatusCode, b)
		}
	}
	if e.q.batches() != 0 {
		t.Fatal("malformed envelope reached the queue")
	}
}

func TestSubmitBodyLimit(t *testing.T) {
	e := newEnv(t, nil)
	big := `{"records":[` + strings.Repeat(" ", MaxSubmitBodyBytes+10) + `]}`
	resp, b := e.json(t, "/torrents", big)
	if resp.StatusCode != 413 || errCode(t, b) != "payload_too_large" {
		t.Fatalf("oversize body: %d %s", resp.StatusCode, b)
	}
	if e.q.batches() != 0 {
		t.Fatal("oversize body reached the queue")
	}
}

func TestSingleRecordRejectionCodes(t *testing.T) {
	e := newEnv(t, nil)
	pad := strings.Repeat("a", record.MaxRecordBytes) // makes the member exceed MaxRecordBytes
	cases := []struct {
		name   string
		raw    string
		status int
		code   string
	}{
		{"unsupported version", strings.Replace(recJSON(t, 1), `"v":1`, `"v":2`, 1), 400, "unsupported_version"},
		{"invalid record", `{"v":1,"info_hash":"zz"}`, 400, "invalid_record"},
		{"unknown field", strings.Replace(recJSON(t, 1), `{"v":1`, `{"v":1,"x":1`, 1), 400, "invalid_record"},
		{"too large", `{"v":1,"pad":"` + pad + `"}`, 413, "record_too_large"},
		// Precedence: unsupported_version > record_too_large > invalid_record.
		{"too large and unsupported", `{"v":9,"pad":"` + pad + `"}`, 400, "unsupported_version"},
		{"invalid and unsupported", `{"v":9,"info_hash":1}`, 400, "unsupported_version"},
	}
	for _, c := range cases {
		resp, b := e.json(t, "/torrents", records(c.raw))
		ct := resp.Header.Get("Content-Type")
		if resp.StatusCode != c.status || errCode(t, b) != c.code || ct != "application/json" {
			t.Errorf("%s: %d %q ct=%q, want %d %s", c.name, resp.StatusCode, firstN(b, 120), ct, c.status, c.code)
		}
	}
	if e.q.batches() != 0 {
		t.Fatal("a rejected record reached the queue")
	}
}

func firstN(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

func TestMultiRecordRejectionIsAllOrNothing(t *testing.T) {
	e := newEnv(t, nil)
	// One invalid among valid: 400 invalid_batch, nothing admitted, with
	// diagnostic item indexes only.
	resp, b := e.json(t, "/torrents", records(recJSON(t, 1), `{"v":1}`, recJSON(t, 3), strings.Replace(recJSON(t, 4), `"v":1`, `"v":7`, 1)))
	if resp.StatusCode != 400 {
		t.Fatalf("status %d %s", resp.StatusCode, b)
	}
	var eb errorBody
	if err := json.Unmarshal(b, &eb); err != nil || eb.Error.Code != "invalid_batch" || len(eb.Error.Items) != 2 ||
		eb.Error.Items[0].Index != 1 || eb.Error.Items[1].Index != 3 || eb.Error.Items[1].Code != "unsupported_version" {
		t.Fatalf("body = %s", b)
	}
	// Oversize member in a multi-record request: 413 payload_too_large wins
	// over 400, and the record codes are never used.
	pad := strings.Repeat("a", record.MaxRecordBytes)
	resp, b = e.json(t, "/torrents", records(recJSON(t, 1), `{"v":1}`, `{"v":1,"pad":"`+pad+`"}`))
	if resp.StatusCode != 413 || errCode(t, b) != "payload_too_large" {
		t.Fatalf("multi oversize: %d %s", resp.StatusCode, firstN(b, 100))
	}
	if e.q.batches() != 0 {
		t.Fatal("a rejected batch reached the queue")
	}
}

func TestSubmitStorageOutcomes(t *testing.T) {
	e := newEnv(t, nil)
	body := records(recJSON(t, 1))

	e.q.err = pgqueue.ErrQueueFull
	resp, b := e.json(t, "/torrents", body)
	if resp.StatusCode != 503 || errCode(t, b) != "queue_full" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("queue_full: %d %s", resp.StatusCode, b)
	}
	e.q.err = fmt.Errorf("%w: password=hunter2 host=db.internal", persistence.ErrStorageIO)
	resp, b = e.json(t, "/torrents", body)
	if resp.StatusCode != 503 || errCode(t, b) != "storage_unavailable" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("storage_unavailable: %d %s", resp.StatusCode, b)
	}
	if strings.Contains(string(b), "hunter2") || strings.Contains(e.logs.String(), "hunter2") {
		t.Fatal("driver error text leaked")
	}
	e.q.err = errors.New("secret internal failure host=db.internal")
	resp, b = e.json(t, "/torrents", body)
	if resp.StatusCode != 500 || errCode(t, b) != "internal_error" {
		t.Fatalf("internal: %d %s", resp.StatusCode, b)
	}
	if strings.Contains(string(b), "secret") || strings.Contains(e.logs.String(), "secret") {
		t.Fatal("internal error text leaked")
	}
	if e.woken.Load() != 0 {
		t.Fatal("consumer woken without a successful admission")
	}
	// Recovery: the same request succeeds once storage is back.
	e.q.mu.Lock()
	e.q.err = nil
	e.q.mu.Unlock()
	if resp, b := e.json(t, "/torrents", body); resp.StatusCode != 202 {
		t.Fatalf("after recovery: %d %s", resp.StatusCode, b)
	}
}

func TestSubmitBusy(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.SubmitSlots = 1 })
	e.q.gate = make(chan struct{})
	e.q.entered = make(chan struct{}, 1)
	done := make(chan int, 1)
	go func() {
		resp, _ := e.json(t, "/torrents", records(recJSON(t, 1)))
		done <- resp.StatusCode
	}()
	<-e.q.entered
	resp, b := e.json(t, "/torrents", records(recJSON(t, 2)))
	if resp.StatusCode != 503 || errCode(t, b) != "busy" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("second request: %d %s", resp.StatusCode, b)
	}
	close(e.q.gate)
	if st := <-done; st != 202 {
		t.Fatalf("first request: %d", st)
	}
}

func TestHandlerPanicIsContained(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.Exist = fakeChecker{fn: func([][20]byte) existence.Result { panic("boom secret-data") }}
	})
	resp, b := e.json(t, "/torrents/exists", `{"info_hashes":[]}`)
	if resp.StatusCode != 500 || strings.Contains(string(b), "boom") || strings.Contains(e.logs.String(), "secret-data") {
		t.Fatalf("panic: %d %s logs=%s", resp.StatusCode, b, e.logs.String())
	}
}

// --- exists ------------------------------------------------------------

func hx(n int) string { h := hashN(n); return hex.EncodeToString(h[:]) }

func TestExistsClassification(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.Exist = fakeChecker{fn: func(h [][20]byte) existence.Result {
			return existence.Result{Exists: h[:1], Missing: h[1:2], Unknown: h[2:]}
		}}
	})
	resp, b := e.json(t, "/torrents/exists", fmt.Sprintf(`{"info_hashes":["%s","%s","%s"]}`, hx(1), hx(2), hx(3)))
	var out existsResponse
	if err := json.Unmarshal(b, &out); err != nil || resp.StatusCode != 200 ||
		len(out.Exists) != 1 || out.Exists[0] != hx(1) || len(out.Missing) != 1 || out.Missing[0] != hx(2) ||
		len(out.Unknown) != 1 || out.Unknown[0] != hx(3) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	// Empty arrays are encoded as [], never null.
	e2 := newEnv(t, nil)
	_, b = e2.json(t, "/torrents/exists", `{"info_hashes":[]}`)
	if strings.Contains(string(b), "null") {
		t.Fatalf("null in %s", b)
	}
}

func TestExistsUnknownIsNeverMissing(t *testing.T) {
	// Every lookup failed: a whole-request storage failure, not "missing".
	e := newEnv(t, func(c *Config) {
		c.Exist = fakeChecker{fn: func(h [][20]byte) existence.Result { return existence.Result{Unknown: h} }}
	})
	resp, b := e.json(t, "/torrents/exists", fmt.Sprintf(`{"info_hashes":["%s"]}`, hx(1)))
	if resp.StatusCode != 503 || errCode(t, b) != "storage_unavailable" || strings.Contains(string(b), hx(1)) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestExistsValidation(t *testing.T) {
	e := newEnv(t, nil)
	var many []string
	for i := 0; i <= MaxExistsHashes; i++ {
		many = append(many, `"`+hx(i)+`"`)
	}
	for name, body := range map[string]string{
		"missing":   `{}`,
		"null":      `{"info_hashes":null}`,
		"short":     `{"info_hashes":["abcd"]}`,
		"not hex":   `{"info_hashes":["` + strings.Repeat("g", 40) + `"]}`,
		"too many":  `{"info_hashes":[` + strings.Join(many, ",") + `]}`,
		"extra key": `{"info_hashes":[],"x":1}`,
		"not json":  `[`,
	} {
		if resp, b := e.json(t, "/torrents/exists", body); resp.StatusCode != 400 || errCode(t, b) != "invalid_request" {
			t.Errorf("%s: %d %s", name, resp.StatusCode, b)
		}
	}
	big := `{"info_hashes":[` + strings.Repeat(" ", MaxExistsBodyBytes+1) + `]}`
	if resp, b := e.json(t, "/torrents/exists", big); resp.StatusCode != 413 || errCode(t, b) != "payload_too_large" {
		t.Errorf("oversize: %d %s", resp.StatusCode, b)
	}
	// Exactly the documented maximum is accepted.
	max := many[:MaxExistsHashes]
	if resp, b := e.json(t, "/torrents/exists", `{"info_hashes":[`+strings.Join(max, ",")+`]}`); resp.StatusCode != 200 {
		t.Errorf("1024 hashes: %d %s", resp.StatusCode, b)
	}
}

type countingFinder struct {
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
}

func (c *countingFinder) ExistingInfoHashes(ctx context.Context, h [][]byte) (map[[20]byte]struct{}, error) {
	c.calls.Add(1)
	select {
	case c.started <- struct{}{}:
	default:
	}
	<-c.release
	return map[[20]byte]struct{}{}, nil
}

func TestExistsConcurrentIdenticalMissesCollapse(t *testing.T) {
	f := &countingFinder{started: make(chan struct{}, 1), release: make(chan struct{})}
	svc := existence.New(f, cache.New(cache.Options{MaxBytes: 1 << 20}), existence.Options{})
	e := newEnv(t, func(c *Config) { c.Exist = svc })
	var wg sync.WaitGroup
	statuses := make(chan int, 8)
	body := fmt.Sprintf(`{"info_hashes":["%s"]}`, hx(42))
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := e.json(t, "/torrents/exists", body)
			statuses <- resp.StatusCode
		}()
	}
	<-f.started
	time.Sleep(100 * time.Millisecond) // let the other requests join the in-flight lookup
	close(f.release)
	wg.Wait()
	close(statuses)
	for st := range statuses {
		if st != 200 {
			t.Fatalf("status %d", st)
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("database lookups = %d, want 1 (in-flight join)", n)
	}
}

// --- per-crawler authentication (revision 2/3) --------------------------

// identityQueue records the crawler identity seen at admission.
type identityQueue struct {
	fakeQueue
	seen []crawlerauth.Identity
}

func (q *identityQueue) Admit(ctx context.Context, items []pgqueue.Item) (pgqueue.AdmitResult, error) {
	id, _ := crawlerauth.FromContext(ctx)
	q.mu.Lock()
	q.seen = append(q.seen, id)
	q.mu.Unlock()
	return q.fakeQueue.Admit(ctx, items)
}

func TestAuthenticationHappensAfterMethodAndBeforeContentTypeAndBody(t *testing.T) {
	e := newEnv(t, nil)
	// Method first: a GET with a bad token is a 405, not a 401.
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+APIPrefix+"/torrents", nil)
	req.Header.Set("Authorization", "Bearer nope")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET with a bad token: %d, want 405", resp.StatusCode)
	}
	// Authentication before the content type: a bad token with a bad content type is a 401.
	resp2, b := e.post(t, "/torrents", "nope", "text/plain", []byte("x"))
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token + bad content type: %d %s, want 401", resp2.StatusCode, b)
	}
	// Authentication before the body: the handler never reads it for an
	// unauthenticated request (the spy sits directly under the handler).
	for _, path := range []string{"/torrents", "/torrents/exists"} {
		var read atomic.Bool
		hreq := httptest.NewRequest(http.MethodPost, APIPrefix+path, io.NopCloser(readerFunc(func(p []byte) (int, error) {
			read.Store(true)
			return 0, io.EOF
		})))
		hreq.Header.Set("Authorization", "Bearer nope")
		hreq.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, hreq)
		if rec.Code != http.StatusUnauthorized || read.Load() {
			t.Fatalf("%s: status %d, body read=%v; want 401 without reading the body", path, rec.Code, read.Load())
		}
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestIdentityComesOnlyFromTheCredential(t *testing.T) {
	q := &identityQueue{}
	e := newEnv(t, func(c *Config) { c.Queue = q })
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+APIPrefix+"/torrents", strings.NewReader(records(recJSON(t, 1))))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	// A forged identity header (the removed X-Magnetico-Crawler-ID) changes nothing.
	req.Header.Set("X-Magnetico-Crawler-ID", "22222222-2222-4222-8222-222222222222")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.seen) != 1 || q.seen[0] != testIdentity {
		t.Fatalf("identity at admission = %+v, want %+v (from the credential only)", q.seen, testIdentity)
	}
}

func TestCrossCrawlerIsolation(t *testing.T) {
	a := crawlerauth.Identity{CrawlerID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Name: "a"}
	b := crawlerauth.Identity{CrawlerID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Name: "b"}
	q := &identityQueue{}
	e := newEnv(t, func(c *Config) {
		c.Queue = q
		c.Auth = fakeAuth{ids: map[string]crawlerauth.Identity{"tok-a": a, "tok-b": b}}
	})
	for _, tc := range []struct {
		tok  string
		want crawlerauth.Identity
	}{{"tok-a", a}, {"tok-b", b}, {"tok-a", a}} {
		if resp, body := e.post(t, "/torrents", tc.tok, "application/json", []byte(records(recJSON(t, 1)))); resp.StatusCode != 202 {
			t.Fatalf("%s: %d %s", tc.tok, resp.StatusCode, body)
		}
		q.mu.Lock()
		got := q.seen[len(q.seen)-1]
		q.mu.Unlock()
		if got != tc.want {
			t.Fatalf("token %s produced identity %+v, want %+v", tc.tok, got, tc.want)
		}
	}
}

func TestAuthStorageFailureIs503RetryableNeverUnauthorized(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code string
		wait string
	}{
		"storage": {crawlerauth.ErrUnavailable{}, "storage_unavailable", "5"},
		"busy":    {crawlerauth.ErrUnavailable{Busy: true}, "busy", "1"},
	} {
		t.Run(name, func(t *testing.T) {
			q := &fakeQueue{}
			e := newEnv(t, func(c *Config) { c.Queue = q; c.Auth = fakeAuth{err: tc.err} })
			for _, path := range []string{"/torrents", "/torrents/exists"} {
				resp, b := e.json(t, path, records(recJSON(t, 1)))
				if resp.StatusCode != http.StatusServiceUnavailable || errCode(t, b) != tc.code ||
					resp.Header.Get("Retry-After") != tc.wait || resp.Header.Get("WWW-Authenticate") != "" {
					t.Fatalf("%s: %d %s retry-after=%q", path, resp.StatusCode, b, resp.Header.Get("Retry-After"))
				}
			}
			if q.batches() != 0 {
				t.Fatal("a request whose authentication failed reached the queue")
			}
			if c := e.srv.Counters(); c.AuthUnavailable != 2 || c.AuthFailures != 0 {
				t.Fatalf("counters = %+v", c)
			}
		})
	}
}
