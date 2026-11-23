package account

import (
	"context"
	"crypto/sha256"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
)

const (
	pw1 = "correct horse battery"
	pw2 = "another long passphrase"
)

func TestRegistrationHappyPathAndQuota(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.inviteQuota = 3 })
	owner := e.mkUser("inviter", pw1)
	codes, _ := e.store.CreateInvites(tctx(t), "inviter", 1, nil)
	_ = owner

	c := e.client()
	f := c.form("/register/"+codes[0], "username", "newbie", "password", pw2, "password2", pw2, "email", "newbie@example.org")
	if !strings.Contains(c.body, "Create your account") {
		t.Fatalf("form page:\n%s", c.body)
	}
	if got := c.last.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", got)
	}
	res := c.post("/register/"+codes[0], f)
	if res.StatusCode != 303 || res.Header.Get("Location") != "/login?notice=registered" {
		t.Fatalf("POST: %d %s\n%s", res.StatusCode, res.Header.Get("Location"), c.body)
	}
	u, err := e.store.Store.GetUser(tctx(t), "newbie")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "newbie@example.org" || u.EmailVerified || u.Role != "user" {
		t.Fatalf("user = %+v", u)
	}
	if n := e.count(`SELECT count(*) FROM magnetico_web.invites WHERE owner_id = $1 AND used_by IS NULL`, u.ID); n != 3 {
		t.Fatalf("new user quota = %d, want 3", n)
	}
	if n := e.count(`SELECT count(*) FROM magnetico_web.invites WHERE code = $1 AND used_by = $2`, codes[0], u.ID); n != 1 {
		t.Fatal("invitation not consumed")
	}
	// The verification email arrives with a working link, and a login works.
	msgs := e.waitMail(1)
	if msgs[0].To != "newbie@example.org" || !strings.Contains(linkOf(t, msgs[0]), "/email/verify/") {
		t.Fatalf("mail = %+v", msgs[0])
	}
	e.client().signIn("newbie", pw2)
}

func TestEveryUnusableInvitationLooksTheSame(t *testing.T) {
	e := newEnv(t)
	e.mkUser("owner", pw1)
	codes, _ := e.store.CreateInvites(tctx(t), "owner", 5, nil)
	e.exec(`UPDATE magnetico_web.invites SET revoked_at = now() WHERE code = $1`, codes[0])
	e.exec(`UPDATE magnetico_web.invites SET expires_at = now() - interval '1 second' WHERE code = $1`, codes[1])
	if _, err := e.store.RegisterWithInvite(tctx(t), accounts.RegisterParams{Code: codes[2], Username: "taker", Hash: mustHash(t, e)}); err != nil {
		t.Fatal(err)
	}
	// codes[3]: its owner is suspended.
	e.mkUser("suspended", pw1)
	susp, _ := e.store.CreateInvites(tctx(t), "suspended", 1, nil)
	if _, err := e.store.Store.Disable(tctx(t), "suspended"); err != nil {
		t.Fatal(err)
	}

	var firstBody string
	var firstStatus int
	for i, code := range []string{codes[0], codes[1], codes[2], susp[0], "does-not-exist-000000", "x"} {
		c := e.client()
		res := c.get("/register/" + code)
		if !strings.Contains(c.body, msgInviteGone) || strings.Contains(c.body, "<form") {
			t.Fatalf("case %d GET:\n%s", i, c.body)
		}
		if i == 0 {
			firstBody, firstStatus = c.body, res.StatusCode
		} else if c.body != firstBody || res.StatusCode != firstStatus {
			t.Fatalf("case %d differs from the first (status %d vs %d)", i, res.StatusCode, firstStatus)
		}
		// A POST with otherwise valid data is refused the same way.
		f := url.Values{"csrf_token": {tokenFromPre(t, e, c)}, "username": {"x" + string(rune('a'+i)) + "yz"}, "password": {pw2}, "password2": {pw2}}
		res = c.post("/register/"+code, f)
		if res.StatusCode != firstStatus || !strings.Contains(c.body, msgInviteGone) {
			t.Fatalf("case %d POST: %d\n%s", i, res.StatusCode, c.body)
		}
	}
	if n := e.count(`SELECT count(*) FROM magnetico_web.users WHERE username LIKE 'x_yz'`); n != 0 {
		t.Fatal("a registration succeeded through an unusable invitation")
	}
	// The suspended owner's invitation was not consumed by the refusal.
	if n := e.count(`SELECT count(*) FROM magnetico_web.invites WHERE code = $1 AND used_by IS NULL AND revoked_at IS NULL`, susp[0]); n != 1 {
		t.Fatal("refusal consumed the invitation")
	}
}

// tokenFromPre returns a pre-session CSRF token for the client (any public
// form page provides one).
func tokenFromPre(t *testing.T, e *env, c *client) string {
	t.Helper()
	c.get("/login")
	return c.token()
}

func mustHash(t *testing.T, e *env) string {
	t.Helper()
	h, err := e.svc.Hash(context.Background(), pw2)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRegistrationValidationAndDuplicateUsernameKeepInvitation(t *testing.T) {
	e := newEnv(t)
	e.mkUser("taken", pw1)
	codes, _ := e.store.CreateInvites(tctx(t), "taken", 1, nil)
	c := e.client()
	base := []string{"username", "fresh", "password", pw2, "password2", pw2}
	for name, kv := range map[string][]string{
		"mismatch":   {"username", "fresh", "password", pw2, "password2", pw2 + "x"},
		"short pw":   {"username", "fresh", "password", "short", "password2", "short"},
		"bad name":   {"username", "x", "password", pw2, "password2", pw2},
		"bad email":  append(append([]string{}, base...), "email", "not-an-address"),
		"duplicated": {"username", "taken", "password", pw2, "password2", pw2},
	} {
		f := c.form("/register/"+codes[0], kv...)
		res := c.post("/register/"+codes[0], f)
		want := 400
		if name == "duplicated" {
			want = 409
		}
		if res.StatusCode != want || !strings.Contains(c.body, "notice error") {
			t.Fatalf("%s: %d\n%s", name, res.StatusCode, c.body)
		}
		if strings.Contains(c.body, pw2) {
			t.Fatalf("%s: the password is echoed", name)
		}
	}
	if ok, _ := e.store.CheckInvite(tctx(t), codes[0]); !ok {
		t.Fatal("a rejected registration consumed the invitation")
	}
	if n := e.count(`SELECT count(*) FROM magnetico_web.users WHERE username = 'fresh'`); n != 0 {
		t.Fatal("user created despite validation errors")
	}
}

func TestRegistrationStaysCommittedWhenTheMailQueueIsFull(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.stopQueue = true; o.queueCap = 1 })
	e.mkUser("owner", pw1)
	codes, _ := e.store.CreateInvites(tctx(t), "owner", 1, nil)
	// Fill the only slot.
	if _, err := e.queue.Reserve(); err != nil {
		t.Fatal(err)
	}
	c := e.client()
	f := c.form("/register/"+codes[0], "username", "newbie", "password", pw2, "password2", pw2, "email", "n@example.org")
	res := c.post("/register/"+codes[0], f)
	if res.StatusCode != 303 || res.Header.Get("Location") != "/login?notice=registered-mailfail" {
		t.Fatalf("POST: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if n := e.count(`SELECT count(*) FROM magnetico_web.users WHERE username = 'newbie'`); n != 1 {
		t.Fatal("registration was rolled back")
	}
	// No link was issued without a slot; the user can ask later.
	if n := e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens WHERE purpose = 'email_verify'`); n != 0 {
		t.Fatal("an email link was issued although nothing could be sent")
	}
	cc := e.client()
	cc.get("/login?notice=registered-mailfail")
	if !strings.Contains(cc.body, "could not be sent") {
		t.Fatalf("login page lacks the notice:\n%s", cc.body)
	}
}

func TestPasswordLinksCompleteAndRevokeEverythingWithBothNotices(t *testing.T) {
	for _, purpose := range []string{accounts.PurposeReset, accounts.PurposeSetup} {
		t.Run(purpose, func(t *testing.T) {
			e := newEnv(t)
			u := e.mkUser("pat", pw1)
			api, _ := e.store.CreateAPIToken(tctx(t), u.ID, "a", nil)
			rss, _ := e.store.CreateRSSToken(tctx(t), u.ID, "r", nil)
			const legacy = "0123456789abcdef0123456789abcdef"
			lh := sha256.Sum256([]byte(legacy))
			e.exec(`INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy) VALUES ($1, $2, true)`, lh[:], u.ID)
			other := e.client()
			other.signIn("pat", pw1)
			if purpose == accounts.PurposeSetup {
				e.exec(`UPDATE magnetico_web.users SET password_hash = NULL WHERE id = $1`, u.ID)
			}
			link, err := e.store.IssuePasswordLink(tctx(t), "pat")
			if err != nil || link.Purpose != purpose {
				t.Fatalf("link = %+v, %v", link, err)
			}
			path := "/password/" + purpose + "/" + link.Token

			c := e.client()
			f := c.form(path, "password", pw2, "password2", pw2)
			for _, want := range []string{"all of your existing API tokens", "all of your existing RSS tokens and RSS feed URLs", "update your torrent clients"} {
				if !strings.Contains(c.body, want) {
					t.Errorf("confirmation page lacks %q", want)
				}
			}
			if c.last.Header.Get("Referrer-Policy") != "no-referrer" {
				t.Error("Referrer-Policy")
			}
			if res := c.post(path, f); res.StatusCode != 303 || res.Header.Get("Location") != "/login?notice=password-set" {
				t.Fatalf("POST: %d\n%s", res.StatusCode, c.body)
			}
			if n := e.count(`SELECT count(*) FROM magnetico_web.sessions WHERE user_id = $1`, u.ID); n != 0 {
				t.Fatal("sessions survived")
			}
			for name, f := range map[string]func() error{
				"api":    func() error { _, e2 := e.store.Store.AuthenticateAPIToken(tctx(t), api.Plaintext); return e2 },
				"rss":    func() error { _, e2 := e.store.Store.AuthenticateRSSToken(tctx(t), rss.Plaintext); return e2 },
				"legacy": func() error { _, e2 := e.store.Store.AuthenticateRSSToken(tctx(t), legacy); return e2 },
			} {
				if f() == nil {
					t.Errorf("%s token survived", name)
				}
			}
			// Reuse fails with the generic message and the new password works.
			c2 := e.client()
			f2 := c2.form(path, "password", pw2, "password2", pw2)
			if res := c2.post(path, f2); res.StatusCode != 400 || !strings.Contains(c2.body, msgLinkGone) {
				t.Fatalf("reuse: %d\n%s", res.StatusCode, c2.body)
			}
			e.client().signIn("pat", pw2)
			c3 := e.client()
			c3.get("/login?notice=password-set")
			if !strings.Contains(c3.body, "API token and RSS token") {
				t.Errorf("login notice:\n%s", c3.body)
			}
		})
	}
}

func TestPasswordLinkRefusalsAreGenericAndDisabledLinkIsNotConsumed(t *testing.T) {
	e := newEnv(t)
	e.mkUser("dee", pw1)
	link, _ := e.store.IssuePasswordLink(tctx(t), "dee")
	path := "/password/reset/" + link.Token

	c := e.client()
	// Mismatch and short passwords keep the form.
	f := c.form(path, "password", pw2, "password2", "different")
	if res := c.post(path, f); res.StatusCode != 400 || !strings.Contains(c.body, "do not match") || !strings.Contains(c.body, "<form") {
		t.Fatalf("mismatch: %d", res.StatusCode)
	}
	// Disabled owner: generic refusal, link survives, works after enable.
	if _, err := e.store.Store.Disable(tctx(t), "dee"); err != nil {
		t.Fatal(err)
	}
	f = c.form(path, "password", pw2, "password2", pw2)
	if res := c.post(path, f); res.StatusCode != 400 || !strings.Contains(c.body, msgLinkGone) {
		t.Fatalf("disabled: %d\n%s", res.StatusCode, c.body)
	}
	if n := e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens WHERE purpose = 'reset'`); n != 1 {
		t.Fatal("a disabled account's link was consumed")
	}
	if n := e.count(`SELECT count(*) FROM magnetico_web.users WHERE username = 'dee' AND active`); n != 0 {
		t.Fatal("completing the link reactivated the account")
	}
	if err := e.store.Store.Enable(tctx(t), "dee"); err != nil {
		t.Fatal(err)
	}
	f = c.form(path, "password", pw2, "password2", pw2)
	if res := c.post(path, f); res.StatusCode != 303 {
		t.Fatalf("after enable: %d\n%s", res.StatusCode, c.body)
	}
	// The wrong purpose path and an unknown token look the same.
	l2, _ := e.store.IssuePasswordLink(tctx(t), "dee")
	for _, p := range []string{"/password/setup/" + l2.Token, "/password/reset/nonsense", "/password/reset/" + strings.Repeat("a", 300)} {
		c := e.client()
		f := c.form(p, "password", pw2, "password2", pw2)
		if res := c.post(p, f); res.StatusCode != 400 || !strings.Contains(c.body, msgLinkGone) {
			t.Fatalf("%s: %d", p, res.StatusCode)
		}
	}
}

func TestForgotPasswordIsIndistinguishableAndDoesNoWorkInTheRequest(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.stopQueue = true })
	// Four account classes plus an unknown address.
	valid := e.mkUser("valid", pw1)
	e.exec(`UPDATE magnetico_web.users SET email = 'valid@example.org', email_verified_at = now() WHERE id = $1`, valid.ID)
	unv := e.mkUser("unverified", pw1)
	e.exec(`UPDATE magnetico_web.users SET email = 'unverified@example.org' WHERE id = $1`, unv.ID)
	dis := e.mkUser("disabled", pw1)
	e.exec(`UPDATE magnetico_web.users SET email = 'disabled@example.org', email_verified_at = now(), active = false WHERE id = $1`, dis.ID)

	type shot struct {
		status int
		body   string
		hdr    string
		dur    time.Duration
	}
	var shots []shot
	for _, addr := range []string{"valid@example.org", "unverified@example.org", "disabled@example.org", "nobody@example.org", "garbage", ""} {
		c := e.client()
		f := c.form("/password/forgot", "email", addr)
		before := e.store.calls.Load()
		start := time.Now()
		res := c.post("/password/forgot", f)
		d := time.Since(start)
		if e.store.calls.Load() != before {
			t.Fatalf("%q: the request handler used the store", addr)
		}
		shots = append(shots, shot{res.StatusCode, c.body, res.Header.Get("Cache-Control"), d})
	}
	for i, s := range shots {
		if s.status != 200 || !strings.Contains(s.body, "If the address you entered is registered and verified, a password reset link has been sent.") {
			t.Fatalf("shot %d: %d\n%s", i, s.status, s.body)
		}
		if s.body != shots[0].body || s.hdr != shots[0].hdr {
			t.Fatalf("shot %d differs from shot 0", i)
		}
	}
	if len(e.mailer.all()) != 0 {
		t.Fatal("mail sent although no worker ran")
	}
}

func TestForgotPasswordFullFlowMailsOnlyTheVerifiedActiveAccount(t *testing.T) {
	e := newEnv(t)
	valid := e.mkUser("valid", pw1)
	e.exec(`UPDATE magnetico_web.users SET email = 'valid@example.org', email_verified_at = now() WHERE id = $1`, valid.ID)
	unv := e.mkUser("unverified", pw1)
	e.exec(`UPDATE magnetico_web.users SET email = 'unverified@example.org' WHERE id = $1`, unv.ID)
	dis := e.mkUser("disabled", pw1)
	e.exec(`UPDATE magnetico_web.users SET email = 'disabled@example.org', email_verified_at = now(), active = false WHERE id = $1`, dis.ID)
	api, _ := e.store.CreateAPIToken(tctx(t), valid.ID, "", nil)

	for _, addr := range []string{"unverified@example.org", "disabled@example.org", "nobody@example.org", "VALID@example.org"} {
		c := e.client()
		f := c.form("/password/forgot", "email", addr)
		if res := c.post("/password/forgot", f); res.StatusCode != 200 {
			t.Fatalf("%s: %d", addr, res.StatusCode)
		}
	}
	e.settle()
	msgs := e.mailer.all()
	if len(msgs) != 1 || msgs[0].To != "valid@example.org" {
		t.Fatalf("messages = %+v", msgs)
	}
	for _, want := range []string{"ALL of your existing API tokens", "ALL of your existing RSS tokens", "1 hour"} {
		if !strings.Contains(msgs[0].Body, want) {
			t.Errorf("mail lacks %q:\n%s", want, msgs[0].Body)
		}
	}
	path := linkOf(t, msgs[0])
	if !strings.HasPrefix(path, "/password/reset/") {
		t.Fatalf("link = %s", path)
	}
	// The emailed link completes the reset and revokes the API token.
	c := e.client()
	f := c.form(path, "password", pw2, "password2", pw2)
	if res := c.post(path, f); res.StatusCode != 303 {
		t.Fatalf("complete: %d\n%s", res.StatusCode, c.body)
	}
	if _, err := e.store.Store.AuthenticateAPIToken(tctx(t), api.Plaintext); err == nil {
		t.Fatal("the reset left an API token valid")
	}
	// No link was left behind for the ineligible accounts.
	if n := e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens WHERE user_id IN ($1, $2)`, unv.ID, dis.ID); n != 0 {
		t.Fatalf("%d links issued for ineligible accounts", n)
	}
}

func TestForgotPasswordRateLimitsAndUnconfiguredMail(t *testing.T) {
	// Per-IP limiter: the answer is 429 with Retry-After, independent of the address.
	lim, _ := ratelimit.New(ratelimit.Config{Burst: 2, Interval: time.Hour, MaxKeys: 10})
	e := newEnv(t, func(o *envOpts) { o.forgotIP = lim; o.stopQueue = true })
	c := e.client()
	f := c.form("/password/forgot", "email", "a@example.org")
	var got []int
	for i := 0; i < 4; i++ {
		got = append(got, c.post("/password/forgot", f).StatusCode)
	}
	if got[0] != 200 || got[1] != 200 || got[2] != 429 || got[3] != 429 {
		t.Fatalf("statuses = %v", got)
	}
	if c.last.Header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}

	// Without SMTP the page says so, nothing is queued, and login still works.
	e2 := newEnv(t, func(o *envOpts) { o.noMail = true })
	e2.mkUser("uu", pw1)
	c2 := e2.client()
	c2.get("/login")
	f2 := url.Values{"csrf_token": {c2.token()}, "email": {"a@example.org"}}
	res := c2.post("/password/forgot", f2)
	if res.StatusCode != 200 || !strings.Contains(c2.body, "not available on this server") || strings.Contains(c2.body, "has been sent") {
		t.Fatalf("unconfigured: %d\n%s", res.StatusCode, c2.body)
	}
	c3 := e2.client()
	c3.get("/login")
	if strings.Contains(c3.body, "Forgot password?") {
		t.Fatal("login offers recovery without SMTP")
	}
	e2.client().signIn("uu", pw1)
}

func TestEmailVerificationPagesAndDuplicateAddress(t *testing.T) {
	e := newEnv(t)
	a := e.mkUser("ann", pw1)
	b := e.mkUser("bob", pw1)
	la, _ := e.store.IssueEmailVerifyLink(tctx(t), a.ID, "shared@example.org")
	lb, _ := e.store.IssueEmailVerifyLink(tctx(t), b.ID, "shared@example.org")

	c := e.client()
	f := c.form("/email/verify/"+la.Token, "x", "y")
	if !strings.Contains(c.body, "Confirm address") || c.last.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("GET page:\n%s", c.body)
	}
	if n := e.count(`SELECT count(*) FROM magnetico_web.users WHERE email_verified_at IS NOT NULL`); n != 0 {
		t.Fatal("a GET confirmed the address")
	}
	if res := c.post("/email/verify/"+la.Token, f); res.StatusCode != 200 || !strings.Contains(c.body, "Email address confirmed") {
		t.Fatalf("confirm: %d\n%s", res.StatusCode, c.body)
	}
	// The second account gets the same generic message as an unknown link and
	// its link is not consumed.
	c2 := e.client()
	f2 := c2.form("/email/verify/" + lb.Token)
	res := c2.post("/email/verify/"+lb.Token, f2)
	body1 := c2.body
	c3 := e.client()
	f3 := c3.form("/email/verify/nonsense")
	res3 := c3.post("/email/verify/nonsense", f3)
	if res.StatusCode != 400 || res3.StatusCode != 400 || body1 != c3.body || !strings.Contains(body1, "cannot be used") {
		t.Fatalf("duplicate vs unknown differ: %d %d\n%s\n---\n%s", res.StatusCode, res3.StatusCode, body1, c3.body)
	}
	if n := e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens WHERE user_id = $1 AND purpose = 'email_verify'`, b.ID); n != 1 {
		t.Fatal("the conflicting link was consumed")
	}
}

func TestEmailChangeSendsNoticeToThePreviousAddress(t *testing.T) {
	e := newEnv(t)
	u := e.mkUser("chg", pw1)
	e.exec(`UPDATE magnetico_web.users SET email = 'old@example.org', email_verified_at = now() WHERE id = $1`, u.ID)
	l, _ := e.store.IssueEmailVerifyLink(tctx(t), u.ID, "new@example.org")
	c := e.client()
	f := c.form("/email/verify/" + l.Token)
	if res := c.post("/email/verify/"+l.Token, f); res.StatusCode != 200 {
		t.Fatalf("%d\n%s", res.StatusCode, c.body)
	}
	msgs := e.waitMail(1)
	if msgs[0].To != "old@example.org" || !strings.Contains(msgs[0].Subject, "changed") {
		t.Fatalf("notice = %+v", msgs[0])
	}
}

func TestConcurrentRegistrationsWithOneInvitationHaveOneWinner(t *testing.T) {
	e := newEnv(t)
	e.mkUser("owner", pw1)
	codes, _ := e.store.CreateInvites(tctx(t), "owner", 1, nil)
	var wg sync.WaitGroup
	statuses := make([]int, 6)
	clients := make([]*client, len(statuses))
	forms := make([]url.Values, len(statuses))
	for i := range statuses {
		clients[i] = e.client()
		forms[i] = clients[i].form("/register/"+codes[0], "username", "racer"+string(rune('a'+i)), "password", pw2, "password2", pw2)
	}
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = clients[i].post("/register/"+codes[0], forms[i]).StatusCode
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, s := range statuses {
		if s == 303 {
			wins++
		}
	}
	if wins != 1 || e.count(`SELECT count(*) FROM magnetico_web.users WHERE username LIKE 'racer_'`) != 1 {
		t.Fatalf("statuses = %v", statuses)
	}
}
