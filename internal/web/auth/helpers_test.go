package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/password"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

func tctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return c
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type fakeBackend struct{}

func (fakeBackend) Verify(context.Context) error { return nil }
func (fakeBackend) Ping(context.Context) error   { return nil }
func (fakeBackend) Close()                       {}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// spyVerifier wraps the real (fast-parameter) password service, counts the
// limiter-admitted verifications and can inject failures.
type spyVerifier struct {
	*password.Service
	admitted  atomic.Int64 // verifications that spent hashing work
	verifyErr atomic.Pointer[error]
	dummyErr  atomic.Pointer[error]
	rehashErr atomic.Pointer[error]
	rehashes  atomic.Int64
}

func (v *spyVerifier) Verify(ctx context.Context, pw, hash string) (bool, error) {
	if e := v.verifyErr.Load(); e != nil {
		return false, *e
	}
	ok, err := v.Service.Verify(ctx, pw, hash)
	if err != password.ErrUnsupportedHash { // an unsupported hash is refused without hashing
		v.admitted.Add(1)
	}
	return ok, err
}

func (v *spyVerifier) DummyVerify(ctx context.Context, pw string) error {
	if e := v.dummyErr.Load(); e != nil {
		return *e
	}
	v.admitted.Add(1)
	return v.Service.DummyVerify(ctx, pw)
}

func (v *spyVerifier) Rehash(ctx context.Context, pw string) (string, error) {
	if e := v.rehashErr.Load(); e != nil {
		return "", *e
	}
	v.rehashes.Add(1)
	return v.Service.Rehash(ctx, pw)
}

// storeSpy wraps the real store to count lookups and inject failures.
type storeSpy struct {
	*accounts.Store
	lookups    atomic.Int64
	deletes    atomic.Int64
	lookupErr  atomic.Pointer[error]
	deleteErr  atomic.Pointer[error]
	credErr    atomic.Pointer[error]
	commitErr  atomic.Pointer[error]
	commitStub func() // runs before every CompleteLogin
}

func (s *storeSpy) LookupSession(ctx context.Context, h []byte, idle time.Duration) (accounts.Session, error) {
	s.lookups.Add(1)
	if e := s.lookupErr.Load(); e != nil {
		return accounts.Session{}, *e
	}
	return s.Store.LookupSession(ctx, h, idle)
}

func (s *storeSpy) DeleteSession(ctx context.Context, h []byte) error {
	s.deletes.Add(1)
	if e := s.deleteErr.Load(); e != nil {
		return *e
	}
	return s.Store.DeleteSession(ctx, h)
}

func (s *storeSpy) LoginCredential(ctx context.Context, u string) (accounts.Credential, error) {
	if e := s.credErr.Load(); e != nil {
		return accounts.Credential{}, *e
	}
	return s.Store.LoginCredential(ctx, u)
}

func (s *storeSpy) CompleteLogin(ctx context.Context, p accounts.LoginParams) (accounts.LoginResult, error) {
	if s.commitStub != nil {
		s.commitStub()
	}
	if e := s.commitErr.Load(); e != nil {
		return accounts.LoginResult{}, *e
	}
	return s.Store.CompleteLogin(ctx, p)
}

var testLifetimes = accounts.SessionLifetimes{Idle: 12 * time.Hour, Absolute: 168 * time.Hour, Remember: 2160 * time.Hour}

type envOpts struct {
	dev     bool
	ipLim   *ratelimit.Limiter // nil → effectively unlimited
	userLim *ratelimit.Limiter
}

type env struct {
	t     *testing.T
	srv   *server.Server
	h     http.Handler
	auth  *Auth
	store *storeSpy
	pool  *pgxpool.Pool
	svc   *spyVerifier
	log   *syncBuf
	clock *fakeClock
	dev   bool
}

func (e *env) origin() string {
	if e.dev {
		return "http://127.0.0.1:8081"
	}
	return "https://magnet.example.org"
}

func newEnv(t *testing.T, mutate ...func(*envOpts)) *env {
	t.Helper()
	var o envOpts
	for _, m := range mutate {
		m(&o)
	}
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := persistence.InitSchema(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	if _, err := webschema.Init(tctx(t), dsn); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 24
	pool, err := pgxpool.NewWithConfig(tctx(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	e := &env{t: t, pool: pool, dev: o.dev, log: &syncBuf{}, clock: &fakeClock{t: time.Unix(1_800_000_000, 0)}}
	pub := "https://magnet.example.org"
	if o.dev {
		pub = "http://127.0.0.1:8081"
	}
	pu, _ := url.Parse(pub)
	srv, err := server.New(server.Config{
		PublicURL:       pu,
		TrustedProxies:  []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		RequestTimeout:  10 * time.Second,
		SearchTimeout:   time.Minute,
		ShutdownTimeout: 5 * time.Second,
		Logger:          slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	e.store = &storeSpy{Store: accounts.New(pool)}
	e.svc = &spyVerifier{Service: password.NewService(password.NewLimiter(4, 30*time.Second), fastParams)}
	if err := e.svc.Prepare(tctx(t)); err != nil {
		t.Fatal(err)
	}
	big := func() *ratelimit.Limiter {
		l, _ := ratelimit.New(ratelimit.Config{Burst: 100000, Interval: time.Millisecond, MaxKeys: 1000})
		return l
	}
	ip, ul := o.ipLim, o.userLim
	if ip == nil {
		ip = big()
	}
	if ul == nil {
		ul = big()
	}
	a, err := Install(srv, Config{
		Store: e.store, Verifier: e.svc, Lifetimes: testLifetimes, DevMode: o.dev,
		Now: e.clock.Now, IPLimiter: ip, UserLimiter: ul,
		RootHandler: http.HandlerFunc(testRoot),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.auth, e.h = a, srv.Handler()
	return e
}

var fastParams = password.Params{Memory: 64, Time: 1, Threads: 1}

// mkUser creates an active user with the given password.
func (e *env) mkUser(name, pw string) accounts.User {
	e.t.Helper()
	res, err := e.store.CreateUser(tctx(e.t), accounts.CreateUserParams{Username: name})
	if err != nil {
		e.t.Fatal(err)
	}
	h, err := e.svc.Hash(tctx(e.t), pw)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.store.CompletePasswordLink(tctx(e.t), res.Setup.Token, accounts.PurposeSetup, h); err != nil {
		e.t.Fatal(err)
	}
	u, err := e.store.GetUser(tctx(e.t), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(tctx(e.t), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(tctx(e.t), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (e *env) sessions() int { return e.count(`SELECT count(*) FROM magnetico_web.sessions`) }

// client is a cookie-jar browser stand-in.
type client struct {
	t    *testing.T
	e    *env
	ip   string
	jar  map[string]*http.Cookie
	hdr  http.Header
	last *http.Response
	body string
}

func (e *env) client() *client { return e.clientAt("203.0.113.9:4000") }

func (e *env) clientAt(addr string) *client {
	return &client{t: e.t, e: e, ip: addr, jar: map[string]*http.Cookie{}, hdr: http.Header{}}
}

func (c *client) cookieValue(name string) string {
	if ck := c.jar[name]; ck != nil {
		return ck.Value
	}
	return ""
}

func (c *client) do(method, path string, form url.Values, extra http.Header) *http.Response {
	c.t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = c.ip
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, vs := range c.hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	for k, vs := range extra {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	for _, ck := range c.jar {
		req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	rec := httptest.NewRecorder()
	c.e.h.ServeHTTP(rec, req)
	res := rec.Result()
	for _, ck := range res.Cookies() {
		if ck.MaxAge < 0 {
			delete(c.jar, ck.Name)
		} else {
			c.jar[ck.Name] = ck
		}
	}
	c.last = res
	c.body = rec.Body.String()
	return res
}

func (c *client) get(path string) *http.Response { return c.do("GET", path, nil, nil) }
func (c *client) post(path string, form url.Values) *http.Response {
	return c.do("POST", path, form, nil)
}

var tokenRe = regexp.MustCompile(`name="csrf_token" value="([^"]*)"`)

func (c *client) token() string {
	c.t.Helper()
	m := tokenRe.FindStringSubmatch(c.body)
	if m == nil {
		c.t.Fatalf("no csrf_token in page:\n%s", c.body)
	}
	return m[1]
}

// loginForm returns a login form for the client's current pre-session.
func (c *client) loginForm(user, pw string, extra ...string) url.Values {
	f := url.Values{"username": {user}, "password": {pw}, "csrf_token": {c.token()}}
	for i := 0; i+1 < len(extra); i += 2 {
		f.Set(extra[i], extra[i+1])
	}
	return f
}

// formFor builds a login form with a token captured earlier (a 503 or 429
// page carries no form, so the token must be taken from the GET).
func formFor(tok, user, pw string) url.Values {
	return url.Values{"username": {user}, "password": {pw}, "csrf_token": {tok}}
}

// signIn performs a successful GET /login + POST /login.
func (c *client) signIn(user, pw string, extra ...string) *http.Response {
	c.t.Helper()
	if res := c.get("/login"); res.StatusCode != 200 {
		c.t.Fatalf("GET /login: %d", res.StatusCode)
	}
	res := c.post("/login", c.loginForm(user, pw, extra...))
	if res.StatusCode != 303 {
		c.t.Fatalf("POST /login: %d\n%s", res.StatusCode, c.body)
	}
	return res
}

type fixture struct {
	ID          string `json:"id"`
	Family      string `json:"family"`
	PasswordB64 string `json:"password_b64"`
	Hash        string `json:"hash"`
	Verifies    bool   `json:"verifies"`
}

func loadFixture(t *testing.T, id string) (hash, pw string) {
	t.Helper()
	raw, err := os.ReadFile("../password/testdata/php/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []fixture `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Cases {
		if c.ID == id {
			var pwb []byte
			if err := json.Unmarshal([]byte(`"`+c.PasswordB64+`"`), &pwb); err != nil {
				t.Fatal(err)
			}
			return c.Hash, string(pwb)
		}
	}
	t.Fatalf("fixture %s not found", id)
	return
}

// testRoot stands in for a signed-in page (W3 had an interim home page): it
// shows the user and carries the session form token, as a real page chrome does.
func testRoot(w http.ResponseWriter, r *http.Request) {
	p, _ := Current(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<p>Signed in as " + p.Username + `</p><form method="post" action="/logout"><input type="hidden" name="csrf_token" value="` +
		SessionFormToken(r) + `"></form>`))
}
