package account

import (
	"bytes"
	"context"
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
	webauth "github.com/skobkin/magnetico-web/internal/web/auth"
	"github.com/skobkin/magnetico-web/internal/web/mail"
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

// recMailer records what the queue "sends".
type recMailer struct {
	mu   sync.Mutex
	msgs []mail.Message
}

func (r *recMailer) Send(_ context.Context, m mail.Message) error {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
	return nil
}

func (r *recMailer) all() []mail.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]mail.Message(nil), r.msgs...)
}

// spyStore counts every call into the store.
type spyStore struct {
	*accounts.Store
	calls atomic.Int64
	// beforeSessionIssue, if set, runs just before a session-bound email link
	// issuance reaches the store: the window between authentication and the
	// issuing transaction.
	beforeSessionIssue func()
}

func (s *spyStore) GetUserByID(ctx context.Context, id int64) (accounts.User, error) {
	s.calls.Add(1)
	return s.Store.GetUserByID(ctx, id)
}
func (s *spyStore) CheckInvite(ctx context.Context, c string) (bool, error) {
	s.calls.Add(1)
	return s.Store.CheckInvite(ctx, c)
}
func (s *spyStore) RegisterWithInvite(ctx context.Context, p accounts.RegisterParams) (int64, error) {
	s.calls.Add(1)
	return s.Store.RegisterWithInvite(ctx, p)
}
func (s *spyStore) CompletePasswordLink(ctx context.Context, t, p, h string) (accounts.CompleteResult, error) {
	s.calls.Add(1)
	return s.Store.CompletePasswordLink(ctx, t, p, h)
}
func (s *spyStore) CompleteEmailVerify(ctx context.Context, t string) (accounts.EmailVerifyResult, error) {
	s.calls.Add(1)
	return s.Store.CompleteEmailVerify(ctx, t)
}
func (s *spyStore) IssueEmailVerifyLink(ctx context.Context, id int64, e string) (accounts.Link, error) {
	s.calls.Add(1)
	return s.Store.IssueEmailVerifyLink(ctx, id, e)
}
func (s *spyStore) IssueEmailVerifyLinkForSession(ctx context.Context, id int64, e string, sess []byte) (accounts.Link, error) {
	s.calls.Add(1)
	if s.beforeSessionIssue != nil {
		s.beforeSessionIssue()
	}
	return s.Store.IssueEmailVerifyLinkForSession(ctx, id, e, sess)
}
func (s *spyStore) IssueRecoveryLink(ctx context.Context, e string) (accounts.RecoveryLink, error) {
	s.calls.Add(1)
	return s.Store.IssueRecoveryLink(ctx, e)
}
func (s *spyStore) PendingEmail(ctx context.Context, id int64) (string, bool, error) {
	s.calls.Add(1)
	return s.Store.PendingEmail(ctx, id)
}
func (s *spyStore) ChangePassword(ctx context.Context, p accounts.ChangePasswordParams) (accounts.ChangePasswordResult, error) {
	s.calls.Add(1)
	return s.Store.ChangePassword(ctx, p)
}
func (s *spyStore) CreateAPIToken(ctx context.Context, id int64, l string, e *time.Time) (accounts.CreatedToken, error) {
	s.calls.Add(1)
	return s.Store.CreateAPIToken(ctx, id, l, e)
}
func (s *spyStore) CreateRSSToken(ctx context.Context, id int64, l string, e *time.Time) (accounts.CreatedToken, error) {
	s.calls.Add(1)
	return s.Store.CreateRSSToken(ctx, id, l, e)
}
func (s *spyStore) ListAPITokens(ctx context.Context, id int64) ([]accounts.TokenInfo, error) {
	s.calls.Add(1)
	return s.Store.ListAPITokens(ctx, id)
}
func (s *spyStore) ListRSSTokens(ctx context.Context, id int64) ([]accounts.TokenInfo, error) {
	s.calls.Add(1)
	return s.Store.ListRSSTokens(ctx, id)
}
func (s *spyStore) RevokeAPIToken(ctx context.Context, u, id int64) error {
	s.calls.Add(1)
	return s.Store.RevokeAPIToken(ctx, u, id)
}
func (s *spyStore) RevokeRSSToken(ctx context.Context, u, id int64) error {
	s.calls.Add(1)
	return s.Store.RevokeRSSToken(ctx, u, id)
}
func (s *spyStore) ListInvitesByOwner(ctx context.Context, id int64) ([]accounts.Invite, error) {
	s.calls.Add(1)
	return s.Store.ListInvitesByOwner(ctx, id)
}
func (s *spyStore) RevokeOwnInvite(ctx context.Context, o, id int64) (accounts.RevokeOutcome, error) {
	s.calls.Add(1)
	return s.Store.RevokeOwnInvite(ctx, o, id)
}

var fastParams = password.Params{Memory: 64, Time: 1, Threads: 1}

var testLifetimes = accounts.SessionLifetimes{Idle: 12 * time.Hour, Absolute: 168 * time.Hour, Remember: 2160 * time.Hour}

type envOpts struct {
	noMail bool
	// stopQueue leaves the mail workers off.
	stopQueue   bool
	queueCap    int
	inviteQuota int
	forgotIP    *ratelimit.Limiter
	tokenLim    *ratelimit.Limiter
	userLim     *ratelimit.Limiter
}

type env struct {
	t      *testing.T
	srv    *server.Server
	h      http.Handler
	store  *spyStore
	pool   *pgxpool.Pool
	svc    *password.Service
	mailer *recMailer
	queue  *mail.Queue
	log    *syncBuf
	auth   *webauth.Auth
}

const publicBase = "https://magnet.example.org"

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

	e := &env{t: t, pool: pool, log: &syncBuf{}, mailer: &recMailer{}}
	pu, _ := url.Parse(publicBase)
	logger := slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv, err := server.New(server.Config{
		PublicURL: pu, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		RequestTimeout: 10 * time.Second, SearchTimeout: time.Minute, ShutdownTimeout: 5 * time.Second, Logger: logger,
	}, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	e.store = &spyStore{Store: accounts.New(pool)}
	e.svc = password.NewService(password.NewLimiter(4, 30*time.Second), fastParams)
	if err := e.svc.Prepare(tctx(t)); err != nil {
		t.Fatal(err)
	}
	big := func() *ratelimit.Limiter {
		l, _ := ratelimit.New(ratelimit.Config{Burst: 100000, Interval: time.Millisecond, MaxKeys: 1000})
		return l
	}
	a, err := webauth.Install(srv, webauth.Config{
		Store: e.store.Store, Verifier: e.svc, Lifetimes: testLifetimes,
		IPLimiter: big(), UserLimiter: big(), RecoveryLink: !o.noMail,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.auth = a
	if !o.noMail {
		capacity := o.queueCap
		if capacity == 0 {
			capacity = 8
		}
		e.queue = mail.NewQueue(e.mailer, logger, mail.Options{Capacity: capacity, Workers: 2})
		if !o.stopQueue {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { e.queue.Run(ctx); close(done) }()
			t.Cleanup(func() { cancel(); <-done })
		}
	}
	c := Config{
		Store: e.store, Passwords: e.svc, Auth: a, PublicBase: pu, InviteQuota: o.inviteQuota,
		RegisterLimiter: big(), LinkLimiter: big(), ForgotIPLimiter: big(), ForgotEmailLimiter: big(), UserLimiter: big(), TokenLimiter: big(),
	}
	if e.queue != nil {
		c.Mail = e.queue
	}
	if o.forgotIP != nil {
		c.ForgotIPLimiter = o.forgotIP
	}
	if o.tokenLim != nil {
		c.TokenLimiter = o.tokenLim
	}
	if o.userLim != nil {
		c.UserLimiter = o.userLim
	}
	if _, err := Install(srv, c); err != nil {
		t.Fatal(err)
	}
	e.h = srv.Handler()
	return e
}

// mkUser creates an active user with a password.
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
	u, err := e.store.GetUserByID(tctx(e.t), res.ID)
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

// waitMail waits until the recorder holds n messages.
func (e *env) waitMail(n int) []mail.Message {
	e.t.Helper()
	for i := 0; i < 500; i++ {
		if m := e.mailer.all(); len(m) >= n {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("expected %d messages, have %d", n, len(e.mailer.all()))
	return nil
}

// settle waits until the mail queue has no pending job.
func (e *env) settle() {
	e.t.Helper()
	for i := 0; i < 500; i++ {
		if e.queue.Pending() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("mail queue did not settle")
}

// client is a cookie-jar browser stand-in.
type client struct {
	t    *testing.T
	e    *env
	ip   string
	jar  map[string]*http.Cookie
	last *http.Response
	body string
}

func (e *env) client() *client {
	return &client{t: e.t, e: e, ip: "203.0.113.9:4000", jar: map[string]*http.Cookie{}}
}

func (c *client) do(method, path string, form url.Values) *http.Response {
	c.t.Helper()
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	req.RemoteAddr = c.ip
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
	c.last, c.body = res, rec.Body.String()
	return res
}

func (c *client) get(path string) *http.Response { return c.do("GET", path, nil) }
func (c *client) post(path string, form url.Values) *http.Response {
	return c.do("POST", path, form)
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

// form fetches path (a GET), takes its CSRF token and returns the form values
// plus the token.
func (c *client) form(path string, kv ...string) url.Values {
	c.t.Helper()
	if res := c.get(path); res.StatusCode != 200 && res.StatusCode != 404 {
		c.t.Fatalf("GET %s: %d\n%s", path, res.StatusCode, c.body)
	}
	f := url.Values{"csrf_token": {c.token()}}
	for i := 0; i+1 < len(kv); i += 2 {
		f.Set(kv[i], kv[i+1])
	}
	return f
}

// signIn logs the user in through the real login handler.
func (c *client) signIn(user, pw string) {
	c.t.Helper()
	f := c.form("/login", "username", user, "password", pw)
	if res := c.post("/login", f); res.StatusCode != 303 {
		c.t.Fatalf("login: %d\n%s", res.StatusCode, c.body)
	}
}

func (c *client) sessionCookie() string {
	if ck := c.jar["__Host-magnetico_session"]; ck != nil {
		return ck.Value
	}
	return ""
}

// accountForm GETs the account page tab and returns a form carrying the
// session CSRF token.
func (c *client) accountForm(tab string, kv ...string) url.Values {
	return c.form("/account?tab="+tab, kv...)
}

var linkRe = regexp.MustCompile(`https://magnet\.example\.org(/[^\s]+)`)

func linkOf(t *testing.T, m mail.Message) string {
	t.Helper()
	g := linkRe.FindStringSubmatch(m.Body)
	if g == nil {
		t.Fatalf("no link in message:\n%s", m.Body)
	}
	return g[1]
}
