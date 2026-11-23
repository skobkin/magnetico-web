package apiv2

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/auth"
	"github.com/skobkin/magnetico-web/internal/web/filecache"
	"github.com/skobkin/magnetico-web/internal/web/fileview"
	"github.com/skobkin/magnetico-web/internal/web/password"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/search"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/torrents"
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
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type fakeBackend struct{}

func (fakeBackend) Verify(context.Context) error { return nil }
func (fakeBackend) Ping(context.Context) error   { return nil }
func (fakeBackend) Close()                       {}

type staticTrackers []string

func (s staticTrackers) URLs() []string { return s }

type envOpts struct {
	view     fileview.Config
	failLim  *ratelimit.Limiter
	useLim   *ratelimit.Limiter
	trackers torrents.TrackerList
	search   func(torrents.Searcher) torrents.Searcher
}

type env struct {
	t      *testing.T
	h      http.Handler
	pool   *pgxpool.Pool
	store  *accounts.Store
	cache  *filecache.Cache
	view   *fileview.Service
	reader *persistence.Reader
	srv    *server.Server
	log    *syncBuf
	n      int
	user   accounts.User
	token  string // a valid API token of user
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

	e := &env{t: t, pool: pool, log: &syncBuf{}}
	logger := slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pu, _ := url.Parse("https://magnet.example.org")
	srv, err := server.New(server.Config{
		PublicURL: pu, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		RequestTimeout: 10 * time.Second, SearchTimeout: time.Minute, ShutdownTimeout: 5 * time.Second, Logger: logger,
	}, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	svc := password.NewService(password.NewLimiter(4, 30*time.Second), password.Params{Memory: 64, Time: 1, Threads: 1})
	if err := svc.Prepare(tctx(t)); err != nil {
		t.Fatal(err)
	}
	big := func() *ratelimit.Limiter {
		l, _ := ratelimit.New(ratelimit.Config{Burst: 1000000, Interval: time.Microsecond, MaxKeys: 1000})
		return l
	}
	e.store = accounts.New(pool)
	if _, err := auth.Install(srv, auth.Config{
		Store: e.store, Verifier: svc, IPLimiter: big(), UserLimiter: big(),
		Lifetimes: accounts.SessionLifetimes{Idle: 12 * time.Hour, Absolute: 168 * time.Hour, Remember: 2160 * time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	e.reader = persistence.NewReader(pool, codec.DefaultLimits())
	searchSvc, err := search.New(e.reader, search.Config{Concurrency: 4, QueueWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if e.cache, err = filecache.New(e.reader, filecache.Config{Logger: logger}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.cache.Close(c)
	})
	o.view.Logger = logger
	if e.view, err = fileview.New(e.cache, e.reader, o.view); err != nil {
		t.Fatal(err)
	}
	var searcher torrents.Searcher = searchSvc
	if o.search != nil {
		searcher = o.search(searchSvc)
	}
	if _, err := torrents.Install(srv, torrents.Deps{Search: searchSvc, Details: e.reader, View: e.view}); err != nil {
		t.Fatal(err)
	}
	c := Config{Auth: e.store, Search: searcher, Details: e.reader, View: e.view, Trackers: o.trackers,
		FailLimiter: big(), UseLimiter: big()}
	if o.failLim != nil {
		c.FailLimiter = o.failLim
	}
	if o.useLim != nil {
		c.UseLimiter = o.useLim
	}
	if _, err := Install(srv, c); err != nil {
		t.Fatal(err)
	}
	e.h = srv.Handler()

	res, err := e.store.CreateUser(tctx(t), accounts.CreateUserParams{Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := svc.Hash(tctx(t), "correct horse battery")
	if _, err := e.store.CompletePasswordLink(tctx(t), res.Setup.Token, accounts.PurposeSetup, hash); err != nil {
		t.Fatal(err)
	}
	e.user, _ = e.store.GetUserByID(tctx(t), res.ID)
	tok, err := e.store.CreateAPIToken(tctx(t), res.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	e.token = tok.Plaintext
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(tctx(e.t), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) seed(name string, total, disc int64, files []codec.File) int32 {
	e.t.Helper()
	e.n++
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", name, e.n)))
	count := int32(1)
	if files != nil {
		count = int32(len(files))
		total = 0
		for _, f := range files {
			total += f.Size
		}
	}
	var id int32
	if err := e.pool.QueryRow(tctx(e.t), `INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
	    VALUES ($1,$2,$3,$4,$5) RETURNING id`, h[:20], name, total, disc, count).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	if files != nil {
		blob, _, err := codec.Encode(files, codec.ProfileCompact, codec.DefaultLimits())
		if err != nil {
			e.t.Fatal(err)
		}
		e.exec(`INSERT INTO magneticod.torrent_file_blobs (torrent_id, data) VALUES ($1,$2)`, id, blob)
	}
	return id
}

// resp is one HTTP exchange.
type resp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r resp) problem(t *testing.T) (typ string, p map[string]any) {
	t.Helper()
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("content type %q, body %s", r.Header.Get("Content-Type"), r.Body)
	}
	if err := json.Unmarshal(r.Body, &p); err != nil {
		t.Fatal(err)
	}
	typ, _ = p["type"].(string)
	return typ, p
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("status %d: %s", r.Status, r.Body)
	}
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("%v: %s", err, r.Body)
	}
}

// do sends a request; hdr entries are added as given.
func (e *env) do(method, path string, hdr map[string]string, cookies ...*http.Cookie) resp {
	e.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "203.0.113.9:4000"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	return resp{Status: res.StatusCode, Header: res.Header, Body: b}
}

// api is a GET with the environment's bearer token.
func (e *env) api(path string) resp {
	e.t.Helper()
	return e.do("GET", path, map[string]string{"Authorization": "Bearer " + e.token})
}

// session returns the UI file-view response of path for a signed-in browser.
func (e *env) session(path string) resp {
	e.t.Helper()
	return e.do("GET", path, nil, e.sessionCookie())
}

func (e *env) sessionCookie() *http.Cookie {
	e.t.Helper()
	// Log in through the real handler.
	req := httptest.NewRequest("GET", "/login", nil)
	req.RemoteAddr = "203.0.113.50:1"
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var pre *http.Cookie
	for _, c := range rec.Result().Cookies() {
		pre = c
	}
	body := rec.Body.String()
	i := strings.Index(body, `name="csrf_token" value="`)
	tok := body[i+len(`name="csrf_token" value="`):]
	tok = tok[:strings.Index(tok, `"`)]
	form := url.Values{"username": {"alice"}, "password": {"correct horse battery"}, "csrf_token": {tok}}
	req = httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.RemoteAddr = "203.0.113.50:1"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(pre)
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if strings.Contains(c.Name, "session") && !strings.Contains(c.Name, "pre") {
			return &http.Cookie{Name: c.Name, Value: c.Value}
		}
	}
	e.t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	return nil
}

func newReq(path string) *http.Request {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "203.0.113.9:4000"
	return req
}

func (e *env) serve(req *http.Request) resp {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	return resp{Status: res.StatusCode, Header: res.Header, Body: b}
}
