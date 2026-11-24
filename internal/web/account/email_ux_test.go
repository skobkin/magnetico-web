package account

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
)

// setEmail puts the account into one address state.
func (e *env) setEmail(userID int64, state string, addr string) {
	e.t.Helper()
	switch state {
	case "none":
		e.exec(`UPDATE magnetico_web.users SET email = NULL, email_verified_at = NULL WHERE id = $1`, userID)
	case "unverified":
		e.exec(`UPDATE magnetico_web.users SET email = $2, email_verified_at = NULL WHERE id = $1`, userID, addr)
	case "verified":
		e.exec(`UPDATE magnetico_web.users SET email = $2, email_verified_at = now() WHERE id = $1`, userID, addr)
	default:
		e.t.Fatalf("state %q", state)
	}
}

func (e *env) setPending(userID int64, addr string) {
	e.t.Helper()
	if _, err := e.store.IssueEmailVerifyLink(tctx(e.t), userID, addr); err != nil {
		e.t.Fatal(err)
	}
}

const (
	textVerifyHint   = "Verify your email address to enable password recovery. If you do not intend to verify it, you can remove it from your account."
	textUnavailable  = "Password recovery is unavailable."
	textAvailable    = "Password recovery is available"
	actionResend     = `action="/account/email/resend"`
	actionChange     = `action="/account/email"`
	actionRemove     = `action="/account/email/remove"`
	textSendVerify   = "Send verification email"
	textNoSMTPVerify = "cannot currently send email, so this address cannot be verified"
)

// The profile page tells the address state and the service state apart in
// every combination, and only offers what can work.
func TestEmailStatesAreExplainedAndOnlyWorkingActionsAreOffered(t *testing.T) {
	type want struct {
		has, not []string
	}
	cases := []struct {
		name    string
		mail    bool
		state   string
		pending string
		want    want
	}{
		{"mail/none", true, "none", "", want{
			has: []string{textUnavailable, "Add an email address and verify it", actionChange},
			not: []string{actionRemove, actionResend, textAvailable}}},
		{"mail/unverified (migrated historical)", true, "unverified", "", want{
			has: []string{"Unverified", textUnavailable, textVerifyHint, textSendVerify, actionResend, actionRemove, actionChange},
			not: []string{textAvailable}}},
		{"mail/verified", true, "verified", "", want{
			has: []string{"Verified", textAvailable, "Any password reset link already issued is cancelled", actionRemove, actionChange},
			not: []string{textUnavailable, actionResend}}},
		{"mail/verified + pending replacement", true, "verified", "new@example.org", want{
			has: []string{textAvailable, "does not replace it until it is confirmed", "new@example.org", actionRemove, "Send the confirmation email again"},
			not: []string{textUnavailable}}},
		{"mail/unverified + pending", true, "unverified", "new@example.org", want{
			has: []string{textUnavailable, "Send the confirmation email again", actionRemove},
			not: []string{textAvailable}}},
		{"mail/pending only", true, "none", "new@example.org", want{
			has: []string{textUnavailable, "Confirm the pending address", "Cancel the pending address", actionRemove},
			not: []string{textAvailable}}},
		{"nomail/none", false, "none", "", want{
			has: []string{textUnavailable, "This server cannot send email"},
			not: []string{actionChange, actionResend, actionRemove, "Verify your email address", textAvailable}}},
		{"nomail/unverified (migrated historical)", false, "unverified", "", want{
			has: []string{"Unverified", textUnavailable, textNoSMTPVerify, "you can remove it from your account", actionRemove},
			// Never the generic instruction to verify, and no verification action
			// that cannot work.
			not: []string{textVerifyHint, "Verify your email address", actionResend, textSendVerify, actionChange, textAvailable}}},
		{"nomail/verified", false, "verified", "", want{
			has: []string{"Verified", textUnavailable, "cannot currently send email, so a reset link cannot be sent", actionRemove},
			not: []string{textAvailable, actionResend, actionChange}}},
		{"nomail/pending only", false, "none", "new@example.org", want{
			has: []string{textUnavailable, "cannot be confirmed", "Cancel the pending address", actionRemove},
			not: []string{"Verify your email address", actionResend, textAvailable}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, func(o *envOpts) { o.noMail = !tc.mail })
			c, u := e.signedIn("state", pw1)
			e.setEmail(u.ID, tc.state, "me@example.org")
			if tc.pending != "" {
				e.setPending(u.ID, tc.pending)
			}
			res := c.get("/account?tab=profile")
			if res.StatusCode != 200 {
				t.Fatalf("status %d", res.StatusCode)
			}
			for _, s := range tc.want.has {
				if !strings.Contains(c.body, s) {
					t.Errorf("page lacks %q", s)
				}
			}
			for _, s := range tc.want.not {
				if strings.Contains(c.body, s) {
					t.Errorf("page contains %q", s)
				}
			}
		})
	}
}

func TestRemoveUnverifiedEmailNeedsPasswordAndCSRF(t *testing.T) {
	e := newEnv(t)
	c, u := e.signedIn("rmu", pw1)
	e.setEmail(u.ID, "unverified", "hist@example.org")
	e.setPending(u.ID, "hist@example.org")

	// Wrong or missing current password: nothing changes.
	for _, pw := range []string{"wrong-wrong-wrong", ""} {
		res := c.post("/account/email/remove", c.accountForm("profile", "current_password", pw))
		if res.StatusCode != 400 || !strings.Contains(c.body, "The current password is not correct.") {
			t.Fatalf("password %q: %d", pw, res.StatusCode)
		}
		cur, _ := e.store.GetUserByID(tctx(t), u.ID)
		if cur.Email != "hist@example.org" || e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens WHERE purpose = 'email_verify'`) != 1 {
			t.Fatalf("changed by a refused request: %+v", cur)
		}
	}
	// No CSRF token: refused before the handler.
	f := url.Values{"current_password": {pw1}}
	if res := c.post("/account/email/remove", f); res.StatusCode != 403 {
		t.Fatalf("without CSRF: %d", res.StatusCode)
	}
	if cur, _ := e.store.GetUserByID(tctx(t), u.ID); cur.Email == "" {
		t.Fatal("removed without a CSRF token")
	}

	res := c.post("/account/email/remove", c.accountForm("profile", "current_password", pw1))
	if res.StatusCode != 303 || res.Header.Get("Location") != "/account?tab=profile&done=email-removed" {
		t.Fatalf("remove: %d %s\n%s", res.StatusCode, res.Header.Get("Location"), c.body)
	}
	cur, _ := e.store.GetUserByID(tctx(t), u.ID)
	if cur.Email != "" || cur.EmailVerified || cur.PasswordHash == "" || !cur.Active {
		t.Fatalf("after removal: %+v", cur)
	}
	if e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens WHERE purpose = 'email_verify'`) != 0 {
		t.Fatal("pending verification link survived")
	}
	c.get("/account?tab=profile&done=email-removed")
	if !strings.Contains(c.body, "Email address removed.") || !strings.Contains(c.body, "None. Email is optional.") {
		t.Fatalf("page after removal:\n%s", c.body)
	}
	// An unverified address was never a recovery channel: no notice mail.
	e.settle()
	if n := len(e.mailer.all()); n != 0 {
		t.Fatalf("%d messages sent for an unverified removal", n)
	}
	// Nothing left to remove.
	if res := c.post("/account/email/remove", c.accountForm("profile", "current_password", pw1)); res.StatusCode != 400 || !strings.Contains(c.body, "no email address to remove") {
		t.Fatalf("second removal: %d", res.StatusCode)
	}
}

func TestRemoveEmailWorksWithoutSMTP(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.noMail = true })
	c, u := e.signedIn("rmn", pw1)
	e.setEmail(u.ID, "unverified", "hist@example.org")
	// Verification and change need email; removal does not.
	if res := c.post("/account/email/resend", c.accountForm("profile")); res.StatusCode != 409 {
		t.Fatalf("resend: %d", res.StatusCode)
	}
	res := c.post("/account/email/remove", c.accountForm("profile", "current_password", pw1))
	if res.StatusCode != 303 {
		t.Fatalf("remove without SMTP: %d\n%s", res.StatusCode, c.body)
	}
	if cur, _ := e.store.GetUserByID(tctx(t), u.ID); cur.Email != "" {
		t.Fatalf("not removed: %+v", cur)
	}
}

func TestRemoveVerifiedEmailCancelsResetLinksAndNotifiesTheAddress(t *testing.T) {
	e := newEnv(t)
	c, u := e.signedIn("rmv", pw1)
	e.setEmail(u.ID, "verified", "old@example.org")
	other := e.mkUser("rmother", pw1)
	e.setEmail(other.ID, "verified", "other@example.org")

	// The user asked for a reset by email before removing the address.
	anon := e.client()
	if res := anon.post("/password/forgot", anon.form("/password/forgot", "email", "old@example.org")); res.StatusCode != 200 {
		t.Fatalf("forgot: %d", res.StatusCode)
	}
	reset := linkOf(t, e.waitMail(1)[0])
	if _, err := e.store.IssueRecoveryLink(tctx(t), "other@example.org"); err != nil {
		t.Fatal(err)
	}

	res := c.post("/account/email/remove", c.accountForm("profile", "current_password", pw1))
	if res.StatusCode != 303 {
		t.Fatalf("remove: %d\n%s", res.StatusCode, c.body)
	}
	// The link that was mailed to the removed address is dead, on the page and
	// in the store; another account's link is untouched.
	page := e.client()
	f := page.form(reset)
	if res := page.post(reset, f); res.StatusCode != 400 {
		t.Fatalf("cancelled link accepted: %d\n%s", res.StatusCode, page.body)
	}
	if n := e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens WHERE user_id = $1`, u.ID); n != 0 {
		t.Fatalf("%d links left for the account", n)
	}
	if n := e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens WHERE user_id = $1`, other.ID); n != 1 {
		t.Fatalf("another account's links = %d", n)
	}
	// The removed address gets a notice (verified removals only), and the page
	// answers the same for the old address afterwards.
	msgs := e.waitMail(2)
	last := msgs[len(msgs)-1]
	if last.To != "old@example.org" || !strings.Contains(last.Subject, "removed") {
		t.Fatalf("notice = %+v", last)
	}
	anon2 := e.client()
	f2 := anon2.form("/password/forgot", "email", "old@example.org")
	r1 := anon2.post("/password/forgot", f2)
	body1 := anon2.body
	anon3 := e.client()
	r2 := anon3.post("/password/forgot", anon3.form("/password/forgot", "email", "nobody@example.org"))
	if r1.StatusCode != r2.StatusCode || body1 != anon3.body {
		t.Fatal("the removed address is distinguishable from an unknown one")
	}
	e.settle()
	if n := e.count(`SELECT count(*) FROM magnetico_web.one_time_tokens WHERE user_id = $1`, u.ID); n != 0 {
		t.Fatalf("a recovery link was issued for the removed address (%d)", n)
	}
}

func TestRemovingAnotherUsersAddressIsImpossible(t *testing.T) {
	e := newEnv(t)
	a, au := e.signedIn("alice1", pw1)
	_, bu := e.signedIn("bob1", pw1)
	e.setEmail(au.ID, "unverified", "a@example.org")
	e.setEmail(bu.ID, "verified", "b@example.org")
	// The route takes no user identifier at all: it only ever acts on the
	// session's own account. A forged field is ignored.
	f := a.accountForm("profile", "current_password", pw1, "user_id", "1", "id", "2")
	if res := a.post("/account/email/remove", f); res.StatusCode != 303 {
		t.Fatalf("%d", res.StatusCode)
	}
	if cur, _ := e.store.GetUserByID(tctx(t), bu.ID); cur.Email != "b@example.org" || !cur.EmailVerified {
		t.Fatalf("another account's address changed: %+v", cur)
	}
	if cur, _ := e.store.GetUserByID(tctx(t), au.ID); cur.Email != "" {
		t.Fatalf("own address not removed: %+v", cur)
	}
}

func TestRemoveEmailIsRateLimitedPerAccount(t *testing.T) {
	lim, _ := ratelimit.New(ratelimit.Config{Burst: 2, Interval: time.Hour, MaxKeys: 10})
	e := newEnv(t, func(o *envOpts) { o.userLim = lim })
	c, u := e.signedIn("rml", pw1)
	e.setEmail(u.ID, "unverified", "x@example.org")
	for i := 0; i < 2; i++ {
		res := c.post("/account/email/remove", c.accountForm("profile", "current_password", "wrong-wrong-wrong"))
		if res.StatusCode != 400 {
			t.Fatalf("attempt %d: %d", i, res.StatusCode)
		}
	}
	// Refused before the password is checked, so a hijacked session cannot
	// guess it, and nothing was removed.
	res := c.post("/account/email/remove", c.accountForm("profile", "current_password", pw1))
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("third attempt: %d", res.StatusCode)
	}
	if cur, _ := e.store.GetUserByID(tctx(t), u.ID); cur.Email != "x@example.org" {
		t.Fatal("removed while limited")
	}
}
