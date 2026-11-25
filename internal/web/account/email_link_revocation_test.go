package account

import (
	"strings"
	"testing"
)

const pendingLinks = `SELECT count(*) FROM magnetico_web.one_time_tokens WHERE user_id = $1 AND purpose = 'email_verify'`

// A pending replacement address survives neither a self-service password
// change nor a password reset; the verified address it would replace does.
func TestPasswordChangeAndResetCancelPendingEmailLinkButKeepVerifiedAddress(t *testing.T) {
	e := newEnv(t)
	c, u := e.signedIn("pcl", pw1)
	e.setEmail(u.ID, "verified", "old@example.org")

	// The user asks to replace the address; the link goes to the new one.
	if res := c.post("/account/email", c.accountForm("profile", "email", "new@example.org", "current_password", pw1)); res.StatusCode != 303 {
		t.Fatalf("request: %d\n%s", res.StatusCode, c.body)
	}
	first := linkOf(t, e.waitMail(1)[0])

	res := c.post("/account/password", c.accountForm("profile", "current_password", pw1, "new_password", pw2, "new_password2", pw2))
	if res.StatusCode != 303 {
		t.Fatalf("password change: %d\n%s", res.StatusCode, c.body)
	}
	if e.count(pendingLinks, u.ID) != 0 {
		t.Fatal("the pending verification link survived the password change")
	}
	cur, _ := e.store.GetUserByID(tctx(t), u.ID)
	if cur.Email != "old@example.org" || !cur.EmailVerified {
		t.Fatalf("the verified address was touched: %+v", cur)
	}
	c.get("/account?tab=profile&done=password")
	if !strings.Contains(c.body, "Password recovery is available") || strings.Contains(c.body, "awaiting confirmation") {
		t.Fatalf("profile page after the change:\n%s", c.body)
	}
	if !strings.Contains(c.body, "no longer works") {
		t.Fatal("the notice does not say that earlier confirmation links stop working")
	}

	// The old link is refused with the generic message and changes nothing.
	anon := e.client()
	f := anon.form(first)
	if res := anon.post(first, f); res.StatusCode != 400 || !strings.Contains(anon.body, "cannot be used") {
		t.Fatalf("old link: %d\n%s", res.StatusCode, anon.body)
	}
	if cur, _ := e.store.GetUserByID(tctx(t), u.ID); cur.Email != "old@example.org" || !cur.EmailVerified {
		t.Fatalf("a cancelled link changed the address: %+v", cur)
	}

	// A fresh request after the change works end to end.
	if res := c.post("/account/email", c.accountForm("profile", "email", "new@example.org", "current_password", pw2)); res.StatusCode != 303 {
		t.Fatalf("fresh request: %d\n%s", res.StatusCode, c.body)
	}
	second := linkOf(t, e.waitMail(2)[1])
	f = anon.form(second)
	if res := anon.post(second, f); res.StatusCode != 200 || !strings.Contains(anon.body, "Email address confirmed") {
		t.Fatalf("fresh link: %d\n%s", res.StatusCode, anon.body)
	}
	if cur, _ := e.store.GetUserByID(tctx(t), u.ID); cur.Email != "new@example.org" || !cur.EmailVerified {
		t.Fatalf("after the fresh confirmation: %+v", cur)
	}

	// Password recovery through the verified address still works, and the reset
	// it completes cancels a pending link the same way.
	if res := c.post("/account/email", c.accountForm("profile", "email", "newer@example.org", "current_password", pw2)); res.StatusCode != 303 {
		t.Fatalf("request: %d", res.StatusCode)
	}
	// Mail 3 was the notice to the replaced address.
	third := linkOf(t, e.waitMail(4)[3])
	r := e.client()
	if res := r.post("/password/forgot", r.form("/password/forgot", "email", "new@example.org")); res.StatusCode != 200 {
		t.Fatalf("forgot: %d", res.StatusCode)
	}
	reset := linkOf(t, e.waitMail(5)[4])
	if !strings.HasPrefix(reset, "/password/reset/") {
		t.Fatalf("reset link = %s", reset)
	}
	if res := r.post(reset, r.form(reset, "password", pw1, "password2", pw1)); res.StatusCode != 303 {
		t.Fatalf("complete reset: %d\n%s", res.StatusCode, r.body)
	}
	if e.count(pendingLinks, u.ID) != 0 {
		t.Fatal("the pending verification link survived the reset")
	}
	if res := anon.post(third, anon.form(third)); res.StatusCode != 400 {
		t.Fatalf("link issued before the reset: %d", res.StatusCode)
	}
	if cur, _ := e.store.GetUserByID(tctx(t), u.ID); cur.Email != "new@example.org" || !cur.EmailVerified {
		t.Fatalf("the reset touched the verified address: %+v", cur)
	}
	e.client().signIn("pcl", pw1)
}

// An unverified address stays stored and unverified after a password change,
// and its verification can be requested again with the usual action.
func TestPasswordChangeKeepsUnverifiedAddressAndResendWorks(t *testing.T) {
	e := newEnv(t)
	c, u := e.signedIn("pcu", pw1)
	e.setEmail(u.ID, "unverified", "hist@example.org")
	if res := c.post("/account/email/resend", c.accountForm("profile")); res.StatusCode != 303 {
		t.Fatalf("resend: %d\n%s", res.StatusCode, c.body)
	}
	old := linkOf(t, e.waitMail(1)[0])

	if res := c.post("/account/password", c.accountForm("profile", "current_password", pw1, "new_password", pw2, "new_password2", pw2)); res.StatusCode != 303 {
		t.Fatalf("password change: %d", res.StatusCode)
	}
	if cur, _ := e.store.GetUserByID(tctx(t), u.ID); cur.Email != "hist@example.org" || cur.EmailVerified {
		t.Fatalf("stored address changed: %+v", cur)
	}
	c.get("/account?tab=profile")
	if !strings.Contains(c.body, textSendVerify) || strings.Contains(c.body, "awaiting confirmation") {
		t.Fatalf("profile page:\n%s", c.body)
	}
	anon := e.client()
	if res := anon.post(old, anon.form(old)); res.StatusCode != 400 {
		t.Fatalf("old link: %d", res.StatusCode)
	}
	if res := c.post("/account/email/resend", c.accountForm("profile")); res.StatusCode != 303 {
		t.Fatalf("resend after the change: %d\n%s", res.StatusCode, c.body)
	}
	fresh := linkOf(t, e.waitMail(2)[1])
	if res := anon.post(fresh, anon.form(fresh)); res.StatusCode != 200 {
		t.Fatalf("fresh link: %d\n%s", res.StatusCode, anon.body)
	}
	if cur, _ := e.store.GetUserByID(tctx(t), u.ID); cur.Email != "hist@example.org" || !cur.EmailVerified {
		t.Fatalf("after the fresh confirmation: %+v", cur)
	}
}

// A request that was authorized before a password change commits cannot
// create a pending link after it: the issuing transaction finds its session
// gone and the handler sends the user to sign in.
func TestEmailRequestAuthorizedBeforeAPasswordChangeCreatesNoLink(t *testing.T) {
	for _, tc := range []struct{ name, path string }{{"change", "/account/email"}, {"resend", "/account/email/resend"}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			c, u := e.signedIn("pcr", pw1)
			e.setEmail(u.ID, "unverified", "hist@example.org")
			f := c.accountForm("profile", "current_password", pw1, "email", "race@example.org")
			e.store.beforeSessionIssue = func() {
				// What a committed password change does to the sessions.
				e.exec(`DELETE FROM magnetico_web.sessions WHERE user_id = $1`, u.ID)
			}
			res := c.post(tc.path, f)
			e.store.beforeSessionIssue = nil
			if res.StatusCode != 303 || res.Header.Get("Location") != "/login" {
				t.Fatalf("%d %s\n%s", res.StatusCode, res.Header.Get("Location"), c.body)
			}
			if e.count(pendingLinks, u.ID) != 0 {
				t.Fatal("a pending link was created for a revoked session")
			}
			e.settle()
			if n := len(e.mailer.all()); n != 0 {
				t.Fatalf("%d message(s) sent", n)
			}
		})
	}
}
