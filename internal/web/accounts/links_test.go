package accounts

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func linkCount(t *testing.T, s *Store, userID int64, purposes string) int {
	t.Helper()
	return count(t, s.pool, `SELECT count(*) FROM magnetico_web.one_time_tokens WHERE user_id = $1 AND purpose IN (`+purposes+`)`, userID)
}

func TestLinkReplacedUsedAndExpired(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "frank")

	first, err := s.IssuePasswordLink(ctx, "frank")
	if err != nil || first.Purpose != PurposeReset {
		t.Fatalf("link = %+v, %v", first, err)
	}
	second, err := s.IssuePasswordLink(ctx, "frank")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompletePasswordLink(ctx, first.Token, PurposeReset, testHash(t)); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("replaced link: %v", err)
	}
	if linkCount(t, s, u.ID, `'setup','reset'`) != 1 {
		t.Fatal("not exactly one outstanding link")
	}
	// Wrong purpose is refused and does not consume.
	if _, err := s.CompletePasswordLink(ctx, second.Token, PurposeSetup, testHash(t)); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("purpose mismatch: %v", err)
	}
	if _, err := s.CompletePasswordLink(ctx, second.Token, PurposeReset, testHash(t)); err != nil {
		t.Fatalf("valid link: %v", err)
	}
	if _, err := s.CompletePasswordLink(ctx, second.Token, PurposeReset, testHash(t)); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("reused link: %v", err)
	}
	if linkCount(t, s, u.ID, `'setup','reset'`) != 0 {
		t.Fatal("consumed link row still exists")
	}

	// Expired link: rejected; issuing a replacement afterwards works without a unique conflict.
	l, _ := s.IssuePasswordLink(ctx, "frank")
	exec(t, pool, `UPDATE magnetico_web.one_time_tokens SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' WHERE user_id = $1`, u.ID)
	if _, err := s.CompletePasswordLink(ctx, l.Token, PurposeReset, testHash(t)); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("expired link: %v", err)
	}
	if _, err := s.IssuePasswordLink(ctx, "frank"); err != nil {
		t.Fatalf("replacement after expiry: %v", err)
	}
	if linkCount(t, s, u.ID, `'setup','reset'`) != 1 {
		t.Fatal("expired link was not replaced")
	}
	if _, err := s.CompletePasswordLink(ctx, "", PurposeReset, testHash(t)); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("empty token: %v", err)
	}
	if _, err := s.CompletePasswordLink(ctx, "no-such-token", PurposeReset, testHash(t)); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("unknown token: %v", err)
	}
}

func TestSetupVsResetPurpose(t *testing.T) {
	s, _ := newStore(t)
	ctx := tctx(t)
	if _, err := s.CreateUser(tctx(t), CreateUserParams{Username: "gina"}); err != nil {
		t.Fatal(err)
	}
	l, err := s.IssuePasswordLink(ctx, "gina")
	if err != nil || l.Purpose != PurposeSetup {
		t.Fatalf("passwordless user link = %+v, %v", l, err)
	}
	if _, err := s.CompletePasswordLink(ctx, l.Token, PurposeReset, testHash(t)); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("setup link used as reset: %v", err)
	}
	if _, err := s.CompletePasswordLink(ctx, l.Token, PurposeSetup, "not-a-hash"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad hash: %v", err)
	}
	if _, err := s.CompletePasswordLink(ctx, l.Token, PurposeSetup, testHash(t)); err != nil {
		t.Fatal(err)
	}
}

// A failed issuance transaction must leave the previous usable link intact.
func TestFailedIssuanceKeepsThePreviousLink(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	mkUser(t, s, pool, "hank")
	old, err := s.IssuePasswordLink(ctx, "hank")
	if err != nil {
		t.Fatal(err)
	}
	s.beforeCommit = func(op string) error {
		if op == "issue-link" {
			return errors.New("injected failure before commit")
		}
		return nil
	}
	if _, err := s.IssuePasswordLink(ctx, "hank"); err == nil {
		t.Fatal("injected failure not reported")
	}
	s.beforeCommit = nil
	if _, err := s.CompletePasswordLink(ctx, old.Token, PurposeReset, testHash(t)); err != nil {
		t.Fatalf("the previous link did not survive the failed issuance: %v", err)
	}
}

func TestCompletionAndSetPasswordRevokeEverything(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "ivy")
	for round, complete := range []bool{true, false} {
		if _, err := s.CreateAPIToken(ctx, u.ID, "", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateAPIToken(ctx, u.ID, "", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateRSSToken(ctx, u.ID, "", nil); err != nil {
			t.Fatal(err)
		}
		exec(t, pool, `INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy) VALUES ($1,$2,true)`, hashToken("legacy"+string(rune('a'+round))), u.ID)
		addSession(t, pool, u.ID)
		exec(t, pool, `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, email, expires_at) VALUES ($1,$2,'email_verify','x@example.org', now() + interval '1 day')`, hashToken("ev"+string(rune('a'+round))), u.ID)
		var r Revocations
		var err error
		if complete {
			l, _ := s.IssuePasswordLink(ctx, "ivy")
			var res CompleteResult
			res, err = s.CompletePasswordLink(ctx, l.Token, PurposeReset, testHash(t))
			r = res.Revocations
		} else {
			if _, err := s.IssuePasswordLink(ctx, "ivy"); err != nil {
				t.Fatal(err)
			}
			r, err = s.SetPassword(ctx, "ivy", testHash(t))
		}
		if err != nil {
			t.Fatal(err)
		}
		if r.Sessions != 1 || r.APITokens != 2 || r.RSSTokens != 2 {
			t.Fatalf("round %d: revocations = %+v, want sessions 1, api 2, rss 2 counted separately", round, r)
		}
		if n := count(t, pool, `SELECT count(*) FROM magnetico_web.api_tokens WHERE user_id = $1 AND revoked_at IS NULL`, u.ID) +
			count(t, pool, `SELECT count(*) FROM magnetico_web.rss_tokens WHERE user_id = $1 AND revoked_at IS NULL`, u.ID); n != 0 {
			t.Fatalf("%d tokens survived", n)
		}
		if linkCount(t, s, u.ID, `'setup','reset'`) != 0 {
			t.Fatal("outstanding password links survived")
		}
		if linkCount(t, s, u.ID, `'email_verify'`) != 1 {
			t.Fatal("the email verification link was touched by a password operation")
		}
		exec(t, pool, `DELETE FROM magnetico_web.one_time_tokens WHERE user_id = $1`, u.ID)
	}
}

func TestCompletingOneLinkDeletesOtherPasswordLinks(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "jack")
	l, _ := s.IssuePasswordLink(ctx, "jack")
	// Force a second password link row for the same user (a state the store
	// itself cannot create): it must still be removed by the completion.
	exec(t, pool, `DROP INDEX magnetico_web.one_time_tokens_one_per_kind`)
	exec(t, pool, `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, expires_at) VALUES ($1,$2,'setup', now() + interval '1 day')`, hashToken("other"), u.ID)
	if _, err := s.CompletePasswordLink(ctx, l.Token, PurposeReset, testHash(t)); err != nil {
		t.Fatal(err)
	}
	if linkCount(t, s, u.ID, `'setup','reset'`) != 0 {
		t.Fatal("another outstanding password link survived")
	}
}

func TestCompletionOnDisabledAccountIsRefusedAndNotConsumed(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "kate")
	l, _ := s.IssuePasswordLink(ctx, "kate")
	if _, err := s.Disable(ctx, "kate"); err != nil {
		t.Fatal(err)
	}
	before := tableDump(t, pool, u.ID)
	if _, err := s.CompletePasswordLink(ctx, l.Token, PurposeReset, testHash(t)); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("disabled owner: %v", err)
	}
	if got := tableDump(t, pool, u.ID); got != before {
		t.Fatal("refused completion changed rows")
	}
	// Issuing still works while disabled and reports it.
	if l2, err := s.IssuePasswordLink(ctx, "kate"); err != nil || l2.OwnerActive {
		t.Fatalf("issue on disabled account: %+v, %v", l2, err)
	}
	l3, _ := s.IssuePasswordLink(ctx, "kate")
	if err := s.Enable(ctx, "kate"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompletePasswordLink(ctx, l3.Token, PurposeReset, testHash(t)); err != nil {
		t.Fatalf("after enable: %v", err)
	}
	if u2, _ := s.GetUser(ctx, "kate"); !u2.Active {
		t.Fatal("completion wrote active")
	}
}

func TestConcurrentCompletionOfOneLinkSucceedsOnce(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "liam")
	if _, err := s.CreateAPIToken(ctx, u.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	l, _ := s.IssuePasswordLink(ctx, "liam")
	var ok, invalid atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.CompletePasswordLink(ctx, l.Token, PurposeReset, testHash(t))
			switch {
			case err == nil:
				ok.Add(1)
				if res.APITokens != 1 {
					t.Errorf("revocations = %+v", res)
				}
			case errors.Is(err, ErrLinkInvalid):
				invalid.Add(1)
			default:
				t.Errorf("unexpected %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || invalid.Load() != 11 {
		t.Fatalf("ok=%d invalid=%d", ok.Load(), invalid.Load())
	}
}

func TestConcurrentIssuanceLeavesOneLink(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "mona")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.IssuePasswordLink(ctx, "mona"); err != nil {
				t.Errorf("issue: %v", err)
			}
		}()
	}
	wg.Wait()
	if linkCount(t, s, u.ID, `'setup','reset'`) != 1 {
		t.Fatal("not exactly one outstanding link")
	}
}

// Completion versus replacement, both orders, deterministically.
func TestCompletionVersusReplacement(t *testing.T) {
	t.Run("completion holds the lock, replacement waits", func(t *testing.T) {
		s, pool := newStore(t)
		ctx := tctx(t)
		u := mkUser(t, s, pool, "nina")
		old, _ := s.IssuePasswordLink(ctx, "nina")
		inCommit := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		s.beforeCommit = func(op string) error {
			if op == "complete-link" {
				once.Do(func() { close(inCommit) })
				<-release
			}
			return nil
		}
		done := make(chan error, 1)
		go func() { _, err := s.CompletePasswordLink(ctx, old.Token, PurposeReset, testHash(t)); done <- err }()
		<-inCommit
		repl := make(chan Link, 1)
		go func() {
			l, err := s.IssuePasswordLink(ctx, "nina")
			if err != nil {
				t.Error(err)
			}
			repl <- l
		}()
		select {
		case <-repl:
			t.Fatal("replacement did not wait for the completion")
		default:
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		fresh := <-repl
		if _, err := s.CompletePasswordLink(ctx, fresh.Token, PurposeReset, testHash(t)); err != nil {
			t.Fatalf("the replacement was consumed or lost: %v", err)
		}
		_ = u
	})
	t.Run("replacement commits first, old token cannot be revived", func(t *testing.T) {
		s, pool := newStore(t)
		ctx := tctx(t)
		mkUser(t, s, pool, "otto")
		old, _ := s.IssuePasswordLink(ctx, "otto")
		fresh, _ := s.IssuePasswordLink(ctx, "otto")
		if _, err := s.CompletePasswordLink(ctx, old.Token, PurposeReset, testHash(t)); !errors.Is(err, ErrLinkInvalid) {
			t.Fatalf("old token: %v", err)
		}
		if _, err := s.CompletePasswordLink(ctx, fresh.Token, PurposeReset, testHash(t)); err != nil {
			t.Fatalf("new token: %v", err)
		}
	})
	t.Run("stress", func(t *testing.T) {
		s, pool := newStore(t)
		ctx := tctx(t)
		u := mkUser(t, s, pool, "pia")
		for i := 0; i < 100; i++ {
			old, _ := s.IssuePasswordLink(ctx, "pia")
			var wg sync.WaitGroup
			var fresh Link
			wg.Add(2)
			go func() { defer wg.Done(); _, _ = s.CompletePasswordLink(ctx, old.Token, PurposeReset, testHash(t)) }() // racing the second issue: either outcome is legitimate
			go func() {
				defer wg.Done()
				var err error
				if fresh, err = s.IssuePasswordLink(ctx, "pia"); err != nil {
					t.Error(err)
				}
			}()
			wg.Wait()
			if n := linkCount(t, s, u.ID, `'setup','reset'`); n > 1 {
				t.Fatalf("iteration %d: %d outstanding links", i, n)
			}
			// The replacement is either still valid (completion ran first)
			// or was never consumed by the old token.
			if _, err := s.CompletePasswordLink(ctx, fresh.Token, PurposeReset, testHash(t)); err != nil && !errors.Is(err, ErrLinkInvalid) {
				t.Fatal(err)
			}
		}
	})
}

func TestCompletionVersusDisable(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "quin")
	l, _ := s.IssuePasswordLink(ctx, "quin")
	if _, err := s.Disable(ctx, "quin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompletePasswordLink(ctx, l.Token, PurposeReset, testHash(t)); !errors.Is(err, ErrLinkInvalid) {
		t.Fatal("completion after a committed disable")
	}
	if linkCount(t, s, u.ID, `'setup','reset'`) != 1 {
		t.Fatal("link did not survive")
	}
	if err := s.Enable(ctx, "quin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompletePasswordLink(ctx, l.Token, PurposeReset, testHash(t)); err != nil {
		t.Fatal(err)
	}
	// Disable after a committed completion leaves a disabled user with the new password.
	before, _ := s.GetUser(ctx, "quin")
	if _, err := s.Disable(ctx, "quin"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetUser(ctx, "quin")
	if after.Active || after.PasswordHash != before.PasswordHash {
		t.Fatalf("after = %+v", after)
	}
}
