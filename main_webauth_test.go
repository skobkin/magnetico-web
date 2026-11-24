package main

// Real-binary tests of the W3 authentication layer: login, logout, sessions,
// CSRF, rehash, rate limiting and cookie attributes against `magnetico web`.
// Disposable PostgreSQL only; loopback only; synthetic users.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// webAuthRoles creates the documented runtime and operator roles (the exact
// GRANT statements of docs/web-deployment.md) and returns their DSNs, so every
// statement the login path runs is proven to fit the documented minimum.
func webAuthRoles(t *testing.T, dsn string) (rtDSN, opDSN string) {
	t.Helper()
	cl := pgtest.IntegrationCluster(t)
	suffix := pgtest.RandomSuffix()
	rt, op := "lrt_"+suffix, "lop_"+suffix
	admin, err := pgx.Connect(context.Background(), cl.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	for _, r := range []string{rt, op} {
		if _, err := admin.Exec(context.Background(), "CREATE ROLE "+pgtest.QuoteIdent(r)+" LOGIN"); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := url.Parse(dsn)
	dbName := strings.TrimPrefix(u.Path, "/")
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), cl.AdminDSN)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgtest.QuoteIdent(dbName)+" WITH (FORCE)")
		for _, r := range []string{op, rt} {
			_, _ = c.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgtest.QuoteIdent(r))
		}
	})
	db, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	grants := strings.NewReplacer("magnetico_web_rt", pgtest.QuoteIdent(rt), "magnetico_web_op", pgtest.QuoteIdent(op)).Replace(docsGrants(t))
	if _, err := db.Exec(context.Background(), grants); err != nil {
		t.Fatalf("documented grants: %v", err)
	}
	as := func(role string) string {
		ru := *u
		ru.User = url.UserPassword(role, "role-secret-pw")
		return ru.String()
	}
	return as(rt), as(op)
}

type runningWeb struct {
	addr   string
	cmd    *exec.Cmd
	exited chan error
	out    *syncBuf // read while the process may still run: goroutine-safe
}

// startWeb launches `magnetico web` and waits for /readyz.
func startWeb(t *testing.T, bin, dsn, publicURLScheme string, extraEnv ...string) *runningWeb {
	t.Helper()
	addr := freeLoopbackAddr(t)
	cmd := exec.Command(bin, "web")
	pub := publicURLScheme + "://" + addr
	if publicURLScheme == "https" {
		pub = "https://magnet.example.org"
	}
	cmd.Env = webEnv(append([]string{"MAGNETICO_WEB_DATABASE=" + dsn, "MAGNETICO_WEB_LISTEN=" + addr, "MAGNETICO_WEB_PUBLIC_URL=" + pub}, extraEnv...)...)
	out := &syncBuf{}
	cmd.Stderr, cmd.Stdout = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := &runningWeb{addr: addr, cmd: cmd, exited: make(chan error, 1), out: out}
	go func() { w.exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(30 * time.Second)
	for {
		if resp, err := http.Get("http://" + addr + "/readyz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return w
			}
		}
		select {
		case err := <-w.exited:
			t.Fatalf("web exited early: %v\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stop sends SIGTERM and requires a clean exit (status 0).
func (w *runningWeb) stop(t *testing.T) {
	t.Helper()
	if err := w.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-w.exited:
		if err != nil {
			t.Fatalf("exit after SIGTERM: %v\n%s", err, w.out.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("no exit after SIGTERM:\n%s", w.out.String())
	}
}

type webBrowser struct {
	t    *testing.T
	base string
	c    *http.Client
	body string
	// host, when set, is sent as the Host header of every request (the
	// connection still goes to base): a client or proxy that forges it.
	host string
}

func newBrowser(t *testing.T, addr string) *webBrowser {
	jar, _ := cookiejar.New(nil)
	return &webBrowser{t: t, base: "http://" + addr, c: &http.Client{
		Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *webBrowser) do(req *http.Request) *http.Response {
	b.t.Helper()
	if b.host != "" {
		req.Host = b.host
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	b.body = string(raw)
	return resp
}

func (b *webBrowser) get(path string, hdr ...string) *http.Response {
	req, _ := http.NewRequest("GET", b.base+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return b.do(req)
}

func (b *webBrowser) post(path string, form url.Values, hdr ...string) *http.Response {
	req, _ := http.NewRequest("POST", b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return b.do(req)
}

var binTokenRe = regexp.MustCompile(`name="csrf_token" value="([^"]*)"`)

func (b *webBrowser) token() string {
	m := binTokenRe.FindStringSubmatch(b.body)
	if m == nil {
		b.t.Fatalf("no csrf token in page:\n%s", b.body)
	}
	return m[1]
}

func (b *webBrowser) login(user, pw string) *http.Response {
	b.t.Helper()
	b.get("/login")
	return b.post("/login", url.Values{"username": {user}, "password": {pw}, "csrf_token": {b.token()}})
}

func sqlCount(t *testing.T, dsn, q string, args ...any) int {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var n int
	if err := c.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func sqlExec(t *testing.T, dsn, q string, args ...any) {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	if _, err := c.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func phpFixture(t *testing.T, id string) (hash, pw string) {
	t.Helper()
	raw, err := os.ReadFile("internal/web/password/testdata/php/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			ID, Hash string
			PW       string `json:"password_b64"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Cases {
		if c.ID == id {
			b, err := base64.StdEncoding.DecodeString(c.PW)
			if err != nil {
				t.Fatal(err)
			}
			return c.Hash, string(b)
		}
	}
	t.Fatalf("fixture %s missing", id)
	return
}

// The full browser flow through the real binary, as the documented minimum
// runtime role: sign in, private page, disable/enable by the operator CLI,
// sign out, legacy hash upgrade, and a clean SIGTERM.
func TestWebBinaryLoginFlow(t *testing.T) {
	bin := buildBinary(t)
	dsn := webTarget(t)
	if out, code := runWebSchemaInit(t, bin, dsn, "web-schema-init"); code != 0 {
		t.Fatalf("web-schema-init: exit %d: %s", code, out)
	}
	rtDSN, opDSN := webAuthRoles(t, dsn)
	const pw = "binary-flow-password-1"
	cli := func(stdin string, args ...string) string {
		out, errOut, code := runWebAdminBinary(t, bin, opDSN, stdin, nil, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
		return out
	}
	cli("", "user", "add", "alice")
	cli(pw+"\n", "user", "set-password", "alice")

	w := startWeb(t, bin, rtDSN, "http")
	b := newBrowser(t, w.addr)

	// Anonymous: the root redirects to /login, unknown paths are 404.
	if resp := b.get("/"); resp.StatusCode != 303 || resp.Header.Get("Location") != "/login?next=%2F" {
		t.Fatalf("anonymous /: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := b.get("/torrents"); resp.StatusCode != 303 || resp.Header.Get("Location") != "/login?next=%2Ftorrents" {
		t.Fatalf("anonymous /torrents: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Wrong password, then the right one.
	if resp := b.login("alice", "not-the-password"); resp.StatusCode != 401 || !strings.Contains(b.body, "Wrong username or password") {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp := b.post("/login", url.Values{"username": {"alice"}, "password": {pw}, "csrf_token": {b.token()}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/" {
		t.Fatalf("login: %d %q\n%s", resp.StatusCode, resp.Header.Get("Location"), b.body)
	}
	if resp := b.get("/torrents"); resp.StatusCode != 200 || !strings.Contains(b.body, `<span class="header-user">alice</span>`) {
		t.Fatalf("home: %d\n%s", resp.StatusCode, b.body)
	}
	if n := sqlCount(t, dsn, `SELECT count(*) FROM magnetico_web.sessions`); n != 1 {
		t.Fatalf("%d session rows, want 1", n)
	}
	if n := sqlCount(t, dsn, `SELECT count(*) FROM magnetico_web.users WHERE last_login_at IS NOT NULL`); n != 1 {
		t.Fatal("last_login_at not recorded")
	}

	// Operator disables the account: the very next request is anonymous; enabling
	// does not bring the session back.
	cli("", "user", "disable", "alice")
	if resp := b.get("/"); resp.StatusCode != 303 {
		t.Fatalf("after disable: %d", resp.StatusCode)
	}
	cli("", "user", "enable", "alice")
	if resp := b.get("/"); resp.StatusCode != 303 {
		t.Fatalf("after enable the old session came back: %d", resp.StatusCode)
	}
	if n := sqlCount(t, dsn, `SELECT count(*) FROM magnetico_web.sessions`); n != 0 {
		t.Fatalf("%d session rows after disable", n)
	}

	// Sign in again, then out: POST with the session token; GET is refused.
	if resp := b.login("alice", pw); resp.StatusCode != 303 {
		t.Fatalf("second login: %d", resp.StatusCode)
	}
	b.get("/torrents")
	tok := b.token()
	if resp := b.get("/logout"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /logout: %d", resp.StatusCode)
	}
	if resp := b.post("/logout", url.Values{"csrf_token": {"wrong"}}); resp.StatusCode != 403 {
		t.Fatalf("logout with a wrong token: %d", resp.StatusCode)
	}
	if resp := b.post("/logout", url.Values{"csrf_token": {tok}}, "Sec-Fetch-Site", "cross-site"); resp.StatusCode != 403 {
		t.Fatalf("cross-site logout: %d", resp.StatusCode)
	}
	if resp := b.post("/logout", url.Values{"csrf_token": {tok}}); resp.StatusCode != 303 || resp.Header.Get("Location") != "/login" {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if n := sqlCount(t, dsn, `SELECT count(*) FROM magnetico_web.sessions`); n != 0 {
		t.Fatal("the session survives logout")
	}
	if resp := b.get("/"); resp.StatusCode != 303 {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}

	// Legacy PHP bcrypt hash: login works and the hash becomes current argon2id.
	legacy, legacyPW := phpFixture(t, "bcrypt_2y_cost10/ascii")
	sqlExec(t, dsn, `UPDATE magnetico_web.users SET password_hash = $1 WHERE username = 'alice'`, legacy)
	if resp := b.login("alice", legacyPW); resp.StatusCode != 303 {
		t.Fatalf("legacy login: %d\n%s", resp.StatusCode, b.body)
	}
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	var upgraded string
	if err := c.QueryRow(context.Background(), `SELECT password_hash FROM magnetico_web.users WHERE username = 'alice'`).Scan(&upgraded); err != nil {
		t.Fatal(err)
	}
	c.Close(context.Background())
	if !strings.HasPrefix(upgraded, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("legacy hash not upgraded to the current parameters: %.40s", upgraded)
	}

	// Cookies in dev mode: no __Host- prefix, no Secure.
	for _, ck := range b.c.Jar.Cookies(&url.URL{Scheme: "http", Host: w.addr}) {
		if strings.HasPrefix(ck.Name, "__Host-") {
			t.Errorf("dev mode used a __Host- cookie: %s", ck.Name)
		}
	}

	w.stop(t)
	out := w.out.String()
	if !strings.Contains(out, "development mode") {
		t.Error("the development-mode warning was not logged at startup")
	}
	for _, secret := range []string{pw, legacyPW, "not-the-password", "role-secret-pw", tok} {
		if strings.Contains(out, secret) {
			t.Errorf("a secret appears in the service output: %.8q…", secret)
		}
	}
	for _, ck := range b.c.Jar.Cookies(&url.URL{Scheme: "http", Host: w.addr}) {
		if strings.Contains(out, ck.Value) {
			t.Errorf("a cookie value appears in the service output")
		}
	}
}

// Production cookie mode through the binary: an https public URL (TLS at the
// proxy) with a loopback listener. The test plays the proxy: it speaks plain
// HTTP and sends cookies by hand, because a client will not send Secure
// cookies over http.
func TestWebBinaryProductionCookieAttributes(t *testing.T) {
	bin := buildBinary(t)
	dsn := webTarget(t)
	if out, code := runWebSchemaInit(t, bin, dsn, "web-schema-init"); code != 0 {
		t.Fatalf("web-schema-init: exit %d: %s", code, out)
	}
	const pw = "production-mode-password"
	runWebAdminBinary(t, bin, dsn, "", nil, "user", "add", "pia")
	if _, e, code := runWebAdminBinary(t, bin, dsn, pw+"\n", nil, "user", "set-password", "pia"); code != 0 {
		t.Fatalf("set-password: %s", e)
	}
	w := startWeb(t, bin, dsn, "https")
	base := "http://" + w.addr
	hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(req *http.Request) (*http.Response, string) {
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	req, _ := http.NewRequest("GET", base+"/login", nil)
	resp, body := do(req)
	var pre string
	for _, v := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(v, "__Host-magnetico_presession=") {
			pre = v
		}
	}
	for _, want := range []string{"; Path=/", "; HttpOnly", "; Secure", "; SameSite=Strict", "; Max-Age=3600"} {
		if !strings.Contains(pre, want) {
			t.Errorf("pre-session cookie lacks %q: %s", want, pre)
		}
	}
	if strings.Contains(pre, "Domain") {
		t.Errorf("pre-session cookie has a Domain: %s", pre)
	}
	preKV := strings.SplitN(pre, ";", 2)[0]
	tok := binTokenRe.FindStringSubmatch(body)[1]

	form := url.Values{"username": {"pia"}, "password": {pw}, "csrf_token": {tok}}
	req, _ = http.NewRequest("POST", base+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", preKV)
	resp, _ = do(req)
	if resp.StatusCode != 303 {
		t.Fatalf("production login: %d", resp.StatusCode)
	}
	var sess string
	for _, v := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(v, "__Host-magnetico_session=") {
			sess = v
		}
	}
	for _, want := range []string{"; Path=/", "; HttpOnly", "; Secure", "; SameSite=Lax"} {
		if !strings.Contains(sess, want) {
			t.Errorf("session cookie lacks %q: %s", want, sess)
		}
	}
	if strings.Contains(sess, "Domain") || strings.Contains(sess, "Max-Age") || strings.Contains(sess, "Expires") {
		t.Errorf("an ordinary session cookie must have no Domain/Expires/Max-Age: %s", sess)
	}
	req, _ = http.NewRequest("GET", base+"/torrents", nil)
	req.Header.Set("Cookie", strings.SplitN(sess, ";", 2)[0])
	if resp, body = do(req); resp.StatusCode != 200 || !strings.Contains(body, `<span class="header-user">pia</span>`) {
		t.Fatalf("home with the production cookie: %d", resp.StatusCode)
	}
	if resp.Header.Get("Strict-Transport-Security") == "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers: %v", resp.Header)
	}
	w.stop(t)
}

// A spoofed X-Forwarded-For from an untrusted peer (the loopback test client
// is not a trusted proxy) cannot evade the per-IP limit.
func TestWebBinaryRateLimitIgnoresSpoofedForwardedFor(t *testing.T) {
	bin := buildBinary(t)
	dsn := webTarget(t)
	if out, code := runWebSchemaInit(t, bin, dsn, "web-schema-init"); code != 0 {
		t.Fatalf("web-schema-init: exit %d: %s", code, out)
	}
	w := startWeb(t, bin, dsn, "http")
	b := newBrowser(t, w.addr)
	b.get("/login")
	tok := b.token()
	limited := -1
	for i := 0; i < 40; i++ {
		resp := b.post("/login", url.Values{"username": {"ghost" + strconv.Itoa(i)}, "password": {"whatever-password"}, "csrf_token": {tok}},
			"X-Forwarded-For", "198.51.100."+strconv.Itoa(1+i%250))
		if resp.StatusCode == 429 {
			if resp.Header.Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
			limited = i
			break
		}
		if resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	if limited != 20 {
		t.Fatalf("limited at attempt %d, want 20 (the per-IP burst): the spoofed header changed the identity", limited)
	}
	w.stop(t)
	if strings.Contains(w.out.String(), "ghost") || strings.Contains(w.out.String(), "whatever-password") {
		t.Error("a submitted credential appears in the service output")
	}
}
