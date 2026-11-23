package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBackend struct {
	verifyErr error
	pingErr   atomic.Pointer[error]
	closed    atomic.Int32
}

func (b *fakeBackend) Verify(context.Context) error { return b.verifyErr }
func (b *fakeBackend) Ping(context.Context) error {
	if e := b.pingErr.Load(); e != nil {
		return *e
	}
	return nil
}
func (b *fakeBackend) Close() { b.closed.Add(1) }

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type fixture struct {
	srv *Server
	be  *fakeBackend
	log *syncBuf
}

func newFixture(t *testing.T, mutate func(*Config)) *fixture {
	t.Helper()
	pub, _ := url.Parse("https://magnet.example.org")
	buf := &syncBuf{}
	cfg := Config{
		PublicURL:       pub,
		TrustedProxies:  []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		RequestTimeout:  5 * time.Second,
		SearchTimeout:   60 * time.Second,
		ShutdownTimeout: 5 * time.Second,
		Logger:          slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	be := &fakeBackend{}
	s, err := New(cfg, be)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{srv: s, be: be, log: buf}
}

func (f *fixture) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *fixture) get(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "203.0.113.9:5000"
	return f.do(req)
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Config{}, &fakeBackend{}); err == nil {
		t.Fatal("missing logger/public URL must fail")
	}
	f := newFixture(t, nil)
	cfg := f.srv.cfg
	cfg.SearchTimeout = 0
	if _, err := New(cfg, &fakeBackend{}); err == nil {
		t.Fatal("zero timeout must fail")
	}
}

func TestFoundationRoutes(t *testing.T) {
	f := newFixture(t, nil)

	rec := f.get("/healthz")
	if rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body.String())
	}
	rec = f.get("/readyz")
	if rec.Code != 200 || rec.Body.String() != "ready\n" {
		t.Fatalf("readyz: %d %q", rec.Code, rec.Body.String())
	}
	rec = f.get("/robots.txt")
	if rec.Code != 200 || rec.Body.String() != "User-agent: *\nDisallow: /\n" ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("robots: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	// Unknown route: nothing is exposed by default.
	if rec = f.get("/torrents"); rec.Code != 404 {
		t.Fatalf("unrouted path: %d", rec.Code)
	}
	if rec = f.get("/"); rec.Code != 404 {
		t.Fatalf("root: %d", rec.Code)
	}
}

func TestReadinessStatuses(t *testing.T) {
	f := newFixture(t, nil)
	boom := errors.New("pq: password authentication failed for user admin at 10.1.2.3")
	f.be.pingErr.Store(&boom)
	rec := f.get("/readyz")
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "not ready\n" {
		t.Fatalf("db down: %d %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "password") || strings.Contains(f.log.String(), "10.1.2.3") {
		t.Fatalf("database detail leaked (body=%q log=%q)", rec.Body.String(), f.log.String())
	}
	f.be.pingErr.Store(nil)
	if rec = f.get("/readyz"); rec.Code != 200 {
		t.Fatalf("recovered: %d", rec.Code)
	}
	f.srv.draining.Store(true)
	if rec = f.get("/readyz"); rec.Code != 503 {
		t.Fatalf("draining: %d", rec.Code)
	}
	// Liveness is unaffected by draining or database state.
	if rec = f.get("/healthz"); rec.Code != 200 {
		t.Fatalf("healthz while draining: %d", rec.Code)
	}
}

func TestHealthBodiesCarryNoVersionOrConfig(t *testing.T) {
	f := newFixture(t, nil)
	for _, p := range []string{"/healthz", "/readyz"} {
		rec := f.get(p)
		body := rec.Body.String()
		for _, bad := range []string{"0.8", "version", "postgres", "magnet.example.org", "{"} {
			if strings.Contains(strings.ToLower(body), bad) {
				t.Errorf("%s body %q contains %q", p, body, bad)
			}
		}
		if rec.Header().Get("Server") != "" {
			t.Errorf("%s: unexpected Server header", p)
		}
	}
}

// Every response class carries the indexing and security headers.
func TestSecurityHeadersOnAllResponseClasses(t *testing.T) {
	f := newFixture(t, nil)
	f.srv.Router().Handle("GET /boom", KindRequest, Public, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("x") }))
	f.srv.Router().Handle("GET /redir", KindRequest, Public, http.RedirectHandler("/healthz", 302))

	paths := []string{"/healthz", "/readyz", "/robots.txt", "/nope", "/boom", "/redir", "/static/missing.css"}
	for _, p := range paths {
		rec := f.get(p)
		h := rec.Header()
		if h.Get("X-Robots-Tag") != "noindex, nofollow" {
			t.Errorf("%s: X-Robots-Tag = %q", p, h.Get("X-Robots-Tag"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") == "" ||
			!strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") ||
			h.Get("Permissions-Policy") == "" || h.Get("X-Request-ID") == "" {
			t.Errorf("%s: missing security headers: %v", p, h)
		}
		if h.Get("Strict-Transport-Security") == "" {
			t.Errorf("%s: HSTS expected for an https public URL", p)
		}
	}
	// 405 from the mux.
	req := httptest.NewRequest("POST", "/healthz", nil)
	req.RemoteAddr = "203.0.113.9:1"
	rec := f.do(req)
	if rec.Code != 405 || rec.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("405: %d %v", rec.Code, rec.Header())
	}
	// Dynamic responses default to no-store.
	if got := f.get("/healthz").Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestHSTSOnlyForHTTPSPublicURL(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.PublicURL, _ = url.Parse("http://localhost:8081") })
	if got := f.get("/healthz").Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS on dev http: %q", got)
	}
}

func TestPanicRecovery(t *testing.T) {
	f := newFixture(t, nil)
	f.srv.Router().Handle("GET /boom", KindRequest, Public, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("hunter2-in-a-panic-value")
	}))
	rec := f.get("/boom")
	if rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	id := rec.Header().Get("X-Request-ID")
	body := rec.Body.String()
	if id == "" || !strings.Contains(body, id) {
		t.Fatalf("500 page must carry the request ID %q: %q", id, body)
	}
	if strings.Contains(body, "hunter2") || strings.Contains(f.log.String(), "hunter2") {
		t.Fatal("panic value leaked into the response or the log")
	}
	logs := f.log.String()
	if !strings.Contains(logs, "panic in handler") || !strings.Contains(logs, "panic_type=string") ||
		!strings.Contains(logs, "request_id="+id) {
		t.Fatalf("panic not logged with its request ID: %s", logs)
	}
	// The access log sits outside the recoverer and sees the final 500.
	if !strings.Contains(logs, "status=500") || !strings.Contains(logs, `route="GET /boom"`) {
		t.Fatalf("access log of the panic: %s", logs)
	}
	// The server keeps serving.
	if f.get("/healthz").Code != 200 {
		t.Fatal("server unusable after a panic")
	}
}

func TestPanicAfterPartialWriteDoesNotAppendErrorPage(t *testing.T) {
	f := newFixture(t, nil)
	f.srv.Router().Handle("GET /partial", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		_, _ = io.WriteString(w, "partial")
		panic("late")
	}))
	rec := f.get("/partial")
	if rec.Code != 202 || rec.Body.String() != "partial" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestAbortHandlerPanicIsPropagated(t *testing.T) {
	f := newFixture(t, nil)
	f.srv.Router().Handle("GET /abort", KindRequest, Public, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Fatal("http.ErrAbortHandler must pass through the recoverer")
		}
	}()
	f.get("/abort")
}

func TestRequestIDIsGeneratedNotTrusted(t *testing.T) {
	f := newFixture(t, nil)
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.RemoteAddr = "10.0.0.2:1"
	req.Header.Set("X-Request-ID", "attacker\ninjected=1")
	rec := f.do(req)
	id := rec.Header().Get("X-Request-ID")
	if len(id) != 16 || strings.ContainsAny(id, "\n ") || id == "attacker" {
		t.Fatalf("id = %q", id)
	}
	if rec2 := f.get("/healthz"); rec2.Header().Get("X-Request-ID") == id {
		t.Fatal("request IDs must be unique")
	}
}

// The access log must never contain anything taken from the URL, headers or
// cookies.
func TestAccessLogIsSecretSafe(t *testing.T) {
	f := newFixture(t, nil)
	f.srv.Router().Handle("GET /api/v2/feeds/latest.rss", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	f.srv.Router().Handle("GET /register/{code}", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	secrets := []string{"mgf_SECRETFEEDTOKEN", "legacy-api-token-md5", "INVITECODE0123456789abcdef", "sessionsecret", "mgt_BEARERSECRET", "q-secret-search"}
	req := httptest.NewRequest("GET", "/api/v2/feeds/latest.rss?feed_token="+secrets[0]+"&api-token="+secrets[1]+"&q="+secrets[5], nil)
	req.RemoteAddr = "203.0.113.9:1"
	req.Header.Set("Cookie", "__Host-magnetico_session="+secrets[3])
	req.Header.Set("Authorization", "Bearer "+secrets[4])
	req.Header.Set("Referer", "https://elsewhere.example/?t="+secrets[0])
	f.do(req)
	req = httptest.NewRequest("GET", "/register/"+secrets[2], nil)
	req.RemoteAddr = "203.0.113.9:1"
	f.do(req)
	req = httptest.NewRequest("GET", "/unrouted/"+secrets[2], nil)
	req.RemoteAddr = "203.0.113.9:1"
	f.do(req)

	logs := f.log.String()
	for _, s := range secrets {
		if strings.Contains(logs, s) {
			t.Errorf("secret %q leaked into the log:\n%s", s, logs)
		}
	}
	for _, want := range []string{`route="GET /api/v2/feeds/latest.rss"`, `route="GET /register/{code}"`, "route=-", "client_ip=203.0.113.9", "status=404"} {
		if !strings.Contains(logs, want) {
			t.Errorf("access log lacks %q:\n%s", want, logs)
		}
	}
}

func TestProbesLogAtDebugOnly(t *testing.T) {
	buf := &syncBuf{}
	f := newFixture(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	f.get("/healthz")
	f.get("/readyz")
	if buf.String() != "" {
		t.Fatalf("probes logged at info: %s", buf.String())
	}
	f.get("/robots.txt")
	if !strings.Contains(buf.String(), "route=\"GET /robots.txt\"") {
		t.Fatalf("ordinary routes must be logged at info: %s", buf.String())
	}
}

func TestClientIPThroughTheChain(t *testing.T) {
	f := newFixture(t, nil)
	var got string
	f.srv.Router().Handle("GET /ip", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = ClientIP(r.Context()).String()
	}))
	cases := []struct{ remote, xff, want string }{
		{"203.0.113.9:1", "1.2.3.4", "203.0.113.9"},                       // spoof from an untrusted peer
		{"10.0.0.2:1", "198.51.100.7, 10.3.3.3", "198.51.100.7"},          // multi-hop via trusted proxies
		{"10.0.0.2:1", "6.6.6.6, 198.51.100.7, 10.3.3.3", "198.51.100.7"}, // forged left entry
		{"10.0.0.2:1", "garbage", "10.0.0.2"},                             // malformed
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/ip", nil)
		req.RemoteAddr = c.remote
		req.Header.Set("X-Forwarded-For", c.xff)
		f.do(req)
		if got != c.want {
			t.Errorf("%s + %q: got %s want %s", c.remote, c.xff, got, c.want)
		}
	}
	if !strings.Contains(f.log.String(), "client_ip=198.51.100.7") {
		t.Fatalf("access log lacks the resolved client:\n%s", f.log.String())
	}
}

func TestCrossOriginProtection(t *testing.T) {
	f := newFixture(t, nil)
	f.srv.Router().rawHandle("POST /submit", KindRequest, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	post := func(h map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://magnet.example.org/submit", strings.NewReader("a=b"))
		req.RemoteAddr = "203.0.113.9:1"
		for k, v := range h {
			req.Header.Set(k, v)
		}
		return f.do(req)
	}
	if rec := post(nil); rec.Code != 204 { // non-browser clients (API tokens, curl)
		t.Fatalf("no browser headers: %d", rec.Code)
	}
	if rec := post(map[string]string{"Sec-Fetch-Site": "same-origin"}); rec.Code != 204 {
		t.Fatalf("same-origin: %d", rec.Code)
	}
	if rec := post(map[string]string{"Origin": "https://magnet.example.org"}); rec.Code != 204 {
		t.Fatalf("matching Origin: %d", rec.Code)
	}
	for name, h := range map[string]map[string]string{
		"cross-site fetch metadata": {"Sec-Fetch-Site": "cross-site"},
		"foreign origin":            {"Origin": "https://evil.example"},
		"same-site sibling":         {"Sec-Fetch-Site": "same-site"},
	} {
		rec := post(h)
		if rec.Code != 403 {
			t.Errorf("%s: %d", name, rec.Code)
		}
		if rec.Header().Get("X-Robots-Tag") == "" || !strings.Contains(rec.Body.String(), "Forbidden") {
			t.Errorf("%s: denial must use the standard error page: %v %q", name, rec.Header(), rec.Body.String())
		}
	}
	// Safe methods are never blocked.
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.RemoteAddr = "203.0.113.9:1"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := f.do(req); rec.Code != 200 {
		t.Fatalf("cross-site GET: %d", rec.Code)
	}
	// A proxy that rewrites Host must not break same-origin posts: the public
	// origin is trusted explicitly.
	req = httptest.NewRequest("POST", "http://backend.internal:8081/submit", strings.NewReader("a=b"))
	req.RemoteAddr = "10.0.0.2:1"
	req.Header.Set("Origin", "https://magnet.example.org")
	if rec := f.do(req); rec.Code != 204 {
		t.Fatalf("proxied same-origin POST: %d", rec.Code)
	}
}

func TestRequestBodyIsLimited(t *testing.T) {
	f := newFixture(t, nil)
	var readErr error
	f.srv.Router().rawHandle("POST /upload", KindRequest, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.Copy(io.Discard, r.Body)
	}))
	req := httptest.NewRequest("POST", "/upload", strings.NewReader(strings.Repeat("x", maxBodyBytes+1)))
	req.RemoteAddr = "203.0.113.9:1"
	f.do(req)
	if readErr == nil {
		t.Fatal("an oversized body must fail to read")
	}
}

func TestStaticAssetsServedImmutable(t *testing.T) {
	f := newFixture(t, nil)
	u, err := f.srv.assets.URL("app.css")
	if err != nil {
		t.Fatal(err)
	}
	rec := f.get(u)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") ||
		rec.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("static: %d %v", rec.Code, rec.Header())
	}
	if rec = f.get("/static/app.css"); rec.Code != 404 {
		t.Fatalf("un-hashed name must not be served: %d", rec.Code)
	}
	if rec = f.get("/static/../../etc/passwd"); rec.Code == 200 {
		t.Fatal("traversal served")
	}
}

// ---- deadlines ----

func TestRouteDeadlinesByClass(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.RequestTimeout = 5 * time.Second
		c.SearchTimeout = 60 * time.Second
	})
	var reqRemaining, searchRemaining time.Duration
	capture := func(dst *time.Duration) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d, ok := r.Context().Deadline()
			if !ok {
				t.Error("handler context has no deadline")
			}
			*dst = time.Until(d)
		})
	}
	f.srv.Router().Handle("GET /plain", KindRequest, Public, capture(&reqRemaining))
	f.srv.Router().Handle("GET /search", KindSearch, Public, capture(&searchRemaining))
	f.get("/plain")
	f.get("/search")
	if reqRemaining <= 0 || reqRemaining > 5*time.Second {
		t.Errorf("request deadline remaining = %v", reqRemaining)
	}
	if searchRemaining <= 55*time.Second || searchRemaining > 60*time.Second {
		t.Errorf("search deadline must be the 60s SQL timeout, remaining = %v", searchRemaining)
	}
	if h, w := f.srv.router.dl.of(KindSearch); h != 60*time.Second || w != 70*time.Second {
		t.Errorf("search handler/write deadline = %v/%v", h, w)
	}
	if h, w := f.srv.router.dl.of(KindRequest); h != 5*time.Second || w != 10*time.Second {
		t.Errorf("request handler/write deadline = %v/%v", h, w)
	}
}

func TestHandlerContextExpires(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.RequestTimeout = 100 * time.Millisecond })
	var err error
	f.srv.Router().Handle("GET /slow", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		err = r.Context().Err()
	}))
	f.get("/slow")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx err = %v", err)
	}
}

// noKeepAliveClient returns a client with its own transport that never keeps a
// connection alive. The shared http.DefaultTransport can dial a second,
// speculative connection while it reuses an idle one; that extra connection
// never carries a request, stays in net/http's StateNew (which Shutdown
// treats as busy for five seconds, longer than the test grace periods) and
// made shutdown tests flaky. One request is exactly one connection here.
func noKeepAliveClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

func startServer(t *testing.T, f *fixture) (addr string, done chan error, cancel context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.srv.cfg.Listener = ln
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- f.srv.Run(ctx) }()
	// Wait until it serves.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := noKeepAliveClient().Get("http://" + ln.Addr().String() + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ln.Addr().String(), done, cancel
}

// A write deadline kills a slow ordinary response, but a search-class route
// of the same duration completes: no global WriteTimeout defeats the search
// timeout.
func TestWriteDeadlinePerRouteNotGlobal(t *testing.T) {
	f := newFixture(t, func(c *Config) {
		c.RequestTimeout = 150 * time.Millisecond
		c.SearchTimeout = 2 * time.Second
	})
	f.srv.router.dl.requestMargin, f.srv.router.dl.searchMargin = 0, 0
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond) // longer than the request class, shorter than search
		_, _ = io.WriteString(w, "done")
	})
	f.srv.Router().Handle("GET /plain-slow", KindRequest, Public, slow)
	f.srv.Router().Handle("GET /search-slow", KindSearch, Public, slow)
	addr, done, cancel := startServer(t, f)
	defer func() { cancel(); <-done }()

	resp, err := http.Get("http://" + addr + "/search-slow")
	if err != nil {
		t.Fatalf("search-class response was cut off: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "done" {
		t.Fatalf("search body = %q", b)
	}

	resp, err = http.Get("http://" + addr + "/plain-slow")
	if err == nil {
		b, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr == nil && string(b) == "done" {
			t.Fatal("an ordinary route outlived its write deadline")
		}
	}
}

// ---- lifecycle ----

func TestRunFailsClosedOnVerification(t *testing.T) {
	f := newFixture(t, nil)
	sentinel := errors.New("schema bad")
	f.be.verifyErr = sentinel
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	f.srv.cfg.Listener = ln
	err := f.srv.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	if f.be.closed.Load() != 1 {
		t.Fatal("backend not closed after a verification failure")
	}
	// Nothing was ever served on the listener.
	ln.(*net.TCPListener).SetDeadline(time.Now().Add(50 * time.Millisecond))
	if c, err := net.DialTimeout("tcp", ln.Addr().String(), 200*time.Millisecond); err == nil {
		// The kernel accepts into the backlog; the server must not answer.
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, _ = c.Write([]byte("GET /healthz HTTP/1.0\r\n\r\n"))
		if n, _ := c.Read(make([]byte, 16)); n > 0 {
			t.Fatal("server answered despite failed verification")
		}
		c.Close()
	}
}

func TestRunListenFailureClosesBackend(t *testing.T) {
	f := newFixture(t, nil)
	f.srv.cfg.Listen = "256.0.0.1:1" // not bindable
	if err := f.srv.Run(context.Background()); err == nil {
		t.Fatal("expected a listen error")
	}
	if f.be.closed.Load() != 1 {
		t.Fatal("backend not closed")
	}
}

func TestGracefulShutdownLetsInFlightFinish(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.ShutdownTimeout = 3 * time.Second })
	started := make(chan struct{})
	release := make(chan struct{})
	f.srv.Router().Handle("GET /work", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release // finishes only when the test says shutdown is under way
		_, _ = io.WriteString(w, "finished")
	}))
	addr, done, cancel := startServer(t, f)

	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := noKeepAliveClient().Get("http://" + addr + "/work")
		if err != nil {
			res <- result{err: err}
			return
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		res <- result{string(b), err}
	}()
	<-started
	cancel() // SIGTERM equivalent

	// Shutdown has begun while the request is still in flight: the handler
	// is held until the server reports draining, so "finished" can only be
	// delivered by a graceful drain, never by the request racing ahead.
	deadline := time.Now().Add(5 * time.Second)
	for !f.srv.draining.Load() {
		if time.Now().After(deadline) {
			t.Fatal("server never started draining")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned %v while a request was still in flight", err)
	default:
	}
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want clean shutdown", err)
	}
	if r := <-res; r.err != nil || r.body != "finished" {
		t.Fatalf("in-flight request: %+v", r)
	}
	if f.be.closed.Load() != 1 {
		t.Fatalf("backend closed %d times", f.be.closed.Load())
	}
	// New connections are refused after shutdown.
	if _, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		t.Fatal("listener still accepting after shutdown")
	}
}

func TestShutdownTimeoutCancelsStuckRequests(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.ShutdownTimeout = 200 * time.Millisecond })
	started := make(chan struct{})
	cancelled := make(chan struct{})
	f.srv.Router().Handle("GET /stuck", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // only the forced cancellation releases it
		close(cancelled)
	}))
	addr, done, cancel := startServer(t, f)
	go func() {
		if resp, err := http.Get("http://" + addr + "/stuck"); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	t0 := time.Now()
	cancel()

	err := <-done
	if !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("Run = %v, want ErrShutdownIncomplete", err)
	}
	if elapsed := time.Since(t0); elapsed > 3*time.Second {
		t.Fatalf("shutdown took %v: the grace bound was not honoured", elapsed)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the stuck request's context was never cancelled")
	}
	if f.be.closed.Load() != 1 {
		t.Fatalf("backend closed %d times", f.be.closed.Load())
	}
}

func TestNewLogger(t *testing.T) {
	var b bytes.Buffer
	l, err := NewLogger(&b, "warn", "json")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hidden")
	l.Warn("shown", "k", "v")
	if strings.Contains(b.String(), "hidden") || !strings.Contains(b.String(), `"msg":"shown"`) {
		t.Fatalf("level/format: %s", b.String())
	}
	if _, err := NewLogger(&b, "loud", "text"); err == nil {
		t.Fatal("bad level")
	}
	if _, err := NewLogger(&b, "info", "yaml"); err == nil {
		t.Fatal("bad format")
	}
}

// The write margin must not extend handler work: the context is cancelled at
// REQUEST_TIMEOUT, and the (timeout) error response can still be written
// inside the margin.
func TestMarginDoesNotExtendHandlerContextButAllowsErrorResponse(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.RequestTimeout = 150 * time.Millisecond })
	f.srv.router.dl.requestMargin = 2 * time.Second
	var cancelledAfter time.Duration
	f.srv.Router().Handle("GET /slowdb", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		<-r.Context().Done() // stands in for a query honouring its context
		cancelledAfter = time.Since(start)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "timed out")
	}))
	addr, done, cancel := startServer(t, f)
	defer func() { cancel(); <-done }()

	resp, err := http.Get("http://" + addr + "/slowdb")
	if err != nil {
		t.Fatalf("the error response could not be written: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || string(b) != "timed out" {
		t.Fatalf("%d %q", resp.StatusCode, b)
	}
	if cancelledAfter < 100*time.Millisecond || cancelledAfter > 600*time.Millisecond {
		t.Fatalf("context cancelled after %v, want about 150ms (not 150ms + margin)", cancelledAfter)
	}
}

type stuckCloseBackend struct {
	fakeBackend
	release chan struct{}
}

func (b *stuckCloseBackend) Close() { <-b.release }

func TestBackendCloseIsBounded(t *testing.T) {
	f := newFixture(t, nil)
	be := &stuckCloseBackend{release: make(chan struct{})}
	defer close(be.release)
	s, err := New(f.srv.cfg, be)
	if err != nil {
		t.Fatal(err)
	}
	s.closeWait = 150 * time.Millisecond
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	s.cfg.Listener = ln
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	for i := 0; i < 100; i++ {
		if resp, err := http.Get("http://" + ln.Addr().String() + "/healthz"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t0 := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrShutdownIncomplete) {
			t.Fatalf("Run = %v, want ErrShutdownIncomplete", err)
		}
		if time.Since(t0) > 2*time.Second {
			t.Fatalf("shutdown blocked on Close for %v", time.Since(t0))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return although Close is stuck")
	}
}

// rawHandle registers a route without an access class. It exists only in
// tests of the transport layers themselves (cross-origin protection, body
// limit), which need a bare POST route; production code cannot do this.
func (r *Router) rawHandle(pattern string, k Kind, h http.Handler) {
	r.mux.Handle(pattern, routeDeadline(pattern, k, r.dl)(h))
}
