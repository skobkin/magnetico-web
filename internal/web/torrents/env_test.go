package torrents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"regexp"
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
	"github.com/skobkin/magnetico-web/internal/web/password"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/search"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
	"github.com/skobkin/magnetico-web/persistence"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunTests(m)) }

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

func tctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return c
}

type envOpts struct {
	searchConc int
	searchWait time.Duration
	searchTO   time.Duration
	cache      filecache.Config
	poolMax    int32
}

type env struct {
	t      *testing.T
	h      http.Handler
	pool   *pgxpool.Pool
	reader *persistence.Reader
	cache  *filecache.Cache
	search *search.Service
	log    *syncBuf
	n      int
}

const origin = "http://127.0.0.1:8081"

func newEnv(t *testing.T, mutate ...func(*envOpts)) *env {
	t.Helper()
	o := envOpts{searchConc: 4, searchWait: time.Second, searchTO: time.Minute, poolMax: 24}
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
	cfg.MaxConns = o.poolMax
	pool, err := pgxpool.NewWithConfig(tctx(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	e := &env{t: t, pool: pool, log: &syncBuf{}}
	logger := slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pu, _ := url.Parse(origin)
	srv, err := server.New(server.Config{
		PublicURL: pu, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		RequestTimeout: 10 * time.Second, SearchTimeout: o.searchTO, ShutdownTimeout: 5 * time.Second, Logger: logger,
	}, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	svc := password.NewService(password.NewLimiter(4, 30*time.Second), password.Params{Memory: 64, Time: 1, Threads: 1})
	if err := svc.Prepare(tctx(t)); err != nil {
		t.Fatal(err)
	}
	big := func() *ratelimit.Limiter {
		l, _ := ratelimit.New(ratelimit.Config{Burst: 100000, Interval: time.Millisecond, MaxKeys: 1000})
		return l
	}
	store := accounts.New(pool)
	if _, err := auth.Install(srv, auth.Config{
		Store: store, Verifier: svc, DevMode: true, IPLimiter: big(), UserLimiter: big(),
		Lifetimes: accounts.SessionLifetimes{Idle: 12 * time.Hour, Absolute: 168 * time.Hour, Remember: 2160 * time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	e.reader = persistence.NewReader(pool, codec.DefaultLimits())
	if e.search, err = search.New(e.reader, search.Config{Concurrency: o.searchConc, QueueWait: o.searchWait}); err != nil {
		t.Fatal(err)
	}
	o.cache.Logger = logger
	if e.cache, err = filecache.New(e.reader, o.cache); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.cache.Close(c)
	})
	if _, err := Install(srv, Deps{Search: e.search, Details: e.reader, Files: e.cache}); err != nil {
		t.Fatal(err)
	}
	e.h = srv.Handler()

	res, err := store.CreateUser(tctx(t), accounts.CreateUserParams{Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := svc.Hash(tctx(t), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompletePasswordLink(tctx(t), res.Setup.Token, accounts.PurposeSetup, hash); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(tctx(e.t), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// seed inserts a torrent row (and, if files != nil, its blob built with the
// compact profile) and returns its id.
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

// ---- browser stand-in ---------------------------------------------------------

type client struct {
	t    *testing.T
	e    *env
	jar  map[string]*http.Cookie
	res  *http.Response
	body string
}

func (e *env) client() *client { return &client{t: e.t, e: e, jar: map[string]*http.Cookie{}} }

func (c *client) do(method, path string, form url.Values) *http.Response {
	c.t.Helper()
	var rd *strings.Reader
	if form != nil {
		rd = strings.NewReader(form.Encode())
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "203.0.113.9:4000"
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", origin)
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
	c.res, c.body = res, rec.Body.String()
	return res
}

func (c *client) get(path string) *http.Response { return c.do("GET", path, nil) }

var tokenRe = regexp.MustCompile(`name="csrf_token" value="([^"]*)"`)

func (c *client) signIn() {
	c.t.Helper()
	c.get("/login")
	m := tokenRe.FindStringSubmatch(c.body)
	if m == nil {
		c.t.Fatal("no csrf token on the login page")
	}
	res := c.do("POST", "/login", url.Values{"username": {"alice"}, "password": {"correct horse battery"}, "csrf_token": {m[1]}})
	if res.StatusCode != 303 {
		c.t.Fatalf("login: %d\n%s", res.StatusCode, c.body)
	}
}

func (e *env) signedIn() *client { c := e.client(); c.signIn(); return c }

var hrefRe = regexp.MustCompile(`href="([^"]*)"`)

// links returns every href of the last page, HTML-unescaped.
func (c *client) links() []string {
	var out []string
	for _, m := range hrefRe.FindAllStringSubmatch(c.body, -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

func (c *client) link(rel string) string {
	for _, m := range regexp.MustCompile(`href="([^"]*)" rel="`+rel+`"`).FindAllStringSubmatch(c.body, -1) {
		return html.UnescapeString(m[1])
	}
	return ""
}

var rowIDRe = regexp.MustCompile(`<td class="name"><a href="/torrents/(\d+)"`)

func (c *client) rowIDs() []int32 {
	var out []int32
	for _, m := range rowIDRe.FindAllStringSubmatch(c.body, -1) {
		var n int32
		fmt.Sscan(m[1], &n)
		out = append(out, n)
	}
	return out
}
