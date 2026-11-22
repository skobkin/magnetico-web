package accounts

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNullHashHasNoPassword(t *testing.T) {
	s, _ := newStore(t)
	res, err := s.CreateUser(tctx(t), CreateUserParams{Username: "carol", Email: "carol@example.org", Invites: 2})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := s.GetUser(tctx(t), "carol")
	if u.PasswordHash != "" || !u.Active || u.Role != "user" || u.EmailVerified || u.Email != "carol@example.org" {
		t.Fatalf("new user = %+v", u)
	}
	if res.Setup.Purpose != PurposeSetup || len(res.InviteCodes) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if d := time.Until(res.Setup.ExpiresAt); d < 71*time.Hour || d > 73*time.Hour {
		t.Fatalf("setup link lifetime %v, want ~72h", d)
	}
	_, err = s.CreateUser(tctx(t), CreateUserParams{Username: "carol"})
	mustErr(t, err, ErrUsernameTaken, "duplicate")
}

func TestUsernamesAreCaseSensitive(t *testing.T) {
	s, _ := newStore(t)
	for _, n := range []string{"Alice", "alice"} {
		if _, err := s.CreateUser(tctx(t), CreateUserParams{Username: n}); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	if _, err := s.GetUser(tctx(t), "ALICE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ALICE: %v", err)
	}
}

func TestValidation(t *testing.T) {
	s, _ := newStore(t)
	for _, name := range []string{"a", strings.Repeat("x", 26), " bob", "bob ", "bo​b", "bo\x00b", "bo\nb", "\xff\xfeab"} {
		if _, err := s.CreateUser(tctx(t), CreateUserParams{Username: name}); !errors.Is(err, ErrInvalid) {
			t.Errorf("username %q accepted: %v", name, err)
		}
	}
	for _, e := range []string{"x", "no-at", "Bob <b@example.org>", "a@b.c\r\nBcc: x@y.z", strings.Repeat("a", 250) + "@b.org"} {
		if _, err := s.CreateUser(tctx(t), CreateUserParams{Username: "okname", Email: e}); !errors.Is(err, ErrInvalid) {
			t.Errorf("email %q accepted: %v", e, err)
		}
	}
	if err := ValidateUsername("日本語ユーザー"); err != nil {
		t.Errorf("non-ASCII username rejected: %v", err)
	}
	for _, u := range []string{"udp://tracker.example.org:1337/announce", "HTTPS://t.example.org/a", "wss://t.example.org"} {
		if err := ValidateTrackerURL(u); err != nil {
			t.Errorf("tracker %q rejected: %v", u, err)
		}
	}
	for _, u := range []string{"", "ftp://x.org/a", "http://", "http://u:p@x.org/a", "http://x.org/a#f", "http://x.org/a b", "x.org/announce", "http://x.org/" + strings.Repeat("a", 2048)} {
		if err := ValidateTrackerURL(u); !errors.Is(err, ErrInvalid) {
			t.Errorf("tracker %q accepted: %v", u, err)
		}
	}
	if err := ValidateLabel(""); err == nil {
		t.Error("empty label accepted")
	}
	if err := ValidateLabel("ok\x07"); err == nil {
		t.Error("control character in label accepted")
	}
}

// Disable suspends: it deletes sessions and changes nothing else; enable
// restores every credential, and revoked or expired ones stay dead.
func TestDisableSuspendsAndEnableRestores(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "dave")
	if _, err := s.CreateInvites(ctx, "dave", 2, nil); err != nil {
		t.Fatal(err)
	}
	api, _ := s.CreateAPIToken(ctx, u.ID, "cli", nil)
	rss, _ := s.CreateRSSToken(ctx, u.ID, "reader", nil)
	// A migrated legacy RSS token: un-prefixed 32-hex, legacy = true.
	const legacy = "0123456789abcdef0123456789abcdef"
	exec(t, pool, `INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy) VALUES ($1,$2,true)`, hashToken(legacy), u.ID)
	revoked, _ := s.CreateAPIToken(ctx, u.ID, "revoked", nil)
	exec(t, pool, `UPDATE magnetico_web.api_tokens SET revoked_at = now() WHERE id = $1`, revoked.ID)
	expired, _ := s.CreateRSSToken(ctx, u.ID, "expired", nil)
	exec(t, pool, `UPDATE magnetico_web.rss_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, expired.ID)
	if _, err := s.IssuePasswordLink(ctx, "dave"); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, email, expires_at) VALUES ($1,$2,'email_verify','dave@example.org', now() + interval '1 day')`, hashToken("ev"), u.ID)
	addSession(t, pool, u.ID)
	addSession(t, pool, u.ID)

	authOK := func(want bool, when string) {
		t.Helper()
		for name, f := range map[string]func() error{
			"api":    func() error { _, e := s.AuthenticateAPIToken(ctx, api.Plaintext); return e },
			"rss":    func() error { _, e := s.AuthenticateRSSToken(ctx, rss.Plaintext); return e },
			"legacy": func() error { _, e := s.AuthenticateRSSToken(ctx, legacy); return e },
		} {
			err := f()
			if want && err != nil || !want && !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("%s: %s token: err = %v, want ok=%v", when, name, err, want)
			}
		}
	}
	authOK(true, "before disable")
	before := tableDump(t, pool, u.ID)

	n, err := s.Disable(ctx, "dave")
	if err != nil || n != 2 {
		t.Fatalf("Disable = %d, %v", n, err)
	}
	if n, err := s.Disable(ctx, "dave"); err != nil || n != 0 {
		t.Fatalf("second Disable = %d, %v", n, err)
	}
	if got := tableDump(t, pool, u.ID); got != before {
		t.Fatalf("disable changed credential rows:\n--- before\n%s--- after\n%s", before, got)
	}
	authOK(false, "while disabled")

	if err := s.Enable(ctx, "dave"); err != nil {
		t.Fatal(err)
	}
	if got := tableDump(t, pool, u.ID); got != before {
		t.Fatal("enable changed credential rows")
	}
	authOK(true, "after enable")
	if sessions(t, pool, u.ID) != 0 {
		t.Fatal("sessions came back after enable")
	}
	if _, err := s.AuthenticateAPIToken(ctx, revoked.Plaintext); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("revoked token alive after enable: %v", err)
	}
	if _, err := s.AuthenticateRSSToken(ctx, expired.Plaintext); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("expired token alive after enable: %v", err)
	}
}

func TestRevokeTokensIsPermanentAndLeavesTheRestAlone(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "erin")
	api, _ := s.CreateAPIToken(ctx, u.ID, "", nil)
	rss, _ := s.CreateRSSToken(ctx, u.ID, "", nil)
	pre, _ := s.CreateRSSToken(ctx, u.ID, "", nil)
	exec(t, pool, `UPDATE magnetico_web.rss_tokens SET revoked_at = '2020-01-01' WHERE id = $1`, pre.ID)
	addSession(t, pool, u.ID)
	a, r, err := s.RevokeAllTokens(ctx, "erin")
	if err != nil || a != 1 || r != 1 {
		t.Fatalf("RevokeAllTokens = %d, %d, %v (already revoked tokens are not counted)", a, r, err)
	}
	if count(t, pool, `SELECT count(*) FROM magnetico_web.rss_tokens WHERE id = $1 AND revoked_at = '2020-01-01'`, pre.ID) != 1 {
		t.Fatal("an already revoked token lost its original revoked_at")
	}
	if sessions(t, pool, u.ID) != 1 {
		t.Fatal("revoke-tokens touched sessions")
	}
	if err := s.Enable(ctx, "erin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateAPIToken(ctx, api.Plaintext); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("revoked API token works")
	}
	if _, err := s.AuthenticateRSSToken(ctx, rss.Plaintext); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("revoked RSS token works")
	}
	if u2, _ := s.GetUser(tctx(t), "erin"); !u2.Active || u2.PasswordHash != u.PasswordHash {
		t.Fatal("revoke-tokens changed account state")
	}
}

func TestSetRoleAndRegistrationAlwaysCreatesUser(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	mkUser(t, s, pool, "owner")
	if err := s.SetRole(ctx, "owner", "admin"); err != nil {
		t.Fatal(err)
	}
	mustErr(t, s.SetRole(ctx, "owner", "root"), ErrInvalidRole, "bad role")
	mustErr(t, s.SetRole(ctx, "nobody", "user"), ErrNotFound, "unknown user")
	codes, _ := s.CreateInvites(ctx, "owner", 1, nil)
	id, err := s.RegisterWithInvite(ctx, RegisterParams{Code: codes[0], Username: "newbie", Hash: testHash(t), Quota: 3})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := s.GetUser(ctx, "newbie")
	if u.ID != id || u.Role != "user" || !u.Active {
		t.Fatalf("registered user = %+v", u)
	}
	if n := count(t, pool, `SELECT count(*) FROM magnetico_web.invites WHERE owner_id = $1`, id); n != 3 {
		t.Fatalf("quota invitations = %d", n)
	}
}
