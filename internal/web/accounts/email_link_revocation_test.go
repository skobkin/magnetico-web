package accounts

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Every operation that writes a password hash for an existing account must
// delete the account's outstanding email_verify links (web-v1 §4.9.11) while
// leaving the stored address and its verification status alone.

type emailState string

const (
	emailNone       emailState = "no address"
	emailUnverified emailState = "unverified address"
	emailVerified   emailState = "verified address"
)

// pwPath is one way of setting a password.
type pwPath struct {
	name string
	// op is the transaction name the store passes to beforeCommit.
	op string
	// passwordless makes the fixture an account that has no password yet.
	passwordless bool
	// run performs the operation and returns what it revoked.
	run func(t *testing.T, s *Store, pool *pgxpool.Pool, f *pwFixture) (Revocations, error)
}

var pwPaths = []pwPath{
	{name: "self-service change", op: "change-password",
		run: func(t *testing.T, s *Store, _ *pgxpool.Pool, f *pwFixture) (Revocations, error) {
			res, err := s.ChangePassword(tctx(t), ChangePasswordParams{
				UserID: f.user.ID, VerifiedHash: f.user.PasswordHash, NewHash: altHash(t), CurrentSession: f.session,
			})
			return res.Revocations, err
		}},
	{name: "reset link", op: "complete-link",
		run: func(t *testing.T, s *Store, pool *pgxpool.Pool, f *pwFixture) (Revocations, error) {
			l, err := s.IssuePasswordLink(tctx(t), f.user.Username)
			if err != nil || l.Purpose != PurposeReset {
				t.Fatalf("link = %+v, %v", l, err)
			}
			res, err := s.CompletePasswordLink(tctx(t), l.Token, PurposeReset, altHash(t))
			return res.Revocations, err
		}},
	{name: "setup link", op: "complete-link", passwordless: true,
		run: func(t *testing.T, s *Store, pool *pgxpool.Pool, f *pwFixture) (Revocations, error) {
			l, err := s.IssuePasswordLink(tctx(t), f.user.Username)
			if err != nil || l.Purpose != PurposeSetup {
				t.Fatalf("link = %+v, %v", l, err)
			}
			res, err := s.CompletePasswordLink(tctx(t), l.Token, PurposeSetup, altHash(t))
			return res.Revocations, err
		}},
	{name: "administrator set-password", op: "set-password",
		run: func(t *testing.T, s *Store, _ *pgxpool.Pool, f *pwFixture) (Revocations, error) {
			return s.SetPassword(tctx(t), f.user.Username, altHash(t))
		}},
}

// pwFixture is an account with an outstanding email_verify link.
type pwFixture struct {
	user    User
	session []byte
	// pendingAddr is the address the outstanding link would confirm; pendingTok
	// is the link.
	pendingAddr, pendingTok string
	// email and verifiedAt are the stored address state before the operation.
	email      string
	verifiedAt *time.Time
}

type storedEmail struct {
	email      string
	verifiedAt *time.Time
}

func readEmail(t *testing.T, pool *pgxpool.Pool, id int64) storedEmail {
	t.Helper()
	var e *string
	var v *time.Time
	if err := pool.QueryRow(tctx(t), `SELECT email, email_verified_at FROM magnetico_web.users WHERE id = $1`, id).Scan(&e, &v); err != nil {
		t.Fatal(err)
	}
	out := storedEmail{verifiedAt: v}
	if e != nil {
		out.email = *e
	}
	return out
}

func sameEmail(a, b storedEmail) bool {
	if a.email != b.email || (a.verifiedAt == nil) != (b.verifiedAt == nil) {
		return false
	}
	return a.verifiedAt == nil || a.verifiedAt.Equal(*b.verifiedAt)
}

func newPwFixture(t *testing.T, s *Store, pool *pgxpool.Pool, name string, p pwPath, state emailState) *pwFixture {
	t.Helper()
	var u User
	if p.passwordless {
		res, err := s.CreateUser(tctx(t), CreateUserParams{Username: name})
		if err != nil {
			t.Fatal(err)
		}
		if u, err = s.GetUserByID(tctx(t), res.ID); err != nil {
			t.Fatal(err)
		}
	} else {
		u = mkUser(t, s, pool, name)
	}
	f := &pwFixture{user: u}
	switch state {
	case emailNone:
		f.pendingAddr = name + "-pending@example.org"
	case emailUnverified:
		unverifiedEmail(t, pool, u.ID, name+"@example.org")
		f.pendingAddr = name + "@example.org"
	case emailVerified:
		verifiedEmail(t, s, u.ID, name+"@example.org")
		f.pendingAddr = name + "-new@example.org"
	}
	l, err := s.IssueEmailVerifyLink(tctx(t), u.ID, f.pendingAddr)
	if err != nil {
		t.Fatal(err)
	}
	f.pendingTok = l.Token
	if !p.passwordless {
		f.session = mkSession(t, pool, u.ID, false, time.Hour)
	}
	st := readEmail(t, pool, u.ID)
	f.email, f.verifiedAt = st.email, st.verifiedAt
	return f
}

func emailLinks(t *testing.T, pool *pgxpool.Pool, id int64) int {
	t.Helper()
	return count(t, pool, `SELECT count(*) FROM magnetico_web.one_time_tokens WHERE user_id = $1 AND purpose = 'email_verify'`, id)
}

func TestPasswordSettingInvalidatesPendingEmailLinks(t *testing.T) {
	for _, p := range pwPaths {
		for _, state := range []emailState{emailNone, emailUnverified, emailVerified} {
			t.Run(p.name+"/"+string(state), func(t *testing.T) {
				s, pool := newStore(t)
				f := newPwFixture(t, s, pool, "pat", p, state)
				before := readEmail(t, pool, f.user.ID)

				r, err := p.run(t, s, pool, f)
				if err != nil {
					t.Fatal(err)
				}
				if r.EmailLinks != 1 {
					t.Fatalf("revocations.EmailLinks = %d, want 1", r.EmailLinks)
				}
				if n := emailLinks(t, pool, f.user.ID); n != 0 {
					t.Fatalf("%d email_verify links survived the password change", n)
				}
				if _, ok, err := s.PendingEmail(tctx(t), f.user.ID); err != nil || ok {
					t.Fatalf("PendingEmail = %v, %v after the password change", ok, err)
				}
				// The stored address and its verification status are preserved.
				if after := readEmail(t, pool, f.user.ID); !sameEmail(before, after) {
					t.Fatalf("stored address changed: %+v -> %+v", before, after)
				}
				// The link issued before the change no longer works.
				mustErr(t, errOf(s.CompleteEmailVerify(tctx(t), f.pendingTok)), ErrLinkInvalid, "link issued before the change")
				if after := readEmail(t, pool, f.user.ID); !sameEmail(before, after) {
					t.Fatalf("a revoked link changed the address: %+v -> %+v", before, after)
				}

				// A fresh link issued and confirmed afterwards works.
				fresh := "fresh-" + f.pendingAddr
				l, err := s.IssueEmailVerifyLink(tctx(t), f.user.ID, fresh)
				if err != nil {
					t.Fatal(err)
				}
				if e, ok, _ := s.PendingEmail(tctx(t), f.user.ID); !ok || e != fresh {
					t.Fatalf("PendingEmail = %q %v, want the fresh address", e, ok)
				}
				res, err := s.CompleteEmailVerify(tctx(t), l.Token)
				if err != nil {
					t.Fatal(err)
				}
				if res.Email != fresh {
					t.Fatalf("confirmed %q, want %q", res.Email, fresh)
				}
				if got := readEmail(t, pool, f.user.ID); got.email != fresh || got.verifiedAt == nil {
					t.Fatalf("after the fresh confirmation: %+v", got)
				}
			})
		}
	}
}

func TestPasswordSettingWithoutPendingLinkReportsNone(t *testing.T) {
	for _, p := range pwPaths {
		t.Run(p.name, func(t *testing.T) {
			s, pool := newStore(t)
			f := newPwFixture(t, s, pool, "nia", p, emailVerified)
			exec(t, pool, `DELETE FROM magnetico_web.one_time_tokens WHERE user_id = $1`, f.user.ID)
			r, err := p.run(t, s, pool, f)
			if err != nil {
				t.Fatal(err)
			}
			if r.EmailLinks != 0 {
				t.Fatalf("EmailLinks = %d, want 0", r.EmailLinks)
			}
			if st := readEmail(t, pool, f.user.ID); st.email != "nia@example.org" || st.verifiedAt == nil {
				t.Fatalf("verified address changed: %+v", st)
			}
		})
	}
}

func TestPasswordSettingRollbackKeepsPendingEmailLinks(t *testing.T) {
	for _, p := range pwPaths {
		for _, state := range []emailState{emailUnverified, emailVerified} {
			t.Run(p.name+"/"+string(state), func(t *testing.T) {
				s, pool := newStore(t)
				f := newPwFixture(t, s, pool, "rob", p, state)
				before := readEmail(t, pool, f.user.ID)
				hashBefore := count(t, pool, `SELECT count(*) FROM magnetico_web.users WHERE id = $1 AND password_hash IS NOT DISTINCT FROM $2`, f.user.ID, nullable(f.user.PasswordHash))

				boom := errors.New("injected failure before commit")
				s.beforeCommit = func(op string) error {
					if op == p.op {
						return boom
					}
					return nil
				}
				if _, err := p.run(t, s, pool, f); !errors.Is(err, boom) {
					t.Fatalf("err = %v, want the injected failure", err)
				}
				s.beforeCommit = nil

				if n := emailLinks(t, pool, f.user.ID); n != 1 {
					t.Fatalf("%d email_verify links after the rollback, want the original 1", n)
				}
				if got := count(t, pool, `SELECT count(*) FROM magnetico_web.users WHERE id = $1 AND password_hash IS NOT DISTINCT FROM $2`, f.user.ID, nullable(f.user.PasswordHash)); got != hashBefore {
					t.Fatal("the password hash changed although the transaction rolled back")
				}
				if e, ok, _ := s.PendingEmail(tctx(t), f.user.ID); !ok || e != f.pendingAddr {
					t.Fatalf("PendingEmail = %q %v", e, ok)
				}
				if after := readEmail(t, pool, f.user.ID); !sameEmail(before, after) {
					t.Fatalf("stored address changed: %+v -> %+v", before, after)
				}
				// The link is still the one that was issued, and still works.
				res, err := s.CompleteEmailVerify(tctx(t), f.pendingTok)
				if err != nil {
					t.Fatalf("link after the rollback: %v", err)
				}
				if res.Email != f.pendingAddr {
					t.Fatalf("confirmed %q, want %q", res.Email, f.pendingAddr)
				}
			})
		}
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// A failure of the password write itself (the self-service change loses its
// optimistic check) also leaves the links alone.
func TestFailedPasswordChangeKeepsPendingEmailLink(t *testing.T) {
	s, pool := newStore(t)
	f := newPwFixture(t, s, pool, "fay", pwPaths[0], emailVerified)
	_, err := s.ChangePassword(tctx(t), ChangePasswordParams{
		UserID: f.user.ID, VerifiedHash: f.user.PasswordHash + "x", NewHash: altHash(t), CurrentSession: f.session,
	})
	mustErr(t, err, ErrLoginStale, "stale change")
	if n := emailLinks(t, pool, f.user.ID); n != 1 {
		t.Fatalf("%d links, want 1", n)
	}
	if _, err := s.CompleteEmailVerify(tctx(t), f.pendingTok); err != nil {
		t.Fatalf("link after a refused change: %v", err)
	}
}

// Invalidation must not reach other accounts.
func TestPasswordSettingLeavesOtherAccountsEmailLinks(t *testing.T) {
	for _, p := range pwPaths {
		t.Run(p.name, func(t *testing.T) {
			s, pool := newStore(t)
			other := mkUser(t, s, pool, "otto")
			l, err := s.IssueEmailVerifyLink(tctx(t), other.ID, "otto@example.org")
			if err != nil {
				t.Fatal(err)
			}
			f := newPwFixture(t, s, pool, "pia", p, emailVerified)
			if _, err := p.run(t, s, pool, f); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CompleteEmailVerify(tctx(t), l.Token); err != nil {
				t.Fatalf("another account's link: %v", err)
			}
		})
	}
}

// --- races ---

// A password change that commits first leaves a request that was authorized
// by the old credentials unable to create a new pending link; one that
// commits first is undone by the change.

func TestRaceSessionBoundIssuanceVersusPasswordChange(t *testing.T) {
	const next = "race-new@example.org"
	for _, p := range pwPaths {
		t.Run(p.name, func(t *testing.T) {
			t.Run("password change commits first", func(t *testing.T) {
				s, pool := newStore(t)
				f := newPwFixture(t, s, pool, "ray", p, emailVerified)
				// The request being raced was authorized by a session that the
				// change revokes (the change's own rotated session is a new one).
				old := mkSession(t, pool, f.user.ID, false, time.Hour)
				if p.name == "self-service change" {
					f.session = mkSession(t, pool, f.user.ID, false, time.Hour)
				}
				fe, se := race(t, pool, p.op,
					func(s *Store) error { _, err := p.run(t, s, pool, f); return err },
					func(s *Store) error {
						_, err := s.IssueEmailVerifyLinkForSession(tctx(t), f.user.ID, next, old)
						return err
					})
				if fe != nil {
					t.Fatal(fe)
				}
				if !errors.Is(se, ErrUnauthenticated) {
					t.Fatalf("issuance authorized before the change: %v, want ErrUnauthenticated", se)
				}
				if n := emailLinks(t, pool, f.user.ID); n != 0 {
					t.Fatalf("%d pending links exist after the password change", n)
				}
			})
			t.Run("issuance commits first", func(t *testing.T) {
				s, pool := newStore(t)
				f := newPwFixture(t, s, pool, "ray", p, emailVerified)
				sess := mkSession(t, pool, f.user.ID, false, time.Hour)
				if p.name == "self-service change" {
					f.session = sess
				}
				fe, se := race(t, pool, "issue-email-link",
					func(s *Store) error {
						_, err := s.IssueEmailVerifyLinkForSession(tctx(t), f.user.ID, next, sess)
						return err
					},
					func(s *Store) error { _, err := p.run(t, s, pool, f); return err })
				if fe != nil || se != nil {
					t.Fatalf("errors: %v / %v", fe, se)
				}
				if n := emailLinks(t, pool, f.user.ID); n != 0 {
					t.Fatalf("%d pending links survived the password change", n)
				}
			})
		})
	}
}

func TestRaceEmailCompletionVersusPasswordChange(t *testing.T) {
	for _, p := range pwPaths {
		t.Run(p.name, func(t *testing.T) {
			t.Run("password change commits first", func(t *testing.T) {
				s, pool := newStore(t)
				f := newPwFixture(t, s, pool, "cal", p, emailVerified)
				before := readEmail(t, pool, f.user.ID)
				fe, se := race(t, pool, p.op,
					func(s *Store) error { _, err := p.run(t, s, pool, f); return err },
					func(s *Store) error { _, err := s.CompleteEmailVerify(tctx(t), f.pendingTok); return err })
				if fe != nil {
					t.Fatal(fe)
				}
				if !errors.Is(se, ErrLinkInvalid) {
					t.Fatalf("completion racing the change: %v, want ErrLinkInvalid", se)
				}
				if after := readEmail(t, pool, f.user.ID); !sameEmail(before, after) {
					t.Fatalf("address changed by a revoked link: %+v -> %+v", before, after)
				}
			})
			t.Run("completion commits first", func(t *testing.T) {
				s, pool := newStore(t)
				f := newPwFixture(t, s, pool, "cal", p, emailVerified)
				fe, se := race(t, pool, "complete-email-verify",
					func(s *Store) error { _, err := s.CompleteEmailVerify(tctx(t), f.pendingTok); return err },
					func(s *Store) error { _, err := p.run(t, s, pool, f); return err })
				if fe != nil || se != nil {
					t.Fatalf("errors: %v / %v", fe, se)
				}
				// The link was honoured before the change; afterwards none is left.
				if got := readEmail(t, pool, f.user.ID); got.email != f.pendingAddr || got.verifiedAt == nil {
					t.Fatalf("address after completion then change: %+v", got)
				}
				if n := emailLinks(t, pool, f.user.ID); n != 0 {
					t.Fatalf("%d links left", n)
				}
			})
		})
	}
}

// --- session-bound issuance ---

func TestIssueEmailVerifyLinkForSession(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "sia")
	sess := mkSession(t, pool, u.ID, false, time.Hour)

	l, err := s.IssueEmailVerifyLinkForSession(ctx, u.ID, "sia@example.org", sess)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok, _ := s.PendingEmail(ctx, u.ID); !ok || e != "sia@example.org" {
		t.Fatalf("pending = %q %v", e, ok)
	}
	if _, err := s.CompleteEmailVerify(ctx, l.Token); err != nil {
		t.Fatal(err)
	}

	_, err = s.IssueEmailVerifyLinkForSession(ctx, u.ID, "x@example.org", []byte("no such session"))
	mustErr(t, err, ErrUnauthenticated, "unknown session")
	_, err = s.IssueEmailVerifyLinkForSession(ctx, u.ID, "x@example.org", nil)
	mustErr(t, err, ErrUnauthenticated, "no session")
	other := mkUser(t, s, pool, "sib")
	_, err = s.IssueEmailVerifyLinkForSession(ctx, other.ID, "x@example.org", sess)
	mustErr(t, err, ErrUnauthenticated, "another account's session")
	expired := mkSession(t, pool, u.ID, false, time.Hour)
	exec(t, pool, `UPDATE magnetico_web.sessions SET created_at = created_at - interval '2 hours', expires_at = expires_at - interval '2 hours' WHERE token_hash = $1`, expired)
	_, err = s.IssueEmailVerifyLinkForSession(ctx, u.ID, "x@example.org", expired)
	mustErr(t, err, ErrUnauthenticated, "expired session")
	if _, ok, _ := s.PendingEmail(ctx, u.ID); ok {
		t.Fatal("a refused issuance left a link")
	}
	if _, err := s.IssueEmailVerifyLinkForSession(ctx, u.ID, "not an address", sess); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid address: %v", err)
	}
	exec(t, pool, `UPDATE magnetico_web.users SET active = false WHERE id = $1`, u.ID)
	_, err = s.IssueEmailVerifyLinkForSession(ctx, u.ID, "x@example.org", sess)
	mustErr(t, err, ErrNotFound, "inactive account")
}

// After a self-service change the rotated session is a new, valid credential:
// a fresh link may be requested with it.
func TestFreshIssuanceWithRotatedSessionAfterPasswordChange(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	f := newPwFixture(t, s, pool, "rho", pwPaths[0], emailVerified)

	// The rotated session's hash is the one that was inserted by ChangePassword.
	res, err := s.ChangePassword(ctx, ChangePasswordParams{
		UserID: f.user.ID, VerifiedHash: f.user.PasswordHash, NewHash: altHash(t), CurrentSession: f.session,
	})
	if err != nil {
		t.Fatal(err)
	}
	rotated, ok := SessionTokenHash(res.Token)
	if !ok {
		t.Fatal("rotated token is malformed")
	}
	_, err = s.IssueEmailVerifyLinkForSession(ctx, f.user.ID, "rho-new@example.org", f.session)
	mustErr(t, err, ErrUnauthenticated, "the replaced session")
	l, err := s.IssueEmailVerifyLinkForSession(ctx, f.user.ID, "rho-new@example.org", rotated)
	if err != nil {
		t.Fatalf("fresh link with the rotated session: %v", err)
	}
	if _, err := s.CompleteEmailVerify(ctx, l.Token); err != nil {
		t.Fatal(err)
	}
	if got := readEmail(t, pool, f.user.ID); got.email != "rho-new@example.org" || got.verifiedAt == nil {
		t.Fatalf("address = %+v", got)
	}
}
