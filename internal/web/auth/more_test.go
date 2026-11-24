package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/server"
)

func limiters(t *testing.T, ipBurst, userBurst int) (func(*envOpts), *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	ip, err := ratelimit.New(ratelimit.Config{Burst: ipBurst, Interval: 30 * time.Second, MaxKeys: 100, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	ul, err := ratelimit.New(ratelimit.Config{Burst: userBurst, Interval: 60 * time.Second, MaxKeys: 100, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return func(o *envOpts) { o.ipLim, o.userLim = ip, ul }, clk
}

// attempt performs one login POST with a fresh pre-session and returns it.
func attemptLogin(t *testing.T, e *env, c *client, user, pw string) *http.Response {
	t.Helper()
	c.get("/login")
	return c.post("/login", c.loginForm(user, pw))
}

func TestRateLimitPerIP(t *testing.T) {
	opt, clk := limiters(t, 3, 1000)
	e := newEnv(t, opt)
	e.mkUser("uma", "the right password")
	c := e.client()
	for i := 0; i < 3; i++ {
		if res := attemptLogin(t, e, c, "name"+strconv.Itoa(i), "wrong password"); res.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, res.StatusCode)
		}
	}
	res := attemptLogin(t, e, c, "name9", "wrong password")
	if res.StatusCode != 429 || res.Header.Get("Retry-After") != "30" {
		t.Fatalf("4th attempt: %d retry-after=%q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if !strings.Contains(c.body, msgLoginLimited) {
		t.Error("the 429 page lacks the generic message")
	}
	// Even a correct password is throttled while the IP is exhausted; no lockout
	// of the account: another IP is unaffected, and time heals.
	if res := attemptLogin(t, e, c, "uma", "the right password"); res.StatusCode != 429 {
		t.Fatalf("exhausted IP with the right password: %d", res.StatusCode)
	}
	if res := attemptLogin(t, e, e.clientAt("198.51.100.7:1"), "uma", "the right password"); res.StatusCode != 303 {
		t.Fatalf("another IP: %d", res.StatusCode)
	}
	clk.Advance(30 * time.Second)
	if res := attemptLogin(t, e, c, "uma", "the right password"); res.StatusCode != 303 {
		t.Fatalf("after the advertised wait: %d", res.StatusCode)
	}
}

func TestRateLimitPerUsernameIsIdenticalForKnownAndUnknown(t *testing.T) {
	opt, _ := limiters(t, 1000, 2)
	e := newEnv(t, opt)
	e.mkUser("vera", "the right password")
	type result struct {
		status int
		retry  string
		shape  string
	}
	run := func(user string) result {
		var last result
		for i := 0; i < 3; i++ { // from different IPs: only the username bucket can refuse
			c := e.clientAt("198.51.100." + strconv.Itoa(10+i) + ":1")
			res := attemptLogin(t, e, c, user, "wrong password")
			last = result{res.StatusCode, res.Header.Get("Retry-After"), shape(c.body)}
		}
		return last
	}
	known, unknown := run("vera"), run("nobody-here")
	if known.status != 429 || known.retry != "60" {
		t.Fatalf("known username: %+v", known)
	}
	if known != unknown {
		t.Fatalf("429 differs between existing and unknown usernames (enumeration):\n%+v\n%+v", known, unknown)
	}
	// Case-folded sharing: "VERA" hits the same bucket.
	if res := attemptLogin(t, e, e.clientAt("198.51.100.99:1"), "VERA", "x password"); res.StatusCode != 429 {
		t.Errorf("case variant not throttled with the same bucket: %d", res.StatusCode)
	}
}

func TestSuccessfulLoginRefundsTheUsernameUnit(t *testing.T) {
	opt, _ := limiters(t, 1000, 1)
	e := newEnv(t, opt)
	e.mkUser("walt", "the right password")
	for i := 0; i < 4; i++ { // would be throttled after one use without the refund
		c := e.clientAt("198.51.100." + strconv.Itoa(20+i) + ":1")
		if res := attemptLogin(t, e, c, "walt", "the right password"); res.StatusCode != 303 {
			t.Fatalf("login %d: %d (a successful login must not drain the username bucket)", i, res.StatusCode)
		}
	}
	// A failure does consume it.
	c := e.clientAt("198.51.100.50:1")
	attemptLogin(t, e, c, "walt", "wrong password")
	if res := attemptLogin(t, e, e.clientAt("198.51.100.51:1"), "walt", "wrong password"); res.StatusCode != 429 {
		t.Fatalf("after one failure: %d, want 429", res.StatusCode)
	}
}

func TestSpoofedForwardedForDoesNotChangeRateLimitIdentity(t *testing.T) {
	opt, _ := limiters(t, 3, 1000)
	e := newEnv(t, opt)
	// Untrusted peer (203.0.113.9): a rotating X-Forwarded-For must not buy fresh buckets.
	c := e.client()
	var last *http.Response
	for i := 0; i < 5; i++ {
		c.hdr.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(100+i))
		last = attemptLogin(t, e, c, "user"+strconv.Itoa(i), "wrong password")
	}
	if last.StatusCode != 429 {
		t.Fatalf("spoofed X-Forwarded-For evaded the limit: %d", last.StatusCode)
	}
	// A trusted proxy (10.0.0.0/8) is believed: distinct forwarded clients have separate buckets.
	proxy := e.clientAt("10.1.2.3:5000")
	for i := 0; i < 6; i++ {
		p := e.clientAt("10.1.2.3:5000")
		p.hdr.Set("X-Forwarded-For", "192.0.2."+strconv.Itoa(1+i))
		if res := attemptLogin(t, e, p, "x"+strconv.Itoa(i), "wrong password"); res.StatusCode != 401 {
			t.Fatalf("forwarded client %d behind a trusted proxy: %d", i, res.StatusCode)
		}
	}
	// ...and the same forwarded client is limited.
	proxy.hdr.Set("X-Forwarded-For", "192.0.2.200")
	var st int
	for i := 0; i < 5; i++ {
		st = attemptLogin(t, e, proxy, "y"+strconv.Itoa(i), "wrong password").StatusCode
	}
	if st != 429 {
		t.Fatalf("a forwarded client behind a trusted proxy was not limited: %d", st)
	}
}

func TestRejectedCSRFIsNotCountedAgainstTheLimiter(t *testing.T) {
	opt, _ := limiters(t, 3, 1000)
	e := newEnv(t, opt)
	c := e.client()
	for i := 0; i < 20; i++ {
		if res := c.post("/login", url.Values{"username": {"a"}, "password": {"b"}}); res.StatusCode != 403 {
			t.Fatalf("%d", res.StatusCode)
		}
	}
	if res := attemptLogin(t, e, c, "a", "wrong password"); res.StatusCode != 401 {
		t.Fatalf("CSRF rejections consumed the login allowance: %d", res.StatusCode)
	}
}

// ---- handler-level races ----

func TestPasswordResetBetweenVerificationAndCommitFailsGenerically(t *testing.T) {
	e := newEnv(t)
	hash, pw := loadFixture(t, "bcrypt_2y_cost10/ascii")
	u := e.mkUser("xena", "placeholder password")
	e.exec(`UPDATE magnetico_web.users SET password_hash = $2 WHERE id = $1`, u.ID, hash)

	newHash, err := e.svc.Hash(tctx(t), "operator picked this")
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	e.store.commitStub = func() {
		once.Do(func() { // the operator resets the password after our verification
			if _, err := e.store.SetPassword(tctx(t), "xena", newHash); err != nil {
				t.Error(err)
			}
		})
	}
	c := e.client()
	c.get("/login")
	res := c.post("/login", c.loginForm("xena", pw))
	if res.StatusCode != 401 || !strings.Contains(c.body, msgLoginFailed) {
		t.Fatalf("login racing with a reset: %d", res.StatusCode)
	}
	var got string
	if err := e.pool.QueryRow(tctx(t), `SELECT password_hash FROM magnetico_web.users WHERE id = $1`, u.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != newHash {
		t.Fatal("the rehash overwrote the operator's password")
	}
	if e.sessions() != 0 {
		t.Fatal("a session exists for a login that lost the race")
	}
	// The old password no longer works, the new one does.
	if res := attemptLogin(t, e, e.client(), "xena", pw); res.StatusCode != 401 {
		t.Fatalf("old password after the reset: %d", res.StatusCode)
	}
	e.client().signIn("xena", "operator picked this")
}

func TestDisableBetweenVerificationAndCommitRefusesTheLogin(t *testing.T) {
	e := newEnv(t)
	e.mkUser("yuri", "the right password")
	var once sync.Once
	e.store.commitStub = func() {
		once.Do(func() {
			if _, err := e.store.Disable(tctx(t), "yuri"); err != nil {
				t.Error(err) // not the test goroutine: Error, not Fatal
			}
		})
	}
	c := e.client()
	c.get("/login")
	if res := c.post("/login", c.loginForm("yuri", "the right password")); res.StatusCode != 401 {
		t.Fatalf("login racing with a disable: %d", res.StatusCode)
	}
	if e.sessions() != 0 {
		t.Fatal("a disabled user obtained a session")
	}
}

func TestTwoConcurrentLegacyLoginsBothSucceedThroughTheBoundedRetry(t *testing.T) {
	e := newEnv(t)
	hash, pw := loadFixture(t, "argon2i_m16384_t2_p4/ascii")
	u := e.mkUser("zed", "placeholder password")
	e.exec(`UPDATE magnetico_web.users SET password_hash = $2 WHERE id = $1`, u.ID, hash)

	// Both requests verify against the legacy hash and rehash before either commits.
	var arrived atomic.Int64
	both := make(chan struct{})
	e.store.commitStub = func() {
		if n := arrived.Add(1); n <= 2 {
			if n == 2 {
				close(both)
			}
			select {
			case <-both:
			case <-time.After(30 * time.Second):
				t.Error("the two logins never met at the commit")
			}
		}
	}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	jars := []*client{e.client(), e.clientAt("203.0.113.10:1")}
	for i := range jars {
		jars[i].get("/login")
	}
	for i := range jars {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = jars[i].post("/login", jars[i].loginForm("zed", pw)).StatusCode
		}()
	}
	wg.Wait()
	if codes[0] != 303 || codes[1] != 303 {
		t.Fatalf("concurrent logins: %v, want both 303 (benign two-tab race)", codes)
	}
	if e.sessions() != 2 {
		t.Fatalf("%d sessions, want 2", e.sessions())
	}
	var got string
	if err := e.pool.QueryRow(tctx(t), `SELECT password_hash FROM magnetico_web.users WHERE id = $1`, u.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == hash || !strings.HasPrefix(got, "$argon2id$") {
		t.Fatalf("hash not upgraded: %q", got)
	}
	if e.svc.rehashes.Load() != 2 {
		t.Errorf("rehashes: %d (each request rehashed once before the race was resolved)", e.svc.rehashes.Load())
	}
}

func TestParallelLoginsGiveDistinctSessions(t *testing.T) {
	e := newEnv(t)
	e.mkUser("amos", "the right password")
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := e.clientAt("203.0.113." + strconv.Itoa(20+i) + ":1")
			c.signIn("amos", "the right password")
			mu.Lock()
			seen[c.cookieValue("__Host-magnetico_session")] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 32 || e.sessions() != 32 {
		t.Fatalf("%d distinct tokens, %d rows", len(seen), e.sessions())
	}
}

func TestRequestsStartedAfterDisableAreRefused(t *testing.T) {
	e := newEnv(t)
	for round := 0; round < 10; round++ {
		name := "bram" + strconv.Itoa(round)
		e.mkUser(name, "the right password")
		c := e.client()
		c.signIn(name, "the right password")
		cookie := *c.jar["__Host-magnetico_session"]

		var disabled atomic.Bool
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := e.client()
				r.jar[cookie.Name] = &cookie
				for {
					select {
					case <-stop:
						return
					default:
					}
					after := disabled.Load()
					res := r.get("/")
					if after && res.StatusCode == 200 {
						t.Error("a request started after Disable returned was served")
						return
					}
				}
			}()
		}
		time.Sleep(10 * time.Millisecond)
		if _, err := e.store.Disable(tctx(t), name); err != nil {
			t.Fatal(err)
		}
		disabled.Store(true)
		time.Sleep(10 * time.Millisecond)
		close(stop)
		wg.Wait()
		if t.Failed() {
			return
		}
	}
}

// ---- redaction ----

func TestNoSecretsInLogsOrErrorResponses(t *testing.T) {
	opt, clk := limiters(t, 6, 1000)
	e := newEnv(t, opt)
	e.mkUser("conrad", "very-secret-password-1")
	secrets := []string{"very-secret-password-1", "wrong-secret-password-2"}

	collect := func(c *client) {
		for _, ck := range c.jar {
			secrets = append(secrets, ck.Value)
		}
		for _, m := range tokenRe.FindAllStringSubmatch(c.body, -1) {
			secrets = append(secrets, m[1])
		}
	}
	c := e.client()
	c.get("/login?next=%2Fprivate-path-marker")
	collect(c)
	tok := c.token()
	c.post("/login", formFor(tok, "conrad", "wrong-secret-password-2"))
	collect(c)
	c.post("/login", url.Values{"username": {"conrad"}, "password": {"very-secret-password-1"}}) // CSRF failure
	ok := formFor(tok, "conrad", "very-secret-password-1")
	ok.Set("next", "/private-path-marker")
	c.post("/login", ok)
	collect(c)
	c.get("/")
	collect(c)
	c.post("/logout", url.Values{"csrf_token": {c.token()}})
	e.store.lookupErr.Store(&errBoom)
	c.get("/")
	for i := 0; i < 8; i++ { // reach 429
		d := e.client()
		d.get("/login")
		d.post("/login", d.loginForm("conrad", "wrong-secret-password-2"))
	}
	_ = clk

	logs := e.log.String()
	for _, s := range secrets {
		if s != "" && strings.Contains(logs, s) {
			t.Errorf("a secret appears in the logs: %.12q…", s)
		}
	}
	for _, marker := range []string{"conrad", "private-path-marker", "password=", "csrf_token", "Set-Cookie", "next="} {
		if strings.Contains(logs, marker) {
			t.Errorf("the logs contain %q", marker)
		}
	}
	if !strings.Contains(logs, "user_id=") || !strings.Contains(logs, "login succeeded") {
		t.Error("expected the login and user_id to be logged")
	}
}

// ---- purger, startup ----

func TestPurgerRemovesExpiredRowsAndStopsPromptly(t *testing.T) {
	e := newEnv(t)
	u := e.mkUser("dora", "the right password")
	live := e.client()
	live.signIn("dora", "the right password")
	e.exec(`INSERT INTO magnetico_web.sessions (token_hash, user_id, created_at, last_seen_at, expires_at, remember)
        VALUES (decode(repeat('ab', 32), 'hex'), $1, now() - interval '200 hours', now() - interval '200 hours', now() - interval '1 hour', false)`, u.ID)
	if _, err := e.store.IssuePasswordLink(tctx(t), "dora"); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE magnetico_web.one_time_tokens SET created_at = now() - interval '3 hours', expires_at = now() - interval '2 hours'`)

	a := &Auth{cfg: Config{Store: e.store, Lifetimes: testLifetimes, PurgeFirst: 10 * time.Millisecond, PurgeEvery: 20 * time.Millisecond}, log: e.srv.Logger()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Background(ctx); close(done) }()
	deadline := time.Now().Add(15 * time.Second)
	for e.sessions() != 1 || e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens`) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("not purged: sessions=%d links=%d", e.sessions(), e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens`))
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the purger did not stop after cancellation")
	}
	if res := live.get("/"); res.StatusCode != 200 {
		t.Fatal("the purger removed a live session")
	}
}

type failingPrepare struct{ *spyVerifier }

func (failingPrepare) Prepare(context.Context) error { return errors.New("prepare failed") }

func TestStartupPreparesBeforeListeningAndFailsClosed(t *testing.T) {
	e := newEnv(t)
	// Fresh server whose Prepare fails: Run must return before listening.
	srv, err := server.New(server.Config{
		PublicURL: mustURL("https://magnet.example.org"), RequestTimeout: time.Second, SearchTimeout: time.Second,
		ShutdownTimeout: time.Second, Logger: e.srv.Logger(),
	}, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Install(srv, Config{Store: e.store, Verifier: failingPrepare{e.svc}, Lifetimes: testLifetimes}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "prepare failed") {
		t.Fatalf("Run: %v, want the Prepare failure", err)
	}

	// The dev warning is logged by the startup hook only in development mode.
	if err := e.auth.startup(tctx(t)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.log.String(), "development mode") {
		t.Error("production mode logged the development warning")
	}
	d := newEnv(t, func(o *envOpts) { o.dev = true })
	if err := d.auth.startup(tctx(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.log.String(), "development mode") {
		t.Error("development mode must log its warning at startup")
	}
}

func TestInstallValidatesItsConfiguration(t *testing.T) {
	e := newEnv(t)
	srv, _ := server.New(server.Config{PublicURL: mustURL("https://m.example"), RequestTimeout: time.Second, SearchTimeout: time.Second, ShutdownTimeout: time.Second, Logger: e.srv.Logger()}, fakeBackend{})
	if _, err := Install(srv, Config{}); err == nil {
		t.Error("missing store/verifier accepted")
	}
	if _, err := Install(srv, Config{Store: e.store, Verifier: e.svc, Lifetimes: accounts.SessionLifetimes{}}); err == nil {
		t.Error("zero lifetimes accepted")
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
