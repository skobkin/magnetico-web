package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/ingest/crawlerauth"
	"github.com/skobkin/magnetico-web/internal/ingest/existence"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/ingest/record"
	"github.com/skobkin/magnetico-web/internal/ingest/stats"
	"github.com/skobkin/magnetico-web/internal/opstats"
	"github.com/skobkin/magnetico-web/persistence"
)

func statsEnv(t *testing.T, mutate func(*Config)) (*env, *stats.Recorder) {
	t.Helper()
	rec := stats.New(nil)
	e := newEnv(t, func(c *Config) {
		c.Stats = rec
		if mutate != nil {
			mutate(c)
		}
	})
	return e, rec
}

func drainSum(rec *stats.Recorder) (t [opstats.NumCounters]int64, d opstats.Delta) {
	d = rec.Drain()
	for _, h := range d.Hours {
		for i, v := range h.Counters {
			t[i] += v
		}
	}
	return t, d
}

func TestBytesAreBodyBytesNotWireBytes(t *testing.T) {
	e, rec := statsEnv(t, nil)
	body := []byte(records(recJSON(t, 1), recJSON(t, 2)))
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+APIPrefix+"/torrents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Padding", strings.Repeat("h", 2000)) // headers are not body bytes
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	ex := []byte(`{"info_hashes":["` + strings.Repeat("ab", 20) + `"]}`)
	if r, _ := e.post(t, "/torrents/exists", testToken, "application/json", ex); r.StatusCode != 200 {
		t.Fatalf("exists %d", r.StatusCode)
	}
	s, _ := drainSum(rec)
	if want := int64(len(body) + len(ex)); s[opstats.CBytesBody] != want {
		t.Fatalf("bytes_body = %d, want exactly the request bodies read: %d", s[opstats.CBytesBody], want)
	}
	// Requests that fail before authentication read no body and add no bytes.
	e.post(t, "/torrents", "wrong", "application/json", body)
	if s, _ := drainSum(rec); s[opstats.CBytesBody] != 0 {
		t.Fatalf("an unauthenticated request counted %d body bytes", s[opstats.CBytesBody])
	}
}

func TestReceivedIsValidatedBeforeAdmissionAtTheHandler(t *testing.T) {
	q := &fakeQueue{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	e, rec := statsEnv(t, func(c *Config) { c.Queue = q })
	var openOnce sync.Once
	release := func() { openOnce.Do(func() { close(q.gate) }) }
	t.Cleanup(release) // a failing assertion must not leave the handler parked
	done := make(chan int, 1)
	go func() {
		resp, _ := e.json(t, "/torrents", records(recJSON(t, 1), recJSON(t, 2), recJSON(t, 3)))
		done <- resp.StatusCode
	}()
	<-q.entered
	// Admission is parked: the records are received and in flight, none acknowledged.
	if rec.InFlight() != 3 {
		t.Fatalf("in flight = %d while admission is parked", rec.InFlight())
	}
	tot := rec.Totals()
	if tot[opstats.CRecordsReceived] != 3 || tot[opstats.CRecordsAcknowledged] != 0 || tot[opstats.CSubmitValidated] != 1 {
		t.Fatalf("totals while parked: received=%d acknowledged=%d", tot[opstats.CRecordsReceived], tot[opstats.CRecordsAcknowledged])
	}
	// The recorder is not held across the admission: other recording and a drain proceed.
	rec.AuthHit()
	if d := rec.Drain(); d.InFlightAtDrain != 3 {
		t.Fatalf("drain while parked: %+v", d)
	}
	release()
	if st := <-done; st != http.StatusAccepted {
		t.Fatalf("status %d", st)
	}
	tot = rec.Totals()
	if rec.InFlight() != 0 || tot[opstats.CRecordsAcknowledged] != 3 || tot[opstats.CRecordsEnqueued] != 3 {
		t.Fatalf("after admission: in flight %d, acknowledged %d", rec.InFlight(), tot[opstats.CRecordsAcknowledged])
	}
}

type panicQueue struct{}

func (panicQueue) Admit(_ context.Context, _ []pgqueue.Item) (pgqueue.AdmitResult, error) {
	panic("admission exploded")
}

func TestAdmissionPanicLeavesNoRecordInFlight(t *testing.T) {
	e, rec := statsEnv(t, func(c *Config) { c.Queue = panicQueue{} })
	resp, _ := e.json(t, "/torrents", records(recJSON(t, 1), recJSON(t, 2)))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if rec.InFlight() != 0 {
		t.Fatalf("a panic left %d records in flight", rec.InFlight())
	}
	s, _ := drainSum(rec)
	if s[opstats.CRecordsReceived] != 2 || s[opstats.CRecordsRefused] != 2 || s[opstats.CRejectedInternal] != 1 {
		t.Fatalf("counters: received %d refused %d internal %d", s[opstats.CRecordsReceived], s[opstats.CRecordsRefused], s[opstats.CRejectedInternal])
	}
}

// TestRejectClassMappingCoversEveryServerErrorCode drives one request per error
// code and checks that exactly the intended reject class counted it.
func TestRejectClassMappingCoversEveryServerErrorCode(t *testing.T) {
	bigRecord := `{"v":1,"pad":"` + strings.Repeat("x", record.MaxRecordBytes) + `"}`
	unsupported := `{"v":99,"anything":true}`
	type tc struct {
		name   string
		mutate func(*Config)
		do     func(e *env) *http.Response
		class  opstats.RejectClass
		status int
	}
	post := func(path, token, ctype, body string) func(e *env) *http.Response {
		return func(e *env) *http.Response {
			r, _ := e.post(t, path, token, ctype, []byte(body))
			return r
		}
	}
	cases := []tc{
		{"wrong method", nil, func(e *env) *http.Response {
			req, _ := http.NewRequest(http.MethodGet, e.ts.URL+APIPrefix+"/torrents", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp
		}, opstats.RejectMalformed, 405},
		{"unknown credential", nil, post("/torrents", "nope", "application/json", records(recJSON(t, 1))), opstats.RejectAuth, 401},
		{"authentication unavailable", func(c *Config) { c.Auth = fakeAuth{err: crawlerauth.ErrUnavailable{}} }, post("/torrents", testToken, "application/json", records(recJSON(t, 1))), opstats.RejectUnavailableAuth, 503},
		{"authentication busy", func(c *Config) { c.Auth = fakeAuth{err: crawlerauth.ErrUnavailable{Busy: true}} }, post("/torrents", testToken, "application/json", records(recJSON(t, 1))), opstats.RejectUnavailableAuth, 503},
		{"unsupported media type", nil, post("/torrents", testToken, "text/plain", "x"), opstats.RejectMalformed, 415},
		{"invalid envelope", nil, post("/torrents", testToken, "application/json", "not json"), opstats.RejectMalformed, 400},
		{"empty records", nil, post("/torrents", testToken, "application/json", `{"records":[]}`), opstats.RejectMalformed, 400},
		{"bad hash in exists", nil, post("/torrents/exists", testToken, "application/json", `{"info_hashes":["zz"]}`), opstats.RejectMalformed, 400},
		{"exists body too large", nil, post("/torrents/exists", testToken, "application/json", `{"info_hashes":["`+strings.Repeat("a", MaxExistsBodyBytes+10)+`"]}`), opstats.RejectTooLarge, 413},
		{"invalid record", nil, post("/torrents", testToken, "application/json", `{"records":[{"v":1}]}`), opstats.RejectInvalidRecord, 400},
		{"unsupported version", nil, post("/torrents", testToken, "application/json", `{"records":[`+unsupported+`]}`), opstats.RejectInvalidRecord, 400},
		{"invalid batch", nil, post("/torrents", testToken, "application/json", `{"records":[`+recJSON(t, 1)+`,{"v":1}]}`), opstats.RejectInvalidRecord, 400},
		{"record too large", nil, post("/torrents", testToken, "application/json", `{"records":[`+bigRecord+`]}`), opstats.RejectTooLarge, 413},
		{"queue full", func(c *Config) { c.Queue = &fakeQueue{err: pgqueue.ErrQueueFull} }, post("/torrents", testToken, "application/json", records(recJSON(t, 1))), opstats.RejectQueueFull, 503},
		{"storage unavailable", func(c *Config) { c.Queue = &fakeQueue{err: persistence.ErrStorageIO} }, post("/torrents", testToken, "application/json", records(recJSON(t, 1))), opstats.RejectStorageUnavailable, 503},
		{"internal error", func(c *Config) { c.Queue = &fakeQueue{err: errors.New("boom")} }, post("/torrents", testToken, "application/json", records(recJSON(t, 1))), opstats.RejectInternal, 500},
		{"exists storage failure", func(c *Config) {
			c.Exist = fakeChecker{fn: func(h [][20]byte) existence.Result { return existence.Result{Unknown: h} }}
		}, post("/torrents/exists", testToken, "application/json", `{"info_hashes":["`+strings.Repeat("ab", 20)+`"]}`), opstats.RejectStorageUnavailable, 503},
	}
	covered := map[opstats.RejectClass]bool{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, rec := statsEnv(t, c.mutate)
			if got := c.do(e).StatusCode; got != c.status {
				t.Fatalf("status %d, want %d", got, c.status)
			}
			s, _ := drainSum(rec)
			for k := opstats.RejectClass(0); k < opstats.NumRejectClasses; k++ {
				want := int64(0)
				if k == c.class {
					want = 1
				}
				if s[k.Counter()] != want {
					t.Errorf("%s = %d, want %d", k.Counter().Name(), s[k.Counter()], want)
				}
			}
			covered[c.class] = true
		})
	}
	// Busy: a second request while every submit slot is taken.
	t.Run("busy", func(t *testing.T) {
		q := &fakeQueue{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
		e, rec := statsEnv(t, func(c *Config) { c.Queue = q; c.SubmitSlots = 1 })
		go e.json(t, "/torrents", records(recJSON(t, 1)))
		<-q.entered
		if resp, _ := e.json(t, "/torrents", records(recJSON(t, 2))); resp.StatusCode != 503 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		close(q.gate)
		time.Sleep(50 * time.Millisecond)
		if s, _ := drainSum(rec); s[opstats.CRejectedBusy] != 1 {
			t.Fatalf("rejected_busy = %d", s[opstats.CRejectedBusy])
		}
		covered[opstats.RejectBusy] = true
	})
	for k := opstats.RejectClass(0); k < opstats.NumRejectClasses; k++ {
		if !covered[k] {
			t.Errorf("reject class %s is produced by no test case", k.Counter().Name())
		}
	}
}

func TestRejectedRequestOfADisabledCrawlerIsCountedForThatCrawler(t *testing.T) {
	disabled := crawlerauth.Identity{CrawlerID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Name: "off"}
	e, rec := statsEnv(t, func(c *Config) { c.Auth = disabledAuth{id: disabled} })
	if resp, _ := e.json(t, "/torrents", records(recJSON(t, 1))); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	e.post(t, "/torrents", "unknown", "application/json", []byte(records(recJSON(t, 1))))
	s, d := drainSum(rec)
	if s[opstats.CRejectedAuth] != 2 {
		t.Fatalf("rejected_auth = %d, want both requests counted globally", s[opstats.CRejectedAuth])
	}
	if len(d.Crawlers) != 1 || d.Crawlers[0].ID != disabled.CrawlerID || d.Crawlers[0].Rejected != 1 {
		t.Fatalf("crawler deltas: %+v (only the disabled crawler's request counts for it)", d.Crawlers)
	}
}

type disabledAuth struct{ id crawlerauth.Identity }

func (a disabledAuth) Authenticate(_ context.Context, bearer string) (crawlerauth.Identity, error) {
	if bearer == testToken {
		return a.id, crawlerauth.ErrUnauthorized
	}
	return crawlerauth.Identity{}, crawlerauth.ErrUnauthorized
}

func TestTrustedProxyPolicy(t *testing.T) {
	trusted := netip.MustParsePrefix("127.0.0.0/8")
	for name, tc := range map[string]struct {
		proxies []netip.Prefix
		xff     string
		want    string
	}{
		"no trusted proxies: the peer, whatever the header says": {nil, "203.0.113.9", "127.0.0.1"},
		"trusted peer: the forwarded client":                     {[]netip.Prefix{trusted}, "203.0.113.9", "203.0.113.9"},
		"trusted peer, spoofed left entry: right-most untrusted": {[]netip.Prefix{trusted}, "198.51.100.1, 203.0.113.9", "203.0.113.9"},
		"untrusted peer: the header is ignored":                  {[]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, "203.0.113.9", "127.0.0.1"},
	} {
		t.Run(name, func(t *testing.T) {
			e, rec := statsEnv(t, func(c *Config) { c.TrustedProxies = tc.proxies })
			req, _ := http.NewRequest(http.MethodPost, e.ts.URL+APIPrefix+"/torrents", strings.NewReader(records(recJSON(t, 1))))
			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Forwarded-For", tc.xff)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			_, d := drainSum(rec)
			if len(d.Crawlers) != 1 || d.Crawlers[0].LastIP.String() != tc.want {
				t.Fatalf("last ip = %v, want %s", d.Crawlers, tc.want)
			}
		})
	}
}

// Enabling statistics changes no status code and no response body.
func TestStatsNeverChangeResponses(t *testing.T) {
	run := func(withStats bool) []string {
		var out []string
		e := newEnv(t, func(c *Config) {
			if withStats {
				c.Stats = stats.New(nil)
			}
		})
		for _, tc := range []struct{ path, tok, ct, body string }{
			{"/torrents", testToken, "application/json", records(recJSON(t, 1), recJSON(t, 2))},
			{"/torrents", testToken, "application/json", records(recJSON(t, 1))},
			{"/torrents", "bad", "application/json", records(recJSON(t, 3))},
			{"/torrents", testToken, "text/plain", "x"},
			{"/torrents", testToken, "application/json", `{"records":[{"v":1}]}`},
			{"/torrents/exists", testToken, "application/json", `{"info_hashes":["` + strings.Repeat("cd", 20) + `"]}`},
			{"/torrents/exists", testToken, "application/json", `{"info_hashes":["x"]}`},
		} {
			r, b := e.post(t, tc.path, tc.tok, tc.ct, []byte(tc.body))
			out = append(out, fmt.Sprintf("%d %s %s", r.StatusCode, r.Header.Get("Retry-After"), strings.TrimSpace(string(b))))
		}
		return out
	}
	without, with := run(false), run(true)
	for i := range without {
		if without[i] != with[i] {
			t.Errorf("request %d: without statistics %q, with %q", i, without[i], with[i])
		}
	}
}

// TestAnAuthenticatedRequestWithAnUnsupportedContentTypeIsStillAnAttempt (Task 09A,
// M-7): requests are counted when authentication succeeds, whatever happens next,
// so a rejected request never appears without the request it rejected.
func TestAnAuthenticatedRequestWithAnUnsupportedContentTypeIsStillAnAttempt(t *testing.T) {
	e, rec := statsEnv(t, nil)
	body := []byte(records(recJSON(t, 1)))
	if r, _ := e.post(t, "/torrents", testToken, "text/plain", body); r.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("submit status %d", r.StatusCode)
	}
	if r, _ := e.post(t, "/torrents/exists", testToken, "text/plain", []byte(`{"info_hashes":[]}`)); r.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("exists status %d", r.StatusCode)
	}
	s, _ := drainSum(rec)
	if s[opstats.CSubmitRequests] != 1 || s[opstats.CExistsRequests] != 1 || s[opstats.CRejectedMalformed] != 2 {
		t.Fatalf("submit_requests=%d exists_requests=%d rejected_malformed=%d, want 1/1/2", s[opstats.CSubmitRequests], s[opstats.CExistsRequests], s[opstats.CRejectedMalformed])
	}
	// An unauthenticated request is not an attempt of any crawler.
	e.post(t, "/torrents", "wrong", "text/plain", body)
	if s, _ := drainSum(rec); s[opstats.CSubmitRequests] != 0 {
		t.Fatalf("an unauthenticated request counted as an authenticated submit: %d", s[opstats.CSubmitRequests])
	}
}

func existsBody(n int) []byte {
	var hs []string
	for i := 0; i < n; i++ {
		hs = append(hs, fmt.Sprintf("%040x", i+1))
	}
	return []byte(`{"info_hashes":["` + strings.Join(hs, `","`) + `"]}`)
}

// TestAFailedExistenceLookupStillCountsItsHashesAndTheirUnknownOutcome: attempted
// requests, hashes checked and classified outcomes are separate figures, and the
// hashes of a lookup that failed are not silently omitted.
func TestAFailedExistenceLookupStillCountsItsHashesAndTheirUnknownOutcome(t *testing.T) {
	e, rec := statsEnv(t, func(c *Config) {
		c.Exist = fakeChecker{fn: func(h [][20]byte) existence.Result { return existence.Result{Unknown: h, CacheMisses: len(h)} }}
	})
	r, _ := e.post(t, "/torrents/exists", testToken, "application/json", existsBody(5))
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", r.StatusCode)
	}
	s, _ := drainSum(rec)
	if s[opstats.CExistsRequests] != 1 || s[opstats.CExistsHashes] != 5 || s[opstats.CExistsUnknown] != 5 ||
		s[opstats.CExistsFound] != 0 || s[opstats.CExistsMissing] != 0 || s[opstats.CRejectedStorageUnavailable] != 1 {
		t.Fatalf("requests=%d hashes=%d unknown=%d found=%d missing=%d rejected=%d, want 1/5/5/0/0/1",
			s[opstats.CExistsRequests], s[opstats.CExistsHashes], s[opstats.CExistsUnknown], s[opstats.CExistsFound], s[opstats.CExistsMissing], s[opstats.CRejectedStorageUnavailable])
	}
	if s[opstats.CCacheMiss] != 5 {
		t.Fatalf("the cache misses of the failed lookup were dropped: %d", s[opstats.CCacheMiss])
	}
}

func TestACancelledExistenceRequestCountsItsHashesAsUnknown(t *testing.T) {
	started := make(chan struct{}, 1)
	e, rec := statsEnv(t, nil)
	e.srv.cfg.Exist = fakeCheckerCtx{fn: func(ctx context.Context, h [][20]byte) existence.Result {
		started <- struct{}{}
		<-ctx.Done()
		return existence.Result{Unknown: h}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.ts.URL+APIPrefix+"/torrents/exists", bytes.NewReader(existsBody(4)))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	go func() { _, _ = http.DefaultClient.Do(req) }()
	<-started
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for rec.Totals()[opstats.CExistsHashes] == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	tot := rec.Totals()
	if tot[opstats.CExistsRequests] != 1 || tot[opstats.CExistsHashes] != 4 || tot[opstats.CExistsUnknown] != 4 || tot[opstats.CExistsFound]+tot[opstats.CExistsMissing] != 0 {
		t.Fatalf("requests=%d hashes=%d unknown=%d: a cancelled lookup must count its hashes as unknown, not omit them",
			tot[opstats.CExistsRequests], tot[opstats.CExistsHashes], tot[opstats.CExistsUnknown])
	}
}

type fakeCheckerCtx struct {
	fn func(ctx context.Context, h [][20]byte) existence.Result
}

func (f fakeCheckerCtx) Check(ctx context.Context, h [][20]byte) existence.Result {
	return f.fn(ctx, h)
}
