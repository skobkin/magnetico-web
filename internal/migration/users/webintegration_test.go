package users

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/password"
)

// These tests run the real web account paths (login with rehash, token
// authentication, disable/enable, reset, invitation registration, recovery)
// against a database that `migrate users` populated from PHP-generated hashes.

func webStore(t *testing.T, tgt string) (*accounts.Store, *pgxpool.Pool) {
	t.Helper()
	pool, err := pgxpool.New(tctx(t), tgt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return accounts.New(pool), pool
}

func pwService() *password.Service {
	return password.NewService(password.NewLimiter(2, time.Minute), password.CurrentParams)
}

var lifetimes = accounts.SessionLifetimes{Idle: time.Hour, Absolute: 24 * time.Hour, Remember: 48 * time.Hour}

// fixturePassword returns a fixture case's hash and plaintext of one family.
func fixturePassword(t *testing.T, family string) (hash, pw string) {
	t.Helper()
	for _, c := range fixtureCases(t) {
		if f, ok := HashFamily(c.Hash); ok && f == family {
			b, err := base64.StdEncoding.DecodeString(c.PasswordB64)
			if err != nil {
				t.Fatal(err)
			}
			if len(b) >= 8 && len(b) < 72 { // a short ordinary password
				return c.Hash, string(b)
			}
		}
	}
	t.Fatalf("no short fixture for %s", family)
	return "", ""
}

// login is the web login path: look up, verify, rehash if needed, commit.
func login(t *testing.T, s *accounts.Store, svc *password.Service, username, pw string) (accounts.LoginResult, error) {
	t.Helper()
	ctx := tctx(t)
	cred, err := s.LoginCredential(ctx, username)
	if err != nil {
		return accounts.LoginResult{}, err
	}
	if !cred.Active || cred.Hash == nil {
		return accounts.LoginResult{}, accounts.ErrNotFound
	}
	ok, err := svc.Verify(ctx, pw, *cred.Hash)
	if err != nil {
		return accounts.LoginResult{}, err
	}
	if !ok {
		return accounts.LoginResult{}, accounts.ErrUnauthenticated
	}
	p := accounts.LoginParams{UserID: cred.UserID, VerifiedHash: *cred.Hash, Lifetimes: lifetimes}
	if svc.NeedsRehash(*cred.Hash) {
		if p.NewHash, err = svc.Rehash(ctx, pw); err != nil {
			t.Fatal(err)
		}
	}
	return s.CompleteLogin(ctx, p)
}

func TestLegacyPasswordsLogInAndAreRehashed(t *testing.T) {
	families := []string{famSodium, famArgon2i, famBcrypt10, famBcrypt13, famArgonT2, famArgonT3P2}
	src := legacyDB(t)
	d := dataset{}
	pws := map[string]string{}
	for i, fam := range families {
		h, pw := fixturePassword(t, fam)
		name := "user" + string(rune('a'+i))
		pws[name] = pw
		d.Users = append(d.Users, sUser{int64(i + 1), name, h, name + "@example.org", `["ROLE_USER"]`, "2021-01-15 10:00:00"})
	}
	d.Users = append(d.Users, sUser{20, "stuck", "{SSHA}opaque", "stuck@example.org", `["ROLE_USER"]`, "2021-01-15 10:00:00"})
	d.load(t, src)
	tgt := targetDB(t)
	cfg := cfgFor(src, tgt)
	cfg.AcceptNeedsReset = 1
	mustRun(t, cfg)

	s, _ := webStore(t, tgt)
	svc := pwService()
	for name, pw := range pws {
		old, _ := s.LoginCredential(tctx(t), name)
		if password.Classify(*old.Hash) != password.ClassLegacy {
			t.Fatalf("%s: imported hash class %v", name, password.Classify(*old.Hash))
		}
		if _, err := login(t, s, svc, name, "wrong password!"); err == nil {
			t.Errorf("%s: a wrong password logged in", name)
		}
		if _, err := login(t, s, svc, name, pw); err != nil {
			t.Fatalf("%s: legacy login failed: %v", name, err)
		}
		cur, _ := s.LoginCredential(tctx(t), name)
		if password.Classify(*cur.Hash) != password.ClassCurrent || *cur.Hash == *old.Hash {
			t.Errorf("%s: hash was not rehashed to the current argon2id parameters", name)
		}
		if _, err := login(t, s, svc, name, pw); err != nil {
			t.Errorf("%s: login after rehash failed: %v", name, err)
		}
	}

	// Unsupported hash: generic failure, no session, flagged needs-reset.
	cred, err := s.LoginCredential(tctx(t), "stuck")
	if err != nil {
		t.Fatal(err)
	}
	if password.Classify(*cred.Hash) != password.ClassUnsupported {
		t.Error("the unsupported hash is not classified needs-reset")
	}
	if _, err := login(t, s, svc, "stuck", "anything at all"); !errors.Is(err, password.ErrUnsupportedHash) {
		t.Errorf("unsupported hash login: %v", err)
	}
	if n := scalar[int](t, tgt, `SELECT count(*) FROM magnetico_web.sessions s JOIN magnetico_web.users u ON u.id = s.user_id WHERE u.username = 'stuck'`); n != 0 {
		t.Error("a session exists for the needs-reset account")
	}
	// The administrator-issued link is the recovery path and works.
	link, err := s.IssuePasswordLink(tctx(t), "stuck")
	if err != nil || link.Token == "" {
		t.Fatalf("admin reset link: %v", err)
	}
}

func TestUsernamesStayCaseSensitiveAfterMigration(t *testing.T) {
	src, d := seeded(t)
	tgt := targetDB(t)
	mustRun(t, cfgFor(src, tgt))
	s, _ := webStore(t, tgt)
	a, err1 := s.LoginCredential(tctx(t), "Bob")
	b, err2 := s.LoginCredential(tctx(t), "bob")
	if err1 != nil || err2 != nil || a.UserID == b.UserID {
		t.Fatalf("Bob/bob: %v %v", err1, err2)
	}
	for _, name := range []string{"BOB", "Alice", " alice"} {
		if _, err := s.LoginCredential(tctx(t), name); !errors.Is(err, accounts.ErrNotFound) {
			t.Errorf("%q: %v", name, err)
		}
	}
	_ = d
}

func TestLegacyTokensAreRSSOnly(t *testing.T) {
	src, d := seeded(t)
	tgt := targetDB(t)
	mustRun(t, cfgFor(src, tgt))
	s, _ := webStore(t, tgt)
	owner := map[string]int64{}
	for _, k := range d.Tokens {
		owner[k.Key] = k.User
	}
	for _, k := range d.Tokens {
		id, err := s.AuthenticateRSSToken(tctx(t), k.Key)
		if err != nil || id.UserID != k.User {
			t.Fatalf("legacy token of user %d on the RSS path: %v", k.User, err)
		}
		if _, err := s.AuthenticateAPIToken(tctx(t), k.Key); !errors.Is(err, accounts.ErrUnauthenticated) {
			t.Errorf("a legacy token authenticated against the API: %v", err)
		}
	}
	if n := scalar[int](t, tgt, `SELECT count(*) FROM magnetico_web.api_tokens`); n != 0 {
		t.Errorf("%d api_tokens exist", n)
	}
	// The account page lists them masked (legacy flag, no value).
	list, err := s.ListRSSTokens(tctx(t), 1)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	for _, v := range list {
		if !v.Legacy || v.Label != "" {
			t.Errorf("listed token %+v", v)
		}
	}
	// Neither a prefix nor a case variant of a legacy key authenticates.
	if _, err := s.AuthenticateRSSToken(tctx(t), "A0000000000000000000000000000001"); err == nil {
		t.Error("a case variant of a legacy key authenticated")
	}
}

func TestDisableEnableKeepsLegacyTokenAndInvitation(t *testing.T) {
	src, d := seeded(t)
	tgt := targetDB(t)
	mustRun(t, cfgFor(src, tgt))
	s, _ := webStore(t, tgt)
	key := d.Tokens[0].Key // owner alice (id 1)
	row := func() string {
		return scalar[string](t, tgt, `SELECT t::text FROM magnetico_web.rss_tokens t WHERE user_id = 1 ORDER BY id LIMIT 1`)
	}
	before := row()

	if _, err := s.Disable(tctx(t), "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateRSSToken(tctx(t), key); !errors.Is(err, accounts.ErrUnauthenticated) {
		t.Errorf("a disabled owner's legacy token works: %v", err)
	}
	// A migrated unused invitation of the disabled owner is suspended.
	code := d.Invites[1].Code
	reg := accounts.RegisterParams{Code: code, Username: "newcomer", Hash: mustHash(t), Quota: 0}
	if _, err := s.RegisterWithInvite(tctx(t), reg); !errors.Is(err, accounts.ErrInvitationUnavailable) {
		t.Errorf("registration through a suspended migrated invitation: %v", err)
	}
	if err := s.Enable(tctx(t), "alice"); err != nil {
		t.Fatal(err)
	}
	if id, err := s.AuthenticateRSSToken(tctx(t), key); err != nil || id.UserID != 1 {
		t.Errorf("legacy token after enable: %v", err)
	}
	if row() != before {
		t.Error("disable/enable changed the stored token row")
	}
	// Registration through the migrated invitation now succeeds as a plain user.
	id, err := s.RegisterWithInvite(tctx(t), reg)
	if err != nil {
		t.Fatalf("registration through a migrated invitation: %v", err)
	}
	u, err := s.GetUserByID(tctx(t), id)
	if err != nil || u.Role != "user" {
		t.Fatalf("registered user: %+v %v", u, err)
	}
	// A used migrated invitation is gone for good.
	if _, err := s.RegisterWithInvite(tctx(t), accounts.RegisterParams{Code: d.Invites[0].Code, Username: "another", Hash: mustHash(t)}); !errors.Is(err, accounts.ErrInvitationUnavailable) {
		t.Errorf("a used migrated invitation: %v", err)
	}
	// The used relationship survived the migration.
	if n := scalar[int](t, tgt, `SELECT count(*) FROM magnetico_web.invites WHERE id = 3 AND used_by = 2`); n != 1 {
		t.Error("the used-by relationship of invitation 3 is gone")
	}
}

func mustHash(t *testing.T) string {
	t.Helper()
	h, err := password.NewService(password.NewLimiter(1, time.Minute), password.Params{Memory: 64, Time: 1, Threads: 1}).Hash(context.Background(), "a registration password")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestAdminResetRevokesLegacyTokens(t *testing.T) {
	src, d := seeded(t)
	tgt := targetDB(t)
	mustRun(t, cfgFor(src, tgt))
	s, _ := webStore(t, tgt)
	rev, err := s.SetPassword(tctx(t), "alice", mustHash(t))
	if err != nil {
		t.Fatal(err)
	}
	if rev.RSSTokens != 2 || rev.APITokens != 0 {
		t.Errorf("revocations %+v", rev)
	}
	for _, k := range d.Tokens[:2] {
		if _, err := s.AuthenticateRSSToken(tctx(t), k.Key); !errors.Is(err, accounts.ErrUnauthenticated) {
			t.Errorf("a revoked legacy token works: %v", err)
		}
	}
	// Other users' legacy tokens are unaffected.
	if _, err := s.AuthenticateRSSToken(tctx(t), d.Tokens[2].Key); err != nil {
		t.Errorf("another user's token: %v", err)
	}
}

func TestMigratedEmailsAreUnverifiedAndNotUsedForRecovery(t *testing.T) {
	src, d := seeded(t)
	tgt := targetDB(t)
	mustRun(t, cfgFor(src, tgt))
	s, _ := webStore(t, tgt)
	for _, u := range d.Users {
		got, err := s.GetUserByID(tctx(t), u.ID)
		if err != nil || got.Email != u.Email || got.EmailVerified {
			t.Fatalf("user %d: %+v %v", u.ID, got, err)
		}
		if _, err := s.IssueRecoveryLink(tctx(t), u.Email); !errors.Is(err, accounts.ErrNotFound) {
			t.Errorf("a migrated unverified address %d was usable for recovery: %v", u.ID, err)
		}
	}
	// The existing account flow verifies it: request a link, confirm it. The
	// case-variant duplicate (users 2 and 3) cannot both end up verified.
	l2, err := s.IssueEmailVerifyLink(tctx(t), 2, d.Users[1].Email)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := s.CompleteEmailVerify(tctx(t), l2.Token); err != nil {
		t.Fatalf("verify: %v %+v", err, r)
	}
	l3, err := s.IssueEmailVerifyLink(tctx(t), 3, d.Users[2].Email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteEmailVerify(tctx(t), l3.Token); err == nil {
		t.Error("two accounts verified the same address (case variant)")
	}
	if _, err := s.IssueRecoveryLink(tctx(t), d.Users[1].Email); err != nil {
		t.Errorf("recovery for the verified address: %v", err)
	}
}
