package main

// W9 security regression (task 07J, stage S4). The web service runs in-process
// with the exact production wiring (assembleWeb), so every sweep walks the
// route table that actually serves requests: a route added without an
// expectation here fails the test.
//
// Expected behaviour is derived from each route's access class and the
// verified middleware order (internal/web/server/server.go, internal/web/auth/
// guards.go):
//
//	cross-origin protection → limit body → mux (404/405) → route deadline →
//	class guard → handler
//
//	Public        GET only, no credential.
//	PublicForm    unsafe methods need the pre-session CSRF token.
//	Session       the session is resolved first (anonymous: 303 to /login), and
//	              only then is the CSRF token of the session checked.
//	SessionJSON   GET only; anonymous: 401 application/problem+json.
//	APIToken      GET only; Bearer API token only.
//	Feed          GET only; RSS token only, per-route transports.
//
// Disposable PostgreSQL only; synthetic accounts; loopback only.

import (
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/opflags"
)

const (
	secUserPW  = "w9-security-user-password-1"
	secAdminPW = "w9-security-admin-password-2"
)

// w9Sec is an initialized database with two accounts (a user and an admin) and
// an in-process web service on it.
type w9Sec struct {
	t   *testing.T
	bin string
	dsn string
	web *inprocWeb
	// uniqueClients gives every raw request its own client address through a
	// trusted proxy (X-Forwarded-For from 127.0.0.1, which the service then
	// trusts), so a sweep with hundreds of deliberately failing requests does
	// not exhaust the per-address failure limiter it is not testing.
	uniqueClients bool
	reqSeq        int
}

// trustLoopbackProxy makes the service trust X-Forwarded-For from the test
// client (127.0.0.1).
func trustLoopbackProxy(o *opflags.WebOptions) {
	o.TrustedProxyPrefixes = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
}

func newW9Sec(t *testing.T, mod func(o *opflags.WebOptions)) *w9Sec {
	t.Helper()
	s := &w9Sec{t: t, bin: buildBinary(t), dsn: w9AppAccount(t)}
	for _, cmd := range []string{"schema-init", "web-schema-init"} {
		if out, code := runWebSchemaInit(t, s.bin, s.dsn, cmd); code != 0 {
			t.Fatalf("migrate %s: exit %d: %s", cmd, code, out)
		}
	}
	s.addUser("alice", secUserPW, "")
	s.addUser("boss", secAdminPW, "admin")
	s.web = startInprocWeb(t, s.dsn, mod)
	return s
}

// addUser creates an account through the operator CLI.
func (s *w9Sec) addUser(name, pw, role string) {
	s.t.Helper()
	steps := []struct {
		stdin string
		args  []string
	}{{"", []string{"user", "add", name}}, {pw + "\n", []string{"user", "set-password", name}}}
	if role != "" {
		steps = append(steps, struct {
			stdin string
			args  []string
		}{"", []string{"user", "set-role", name, role}})
	}
	for _, c := range steps {
		if out, errOut, code := runWebAdminBinary(s.t, s.bin, s.dsn, c.stdin, nil, c.args...); code != 0 {
			s.t.Fatalf("web %v: %d %s%s", c.args, code, out, errOut)
		}
	}
}

// login returns a signed-in browser.
func (s *w9Sec) login(user, pw string) *webBrowser {
	s.t.Helper()
	b := newBrowser(s.t, s.web.addr)
	if resp := b.login(user, pw); resp.StatusCode != 303 {
		s.t.Fatalf("login %s: %d\n%s", user, resp.StatusCode, b.body)
	}
	return b
}

type secResp struct {
	status int
	hdr    http.Header
	body   string
}

// raw sends one request with no cookie jar and no redirect following.
func (s *w9Sec) raw(method, path string, hdr map[string]string, body string) secResp {
	s.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://"+s.web.addr+path, rd)
	if err != nil {
		s.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if s.uniqueClients {
		s.reqSeq++
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.%d.%d.%d", (s.reqSeq>>16)&255, (s.reqSeq>>8)&255, s.reqSeq&255))
	}
	for k, v := range hdr {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	c := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return secResp{resp.StatusCode, resp.Header, string(b)}
}

// concrete substitutes plausible values for the path parameters of a pattern's
// path.
func concrete(path string) string {
	return strings.NewReplacer(
		"{code}", "0123456789abcdef0123456789abcdef",
		"{token}", strings.Repeat("x", 43),
		"{id}", "424242",
		"{infohash}", strings.Repeat("a", 40),
		"{file}", "missing-file.css",
		"{$}", "",
	).Replace(path)
}

func splitPattern(p string) (method, path string) {
	m, rest, _ := strings.Cut(p, " ")
	return m, rest
}

// bearerHdr and friends build credential headers.
func bearerHdr(tok string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tok}
}

func isProblemJSON(r secResp) bool {
	return strings.HasPrefix(r.hdr.Get("Content-Type"), "application/problem+json")
}

// --- anonymous requests and method restrictions, by access class ---

func TestW9RouteTableAnonymousAndMethodRestrictions(t *testing.T) {
	s := newW9Sec(t, nil)
	routes := s.web.routes()
	if len(routes) < 40 {
		t.Fatalf("suspiciously small route table: %d", len(routes))
	}
	methodsOf := map[string]map[string]bool{}
	for _, r := range routes {
		m, p := splitPattern(r.Pattern)
		if methodsOf[p] == nil {
			methodsOf[p] = map[string]bool{}
		}
		methodsOf[p][m] = true
		if m == "GET" {
			methodsOf[p]["HEAD"] = true
		}
	}

	for _, r := range routes {
		method, pattern := splitPattern(r.Pattern)
		path := concrete(pattern)
		label := r.Access.String() + " " + r.Pattern
		var res secResp
		if method == "POST" {
			res = s.raw("POST", path, nil, "x=1")
		} else {
			res = s.raw(method, path, nil, "")
		}
		switch r.Access {
		case server.Public:
			// No credential is asked for; the answer is the resource itself.
			if res.status == 401 || res.status == 403 || (res.status == 303 && strings.HasPrefix(res.hdr.Get("Location"), "/login")) {
				t.Errorf("%s: a public route demanded authentication: %d", label, res.status)
			}
		case server.PublicForm:
			if method == "POST" {
				// No pre-session token: the CSRF guard refuses, before any handler.
				if res.status != 403 {
					t.Errorf("%s: %d, want the CSRF refusal 403", label, res.status)
				}
			} else if res.status != 200 && res.status != 400 && res.status != 404 {
				t.Errorf("%s: %d, want a form or a generic page", label, res.status)
			}
		case server.Session:
			// The session is resolved before the CSRF token is looked at, so an
			// anonymous request is sent to sign in, never told about CSRF.
			loc := res.hdr.Get("Location")
			wantLoc := loc == "/login"
			if method == "GET" {
				wantLoc = strings.HasPrefix(loc, "/login")
			}
			if res.status != 303 || !wantLoc {
				t.Errorf("%s: %d %q, want 303 to /login", label, res.status, loc)
			}
			if method == "POST" && strings.Contains(loc, "next=") {
				t.Errorf("%s: an anonymous POST must not leave a next target: %q", label, loc)
			}
		case server.SessionJSON, server.APIToken, server.Feed:
			if res.status != 401 {
				t.Errorf("%s: %d, want 401", label, res.status)
			}
			if r.Access != server.Feed || strings.Contains(pattern, "/api/v2/") {
				if !isProblemJSON(res) {
					t.Errorf("%s: 401 is not application/problem+json: %q", label, res.hdr.Get("Content-Type"))
				}
			}
		default:
			t.Errorf("%s: no expectation for access class %v", label, r.Access)
		}
	}

	// Method restrictions: every method that is not registered for a path is
	// answered 405 by the router, before any guard (so an anonymous caller learns
	// nothing about authentication from it).
	paths := make([]string, 0, len(methodsOf))
	for p := range methodsOf {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
			if methodsOf[p][m] {
				continue
			}
			body := ""
			if m == "POST" || m == "PUT" || m == "PATCH" {
				body = "x=1"
			}
			res := s.raw(m, concrete(p), nil, body)
			if res.status != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %d, want 405", m, p, res.status)
			}
			if res.status == http.StatusMethodNotAllowed && res.hdr.Get("Allow") == "" {
				t.Errorf("%s %s: 405 without Allow", m, p)
			}
		}
	}

	// Unknown paths: 404 for anyone, except below /api/v2/ where the bearer
	// guard answers first (401 anonymous, 404 problem+json authenticated).
	for _, p := range []string{"/nope", "/torrents/", "/account/", "/static/", "/api/v3/torrents", "/.git/config", "/%2e%2e/etc/passwd"} {
		if res := s.raw("GET", p, nil, ""); res.status != 404 && !(res.status == 303 && strings.HasPrefix(res.hdr.Get("Location"), "/login")) && res.status != 400 {
			t.Errorf("GET %s: %d, want 404 (or the sign-in redirect of a guarded subtree, or 400 for a malformed path)", p, res.status)
		}
	}
	if res := s.raw("GET", "/api/v2/does-not-exist", nil, ""); res.status != 401 || !isProblemJSON(res) {
		t.Errorf("anonymous GET /api/v2/does-not-exist: %d", res.status)
	}
}

// --- authenticated sessions: CSRF and cross-origin ---

func TestW9RouteTableCSRFAndCrossOrigin(t *testing.T) {
	s := newW9Sec(t, nil)
	b := s.login("alice", secUserPW)
	b.get("/account")
	good := b.token()
	other := s.login("boss", secAdminPW)
	other.get("/account")
	foreign := other.token() // a valid token, but of another session

	origin := "http://" + s.web.addr
	var sessionPosts, publicPosts int
	for _, r := range s.web.routes() {
		method, pattern := splitPattern(r.Pattern)
		if method != "POST" || (r.Access != server.Session && r.Access != server.PublicForm) {
			continue
		}
		path := concrete(pattern)
		if r.Access == server.Session && path == "/logout" {
			continue // exercised last, on a throwaway session
		}
		label := r.Access.String() + " " + r.Pattern

		if r.Access == server.Session {
			sessionPosts++
			post := func(token string, hdr ...string) *http.Response {
				f := url.Values{}
				if token != "" {
					f.Set("csrf_token", token)
				}
				return b.post(path, f, hdr...)
			}
			// No token, an invalid token, a token of another session: 403.
			for name, tok := range map[string]string{"missing": "", "invalid": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "foreign-session": foreign} {
				if resp := post(tok); resp.StatusCode != 403 {
					t.Errorf("%s with a %s CSRF token: %d, want 403", label, name, resp.StatusCode)
				}
			}
			// A GET-style token in the query string is not a token.
			if resp := b.post(path+"?csrf_token="+good, url.Values{}); resp.StatusCode != 403 {
				t.Errorf("%s with the token in the query string: %d, want 403", label, resp.StatusCode)
			}
			// Cross-origin, even with a valid cookie and a valid token: refused
			// before the session is looked at.
			for _, hdr := range [][]string{{"Origin", "http://evil.example"}, {"Sec-Fetch-Site", "cross-site"}, {"Origin", "null"}} {
				if resp := post(good, hdr...); resp.StatusCode != 403 {
					t.Errorf("%s cross-origin %v: %d, want 403", label, hdr, resp.StatusCode)
				}
			}
			// Same origin with the right token reaches the handler: whatever it
			// answers (400, 404, 409, 303, 200), it is neither a refusal for
			// CSRF/authentication nor a sign-in redirect.
			resp := post(good, "Origin", origin, "Sec-Fetch-Site", "same-origin")
			if resp.StatusCode == 403 || resp.StatusCode == 401 || strings.HasPrefix(resp.Header.Get("Location"), "/login") {
				t.Errorf("%s with a valid token: %d %q", label, resp.StatusCode, resp.Header.Get("Location"))
			}
		}
		if r.Access == server.PublicForm {
			publicPosts++
			anon := newBrowser(t, s.web.addr)
			for _, hdr := range [][]string{{"Origin", "http://evil.example"}, {"Sec-Fetch-Site", "cross-site"}} {
				if resp := anon.post(path, url.Values{"x": {"1"}}, hdr...); resp.StatusCode != 403 {
					t.Errorf("%s cross-origin %v: %d, want 403", label, hdr, resp.StatusCode)
				}
			}
		}
	}
	if sessionPosts < 9 || publicPosts < 5 {
		t.Fatalf("the sweep saw %d session and %d public-form POST routes", sessionPosts, publicPosts)
	}

	// The pre-session token of a rendered form passes the guard (the login form
	// answers with its own error, not with a CSRF refusal), and a token of the
	// wrong pre-session cookie does not.
	anon := newBrowser(t, s.web.addr)
	anon.get("/login")
	pre := anon.token()
	if resp := anon.post("/login", url.Values{"username": {"nobody"}, "password": {"x"}, "csrf_token": {pre}}); resp.StatusCode == 403 {
		t.Fatalf("a valid pre-session token was refused: %d", resp.StatusCode)
	}
	if resp := newBrowser(t, s.web.addr).post("/login", url.Values{"username": {"nobody"}, "password": {"x"}, "csrf_token": {pre}}); resp.StatusCode != 403 {
		t.Fatalf("a pre-session token without its cookie: %d, want 403", resp.StatusCode)
	}

	// Logout is a POST with the CSRF token only: a GET does nothing.
	victim := s.login("alice", secUserPW)
	if resp := victim.get("/logout"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /logout: %d, want 405", resp.StatusCode)
	}
	if resp := victim.get("/account"); resp.StatusCode != 200 {
		t.Fatalf("session after the refused logout: %d", resp.StatusCode)
	}
	if resp := victim.post("/logout", url.Values{"csrf_token": {victim.token()}}, "Origin", "http://evil.example"); resp.StatusCode != 403 {
		t.Errorf("cross-origin logout: %d", resp.StatusCode)
	}
	if resp := victim.get("/account"); resp.StatusCode != 200 {
		t.Fatalf("a cross-origin logout ended the session: %d", resp.StatusCode)
	}
	victim.get("/account")
	if resp := victim.post("/logout", url.Values{"csrf_token": {victim.token()}}); resp.StatusCode != 303 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp := victim.get("/account"); resp.StatusCode != 303 {
		t.Fatalf("session after logout: %d", resp.StatusCode)
	}
}

// --- an administrator has no extra web capability ---

func TestW9AdministratorHasNoExtraWebCapability(t *testing.T) {
	s := newW9Sec(t, nil)
	user := s.login("alice", secUserPW)
	admin := s.login("boss", secAdminPW)
	if n := sqlCount(t, s.dsn, `SELECT count(*) FROM magnetico_web.users WHERE role = 'admin'`); n != 1 {
		t.Fatalf("admins: %d", n)
	}
	for _, r := range s.web.routes() {
		method, pattern := splitPattern(r.Pattern)
		if r.Access != server.Session && r.Access != server.SessionJSON {
			continue
		}
		path := concrete(pattern)
		if path == "/logout" {
			continue
		}
		call := func(b *webBrowser) (int, string) {
			var resp *http.Response
			if method == "POST" {
				b.get("/account")
				resp = b.post(path, url.Values{"csrf_token": {b.token()}})
			} else {
				resp = b.get(path)
			}
			return resp.StatusCode, resp.Header.Get("Location")
		}
		us, ul := call(user)
		as, al := call(admin)
		if us != as || ul != al {
			t.Errorf("%s: user %d %q vs admin %d %q", r.Pattern, us, ul, as, al)
		}
	}
	// No route or page mentions an administration capability.
	for _, p := range []string{"/account", "/torrents"} {
		admin.get(p)
		if strings.Contains(strings.ToLower(admin.body), "administration") || strings.Contains(admin.body, "/admin") {
			t.Errorf("%s shows an administration surface to an administrator", p)
		}
	}
	for _, p := range []string{"/admin", "/admin/", "/account/admin", "/users", "/api/v2/admin"} {
		if res := admin.get(p); res.StatusCode == 200 {
			t.Errorf("admin GET %s: 200", p)
		}
	}
}
