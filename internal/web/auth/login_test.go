package auth

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/password"
)

func cookieByName(res *http.Response, name string) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestLoginLogoutFlowInProductionMode(t *testing.T) {
	e := newEnv(t)
	u := e.mkUser("alice", "correct horse battery")
	c := e.client()

	res := c.get("/login")
	if res.StatusCode != 200 || !strings.Contains(c.body, `name="username"`) {
		t.Fatalf("GET /login: %d", res.StatusCode)
	}
	pre := cookieByName(res, "__Host-magnetico_presession")
	if pre == nil || !pre.Secure || !pre.HttpOnly || pre.SameSite != http.SameSiteStrictMode || pre.Path != "/" || pre.Domain != "" || pre.MaxAge != 3600 {
		t.Fatalf("pre-session cookie attributes: %+v", pre)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Error("login page must be no-store")
	}

	res = c.post("/login", c.loginForm("alice", "correct horse battery"))
	if res.StatusCode != 303 || res.Header.Get("Location") != "/" {
		t.Fatalf("POST /login: %d → %q\n%s", res.StatusCode, res.Header.Get("Location"), c.body)
	}
	sc := cookieByName(res, "__Host-magnetico_session")
	if sc == nil || !sc.Secure || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode || sc.Path != "/" || sc.Domain != "" {
		t.Fatalf("session cookie attributes: %+v", sc)
	}
	if !sc.Expires.IsZero() || sc.MaxAge != 0 {
		t.Errorf("an ordinary session must be a browser-session cookie: expires=%v maxage=%d", sc.Expires, sc.MaxAge)
	}
	if len(sc.Value) != accounts.SessionTokenLen {
		t.Errorf("token length %d", len(sc.Value))
	}
	if gone := cookieByName(res, "__Host-magnetico_presession"); gone == nil || gone.MaxAge >= 0 {
		t.Error("the pre-session cookie must be deleted by a successful login")
	}
	if e.count(`SELECT count(*) FROM magnetico_web.users WHERE id = $1 AND last_login_at IS NOT NULL`, u.ID) != 1 {
		t.Error("last_login_at not recorded")
	}

	if res = c.get("/"); res.StatusCode != 200 || !strings.Contains(c.body, "Signed in as alice") {
		t.Fatalf("GET /: %d\n%s", res.StatusCode, c.body)
	}
	if res = c.get("/login"); res.StatusCode != 303 || res.Header.Get("Location") != "/" {
		t.Fatalf("GET /login while signed in: %d", res.StatusCode)
	}

	// Logout: POST with the session token; GET is not allowed.
	if res = c.get("/logout"); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /logout: %d, want 405", res.StatusCode)
	}
	c.get("/")
	oldCookie := *c.jar["__Host-magnetico_session"]
	res = c.post("/logout", map[string][]string{"csrf_token": {c.token()}})
	if res.StatusCode != 303 || res.Header.Get("Location") != "/login" {
		t.Fatalf("POST /logout: %d → %q", res.StatusCode, res.Header.Get("Location"))
	}
	if cleared := cookieByName(res, "__Host-magnetico_session"); cleared == nil || cleared.MaxAge >= 0 {
		t.Error("logout must clear the cookie")
	}
	if e.sessions() != 0 {
		t.Error("the server-side session survives logout")
	}
	if res = c.get("/"); res.StatusCode != 303 || res.Header.Get("Location") != "/login?next=%2F" {
		t.Fatalf("GET / after logout: %d → %q", res.StatusCode, res.Header.Get("Location"))
	}

	// Replay of the old cookie: redirected, cookie cleared, nothing deleted twice.
	r := e.client()
	r.jar[oldCookie.Name] = &oldCookie
	if res = r.get("/"); res.StatusCode != 303 || cookieByName(res, oldCookie.Name) == nil {
		t.Fatalf("replayed session: %d", res.StatusCode)
	}
	before := e.store.deletes.Load()
	r.jar[oldCookie.Name] = &oldCookie
	r.post("/logout", map[string][]string{"csrf_token": {"x"}})
	if e.store.deletes.Load() != before {
		t.Error("a logout replay reached the database")
	}
}

func TestRememberMeCookieMatchesTheServerExpiry(t *testing.T) {
	e := newEnv(t)
	e.mkUser("bob", "remember this one")
	c := e.client()
	res := c.signIn("bob", "remember this one", "remember", "1")
	sc := cookieByName(res, "__Host-magnetico_session")
	if sc == nil || sc.Expires.IsZero() {
		t.Fatalf("remember cookie: %+v", sc)
	}
	var exp time.Time
	var remember bool
	if err := e.pool.QueryRow(tctx(t), `SELECT expires_at, remember FROM magnetico_web.sessions`).Scan(&exp, &remember); err != nil {
		t.Fatal(err)
	}
	if !remember || sc.Expires.Unix() != exp.Unix() {
		t.Errorf("cookie Expires %v != row expires_at %v (remember=%v)", sc.Expires, exp, remember)
	}
	if d := time.Duration(sc.MaxAge) * time.Second; d < testLifetimes.Remember-time.Minute || d > testLifetimes.Remember {
		t.Errorf("Max-Age %v not about the remember lifetime", d)
	}
}

func TestDevelopmentModeCookies(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.dev = true })
	e.mkUser("carol", "dev mode password")
	c := e.client()
	res := c.get("/login")
	pre := cookieByName(res, "magnetico_presession")
	if pre == nil || pre.Secure || !pre.HttpOnly {
		t.Fatalf("dev pre-session cookie: %+v", pre)
	}
	res = c.post("/login", c.loginForm("carol", "dev mode password"))
	sc := cookieByName(res, "magnetico_session")
	if res.StatusCode != 303 || sc == nil || sc.Secure || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode || sc.Path != "/" {
		t.Fatalf("dev session cookie: %d %+v", res.StatusCode, sc)
	}
	for _, ck := range res.Cookies() {
		if strings.HasPrefix(ck.Name, "__Host-") {
			t.Errorf("dev mode set a __Host- cookie: %s", ck.Name)
		}
	}
	if res = c.get("/"); res.StatusCode != 200 {
		t.Fatalf("GET / in dev mode: %d", res.StatusCode)
	}
}

var (
	valueAttr = regexp.MustCompile(`value="[^"]*"`)
)

// shape normalises what legitimately differs between failures (the echoed
// username and the CSRF token) so structures can be compared.
func shape(body string) string { return valueAttr.ReplaceAllString(body, `value=""`) }

func TestGenericFailureForEveryAccountCondition(t *testing.T) {
	e := newEnv(t)
	e.mkUser("dave", "the right password")
	e.mkUser("inactive", "the right password")
	if _, err := e.store.Disable(tctx(t), "inactive"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateUser(tctx(t), accounts.CreateUserParams{Username: "nopass"}); err != nil {
		t.Fatal(err)
	}
	u := e.mkUser("badhash", "the right password")
	e.exec(`UPDATE magnetico_web.users SET password_hash = '$1$legacy$unsupportedunsupported' WHERE id = $1`, u.ID)

	type tc struct{ name, user, pw string }
	cases := []tc{
		{"wrong password", "dave", "definitely not it"},
		{"unknown user", "ghost", "the right password"},
		{"inactive user, right password", "inactive", "the right password"},
		{"passwordless", "nopass", "the right password"},
		{"unsupported stored hash", "badhash", "the right password"},
		{"case-sensitive username", "Dave", "the right password"},
		{"invalid UTF-8 username", "bad\xffname", "the right password"},
		{"NUL in username", "nul\x00name", "the right password"},
		{"over-long username", strings.Repeat("u", 300), "the right password"},
	}
	var wantShape string
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := e.client()
			cl.get("/login")
			e.svc.admitted.Store(0)
			res := cl.post("/login", cl.loginForm(c.user, c.pw))
			if res.StatusCode != 401 {
				t.Fatalf("status %d, want 401", res.StatusCode)
			}
			if !strings.Contains(cl.body, msgLoginFailed) {
				t.Errorf("generic message missing:\n%s", cl.body)
			}
			if strings.Contains(cl.body, c.pw) {
				t.Error("the password was echoed")
			}
			if len(res.Cookies()) != 0 {
				t.Errorf("a failed login set cookies: %v", res.Cookies())
			}
			if n := e.svc.admitted.Load(); n != 1 {
				t.Errorf("%d limiter-admitted verifications, want exactly 1 (the timing shape)", n)
			}
			if s := shape(cl.body); wantShape == "" {
				wantShape = s
			} else if s != wantShape {
				t.Errorf("response structure differs from the other failures:\n%s\n--- vs ---\n%s", s, wantShape)
			}
			if e.sessions() != 0 {
				t.Error("a session exists after a failed login")
			}
		})
	}

	// With identical input and pre-session state the whole body is comparable.
	cl := e.client()
	cl.get("/login")
	form := cl.loginForm("ghost", "x password")
	cl.post("/login", form)
	b1 := cl.body
	cl.post("/login", form)
	if b1 != cl.body {
		t.Error("identical failed attempts produced different bodies")
	}
	if n := e.count(`SELECT count(*) FROM magnetico_web.users WHERE last_login_at IS NOT NULL`); n != 0 {
		t.Errorf("%d users have last_login_at after failed logins", n)
	}
	if !strings.Contains(e.log.String(), "unsupported format") {
		t.Error("the operator log should flag the account that needs a reset")
	}
}

func TestEmptyAndOversizedPasswordsFailWithoutHashing(t *testing.T) {
	e := newEnv(t)
	e.mkUser("erin", "a perfectly fine password")
	c := e.client()
	c.get("/login")
	for _, pw := range []string{"", strings.Repeat("p", password.MaxLoginLength+1)} {
		e.svc.admitted.Store(0)
		if res := c.post("/login", c.loginForm("erin", pw)); res.StatusCode != 401 {
			t.Fatalf("len %d: status %d", len(pw), res.StatusCode)
		}
		if e.svc.admitted.Load() != 0 {
			t.Errorf("len %d: input-dependent failure must not hash", len(pw))
		}
	}
}

func TestLegacyHashesLogInAndAreRehashed(t *testing.T) {
	for _, id := range []string{"argon2i_m16384_t2_p4/ascii", "bcrypt_2y_cost10/ascii", "argon2id_sodium_m65536_t4_p1/ascii"} {
		t.Run(id, func(t *testing.T) {
			e := newEnv(t)
			hash, pw := loadFixture(t, id)
			u := e.mkUser("frank", "placeholder password")
			e.exec(`UPDATE magnetico_web.users SET password_hash = $2 WHERE id = $1`, u.ID, hash)
			var changed0 *time.Time
			if err := e.pool.QueryRow(tctx(t), `SELECT password_changed_at FROM magnetico_web.users WHERE id = $1`, u.ID).Scan(&changed0); err != nil {
				t.Fatal(err)
			}

			c := e.client()
			c.signIn("frank", pw)
			var got string
			var changed1 *time.Time
			if err := e.pool.QueryRow(tctx(t), `SELECT password_hash, password_changed_at FROM magnetico_web.users WHERE id = $1`, u.ID).Scan(&got, &changed1); err != nil {
				t.Fatal(err)
			}
			if got == hash || !strings.HasPrefix(got, "$argon2id$v=19$m=64,t=1,p=1$") {
				t.Fatalf("hash not upgraded: %q", got)
			}
			if (changed0 == nil) != (changed1 == nil) || changed0 != nil && !changed0.Equal(*changed1) {
				t.Error("a rehash must not change password_changed_at")
			}
			// The upgraded hash works without further rehashing.
			c2 := e.client()
			e.svc.rehashes.Store(0)
			c2.signIn("frank", pw)
			if e.svc.rehashes.Load() != 0 {
				t.Error("a current hash was rehashed again")
			}
			if res := c2.get("/"); res.StatusCode != 200 {
				t.Fatalf("GET /: %d", res.StatusCode)
			}
		})
	}
}

func TestRehashBusyStillLogsInWithoutRehash(t *testing.T) {
	e := newEnv(t)
	hash, pw := loadFixture(t, "bcrypt_2y_cost10/ascii")
	u := e.mkUser("gina", "placeholder password")
	e.exec(`UPDATE magnetico_web.users SET password_hash = $2 WHERE id = $1`, u.ID, hash)
	busy := password.ErrBusy
	e.svc.rehashErr.Store(&busy)

	c := e.client()
	c.signIn("gina", pw) // verification itself was not skipped (it succeeded), login completes
	var got string
	if err := e.pool.QueryRow(tctx(t), `SELECT password_hash FROM magnetico_web.users WHERE id = $1`, u.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != hash {
		t.Fatal("the hash changed although the rehash was refused")
	}
	if res := c.get("/"); res.StatusCode != 200 {
		t.Fatal("not signed in")
	}
	e.svc.rehashErr.Store(nil)
	e.client().signIn("gina", pw) // the next login upgrades it
	if err := e.pool.QueryRow(tctx(t), `SELECT password_hash FROM magnetico_web.users WHERE id = $1`, u.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got == hash {
		t.Fatal("the upgrade did not happen at the next login")
	}
	// A different rehash failure is not suppressed.
	e.svc.rehashErr.Store(&errBoom)
	e.exec(`UPDATE magnetico_web.users SET password_hash = $2 WHERE id = $1`, u.ID, hash)
	cl := e.client()
	cl.get("/login")
	if res := cl.post("/login", cl.loginForm("gina", pw)); res.StatusCode != 503 {
		t.Fatalf("a non-busy rehash failure must fail the login (503), got %d", res.StatusCode)
	}
}

var errBoom = errors.New("boom")

func TestBusyVerificationIs503WithRetryAfter(t *testing.T) {
	e := newEnv(t)
	e.mkUser("hank", "the right password")
	c := e.client()
	c.get("/login")
	tok := c.token()
	busy := password.ErrBusy

	e.svc.verifyErr.Store(&busy)
	res := c.post("/login", formFor(tok, "hank", "the right password"))
	if res.StatusCode != 503 || res.Header.Get("Retry-After") != "5" {
		t.Fatalf("busy verify: %d retry-after=%q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	e.svc.verifyErr.Store(nil)
	e.svc.dummyErr.Store(&busy)
	res = c.post("/login", formFor(tok, "ghost", "whatever password"))
	if res.StatusCode != 503 || res.Header.Get("Retry-After") != "5" {
		t.Fatalf("busy dummy verify: %d retry-after=%q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if e.sessions() != 0 || e.count(`SELECT count(*) FROM magnetico_web.users WHERE last_login_at IS NOT NULL`) != 0 {
		t.Error("a 503 login left state behind")
	}
	if len(res.Cookies()) != 0 {
		t.Error("a 503 login set cookies")
	}
}

func TestDatabaseFailureDuringLoginIs503(t *testing.T) {
	e := newEnv(t)
	e.mkUser("ivy", "the right password")
	c := e.client()
	c.get("/login")
	tok := c.token()
	e.store.credErr.Store(&errBoom)
	if res := c.post("/login", formFor(tok, "ivy", "the right password")); res.StatusCode != 503 {
		t.Fatalf("credential lookup failure: %d", res.StatusCode)
	}
	e.store.credErr.Store(nil)
	e.store.commitErr.Store(&errBoom)
	res := c.post("/login", formFor(tok, "ivy", "the right password"))
	if res.StatusCode != 503 || len(res.Cookies()) != 0 {
		t.Fatalf("commit failure: %d cookies=%v", res.StatusCode, res.Cookies())
	}
}

func TestSessionFixationIsPrevented(t *testing.T) {
	e := newEnv(t)
	e.mkUser("jack", "the right password")

	// An attacker-chosen (well-formed) cookie is never adopted.
	attacker, _, _ := accounts.NewSessionToken()
	c := e.client()
	c.get("/login")
	c.jar["__Host-magnetico_session"] = &http.Cookie{Name: "__Host-magnetico_session", Value: attacker}
	res := c.post("/login", c.loginForm("jack", "the right password"))
	if res.StatusCode != 303 {
		t.Fatalf("login: %d", res.StatusCode)
	}
	sc := cookieByName(res, "__Host-magnetico_session")
	if sc == nil || sc.Value == attacker {
		t.Fatal("the attacker-chosen session token was adopted")
	}
	h, _ := accounts.SessionTokenHash(attacker)
	if e.count(`SELECT count(*) FROM magnetico_web.sessions WHERE token_hash = $1`, h) != 0 {
		t.Fatal("a session exists for the attacker-chosen token")
	}

	// A valid old session presented at login is deleted in the same transaction.
	old := e.client()
	old.signIn("jack", "the right password")
	oldValue := old.cookieValue("__Host-magnetico_session")
	oh, _ := accounts.SessionTokenHash(oldValue)
	old.get("/login") // signed in → redirect, so log out of the form path by hand:
	delete(old.jar, "__Host-magnetico_presession")
	pre := e.client()
	pre.get("/login")
	pre.jar["__Host-magnetico_session"] = &http.Cookie{Name: "__Host-magnetico_session", Value: oldValue}
	if res = pre.post("/login", pre.loginForm("jack", "the right password")); res.StatusCode != 303 {
		t.Fatalf("second login: %d", res.StatusCode)
	}
	if e.count(`SELECT count(*) FROM magnetico_web.sessions WHERE token_hash = $1`, oh) != 0 {
		t.Fatal("the presented old session survived the login")
	}
	if nv := pre.cookieValue("__Host-magnetico_session"); nv == oldValue || nv == "" {
		t.Fatal("the new session reuses the old token")
	}
}

func TestSessionEnforcement(t *testing.T) {
	e := newEnv(t)
	e.mkUser("kim", "the right password")

	// Anonymous.
	c := e.client()
	if res := c.get("/"); res.StatusCode != 303 || res.Header.Get("Location") != "/login?next=%2F" || len(res.Cookies()) != 0 {
		t.Fatalf("anonymous /: %d %q cookies=%v", res.StatusCode, res.Header.Get("Location"), res.Cookies())
	}

	// A malformed cookie never reaches the database and is cleared.
	before := e.store.lookups.Load()
	c.jar["__Host-magnetico_session"] = &http.Cookie{Name: "__Host-magnetico_session", Value: "not-a-token"}
	res := c.get("/")
	if res.StatusCode != 303 || e.store.lookups.Load() != before {
		t.Fatalf("malformed cookie: %d, lookups %d→%d", res.StatusCode, before, e.store.lookups.Load())
	}
	if cl := cookieByName(res, "__Host-magnetico_session"); cl == nil || cl.MaxAge >= 0 {
		t.Error("a malformed cookie must be cleared")
	}

	// A well-formed but unknown cookie is cleared too.
	unk, _, _ := accounts.NewSessionToken()
	c.jar["__Host-magnetico_session"] = &http.Cookie{Name: "__Host-magnetico_session", Value: unk}
	if res = c.get("/"); res.StatusCode != 303 || cookieByName(res, "__Host-magnetico_session") == nil {
		t.Fatalf("unknown session: %d", res.StatusCode)
	}

	// Expired (absolute and idle) sessions.
	for _, how := range []string{
		`UPDATE magnetico_web.sessions SET created_at = created_at - interval '169 hours', last_seen_at = last_seen_at - interval '169 hours', expires_at = expires_at - interval '169 hours'`,
		`UPDATE magnetico_web.sessions SET last_seen_at = last_seen_at - interval '13 hours'`,
	} {
		s := e.client()
		s.signIn("kim", "the right password")
		e.exec(how)
		res := s.get("/")
		if res.StatusCode != 303 || cookieByName(res, "__Host-magnetico_session") == nil {
			t.Fatalf("expired session (%s): %d", how[:40], res.StatusCode)
		}
		e.exec(`DELETE FROM magnetico_web.sessions`)
	}

	// A database error is a 503, never a redirect (and never authenticated).
	s := e.client()
	s.signIn("kim", "the right password")
	e.store.lookupErr.Store(&errBoom)
	res = s.get("/")
	if res.StatusCode != 503 || res.Header.Get("Location") != "" {
		t.Fatalf("lookup error: %d location=%q", res.StatusCode, res.Header.Get("Location"))
	}
	if res = s.get("/login"); res.StatusCode != 503 {
		t.Fatalf("GET /login with a failing lookup: %d", res.StatusCode)
	}
	e.store.lookupErr.Store(nil)
	if res = s.get("/"); res.StatusCode != 200 {
		t.Fatalf("recovery: %d", res.StatusCode)
	}
}

func TestDisabledUserSessionDoesNotAuthenticate(t *testing.T) {
	e := newEnv(t)
	e.mkUser("leo", "the right password")
	c := e.client()
	c.signIn("leo", "the right password")
	if res := c.get("/"); res.StatusCode != 200 {
		t.Fatal("not signed in")
	}
	if _, err := e.store.Disable(tctx(t), "leo"); err != nil {
		t.Fatal(err)
	}
	if res := c.get("/"); res.StatusCode != 303 {
		t.Fatalf("disabled user: %d", res.StatusCode)
	}
	if err := e.store.Enable(tctx(t), "leo"); err != nil {
		t.Fatal(err)
	}
	if res := c.get("/"); res.StatusCode != 303 {
		t.Fatalf("enable restored a session: %d", res.StatusCode)
	}
	// Disabled accounts cannot log in, whatever the password.
	if _, err := e.store.Disable(tctx(t), "leo"); err != nil {
		t.Fatal(err)
	}
	d := e.client()
	d.get("/login")
	if res := d.post("/login", d.loginForm("leo", "the right password")); res.StatusCode != 401 {
		t.Fatalf("disabled login: %d", res.StatusCode)
	}
}

func TestRoutingExactRootAndUnknownPaths(t *testing.T) {
	e := newEnv(t)
	e.mkUser("mia", "the right password")
	anon := e.client()
	authed := e.client()
	authed.signIn("mia", "the right password")
	for _, p := range []string{"/torrents", "/torrents/1", "/x", "/auth/login", "/profile", "/api/v2/torrents", "/login/extra", "/logout/x", "/index.html"} {
		for who, c := range map[string]*client{"anonymous": anon, "authenticated": authed} {
			if res := c.get(p); res.StatusCode != 404 {
				t.Errorf("%s GET %s: %d, want 404 (the interim home must not swallow unknown paths)", who, p, res.StatusCode)
			}
		}
	}
	// The exact root still works, with and without a query.
	if res := authed.get("/?x=1"); res.StatusCode != 200 {
		t.Errorf("GET /?x=1: %d", res.StatusCode)
	}
	if res := anon.get("/"); res.StatusCode != 303 {
		t.Errorf("anonymous GET /: %d", res.StatusCode)
	}
}

func TestNextRedirects(t *testing.T) {
	cases := map[string]string{
		"/":                             "/",
		"/torrents?q=abc":               "/torrents?q=abc",
		"/a/b":                          "/a/b",
		"":                              "",
		"https://evil.example":          "",
		"//evil.example":                "",
		"/\\evil.example":               "",
		"/%2F%2Fevil.example":           "",
		"/%5Cevil.example":              "",
		"/%2f/evil.example":             "",
		"/a\\b":                         "",
		"javascript:alert(1)":           "",
		"evil":                          "",
		"/a\nb":                         "",
		"/a\x00b":                       "",
		"/\x7f":                         "",
		"/" + strings.Repeat("a", 2048): "",
		"http:evil.example":             "",
	}
	for in, want := range cases {
		if got := validNext(in); got != want {
			t.Errorf("validNext(%q) = %q, want %q", in, got, want)
		}
	}

	e := newEnv(t)
	e.mkUser("ned", "the right password")
	for in, want := range map[string]string{
		"/torrents?q=1": "/torrents?q=1", "//evil.example": "/", "/\\evil.example": "/", "https://evil.example/": "/", "/%2F%2Fevil.example": "/",
	} {
		c := e.client()
		c.get("/login?next=" + url.QueryEscape(in))
		res := c.post("/login", c.loginForm("ned", "the right password", "next", in))
		if res.StatusCode != 303 || res.Header.Get("Location") != want {
			t.Errorf("next=%q: %d → %q, want %q", in, res.StatusCode, res.Header.Get("Location"), want)
		}
	}
	// The anonymous redirect carries the requested local path and the form keeps it.
	c := e.client()
	if res := c.get("/?a=b"); res.Header.Get("Location") != "/login?next=%2F%3Fa%3Db" {
		t.Errorf("redirect location %q", res.Header.Get("Location"))
	}
	c.get("/login?next=%2F%3Fa%3Db")
	if !strings.Contains(c.body, `name="next" value="/?a=b"`) {
		t.Errorf("the form lost next:\n%s", c.body)
	}
	c.get("/login?next=https%3A%2F%2Fevil.example")
	if strings.Contains(c.body, `name="next"`) {
		t.Error("an invalid next was put into the form")
	}
}
