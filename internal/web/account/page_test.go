package account

import (
	"crypto/sha256"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/server"
)

func (e *env) signedIn(name, pw string) (*client, accounts.User) {
	e.t.Helper()
	u := e.mkUser(name, pw)
	c := e.client()
	c.signIn(name, pw)
	return c, u
}

func TestAccountPageRequiresASessionAndRendersEveryTab(t *testing.T) {
	e := newEnv(t)
	anon := e.client()
	if res := anon.get("/account"); res.StatusCode != 303 || !strings.HasPrefix(res.Header.Get("Location"), "/login") {
		t.Fatalf("anonymous: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	c, _ := e.signedIn("tab", pw1)
	for tab, want := range map[string]string{
		"profile": "Password &amp; security", "invitations": "Invitations", "rss": "RSS feed tokens", "api": "API tokens", "bogus": "Password &amp; security",
	} {
		res := c.get("/account?tab=" + tab)
		if res.StatusCode != 200 || !strings.Contains(c.body, want) {
			t.Fatalf("tab %s: %d", tab, res.StatusCode)
		}
		if got := res.Header.Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("tab %s Referrer-Policy = %q", tab, got)
		}
		if got := res.Header.Get("Cache-Control"); got != "private, no-store" {
			t.Errorf("tab %s Cache-Control = %q", tab, got)
		}
		if res.Header.Get("X-Robots-Tag") == "" {
			t.Errorf("tab %s lacks X-Robots-Tag", tab)
		}
	}
	// Notices come from a whitelist; arbitrary text in the query is ignored.
	c.get("/account?done=<script>alert(1)</script>")
	if strings.Contains(c.body, "alert(1)") {
		t.Fatal("done parameter is reflected")
	}
	c.get("/account?tab=profile&done=password")
	if !strings.Contains(c.body, "Password changed") {
		t.Fatal("known notice not shown")
	}
}

func TestChangePasswordRotatesSessionKeepsTokensByDefault(t *testing.T) {
	e := newEnv(t)
	c, u := e.signedIn("pwc", pw1)
	other := e.client()
	other.signIn("pwc", pw1)
	api, _ := e.store.CreateAPIToken(tctx(t), u.ID, "", nil)
	rss, _ := e.store.CreateRSSToken(tctx(t), u.ID, "", nil)
	oldCookie := c.sessionCookie()

	f := c.accountForm("profile", "current_password", pw1, "new_password", pw2, "new_password2", pw2)
	res := c.post("/account/password", f)
	if res.StatusCode != 303 || res.Header.Get("Location") != "/account?tab=profile&done=password" {
		t.Fatalf("%d %s\n%s", res.StatusCode, res.Header.Get("Location"), c.body)
	}
	if c.sessionCookie() == oldCookie || c.sessionCookie() == "" {
		t.Fatal("the session cookie was not rotated")
	}
	if e.count(`SELECT count(*) FROM magnetico_web.sessions WHERE user_id = $1`, u.ID) != 1 {
		t.Fatal("other sessions survived")
	}
	// The rotated session works; the other browser is signed out; tokens remain.
	if res := c.get("/account"); res.StatusCode != 200 {
		t.Fatalf("rotated session: %d", res.StatusCode)
	}
	if res := other.get("/account"); res.StatusCode != 303 {
		t.Fatalf("other session still valid: %d", res.StatusCode)
	}
	if _, err := e.store.AuthenticateAPIToken(tctx(t), api.Plaintext); err != nil {
		t.Fatal("API token revoked by default")
	}
	if _, err := e.store.AuthenticateRSSToken(tctx(t), rss.Plaintext); err != nil {
		t.Fatal("RSS token revoked by default")
	}
	e.client().signIn("pwc", pw2)
}

func TestChangePasswordOptionsAreSeparate(t *testing.T) {
	for _, tc := range []struct{ api, rss bool }{{true, false}, {false, true}} {
		e := newEnv(t)
		c, u := e.signedIn("opt", pw1)
		api, _ := e.store.CreateAPIToken(tctx(t), u.ID, "", nil)
		rss, _ := e.store.CreateRSSToken(tctx(t), u.ID, "", nil)
		kv := []string{"current_password", pw1, "new_password", pw2, "new_password2", pw2}
		if tc.api {
			kv = append(kv, "revoke_api", "1")
		}
		if tc.rss {
			kv = append(kv, "revoke_rss", "1")
		}
		if res := c.post("/account/password", c.accountForm("profile", kv...)); res.StatusCode != 303 {
			t.Fatalf("%+v: %d\n%s", tc, res.StatusCode, c.body)
		}
		_, apiErr := e.store.AuthenticateAPIToken(tctx(t), api.Plaintext)
		_, rssErr := e.store.AuthenticateRSSToken(tctx(t), rss.Plaintext)
		if (apiErr != nil) != tc.api || (rssErr != nil) != tc.rss {
			t.Fatalf("%+v: api revoked=%v rss revoked=%v", tc, apiErr != nil, rssErr != nil)
		}
	}
}

func TestChangePasswordRefusals(t *testing.T) {
	e := newEnv(t)
	c, u := e.signedIn("ref", pw1)
	before := e.count(`SELECT count(*) FROM magnetico_web.sessions WHERE user_id = $1`, u.ID)
	for name, kv := range map[string][]string{
		"wrong current": {"current_password", "nope-nope-nope", "new_password", pw2, "new_password2", pw2},
		"mismatch":      {"current_password", pw1, "new_password", pw2, "new_password2", pw2 + "x"},
		"too short":     {"current_password", pw1, "new_password", "short", "new_password2", "short"},
		"empty current": {"current_password", "", "new_password", pw2, "new_password2", pw2},
	} {
		res := c.post("/account/password", c.accountForm("profile", kv...))
		if res.StatusCode != 400 || !strings.Contains(c.body, "notice error") {
			t.Fatalf("%s: %d", name, res.StatusCode)
		}
		for _, secret := range []string{pw1, pw2, "nope-nope-nope"} {
			if strings.Contains(c.body, secret) {
				t.Fatalf("%s: the page echoes a password", name)
			}
		}
	}
	if e.count(`SELECT count(*) FROM magnetico_web.sessions WHERE user_id = $1`, u.ID) != before {
		t.Fatal("a refused change touched sessions")
	}
	e.client().signIn("ref", pw1) // the old password still works
}

func TestChangePasswordIsRateLimitedPerAccount(t *testing.T) {
	lim, _ := ratelimit.New(ratelimit.Config{Burst: 2, Interval: time.Hour, MaxKeys: 10})
	e := newEnv(t, func(o *envOpts) { o.userLim = lim })
	c, _ := e.signedIn("lim", pw1)
	wrong := []string{"current_password", "wrong-wrong-wrong", "new_password", pw2, "new_password2", pw2}
	for i := 0; i < 2; i++ {
		if res := c.post("/account/password", c.accountForm("profile", wrong...)); res.StatusCode != 400 {
			t.Fatalf("attempt %d: %d", i, res.StatusCode)
		}
	}
	// The third attempt is refused before the password is even checked, so a
	// hijacked session cannot be used to guess the current password.
	good := []string{"current_password", pw1, "new_password", pw2, "new_password2", pw2}
	res := c.post("/account/password", c.accountForm("profile", good...))
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("third attempt: %d", res.StatusCode)
	}
	e.client().signIn("lim", pw1) // nothing changed
}

func TestEmailChangeNeedsPasswordAndQueue(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.stopQueue = true; o.queueCap = 1 })
	c, u := e.signedIn("eml", pw1)
	e.exec(`UPDATE magnetico_web.users SET email = 'old@example.org', email_verified_at = now() WHERE id = $1`, u.ID)

	// Wrong password: nothing issued.
	res := c.post("/account/email", c.accountForm("profile", "email", "new@example.org", "current_password", "wrong-wrong-wrong"))
	if res.StatusCode != 400 || e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens WHERE purpose = 'email_verify'`) != 0 {
		t.Fatalf("wrong password: %d", res.StatusCode)
	}
	// Same verified address: refused.
	res = c.post("/account/email", c.accountForm("profile", "email", "OLD@example.org", "current_password", pw1))
	if res.StatusCode != 400 || !strings.Contains(c.body, "already confirmed") {
		t.Fatalf("same address: %d", res.StatusCode)
	}
	// A full queue is reported before anything is written.
	slot, _ := e.queue.Reserve()
	res = c.post("/account/email", c.accountForm("profile", "email", "new@example.org", "current_password", pw1))
	if res.StatusCode != 503 || e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens WHERE purpose = 'email_verify'`) != 0 {
		t.Fatalf("full queue: %d", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" || !strings.Contains(c.body, "Nothing was changed") {
		t.Fatal("full-queue response")
	}
	slot.Release()
	// With a slot the link is issued and queued; the old address stays in force.
	res = c.post("/account/email", c.accountForm("profile", "email", "new@example.org", "current_password", pw1))
	if res.StatusCode != 303 || res.Header.Get("Location") != "/account?tab=profile&done=email-sent" {
		t.Fatalf("queued: %d\n%s", res.StatusCode, c.body)
	}
	cur, _ := e.store.GetUserByID(tctx(t), u.ID)
	if cur.Email != "old@example.org" || !cur.EmailVerified {
		t.Fatalf("old address changed before confirmation: %+v", cur)
	}
	c.get("/account?tab=profile")
	if !strings.Contains(c.body, "new@example.org") || !strings.Contains(c.body, "awaiting confirmation") {
		t.Fatalf("pending address not shown:\n%s", c.body)
	}
}

func TestEmailFlowEndToEndWithResend(t *testing.T) {
	e := newEnv(t)
	c, u := e.signedIn("e2e", pw1)
	res := c.post("/account/email", c.accountForm("profile", "email", "me@example.org", "current_password", pw1))
	if res.StatusCode != 303 {
		t.Fatalf("%d\n%s", res.StatusCode, c.body)
	}
	first := e.waitMail(1)[0]
	// Resend replaces the first link.
	if res := c.post("/account/email/resend", c.accountForm("profile")); res.StatusCode != 303 {
		t.Fatalf("resend: %d\n%s", res.StatusCode, c.body)
	}
	second := e.waitMail(2)[1]
	if linkOf(t, first) == linkOf(t, second) {
		t.Fatal("resend reused the link")
	}
	anon := e.client()
	f := anon.form(linkOf(t, first))
	if res := anon.post(linkOf(t, first), f); res.StatusCode != 400 {
		t.Fatalf("replaced link accepted: %d", res.StatusCode)
	}
	f = anon.form(linkOf(t, second))
	if res := anon.post(linkOf(t, second), f); res.StatusCode != 200 {
		t.Fatalf("confirm: %d\n%s", res.StatusCode, anon.body)
	}
	cur, _ := e.store.GetUserByID(tctx(t), u.ID)
	if cur.Email != "me@example.org" || !cur.EmailVerified {
		t.Fatalf("user = %+v", cur)
	}
	// Nothing left to resend.
	if res := c.post("/account/email/resend", c.accountForm("profile")); res.StatusCode != 400 {
		t.Fatalf("resend with nothing pending: %d", res.StatusCode)
	}
}

func TestEmailControlsAreUnavailableWithoutSMTP(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.noMail = true })
	c, _ := e.signedIn("nom", pw1)
	c.get("/account?tab=profile")
	if !strings.Contains(c.body, "cannot send email") || strings.Contains(c.body, `action="/account/email"`) {
		t.Fatalf("profile page:\n%s", c.body)
	}
	if res := c.post("/account/email", c.accountForm("profile", "email", "a@example.org", "current_password", pw1)); res.StatusCode != 409 {
		t.Fatalf("POST /account/email: %d", res.StatusCode)
	}
	if res := c.post("/account/email/resend", c.accountForm("profile")); res.StatusCode != 409 {
		t.Fatalf("POST resend: %d", res.StatusCode)
	}
	// Everything else still works.
	if res := c.post("/account/api-tokens", c.accountForm("api", "label", "x")); res.StatusCode != 200 {
		t.Fatalf("token creation: %d", res.StatusCode)
	}
	if res := c.post("/account/password", c.accountForm("profile", "current_password", pw1, "new_password", pw2, "new_password2", pw2)); res.StatusCode != 303 {
		t.Fatalf("password change: %d", res.StatusCode)
	}
}

var secretCode = regexp.MustCompile(`<code>(m[gt][tf]_[A-Za-z0-9_-]+)</code>`)

func TestTokensCreateShowOnceListRevoke(t *testing.T) {
	e := newEnv(t)
	c, u := e.signedIn("tok", pw1)

	// RSS token: plaintext and a ready-to-copy v2 URL, once.
	res := c.post("/account/rss-tokens", c.accountForm("rss", "label", "qBittorrent", "expires", "30"))
	if res.StatusCode != 200 {
		t.Fatalf("%d\n%s", res.StatusCode, c.body)
	}
	m := secretCode.FindStringSubmatch(c.body)
	if m == nil || !strings.HasPrefix(m[1], "mgf_") {
		t.Fatalf("no RSS token shown:\n%s", c.body)
	}
	rssPlain := m[1]
	if !strings.Contains(c.body, publicBase+"/api/v2/feeds/latest.rss?feed_token="+rssPlain) {
		t.Fatal("ready-to-copy v2 URL missing")
	}
	if res.Header.Get("Cache-Control") != "private, no-store" || res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("secret response headers")
	}
	// A later view never shows the plaintext again, only metadata.
	c.get("/account?tab=rss")
	if strings.Contains(c.body, rssPlain) || !strings.Contains(c.body, "qBittorrent") || !strings.Contains(c.body, "Expires ") {
		t.Fatalf("list page:\n%s", c.body)
	}
	for _, want := range []string{"deprecated", "/api/v1/feed/rss/last", "cannot be used for the API"} {
		if !strings.Contains(c.body, want) {
			t.Errorf("RSS guidance lacks %q", want)
		}
	}
	if _, err := e.store.AuthenticateRSSToken(tctx(t), rssPlain); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AuthenticateAPIToken(tctx(t), rssPlain); err == nil {
		t.Fatal("RSS token authenticates as API")
	}

	// API token: Bearer explanation, separate list.
	res = c.post("/account/api-tokens", c.accountForm("api", "label", "automation"))
	m = secretCode.FindStringSubmatch(c.body)
	if res.StatusCode != 200 || m == nil || !strings.HasPrefix(m[1], "mgt_") {
		t.Fatalf("API token: %d\n%s", res.StatusCode, c.body)
	}
	if strings.Contains(c.body, "feed_token") {
		t.Fatal("an API token page offers an RSS URL")
	}
	apiPlain := m[1]
	c.get("/account?tab=api")
	if strings.Contains(c.body, apiPlain) || !strings.Contains(c.body, "automation") || !strings.Contains(c.body, "Authorization") {
		t.Fatalf("API list:\n%s", c.body)
	}
	c.get("/account?tab=rss")
	if strings.Contains(c.body, "automation") {
		t.Fatal("API token listed in the RSS section")
	}

	// Revoke each independently; the other type is untouched.
	rssList, _ := e.store.ListRSSTokens(tctx(t), u.ID)
	apiList, _ := e.store.ListAPITokens(tctx(t), u.ID)
	res = c.post("/account/rss-tokens/"+itoa(rssList[0].ID)+"/revoke", c.accountForm("rss"))
	if res.StatusCode != 303 || res.Header.Get("Location") != "/account?tab=rss&done=rss-revoked" {
		t.Fatalf("revoke rss: %d", res.StatusCode)
	}
	if _, err := e.store.AuthenticateRSSToken(tctx(t), rssPlain); err == nil {
		t.Fatal("revoked RSS token works")
	}
	if _, err := e.store.AuthenticateAPIToken(tctx(t), apiPlain); err != nil {
		t.Fatal("RSS revocation killed the API token")
	}
	res = c.post("/account/api-tokens/"+itoa(apiList[0].ID)+"/revoke", c.accountForm("api"))
	if res.StatusCode != 303 {
		t.Fatalf("revoke api: %d", res.StatusCode)
	}
	if _, err := e.store.AuthenticateAPIToken(tctx(t), apiPlain); err == nil {
		t.Fatal("revoked API token works")
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func TestLegacyRSSTokensAreListedMaskedOnly(t *testing.T) {
	e := newEnv(t)
	c, u := e.signedIn("leg", pw1)
	const legacy = "00ff00ff00ff00ff00ff00ff00ff00ff"
	h := sha256.Sum256([]byte(legacy))
	e.exec(`INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy, created_at) VALUES ($1,$2,true, '2020-01-02 03:04:05+00')`, h[:], u.ID)
	c.get("/account?tab=rss")
	if !strings.Contains(c.body, "Legacy RSS token") || !strings.Contains(c.body, "Secret not available") || !strings.Contains(c.body, "2020-01-02") {
		t.Fatalf("legacy entry:\n%s", c.body)
	}
	if strings.Contains(c.body, legacy) || strings.Contains(c.body, "00ff00ff") {
		t.Fatal("the legacy token value is shown")
	}
}

func TestTokenCapRateLimitAndValidation(t *testing.T) {
	lim, _ := ratelimit.New(ratelimit.Config{Burst: 3, Interval: time.Hour, MaxKeys: 10})
	e := newEnv(t, func(o *envOpts) { o.tokenLim = lim })
	c, _ := e.signedIn("cap", pw1)
	for i := 0; i < 3; i++ {
		if res := c.post("/account/api-tokens", c.accountForm("api")); res.StatusCode != 200 {
			t.Fatalf("create %d: %d", i, res.StatusCode)
		}
	}
	res := c.post("/account/api-tokens", c.accountForm("api"))
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d", res.StatusCode)
	}
	// The RSS bucket is separate.
	if res := c.post("/account/rss-tokens", c.accountForm("rss")); res.StatusCode != 200 {
		t.Fatalf("rss bucket: %d", res.StatusCode)
	}

	e2 := newEnv(t)
	c2, u := e2.signedIn("cap2", pw1)
	for i := 0; i < accounts.MaxActiveTokens; i++ {
		if _, err := e2.store.CreateAPIToken(tctx(t), u.ID, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	res = c2.post("/account/api-tokens", c2.accountForm("api"))
	if res.StatusCode != 400 || !strings.Contains(c2.body, "maximum of 50") {
		t.Fatalf("cap: %d\n%s", res.StatusCode, c2.body)
	}
	for name, kv := range map[string][]string{
		"bad expiry": {"expires", "7"},
		"long label": {"label", strings.Repeat("x", 101)},
	} {
		if res := c2.post("/account/rss-tokens", c2.accountForm("rss", kv...)); res.StatusCode != 400 {
			t.Fatalf("%s: %d", name, res.StatusCode)
		}
	}
}

func TestTokensAndInvitationsOfOtherUsersAreInvisibleAndUntouchable(t *testing.T) {
	e := newEnv(t)
	a, ua := e.signedIn("usera", pw1)
	b, ub := e.signedIn("userb", pw1)
	bAPI, _ := e.store.CreateAPIToken(tctx(t), ub.ID, "bobs-api-label", nil)
	bRSS, _ := e.store.CreateRSSToken(tctx(t), ub.ID, "bobs-rss-label", nil)
	bInv, _ := e.store.CreateInvites(tctx(t), "userb", 1, nil)
	aInv, _ := e.store.CreateInvites(tctx(t), "usera", 1, nil)

	for _, tab := range []string{"api", "rss", "invitations", "profile"} {
		a.get("/account?tab=" + tab)
		for _, secret := range []string{"bobs-api-label", "bobs-rss-label", bInv[0], bAPI.Plaintext, bRSS.Plaintext} {
			if strings.Contains(a.body, secret) {
				t.Fatalf("tab %s shows another user's data (%q)", tab, secret)
			}
		}
	}
	bl, _ := e.store.ListAPITokens(tctx(t), ub.ID)
	rl, _ := e.store.ListRSSTokens(tctx(t), ub.ID)
	il, _ := e.store.ListInvitesByOwner(tctx(t), ub.ID)
	for path, tab := range map[string]string{
		"/account/api-tokens/" + itoa(bl[0].ID) + "/revoke": "api",
		"/account/rss-tokens/" + itoa(rl[0].ID) + "/revoke": "rss",
		"/account/invites/" + itoa(il[0].ID) + "/revoke":    "invitations",
		"/account/api-tokens/99999/revoke":                  "api",
		"/account/rss-tokens/abc/revoke":                    "rss",
		"/account/invites/-1/revoke":                        "invitations",
	} {
		if res := a.post(path, a.accountForm(tab)); res.StatusCode != 404 {
			t.Fatalf("POST %s: %d", path, res.StatusCode)
		}
	}
	if _, err := e.store.AuthenticateAPIToken(tctx(t), bAPI.Plaintext); err != nil {
		t.Fatal("a foreign revoke took effect (api)")
	}
	if _, err := e.store.AuthenticateRSSToken(tctx(t), bRSS.Plaintext); err != nil {
		t.Fatal("a foreign revoke took effect (rss)")
	}
	if ok, _ := e.store.CheckInvite(tctx(t), bInv[0]); !ok {
		t.Fatal("a foreign revoke took effect (invite)")
	}
	_, _ = b, ua
	_ = aInv
}

func TestInvitationsTabListsCopyLinksAndRevokes(t *testing.T) {
	e := newEnv(t)
	c, u := e.signedIn("inv", pw1)
	codes, _ := e.store.CreateInvites(tctx(t), "inv", 3, nil)
	// One used, one revoked later through the UI.
	if _, err := e.store.RegisterWithInvite(tctx(t), accounts.RegisterParams{Code: codes[0], Username: "guest", Hash: mustHash(t, e)}); err != nil {
		t.Fatal(err)
	}
	c.get("/account?tab=invitations")
	for _, want := range []string{publicBase + "/register/" + codes[1], publicBase + "/register/" + codes[2], "used by guest", "2 available", "data-copy="} {
		if !strings.Contains(c.body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(c.body, publicBase+"/register/"+codes[0]) {
		t.Error("a used invitation still shows its link")
	}
	if !strings.Contains(c.body, "cannot create invitations yourself") || strings.Contains(c.body, `action="/account/invites"`) {
		t.Error("the page offers or hides the no-self-service rule wrongly")
	}
	list, _ := e.store.ListInvitesByOwner(tctx(t), u.ID)
	var avail, used int64
	for _, v := range list {
		if v.Code == codes[1] {
			avail = v.ID
		}
		if v.Code == codes[0] {
			used = v.ID
		}
	}
	if res := c.post("/account/invites/"+itoa(avail)+"/revoke", c.accountForm("invitations")); res.StatusCode != 303 {
		t.Fatalf("revoke: %d", res.StatusCode)
	}
	if ok, _ := e.store.CheckInvite(tctx(t), codes[1]); ok {
		t.Fatal("revoked invitation still usable")
	}
	if res := c.post("/account/invites/"+itoa(used)+"/revoke", c.accountForm("invitations")); res.StatusCode != 409 {
		t.Fatalf("revoke used: %d", res.StatusCode)
	}
	// There is no creation route.
	for _, p := range []string{"/account/invites", "/account/invites/new"} {
		if res := c.post(p, c.accountForm("invitations")); res.StatusCode != 404 && res.StatusCode != 405 {
			t.Fatalf("POST %s: %d", p, res.StatusCode)
		}
	}
	// A suspended owner's invitations are listed but a disabled account cannot sign in.
	if _, err := e.store.Disable(tctx(t), "inv"); err != nil {
		t.Fatal(err)
	}
	if res := c.get("/account"); res.StatusCode != 303 {
		t.Fatalf("disabled account reaches the page: %d", res.StatusCode)
	}
}

// Every state-changing route refuses a request without a valid CSRF token,
// whether or not the visitor has a session.
func TestEveryPostRouteRequiresCSRF(t *testing.T) {
	e := newEnv(t)
	c, _ := e.signedIn("csrf", pw1)
	anon := e.client()
	anon.get("/login") // pre-session cookie, but the form carries no token
	posts := 0
	for _, r := range e.srv.Router().Routes() {
		method, path, _ := strings.Cut(r.Pattern, " ")
		if method != "POST" {
			continue
		}
		posts++
		path = strings.NewReplacer("{code}", "abc", "{token}", "abc", "{id}", "1").Replace(path)
		var cl *client
		switch r.Access {
		case server.Session:
			cl = c
		case server.PublicForm:
			cl = anon
		default:
			t.Fatalf("POST route %s has access %v", r.Pattern, r.Access)
		}
		for name, form := range map[string]url.Values{"missing": {"x": {"y"}}, "wrong": {"csrf_token": {"not-a-valid-token"}}} {
			if res := cl.post(path, form); res.StatusCode != 403 {
				t.Errorf("%s with a %s token: %d", r.Pattern, name, res.StatusCode)
			}
		}
	}
	if posts < 14 {
		t.Fatalf("only %d POST routes found", posts)
	}
	// Nothing changed.
	if e.count(`SELECT count(*) FROM magnetico_web.api_tokens`)+e.count(`SELECT count(*) FROM magnetico_web.rss_tokens`) != 0 {
		t.Fatal("a token was created without CSRF")
	}
}

// Sensitive values never reach the log, whatever the flow.
func TestLogsContainNoSecrets(t *testing.T) {
	e := newEnv(t)
	c, u := e.signedIn("logger", pw1)
	e.exec(`UPDATE magnetico_web.users SET email = 'logger@example.org', email_verified_at = now() WHERE id = $1`, u.ID)
	codes, _ := e.store.CreateInvites(tctx(t), "logger", 1, nil)

	// Exercise: tokens, password change, email, forgot, registration.
	c.post("/account/api-tokens", c.accountForm("api", "label", "label-secret-xyz"))
	apiTok := secretCode.FindStringSubmatch(c.body)[1]
	c.post("/account/rss-tokens", c.accountForm("rss"))
	rssTok := secretCode.FindStringSubmatch(c.body)[1]
	anon := e.client()
	f := anon.form("/password/forgot", "email", "logger@example.org")
	anon.post("/password/forgot", f)
	msgs := e.waitMail(1)
	resetPath := linkOf(t, msgs[0])
	reg := e.client()
	rf := reg.form("/register/"+codes[0], "username", "someone-new", "password", pw2, "password2", pw2, "email", "someone@example.org")
	reg.post("/register/"+codes[0], rf)
	pf := e.client().form(resetPath, "password", "brand-new-pass-1", "password2", "brand-new-pass-1")
	_ = pf
	cc := e.client()
	pf = cc.form(resetPath, "password", "brand-new-pass-1", "password2", "brand-new-pass-1")
	cc.post(resetPath, pf)
	// A failing request too.
	cc.post("/password/reset/bogus-token-value", cc.form("/password/reset/bogus-token-value", "password", pw2, "password2", pw2))
	e.settle()

	logs := e.log.String()
	if logs == "" {
		t.Fatal("no logs captured")
	}
	for _, secret := range []string{
		pw1, pw2, "brand-new-pass-1", apiTok, rssTok, strings.TrimPrefix(apiTok, "mgt_"), strings.TrimPrefix(rssTok, "mgf_"),
		codes[0], "logger@example.org", "someone@example.org", "someone-new", "label-secret-xyz",
		strings.TrimPrefix(resetPath, "/password/reset/"), "bogus-token-value", c.sessionCookie(),
	} {
		if secret != "" && strings.Contains(logs, secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
}

var _ = server.KindRequest
