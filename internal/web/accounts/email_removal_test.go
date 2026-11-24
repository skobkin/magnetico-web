package accounts

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// verifiedEmail gives the user a verified address through the real flow.
func verifiedEmail(t *testing.T, s *Store, userID int64, email string) {
	t.Helper()
	l, err := s.IssueEmailVerifyLink(tctx(t), userID, email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteEmailVerify(tctx(t), l.Token); err != nil {
		t.Fatal(err)
	}
}

// unverifiedEmail stores an address the way an administrator or the migration
// does: present, not verified.
func unverifiedEmail(t *testing.T, pool *pgxpool.Pool, userID int64, email string) {
	t.Helper()
	exec(t, pool, `UPDATE magnetico_web.users SET email = $2, email_verified_at = NULL WHERE id = $1`, userID, email)
}

func emailOf(t *testing.T, s *Store, userID int64) (string, bool) {
	t.Helper()
	u, err := s.GetUserByID(tctx(t), userID)
	if err != nil {
		t.Fatal(err)
	}
	return u.Email, u.EmailVerified
}

const passwordLinks = `'setup','reset'`

func TestRemoveEmailUnverifiedKeepsPasswordLinks(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "ursula")
	unverifiedEmail(t, pool, u.ID, "ursula@example.org")
	// An administrator-issued link and a pending verification for the same address.
	if _, err := s.IssuePasswordLink(ctx, "ursula"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IssueEmailVerifyLink(ctx, u.ID, "ursula@example.org"); err != nil {
		t.Fatal(err)
	}
	addSession(t, pool, u.ID)
	pwBefore := u.PasswordHash

	res, err := s.RemoveEmail(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Email != "ursula@example.org" || res.WasVerified || !res.PendingCancelled || res.RecoveryLinksCancelled != 0 {
		t.Fatalf("result = %+v", res)
	}
	if e, v := emailOf(t, s, u.ID); e != "" || v {
		t.Fatalf("address left: %q %v", e, v)
	}
	// An unverified address never was a recovery channel: password links stay.
	if n := linkCount(t, s, u.ID, passwordLinks); n != 1 {
		t.Fatalf("password links = %d, want 1", n)
	}
	if n := linkCount(t, s, u.ID, `'email_verify'`); n != 0 {
		t.Fatalf("email links = %d", n)
	}
	if sessions(t, pool, u.ID) != 1 {
		t.Fatal("session removed")
	}
	if cur, _ := s.GetUserByID(ctx, u.ID); cur.PasswordHash != pwBefore || !cur.Active {
		t.Fatalf("password or active changed: %+v", cur)
	}
}

func TestRemoveEmailVerifiedCancelsEveryPasswordLink(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "vera")
	other := mkUser(t, s, pool, "otto")
	verifiedEmail(t, s, u.ID, "vera@example.org")
	verifiedEmail(t, s, other.ID, "otto@example.org")

	// An email-issued reset link (the only password link a user can hold: a
	// new one replaces the old).
	rl, err := s.IssueRecoveryLink(ctx, "vera@example.org")
	if err != nil {
		t.Fatal(err)
	}
	otherLink, err := s.IssueRecoveryLink(ctx, "otto@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAPIToken(ctx, u.ID, "ci", nil); err != nil {
		t.Fatal(err)
	}
	addSession(t, pool, u.ID)
	otherDump := tableDump(t, pool, other.ID)

	res, err := s.RemoveEmail(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Email != "vera@example.org" || !res.WasVerified || res.RecoveryLinksCancelled != 1 {
		t.Fatalf("result = %+v", res)
	}
	if e, v := emailOf(t, s, u.ID); e != "" || v {
		t.Fatalf("address left: %q %v", e, v)
	}
	// The mailed link is dead and the former address cannot start a new one.
	mustErr(t, errOf(s.CompletePasswordLink(ctx, rl.Token, PurposeReset, testHash(t))), ErrLinkInvalid, "reset link after removal")
	if _, err := s.IssueRecoveryLink(ctx, "vera@example.org"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("recovery for the removed address: %v", err)
	}
	// Untouched: sessions, tokens, and another user's data (including their link).
	if sessions(t, pool, u.ID) != 1 {
		t.Fatal("session removed")
	}
	if n := count(t, pool, `SELECT count(*) FROM magnetico_web.api_tokens WHERE user_id = $1 AND revoked_at IS NULL`, u.ID); n != 1 {
		t.Fatalf("api tokens = %d", n)
	}
	if tableDump(t, pool, other.ID) != otherDump {
		t.Fatal("another user's rows changed")
	}
	if _, err := s.CompletePasswordLink(ctx, otherLink.Token, PurposeReset, testHash(t)); err != nil {
		t.Fatalf("another user's link: %v", err)
	}
}

func TestRemoveEmailAdministratorLinkIsCancelledToo(t *testing.T) {
	// Password links carry no origin: an administrator-issued link outstanding
	// when a verified address is removed is cancelled as well (documented).
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "wanda")
	verifiedEmail(t, s, u.ID, "wanda@example.org")
	if _, err := s.IssuePasswordLink(ctx, "wanda"); err != nil {
		t.Fatal(err)
	}
	res, err := s.RemoveEmail(ctx, u.ID)
	if err != nil || res.RecoveryLinksCancelled != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if linkCount(t, s, u.ID, passwordLinks) != 0 {
		t.Fatal("administrator link survived")
	}
	// The operator can issue a new one.
	if _, err := s.IssuePasswordLink(ctx, "wanda"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveEmailEdgeCases(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "xena")

	// Nothing to remove: no error, nothing changes.
	res, err := s.RemoveEmail(ctx, u.ID)
	if err != nil || res != (EmailRemoveResult{}) {
		t.Fatalf("empty: %+v %v", res, err)
	}
	// A pending address only (no current address): removal cancels the link.
	if _, err := s.IssueEmailVerifyLink(ctx, u.ID, "xena@example.org"); err != nil {
		t.Fatal(err)
	}
	res, err = s.RemoveEmail(ctx, u.ID)
	if err != nil || !res.PendingCancelled || res.Email != "" {
		t.Fatalf("pending only: %+v %v", res, err)
	}
	if _, ok, _ := s.PendingEmail(ctx, u.ID); ok {
		t.Fatal("pending address survived")
	}
	// Unknown and inactive accounts.
	if _, err := s.RemoveEmail(ctx, 987654); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	unverifiedEmail(t, pool, u.ID, "xena@example.org")
	if _, err := s.Disable(ctx, "xena"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveEmail(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inactive: %v", err)
	}
	if e, _ := emailOf(t, s, u.ID); e != "xena@example.org" {
		t.Fatal("inactive account's address changed")
	}
}

func TestOldVerificationLinkCannotReAddARemovedAddress(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "yuri")
	unverifiedEmail(t, pool, u.ID, "yuri@example.org")
	l, err := s.IssueEmailVerifyLink(ctx, u.ID, "yuri@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveEmail(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	mustErr(t, errOf(s.CompleteEmailVerify(ctx, l.Token)), ErrLinkInvalid, "link after removal")
	if e, v := emailOf(t, s, u.ID); e != "" || v {
		t.Fatalf("address re-added: %q %v", e, v)
	}
}

func TestReplacingAVerifiedAddressCancelsRecoveryLinks(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "zed")
	verifiedEmail(t, s, u.ID, "zed@example.org")
	rl, err := s.IssueRecoveryLink(ctx, "zed@example.org")
	if err != nil {
		t.Fatal(err)
	}

	// A case-only "replacement" is the same address: nothing is cancelled.
	l1, _ := s.IssueEmailVerifyLink(ctx, u.ID, "ZED@example.org")
	res, err := s.CompleteEmailVerify(ctx, l1.Token)
	if err != nil || res.PreviousEmail != "" || res.RecoveryLinksCancelled != 0 {
		t.Fatalf("case-only: %+v %v", res, err)
	}
	if linkCount(t, s, u.ID, passwordLinks) != 1 {
		t.Fatal("link cancelled by a case-only change")
	}

	// A different address replaces it: the link mailed to the old one dies.
	l2, _ := s.IssueEmailVerifyLink(ctx, u.ID, "zed2@example.org")
	res, err = s.CompleteEmailVerify(ctx, l2.Token)
	if err != nil || res.PreviousEmail != "ZED@example.org" || res.RecoveryLinksCancelled != 1 {
		t.Fatalf("replacement: %+v %v", res, err)
	}
	mustErr(t, errOf(s.CompletePasswordLink(ctx, rl.Token, PurposeReset, testHash(t))), ErrLinkInvalid, "old reset link")
	if _, err := s.IssueRecoveryLink(ctx, "zed@example.org"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old address still recovers: %v", err)
	}
	if got, err := s.IssueRecoveryLink(ctx, "zed2@example.org"); err != nil || got.Email != "zed2@example.org" {
		t.Fatalf("new address: %+v %v", got, err)
	}

	// Verifying a new address when the current one was NOT verified is not a
	// replacement of a recovery channel: password links stay.
	v := mkUser(t, s, pool, "zoe")
	unverifiedEmail(t, pool, v.ID, "zoe-old@example.org")
	if _, err := s.IssuePasswordLink(ctx, "zoe"); err != nil {
		t.Fatal(err)
	}
	l3, _ := s.IssueEmailVerifyLink(ctx, v.ID, "zoe@example.org")
	res, err = s.CompleteEmailVerify(ctx, l3.Token)
	if err != nil || res.RecoveryLinksCancelled != 0 || linkCount(t, s, v.ID, passwordLinks) != 1 {
		t.Fatalf("unverified replaced: %+v %v", res, err)
	}
}

// --- races: both operation orders ---

// pausedStore is a Store over the same pool whose transaction for op stops
// just before COMMIT (all statements done, every lock held). reached is closed
// when it gets there; release lets it commit.
func pausedStore(pool *pgxpool.Pool, op string) (st *Store, reached <-chan struct{}, release func()) {
	r := make(chan struct{})
	rel := make(chan struct{})
	var once sync.Once
	st = &Store{pool: pool}
	st.beforeCommit = func(o string) error {
		if o == op {
			once.Do(func() { close(r) })
			<-rel
		}
		return nil
	}
	return st, r, func() { close(rel) }
}

// waitLockWait waits until some session of this database is blocked on a lock,
// or until the second operation has finished without ever needing one (a
// statement whose predicate matches nothing in its snapshot does not wait).
func waitLockWait(t *testing.T, pool *pgxpool.Pool, finished <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-finished:
			return
		default:
		}
		if count(t, pool, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the second operation never blocked on the user row lock")
}

// race runs first (stopped before its COMMIT) and second (blocks on the user
// row lock), then releases first. firstOp and secondOp are the operations'
// store method calls; it returns their errors.
func race(t *testing.T, pool *pgxpool.Pool, firstName string, firstOp func(*Store) error, secondOp func(*Store) error) (firstErr, secondErr error) {
	t.Helper()
	pausedFirst, reached, rel := pausedStore(pool, firstName)
	release := sync.OnceFunc(rel)
	// A failure must never leave the paused transaction holding its locks:
	// pool.Close would wait for it forever.
	t.Cleanup(release)
	plain := &Store{pool: pool}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		firstErr = firstOp(pausedFirst)
	}()
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the first operation never reached its commit")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		secondErr = secondOp(plain)
	}()
	waitLockWait(t, pool, done)
	release()
	wg.Wait()
	<-done
	return
}

// raceScenario builds a user with a verified address and returns what the
// pairs need.
func raceUser(t *testing.T, name, email string) (*Store, *pgxpool.Pool, User) {
	t.Helper()
	s, pool := newStore(t)
	u := mkUser(t, s, pool, name)
	verifiedEmail(t, s, u.ID, email)
	return s, pool, u
}

func TestRaceRemoveVersusRecoveryIssuance(t *testing.T) {
	const email = "rita@example.org"
	remove := func(u User) func(*Store) error {
		return func(s *Store) error { _, err := s.RemoveEmail(tctx(t), u.ID); return err }
	}
	issue := func(s *Store) error { _, err := s.IssueRecoveryLink(tctx(t), email); return err }

	t.Run("issuance commits first", func(t *testing.T) {
		_, pool, u := raceUser(t, "rita", email)
		fe, se := race(t, pool, "issue-recovery-link", issue, remove(u))
		if fe != nil || se != nil {
			t.Fatalf("errors: %v / %v", fe, se)
		}
		// The link existed for an instant; removal deleted it.
		if n := linkCount(t, &Store{pool: pool}, u.ID, passwordLinks); n != 0 {
			t.Fatalf("%d recovery links left for the removed address", n)
		}
		if e, _ := emailOf(t, &Store{pool: pool}, u.ID); e != "" {
			t.Fatal("address left")
		}
	})
	t.Run("removal commits first", func(t *testing.T) {
		_, pool, u := raceUser(t, "rita", email)
		fe, se := race(t, pool, "remove-email", remove(u), issue)
		if fe != nil {
			t.Fatal(fe)
		}
		if !errors.Is(se, ErrNotFound) {
			t.Fatalf("issuance after removal: %v, want ErrNotFound", se)
		}
		if n := linkCount(t, &Store{pool: pool}, u.ID, passwordLinks); n != 0 {
			t.Fatalf("%d recovery links created for the removed address", n)
		}
	})
}

func TestRaceReplacementVersusRecoveryIssuance(t *testing.T) {
	const oldAddr, newAddr = "rex@example.org", "rex-new@example.org"
	pending := func(t *testing.T, s *Store, u User) string {
		t.Helper()
		l, err := s.IssueEmailVerifyLink(tctx(t), u.ID, newAddr)
		if err != nil {
			t.Fatal(err)
		}
		return l.Token
	}

	t.Run("issuance by the old address commits first", func(t *testing.T) {
		s, pool, u := raceUser(t, "rex", oldAddr)
		tok := pending(t, s, u)
		fe, se := race(t, pool, "issue-recovery-link",
			func(s *Store) error { _, err := s.IssueRecoveryLink(tctx(t), oldAddr); return err },
			func(s *Store) error { _, err := s.CompleteEmailVerify(tctx(t), tok); return err })
		if fe != nil || se != nil {
			t.Fatalf("errors: %v / %v", fe, se)
		}
		if n := linkCount(t, s, u.ID, passwordLinks); n != 0 {
			t.Fatalf("%d links left that were mailed to the replaced address", n)
		}
		if e, v := emailOf(t, s, u.ID); e != newAddr || !v {
			t.Fatalf("address = %q %v", e, v)
		}
	})
	t.Run("replacement commits first: old address", func(t *testing.T) {
		s, pool, u := raceUser(t, "rex", oldAddr)
		tok := pending(t, s, u)
		fe, se := race(t, pool, "complete-email-verify",
			func(s *Store) error { _, err := s.CompleteEmailVerify(tctx(t), tok); return err },
			func(s *Store) error { _, err := s.IssueRecoveryLink(tctx(t), oldAddr); return err })
		if fe != nil {
			t.Fatal(fe)
		}
		if !errors.Is(se, ErrNotFound) {
			t.Fatalf("issuance by the replaced address: %v, want ErrNotFound", se)
		}
		if n := linkCount(t, s, u.ID, passwordLinks); n != 0 {
			t.Fatalf("%d links created for the replaced address", n)
		}
	})
	t.Run("replacement commits first: new address", func(t *testing.T) {
		s, pool, u := raceUser(t, "rex", oldAddr)
		tok := pending(t, s, u)
		fe, se := race(t, pool, "complete-email-verify",
			func(s *Store) error { _, err := s.CompleteEmailVerify(tctx(t), tok); return err },
			func(s *Store) error { _, err := s.IssueRecoveryLink(tctx(t), newAddr); return err })
		if fe != nil {
			t.Fatal(fe)
		}
		// While the replacement is uncommitted the new address is not verified
		// yet, so a concurrent issuance for it finds nothing (and creates nothing).
		if !errors.Is(se, ErrNotFound) {
			t.Fatalf("concurrent issuance for the not-yet-verified address: %v, want ErrNotFound", se)
		}
		if n := linkCount(t, s, u.ID, passwordLinks); n != 0 {
			t.Fatalf("links = %d", n)
		}
		// After the commit the new address is the recovery channel, and the link
		// is bound to it.
		got, err := s.IssueRecoveryLink(tctx(t), newAddr)
		if err != nil || got.Email != newAddr {
			t.Fatalf("after the replacement: %+v %v", got, err)
		}
		if n := linkCount(t, s, u.ID, passwordLinks); n != 1 {
			t.Fatalf("links = %d, want exactly the new address's", n)
		}
	})
}

func TestRaceRemoveVersusVerificationAndIssuance(t *testing.T) {
	const cur, next = "ron@example.org", "ron-new@example.org"
	remove := func(u User) func(*Store) error {
		return func(s *Store) error { _, err := s.RemoveEmail(tctx(t), u.ID); return err }
	}

	t.Run("verification commits first", func(t *testing.T) {
		s, pool, u := raceUser(t, "ron", cur)
		l, _ := s.IssueEmailVerifyLink(tctx(t), u.ID, next)
		fe, se := race(t, pool, "complete-email-verify",
			func(s *Store) error { _, err := s.CompleteEmailVerify(tctx(t), l.Token); return err }, remove(u))
		if fe != nil || se != nil {
			t.Fatalf("errors: %v / %v", fe, se)
		}
		if e, v := emailOf(t, s, u.ID); e != "" || v {
			t.Fatalf("address after verify then remove: %q %v", e, v)
		}
	})
	t.Run("removal commits first", func(t *testing.T) {
		s, pool, u := raceUser(t, "ron", cur)
		l, _ := s.IssueEmailVerifyLink(tctx(t), u.ID, next)
		fe, se := race(t, pool, "remove-email", remove(u),
			func(s *Store) error { _, err := s.CompleteEmailVerify(tctx(t), l.Token); return err })
		if fe != nil {
			t.Fatal(fe)
		}
		if !errors.Is(se, ErrLinkInvalid) {
			t.Fatalf("verification after removal: %v, want ErrLinkInvalid", se)
		}
		if e, v := emailOf(t, s, u.ID); e != "" || v {
			t.Fatalf("address resurrected: %q %v", e, v)
		}
	})
	t.Run("link issuance commits first", func(t *testing.T) {
		s, pool, u := raceUser(t, "ron", cur)
		fe, se := race(t, pool, "issue-email-link",
			func(s *Store) error { _, err := s.IssueEmailVerifyLink(tctx(t), u.ID, next); return err }, remove(u))
		if fe != nil || se != nil {
			t.Fatalf("errors: %v / %v", fe, se)
		}
		// Removal deleted the freshly issued link.
		if _, ok, _ := s.PendingEmail(tctx(t), u.ID); ok {
			t.Fatal("pending link survived the removal")
		}
	})
	t.Run("removal commits first, then issuance", func(t *testing.T) {
		s, pool, u := raceUser(t, "ron", cur)
		fe, se := race(t, pool, "remove-email", remove(u),
			func(s *Store) error { _, err := s.IssueEmailVerifyLink(tctx(t), u.ID, next); return err })
		if fe != nil || se != nil {
			t.Fatalf("errors: %v / %v", fe, se)
		}
		// A link requested after the removal is legitimate (a new request).
		if e, ok, _ := s.PendingEmail(tctx(t), u.ID); !ok || e != next {
			t.Fatalf("pending = %q %v", e, ok)
		}
		if e, _ := emailOf(t, s, u.ID); e != "" {
			t.Fatal("address left")
		}
	})
}

func TestRaceRemoveVersusPasswordLinkCompletion(t *testing.T) {
	const email = "rob@example.org"
	remove := func(u User) func(*Store) error {
		return func(s *Store) error { _, err := s.RemoveEmail(tctx(t), u.ID); return err }
	}
	t.Run("completion commits first", func(t *testing.T) {
		s, pool, u := raceUser(t, "rob", email)
		rl, _ := s.IssueRecoveryLink(tctx(t), email)
		fe, se := race(t, pool, "complete-link",
			func(s *Store) error {
				_, err := s.CompletePasswordLink(tctx(t), rl.Token, PurposeReset, altHash(t))
				return err
			}, remove(u))
		if fe != nil || se != nil {
			t.Fatalf("errors: %v / %v", fe, se)
		}
		// The reset committed first: the password changed, and the removal
		// that followed still cleared the address.
		if cur, _ := s.GetUserByID(tctx(t), u.ID); cur.PasswordHash == u.PasswordHash {
			t.Fatal("the completed reset did not change the password")
		}
		if e, _ := emailOf(t, s, u.ID); e != "" {
			t.Fatal("address left")
		}
	})
	t.Run("removal commits first", func(t *testing.T) {
		s, pool, u := raceUser(t, "rob", email)
		rl, _ := s.IssueRecoveryLink(tctx(t), email)
		fe, se := race(t, pool, "remove-email", remove(u),
			func(s *Store) error {
				_, err := s.CompletePasswordLink(tctx(t), rl.Token, PurposeReset, altHash(t))
				return err
			})
		if fe != nil {
			t.Fatal(fe)
		}
		if !errors.Is(se, ErrLinkInvalid) {
			t.Fatalf("reset after removal: %v, want ErrLinkInvalid", se)
		}
		if cur, _ := s.GetUserByID(tctx(t), u.ID); cur.PasswordHash != u.PasswordHash {
			t.Fatal("password changed through a cancelled link")
		}
	})
}

// Repeated concurrent runs under the race detector: no deadlock, and the
// invariant "no valid recovery link exists for an address that is not the
// verified one" holds after every interleaving.
func TestRecoveryInvariantUnderConcurrentEmailChanges(t *testing.T) {
	s, pool, u := raceUser(t, "ruth", "ruth@example.org")
	ctx := tctx(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _, _ = s.IssueRecoveryLink(ctx, "ruth@example.org") }()
		go func() { defer wg.Done(); _, _ = s.RemoveEmail(ctx, u.ID) }()
		go func() {
			defer wg.Done()
			if l, err := s.IssueEmailVerifyLink(ctx, u.ID, "ruth@example.org"); err == nil {
				_, _ = s.CompleteEmailVerify(ctx, l.Token)
			}
		}()
	}
	wg.Wait()
	// Whatever the interleaving, a surviving password link implies a verified
	// address (a link without one would be a recovery token for nothing).
	e, v := emailOf(t, s, u.ID)
	if n := linkCount(t, s, u.ID, passwordLinks); n > 0 && !(v && e != "") {
		t.Fatalf("%d password links remain but the address is %q verified=%v", n, e, v)
	}
	_ = pool
}
