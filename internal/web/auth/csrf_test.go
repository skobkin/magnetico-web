package auth

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/server"
)

func TestCSRFOnUnauthenticatedLoginPost(t *testing.T) {
	e := newEnv(t)
	e.mkUser("olga", "the right password")

	deny := func(what string, c *client, form url.Values) {
		t.Helper()
		sessionsBefore := e.sessions()
		res := c.post("/login", form)
		if res.StatusCode != 403 {
			t.Errorf("%s: status %d, want 403", what, res.StatusCode)
		}
		if strings.Contains(c.body, "the right password") {
			t.Errorf("%s: the form was echoed", what)
		}
		if e.sessions() != sessionsBefore {
			t.Errorf("%s: a session was created", what)
		}
		if len(res.Cookies()) != 0 {
			t.Errorf("%s: a rejected request set cookies", what)
		}
	}

	// No cookie, no token at all.
	deny("no cookie", e.client(), url.Values{"username": {"olga"}, "password": {"the right password"}})

	// Cookie but no token / empty / wrong token / token only in the query string.
	c := e.client()
	c.get("/login")
	tok := c.token()
	deny("missing token", c, url.Values{"username": {"olga"}, "password": {"the right password"}})
	deny("empty token", c, formFor("", "olga", "the right password"))
	deny("wrong token", c, formFor(tok[:len(tok)-1]+"x", "olga", "the right password"))
	res := c.do("POST", "/login?csrf_token="+url.QueryEscape(tok), url.Values{"username": {"olga"}, "password": {"the right password"}}, nil)
	if res.StatusCode != 403 {
		t.Errorf("token in the query string: %d", res.StatusCode)
	}

	// Malformed pre-session cookie.
	m := e.client()
	m.jar["__Host-magnetico_presession"] = &http.Cookie{Name: "__Host-magnetico_presession", Value: "garbage"}
	deny("malformed pre-session cookie", m, formFor(tok, "olga", "the right password"))

	// A token of a different pre-session.
	other := e.client()
	other.get("/login")
	deny("token of another pre-session", c, formFor(other.token(), "olga", "the right password"))

	// A token without its cookie.
	deny("token without the cookie", e.client(), formFor(tok, "olga", "the right password"))

	// A session-class token is not a pre-session token.
	s := e.client()
	s.signIn("olga", "the right password")
	s.get("/")
	sessTok := s.token()
	p := e.client()
	p.get("/login")
	deny("session token on a pre-session form", p, formFor(sessTok, "olga", "the right password"))

	// Expiry of the pre-session cookie (fake clock).
	x := e.client()
	x.get("/login")
	xt := x.token()
	e.clock.Advance(time.Hour - time.Second)
	if res := x.post("/login", formFor(xt, "olga", "the right password")); res.StatusCode != 303 {
		t.Fatalf("a pre-session just inside its lifetime was refused: %d", res.StatusCode)
	}
	x2 := e.client()
	x2.get("/login")
	x2t := x2.token()
	e.clock.Advance(time.Hour + time.Second)
	deny("expired pre-session cookie", x2, formFor(x2t, "olga", "the right password"))
	// A GET reissues a fresh one.
	if res := x2.get("/login"); cookieByName(res, "__Host-magnetico_presession") == nil {
		t.Error("an expired pre-session cookie was not reissued on GET")
	}
	if res := x2.post("/login", x2.loginForm("olga", "the right password")); res.StatusCode != 303 {
		t.Errorf("fresh pre-session refused: %d", res.StatusCode)
	}

	// After a successful login the old login token is dead for the browser: the
	// pre-session cookie was deleted, so replaying the old form fails.
	r := e.client()
	r.get("/login")
	rt := r.token()
	r.post("/login", formFor(rt, "olga", "the right password"))
	deny("replay of the login form after sign-in", r, formFor(rt, "olga", "the right password"))
}

func TestCSRFOnAuthenticatedPost(t *testing.T) {
	e := newEnv(t)
	e.mkUser("pat", "the right password")
	e.mkUser("quinn", "the right password")

	a := e.client()
	a.signIn("pat", "the right password")
	b := e.client()
	b.signIn("quinn", "the right password")
	a.get("/")
	aTok := a.token()
	b.get("/")
	bTok := b.token()

	for what, form := range map[string]url.Values{
		"missing token":           {},
		"another session's token": {"csrf_token": {bTok}},
		"garbage":                 {"csrf_token": {"x"}},
		"query-only token":        nil,
	} {
		var res *http.Response
		if form == nil {
			res = a.do("POST", "/logout?csrf_token="+url.QueryEscape(aTok), url.Values{}, nil)
		} else {
			res = a.post("/logout", form)
		}
		if res.StatusCode != 403 {
			t.Errorf("%s: %d, want 403", what, res.StatusCode)
		}
		if e.sessions() != 2 {
			t.Fatalf("%s: a rejected logout deleted a session", what)
		}
	}
	// A pre-session token is not a session token.
	p := e.client()
	p.get("/login")
	if res := a.post("/logout", url.Values{"csrf_token": {p.token()}}); res.StatusCode != 403 {
		t.Errorf("pre-session token on a session route: %d", res.StatusCode)
	}
	// The right token works, and only for its own session.
	if res := a.post("/logout", url.Values{"csrf_token": {aTok}}); res.StatusCode != 303 {
		t.Fatalf("valid logout: %d", res.StatusCode)
	}
	if e.sessions() != 1 {
		t.Fatal("logout must delete exactly its own session")
	}
	// After logout its token is dead (the session is gone).
	if res := a.post("/logout", url.Values{"csrf_token": {aTok}}); res.StatusCode != 303 || res.Header.Get("Location") != "/login" {
		t.Errorf("replayed logout: %d", res.StatusCode)
	}
	if res := b.get("/"); res.StatusCode != 200 {
		t.Error("the other user lost their session")
	}
}

func TestCrossOriginRequestsAreRefusedEvenWithAValidToken(t *testing.T) {
	e := newEnv(t)
	e.mkUser("rita", "the right password")

	for _, h := range []http.Header{
		{"Sec-Fetch-Site": {"cross-site"}},
		{"Origin": {"https://evil.example"}},
		{"Origin": {"null"}},
	} {
		c := e.client()
		c.get("/login")
		if res := c.do("POST", "/login", c.loginForm("rita", "the right password"), h); res.StatusCode != 403 {
			t.Errorf("login with %v: %d, want 403", h, res.StatusCode)
		}
		if e.sessions() != 0 {
			t.Fatalf("login with %v created a session", h)
		}
	}
	s := e.client()
	s.signIn("rita", "the right password")
	s.get("/")
	tok := s.token()
	for _, h := range []http.Header{{"Sec-Fetch-Site": {"cross-site"}}, {"Origin": {"https://evil.example"}}} {
		if res := s.do("POST", "/logout", url.Values{"csrf_token": {tok}}, h); res.StatusCode != 403 {
			t.Errorf("logout with %v: %d", h, res.StatusCode)
		}
	}
	if e.sessions() != 1 {
		t.Fatal("a cross-origin logout deleted the session")
	}
	// Same-origin browser requests and header-less clients pass.
	if res := s.do("POST", "/logout", url.Values{"csrf_token": {tok}}, http.Header{"Origin": {e.origin()}, "Sec-Fetch-Site": {"same-origin"}}); res.StatusCode != 303 {
		t.Errorf("same-origin logout: %d", res.StatusCode)
	}
	c := e.client()
	c.get("/login")
	if res := c.do("POST", "/login", c.loginForm("rita", "the right password"), http.Header{"Origin": {e.origin()}}); res.StatusCode != 303 {
		t.Errorf("same-origin login: %d", res.StatusCode)
	}
}

func TestStructuralRouteClassification(t *testing.T) {
	e := newEnv(t)
	want := map[string]server.Access{
		"GET /login": server.PublicForm, "POST /login": server.PublicForm,
		"POST /logout": server.Session, "GET /{$}": server.Session,
		"GET /healthz": server.Public,
	}
	got := map[string]server.Access{}
	for _, r := range e.srv.Router().Routes() {
		got[r.Pattern] = r.Access
		method, _, _ := strings.Cut(r.Pattern, " ")
		if method != "GET" && method != "HEAD" && r.Access == server.Public {
			t.Errorf("state-changing route %s is Public", r.Pattern)
		}
	}
	for p, a := range want {
		if g, ok := got[p]; !ok || g != a {
			t.Errorf("%s: %v (registered=%v), want %v", p, g, ok, a)
		}
	}
}

func TestSafeRequestsChangeNoApplicationState(t *testing.T) {
	e := newEnv(t)
	u := e.mkUser("sven", "the right password")
	e.store.CreateAPIToken(tctx(t), u.ID, "t", nil)
	e.store.CreateRSSToken(tctx(t), u.ID, "t", nil)
	e.store.CreateInvites(tctx(t), "sven", 2, nil)
	e.store.IssuePasswordLink(tctx(t), "sven")

	dump := func() string {
		var out string
		for _, q := range []string{
			`SELECT t::text FROM magnetico_web.users t ORDER BY id`,
			`SELECT t::text FROM magnetico_web.api_tokens t ORDER BY id`,
			`SELECT t::text FROM magnetico_web.rss_tokens t ORDER BY id`,
			`SELECT t::text FROM magnetico_web.invites t ORDER BY id`,
			`SELECT t::text FROM magnetico_web.one_time_tokens t ORDER BY created_at`,
			`SELECT t::text FROM magnetico_web.trackers t ORDER BY id`,
		} {
			rows, err := e.pool.Query(tctx(t), q)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var s string
				rows.Scan(&s)
				out += s + "\n"
			}
			rows.Close()
		}
		return out
	}
	c := e.client()
	c.signIn("sven", "the right password") // writes last_login_at; snapshot after it
	before := dump()
	sessionsBefore := e.sessions()

	anon := e.client()
	for _, p := range []string{"/login", "/login?next=%2F", "/", "/healthz", "/readyz", "/robots.txt", "/nope"} {
		anon.get(p)
		c.get(p)
	}
	if after := dump(); after != before {
		t.Fatalf("safe requests changed application state:\n--- before\n%s--- after\n%s", before, after)
	}
	if e.sessions() != sessionsBefore {
		t.Fatal("safe requests created or deleted sessions")
	}

	// Documented bookkeeping: a due touch moves last_seen_at (not expires_at),
	// and a public GET form issues a pre-session cookie.
	e.exec(`UPDATE magnetico_web.sessions SET last_seen_at = last_seen_at - interval '10 minutes'`)
	var seen0, exp0, seen1, exp1 time.Time
	e.pool.QueryRow(tctx(t), `SELECT last_seen_at, expires_at FROM magnetico_web.sessions`).Scan(&seen0, &exp0)
	c.get("/")
	e.pool.QueryRow(tctx(t), `SELECT last_seen_at, expires_at FROM magnetico_web.sessions`).Scan(&seen1, &exp1)
	if !seen1.After(seen0) || !exp1.Equal(exp0) {
		t.Errorf("touch: last_seen %v→%v, expires %v→%v", seen0, seen1, exp0, exp1)
	}
	c.get("/")
	var seen2 time.Time
	e.pool.QueryRow(tctx(t), `SELECT last_seen_at FROM magnetico_web.sessions`).Scan(&seen2)
	if !seen2.Equal(seen1) {
		t.Error("a second request inside the throttle wrote last_seen_at again")
	}
	if res := e.client().get("/login"); cookieByName(res, "__Host-magnetico_presession") == nil {
		t.Error("GET /login issued no pre-session cookie")
	}
}

func TestNoSessionIsCreatedBySessionRowDirectEditing(t *testing.T) {
	// Defence in depth: a session row of a user who was disabled behind the
	// store's back still does not authenticate (active is read per request).
	e := newEnv(t)
	u := e.mkUser("tom", "the right password")
	c := e.client()
	c.signIn("tom", "the right password")
	e.exec(`UPDATE magnetico_web.users SET active = false WHERE id = $1`, u.ID)
	if res := c.get("/"); res.StatusCode != 303 {
		t.Fatalf("inactive owner: %d", res.StatusCode)
	}
	_ = accounts.ErrUnauthenticated
}
