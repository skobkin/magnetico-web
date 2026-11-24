package accounts

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/web/password"
)

// hashOfPassword returns a fast, current-class argon2id hash of pw.
func hashOfPassword(t *testing.T, pw string) string {
	t.Helper()
	svc := password.NewService(password.NewLimiter(1, time.Minute), password.Params{Memory: 64, Time: 1, Threads: 1})
	h, err := svc.Hash(context.Background(), pw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func userRow(t *testing.T, pool *pgxpool.Pool, id int64) (hash string, changed, lastLogin *time.Time, active bool) {
	t.Helper()
	err := pool.QueryRow(tctx(t), `SELECT password_hash, password_changed_at, last_login_at, active FROM magnetico_web.users WHERE id = $1`, id).
		Scan(&hash, &changed, &lastLogin, &active)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestLoginCredential(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "Gus")
	if _, err := s.CreateUser(ctx, CreateUserParams{Username: "nopass"}); err != nil {
		t.Fatal(err)
	}
	mkUser(t, s, pool, "off")
	if _, err := s.Disable(ctx, "off"); err != nil {
		t.Fatal(err)
	}

	c, err := s.LoginCredential(ctx, "Gus")
	if err != nil || c.UserID != u.ID || c.Hash == nil || *c.Hash != testHash(t) || !c.Active {
		t.Fatalf("known: %+v %v", c, err)
	}
	if c, err := s.LoginCredential(ctx, "nopass"); err != nil || c.Hash != nil || !c.Active {
		t.Fatalf("passwordless: %+v %v", c, err)
	}
	if c, err := s.LoginCredential(ctx, "off"); err != nil || c.Active {
		t.Fatalf("disabled: %+v %v", c, err)
	}
	// Case-sensitive, and impossible names are "unknown", never a database error.
	for _, name := range []string{"gus", "GUS", "ghost", "", "bad\xffutf8", "nul\x00byte", strings.Repeat("x", 101), strings.Repeat("é", 60)} {
		if _, err := s.LoginCredential(ctx, name); err != ErrNotFound {
			t.Errorf("username %q: err = %v, want ErrNotFound", name, err)
		}
	}
}

func TestCompleteLoginWritesLoginAndSessionTogether(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "hal")
	_, changed0, last0, _ := userRow(t, pool, u.ID)
	if last0 != nil {
		t.Fatal("last_login_at set before any login")
	}

	res := login(t, s, u, false)
	hash, changed, last, _ := userRow(t, pool, u.ID)
	if !closeTo(last, res.Now) {
		t.Fatalf("last_login_at %v, want it at the login transaction time (session insert %v)", last, res.Now)
	}
	if hash != testHash(t) || !equalTime(changed, changed0) {
		t.Fatal("a plain login changed the hash or password_changed_at")
	}
	if res.Username != "hal" || res.Role != "user" || sessions(t, pool, u.ID) != 1 {
		t.Fatalf("result/session: %+v", res)
	}

	// A failing commit leaves neither the session nor last_login_at.
	u2 := mkUser(t, s, pool, "ida")
	s.beforeCommit = func(op string) error {
		if op == "login" {
			return fmt.Errorf("injected failure")
		}
		return nil
	}
	if _, err := s.CompleteLogin(ctx, LoginParams{UserID: u2.ID, VerifiedHash: testHash(t), NewHash: hashOfPassword(t, "other password"), Lifetimes: lt}); err == nil {
		t.Fatal("aborted commit reported success")
	}
	s.beforeCommit = nil
	hash2, _, last2, _ := userRow(t, pool, u2.ID)
	if last2 != nil || sessions(t, pool, u2.ID) != 0 || hash2 != testHash(t) {
		t.Fatal("a rolled-back login left state behind")
	}
}

// closeTo: last_login_at and the session insert are separate statements of one
// transaction, so their statement times differ by microseconds.
func closeTo(last *time.Time, now time.Time) bool {
	return last != nil && !last.After(now) && now.Sub(*last) < 5*time.Second
}

func equalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func TestCompleteLoginRehashKeepsPasswordChangedAt(t *testing.T) {
	s, pool := newStore(t)
	u := mkUser(t, s, pool, "jon")
	_, changed0, _, _ := userRow(t, pool, u.ID)
	nh := hashOfPassword(t, "test password 1")

	res, err := s.CompleteLogin(tctx(t), LoginParams{UserID: u.ID, VerifiedHash: testHash(t), NewHash: nh, Lifetimes: lt})
	if err != nil {
		t.Fatal(err)
	}
	hash, changed, last, _ := userRow(t, pool, u.ID)
	if hash != nh || hash == testHash(t) {
		t.Fatal("hash not replaced by the rehash")
	}
	if !equalTime(changed, changed0) {
		t.Fatal("a rehash must not touch password_changed_at")
	}
	if !closeTo(last, res.Now) {
		t.Fatal("last_login_at not recorded with the rehash")
	}
	// An unsupported replacement hash is refused before anything is written.
	u2 := mkUser(t, s, pool, "kay")
	if _, err := s.CompleteLogin(tctx(t), LoginParams{UserID: u2.ID, VerifiedHash: testHash(t), NewHash: "$1$md5$notsupported", Lifetimes: lt}); err == nil {
		t.Fatal("unsupported NewHash accepted")
	}
}

func TestCompleteLoginStaleCases(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "lou")
	old := login(t, s, u, false)

	// Wrong verified hash: nothing is written, the presented session survives.
	before := count(t, pool, `SELECT count(*) FROM magnetico_web.sessions`)
	_, err := s.CompleteLogin(ctx, LoginParams{UserID: u.ID, VerifiedHash: hashOfPassword(t, "wrong one 1"), NewHash: hashOfPassword(t, "other one 2"),
		Lifetimes: lt, ReplaceSession: hashOf(t, old.Token)})
	mustErr(t, err, ErrLoginStale, "wrong verified hash")
	hash, _, _, _ := userRow(t, pool, u.ID)
	if hash != testHash(t) || count(t, pool, `SELECT count(*) FROM magnetico_web.sessions`) != before {
		t.Fatal("a stale login wrote something")
	}
	if _, err := s.LookupSession(ctx, hashOf(t, old.Token), lt.Idle); err != nil {
		t.Fatal("a stale login deleted the presented session")
	}

	// Inactive account.
	exec(t, pool, `UPDATE magnetico_web.users SET active = false WHERE id = $1`, u.ID)
	_, err = s.CompleteLogin(ctx, LoginParams{UserID: u.ID, VerifiedHash: testHash(t), Lifetimes: lt})
	mustErr(t, err, ErrLoginStale, "inactive")

	// Unknown user id and bad parameters.
	_, err = s.CompleteLogin(ctx, LoginParams{UserID: 999999, VerifiedHash: testHash(t), Lifetimes: lt})
	mustErr(t, err, ErrLoginStale, "unknown id")
	if _, err := s.CompleteLogin(ctx, LoginParams{UserID: u.ID, VerifiedHash: testHash(t)}); err == nil {
		t.Fatal("zero lifetimes accepted")
	}
}

func TestCompleteLoginReplacesPresentedSession(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "max")
	old := login(t, s, u, false)

	res, err := s.CompleteLogin(ctx, LoginParams{UserID: u.ID, VerifiedHash: testHash(t), Lifetimes: lt, ReplaceSession: hashOf(t, old.Token)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Token == old.Token {
		t.Fatal("the new session reuses the presented token")
	}
	if _, err := s.LookupSession(ctx, hashOf(t, old.Token), lt.Idle); err != ErrUnauthenticated {
		t.Fatal("presented session not deleted")
	}
	if sessions(t, pool, u.ID) != 1 {
		t.Fatalf("%d sessions, want 1", sessions(t, pool, u.ID))
	}
	// A presented hash that matches nothing (attacker-chosen cookie) is harmless.
	junk, _, _ := NewSessionToken()
	if _, err := s.CompleteLogin(ctx, LoginParams{UserID: u.ID, VerifiedHash: testHash(t), Lifetimes: lt, ReplaceSession: hashOf(t, junk)}); err != nil {
		t.Fatalf("unknown presented session: %v", err)
	}
}

// raceLogin drives CompleteLogin against a concurrent writer deterministically.
// writerFirst: the writer is held at its commit (holding the user lock) while
// the login blocks on it. Otherwise the login is held at its commit while the
// writer blocks.
//
// It cannot hang: the release channel is closed on every exit path (deferred,
// before the goroutines are joined), the hook itself gives up after blockWait,
// the wait for the first operation to reach its commit point and the wait for
// the second to block are both bounded and fail promptly if the expected state
// never occurs, and the goroutines are always joined before returning, so the
// connections they hold are back in the pool before the test's cleanup closes it.
func raceLogin(t *testing.T, s *Store, pool *pgxpool.Pool, p LoginParams, writerOp string, writer func() error, writerFirst bool) (loginErr, writerErr error) {
	t.Helper()
	holdOp := "login"
	if writerFirst {
		holdOp = writerOp
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	s.beforeCommit = func(op string) error {
		if op == holdOp {
			enterOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-time.After(blockWait + 10*time.Second):
			}
		}
		return nil
	}

	doLogin := func() error { _, err := s.CompleteLogin(tctx(t), p); return err }
	first, second := doLogin, writer
	if writerFirst {
		first, second = writer, doLogin
	}
	var e1, e2 error
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	defer func() { // runs on every exit, including t.Fatal: release, then join, then unhook
		doRelease()
		wg.Wait()
		s.beforeCommit = nil
	}()

	wg.Add(1)
	go func() { defer wg.Done(); defer close(firstDone); e1 = first() }()
	select {
	case <-entered:
	case <-firstDone:
		t.Fatal("the first operation finished without reaching its commit point")
	case <-time.After(blockWait):
		t.Fatal("the first operation never reached its commit point")
	}
	wg.Add(1)
	go func() { defer wg.Done(); defer close(secondDone); e2 = second() }()
	waitBlockedOr(t, pool, secondDone)
	doRelease()
	wg.Wait()
	if writerFirst {
		return e2, e1
	}
	return e1, e2
}

func TestLoginRacesWithPasswordWriters(t *testing.T) {
	type writerCase struct {
		name   string
		op     string
		setup  func(t *testing.T, s *Store, u User) func() error
		newPW  string // the password hash the writer installs ("" = none)
		active bool   // account active afterwards
	}
	setHash := hashOfPassword(t, "set by operator")
	cases := []writerCase{
		{name: "set-password", op: "set-password", newPW: setHash, active: true,
			setup: func(t *testing.T, s *Store, u User) func() error {
				return func() error { _, err := s.SetPassword(tctx(t), u.Username, setHash); return err }
			}},
		{name: "complete-link", op: "complete-link", newPW: setHash, active: true,
			setup: func(t *testing.T, s *Store, u User) func() error {
				l, err := s.IssuePasswordLink(tctx(t), u.Username)
				if err != nil {
					t.Fatal(err)
				}
				return func() error { _, err := s.CompletePasswordLink(tctx(t), l.Token, l.Purpose, setHash); return err }
			}},
		{name: "disable", op: "disable", active: false,
			setup: func(t *testing.T, s *Store, u User) func() error {
				return func() error { _, err := s.Disable(tctx(t), u.Username); return err }
			}},
	}
	for _, c := range cases {
		for _, writerFirst := range []bool{true, false} {
			for _, rehash := range []bool{false, true} {
				name := fmt.Sprintf("%s/writerFirst=%v/rehash=%v", c.name, writerFirst, rehash)
				t.Run(name, func(t *testing.T) {
					s, pool := newStore(t)
					u := mkUser(t, s, pool, "ned")
					p := LoginParams{UserID: u.ID, VerifiedHash: testHash(t), Lifetimes: lt}
					rehashed := hashOfPassword(t, "test password 1")
					if rehash {
						p.NewHash = rehashed
					}
					loginErr, writerErr := raceLogin(t, s, pool, p, c.op, c.setup(t, s, u), writerFirst)
					if writerErr != nil {
						t.Fatalf("writer failed: %v", writerErr)
					}
					hash, _, last, active := userRow(t, pool, u.ID)
					if active != c.active {
						t.Fatalf("active = %v, want %v", active, c.active)
					}
					if writerFirst {
						mustErr(t, loginErr, ErrLoginStale, "login after the writer committed")
						if last != nil {
							t.Fatal("a refused login recorded last_login_at")
						}
					} else if loginErr != nil {
						t.Fatalf("login that committed first failed: %v", loginErr)
					}
					// The decisive invariants, for both orders:
					if c.newPW != "" && hash != c.newPW {
						t.Fatalf("the writer's password was overwritten (rehash=%v)", rehash)
					}
					if n := sessions(t, pool, u.ID); n != 0 {
						t.Fatalf("%d session(s) survive the writer", n)
					}
				})
			}
		}
	}
}

// TestStaleLegacyLoginThenBoundedRetry covers the *sequential* stale/retry
// sequence at store level: two logins verified against the same legacy hash,
// the second commit going stale after the first one rehashed, then the
// caller's single retry. The genuinely concurrent two-login behaviour is
// covered by the HTTP integration tests (W3 steps 5–6).
func TestStaleLegacyLoginThenBoundedRetry(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "oli")
	h1, h2 := hashOfPassword(t, "test password 1"), hashOfPassword(t, "test password 1")

	// Both tabs verified against the same legacy hash and rehashed separately.
	_, err1 := s.CompleteLogin(ctx, LoginParams{UserID: u.ID, VerifiedHash: testHash(t), NewHash: h1, Lifetimes: lt})
	_, err2 := s.CompleteLogin(ctx, LoginParams{UserID: u.ID, VerifiedHash: testHash(t), NewHash: h2, Lifetimes: lt})
	if err1 != nil {
		t.Fatal(err1)
	}
	mustErr(t, err2, ErrLoginStale, "second login with the superseded hash")
	if hash, _, _, _ := userRow(t, pool, u.ID); hash != h1 {
		t.Fatal("the first rehash must stand")
	}
	// The caller's single retry: reload, verify against the new hash, commit
	// without rehash. The benign race succeeds.
	c, err := s.LoginCredential(ctx, "oli")
	if err != nil || c.Hash == nil || *c.Hash != h1 {
		t.Fatalf("reload: %+v %v", c, err)
	}
	if _, err := s.CompleteLogin(ctx, LoginParams{UserID: u.ID, VerifiedHash: *c.Hash, Lifetimes: lt}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if sessions(t, pool, u.ID) != 2 {
		t.Fatalf("%d sessions, want 2", sessions(t, pool, u.ID))
	}
}

func TestLoginVersusDisableStress(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "pam")
	for i := 0; i < 200; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.CompleteLogin(ctx, LoginParams{UserID: u.ID, VerifiedHash: testHash(t), Lifetimes: lt}); err != nil && err != ErrLoginStale {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := s.Disable(ctx, "pam"); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		if _, _, _, active := userRow(t, pool, u.ID); !active && sessions(t, pool, u.ID) != 0 {
			t.Fatalf("iteration %d: a disabled user has a live session", i)
		}
		if err := s.Enable(ctx, "pam"); err != nil {
			t.Fatal(err)
		}
		exec(t, pool, `DELETE FROM magnetico_web.sessions WHERE user_id = $1`, u.ID)
		if t.Failed() {
			return
		}
	}
}

func TestLoginVersusSetPasswordStress(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "quin")
	setHash := hashOfPassword(t, "operator choice")
	rehash := hashOfPassword(t, "test password 1")
	for i := 0; i < 200; i++ {
		exec(t, pool, `UPDATE magnetico_web.users SET password_hash = $2 WHERE id = $1`, u.ID, testHash(t))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.CompleteLogin(ctx, LoginParams{UserID: u.ID, VerifiedHash: testHash(t), NewHash: rehash, Lifetimes: lt}); err != nil && err != ErrLoginStale {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := s.SetPassword(ctx, "quin", setHash); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		hash, _, _, _ := userRow(t, pool, u.ID)
		if hash != setHash {
			t.Fatalf("iteration %d: the operator's password was overwritten by a rehash", i)
		}
		if n := sessions(t, pool, u.ID); n != 0 {
			t.Fatalf("iteration %d: %d session(s) survive set-password", i, n)
		}
	}
}
