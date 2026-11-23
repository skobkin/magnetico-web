package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/internal/web/password"
)

// mkSession inserts a session with the given absolute expiry and returns its
// token hash.
func mkSession(t *testing.T, pool *pgxpool.Pool, userID int64, remember bool, expiresIn time.Duration) []byte {
	t.Helper()
	_, th, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO magnetico_web.sessions (token_hash, user_id, expires_at, remember)
        VALUES ($1,$2, now() + make_interval(secs => $3), $4)`, th, userID, expiresIn.Seconds(), remember)
	return th
}

func TestChangePasswordKeepsTokensAndRotatesSession(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "pam")
	api, _ := s.CreateAPIToken(ctx, u.ID, "a", nil)
	rss, _ := s.CreateRSSToken(ctx, u.ID, "r", nil)
	cur := mkSession(t, pool, u.ID, true, 3*time.Hour)
	mkSession(t, pool, u.ID, false, time.Hour)
	mkSession(t, pool, u.ID, false, time.Hour)
	if _, err := s.IssuePasswordLink(ctx, "pam"); err != nil {
		t.Fatal(err)
	}
	var oldExpiry time.Time
	if err := pool.QueryRow(ctx, `SELECT expires_at FROM magnetico_web.sessions WHERE token_hash = $1`, cur).Scan(&oldExpiry); err != nil {
		t.Fatal(err)
	}

	res, err := s.ChangePassword(ctx, ChangePasswordParams{UserID: u.ID, VerifiedHash: u.PasswordHash, NewHash: altHash(t), CurrentSession: cur})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 2 || res.APITokens != 0 || res.RSSTokens != 0 {
		t.Fatalf("revocations = %+v", res.Revocations)
	}
	if !res.Remember || !res.ExpiresAt.Equal(oldExpiry) {
		t.Fatalf("rotation changed lifetime: remember=%v expires=%v want %v", res.Remember, res.ExpiresAt, oldExpiry)
	}
	if n := sessions(t, pool, u.ID); n != 1 {
		t.Fatalf("sessions = %d, want only the rotated one", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM magnetico_web.sessions WHERE token_hash = $1`, cur); n != 0 {
		t.Fatal("the old current session survived")
	}
	newHash, _ := SessionTokenHash(res.Token)
	if _, err := s.LookupSession(ctx, newHash, time.Hour); err != nil {
		t.Fatalf("rotated session does not resolve: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM magnetico_web.one_time_tokens WHERE user_id = $1`, u.ID); n != 0 {
		t.Fatal("password links survived a password change")
	}
	if _, err := s.AuthenticateAPIToken(ctx, api.Plaintext); err != nil {
		t.Fatalf("API token revoked by default: %v", err)
	}
	if _, err := s.AuthenticateRSSToken(ctx, rss.Plaintext); err != nil {
		t.Fatalf("RSS token revoked by default: %v", err)
	}
}

func TestChangePasswordOptionsRevokeEachTypeSeparately(t *testing.T) {
	for _, tc := range []struct{ api, rss bool }{{true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("api=%v/rss=%v", tc.api, tc.rss), func(t *testing.T) {
			s, pool := newStore(t)
			ctx := tctx(t)
			u := mkUser(t, s, pool, "quinn")
			api, _ := s.CreateAPIToken(ctx, u.ID, "", nil)
			rss, _ := s.CreateRSSToken(ctx, u.ID, "", nil)
			const legacy = "00112233445566778899aabbccddeeff"
			exec(t, pool, `INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy) VALUES ($1,$2,true)`, hashToken(legacy), u.ID)
			cur := mkSession(t, pool, u.ID, false, time.Hour)
			res, err := s.ChangePassword(ctx, ChangePasswordParams{UserID: u.ID, VerifiedHash: u.PasswordHash, NewHash: testHash(t), CurrentSession: cur, RevokeAPI: tc.api, RevokeRSS: tc.rss})
			if err != nil {
				t.Fatal(err)
			}
			_, apiErr := s.AuthenticateAPIToken(ctx, api.Plaintext)
			_, rssErr := s.AuthenticateRSSToken(ctx, rss.Plaintext)
			_, legErr := s.AuthenticateRSSToken(ctx, legacy)
			if (apiErr != nil) != tc.api || (rssErr != nil) != tc.rss || (legErr != nil) != tc.rss {
				t.Fatalf("api revoked=%v rss revoked=%v legacy revoked=%v, options %+v", apiErr != nil, rssErr != nil, legErr != nil, tc)
			}
			if (res.APITokens == 1) != tc.api || (res.RSSTokens == 2) != tc.rss {
				t.Fatalf("counts = %+v", res.Revocations)
			}
		})
	}
}

func TestChangePasswordStaleAndExpiredSession(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "rex")
	cur := mkSession(t, pool, u.ID, false, time.Hour)

	_, err := s.ChangePassword(ctx, ChangePasswordParams{UserID: u.ID, VerifiedHash: u.PasswordHash + "x", NewHash: testHash(t), CurrentSession: cur})
	mustErr(t, err, ErrLoginStale, "wrong verified hash")

	expired := mkSession(t, pool, u.ID, false, time.Hour)
	exec(t, pool, `UPDATE magnetico_web.sessions SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 minute' WHERE token_hash = $1`, expired)
	before := tableDump(t, pool, u.ID)
	_, err = s.ChangePassword(ctx, ChangePasswordParams{UserID: u.ID, VerifiedHash: u.PasswordHash, NewHash: testHash(t), CurrentSession: expired})
	mustErr(t, err, ErrUnauthenticated, "expired current session")
	if after := tableDump(t, pool, u.ID); after != before {
		t.Fatal("a refused change wrote something")
	}

	if _, err := s.Disable(ctx, "rex"); err != nil {
		t.Fatal(err)
	}
	_, err = s.ChangePassword(ctx, ChangePasswordParams{UserID: u.ID, VerifiedHash: u.PasswordHash, NewHash: testHash(t), CurrentSession: cur})
	mustErr(t, err, ErrLoginStale, "disabled account")
}

func TestChangePasswordRacesWithReset(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "sky")
	cur := mkSession(t, pool, u.ID, false, time.Hour)
	// A reset commits between the verification and the change: the change must
	// fail without overwriting the reset's hash.
	if _, err := s.SetPassword(ctx, "sky", altHash(t)); err != nil {
		t.Fatal(err)
	}
	_, err := s.ChangePassword(ctx, ChangePasswordParams{UserID: u.ID, VerifiedHash: u.PasswordHash, NewHash: testHash(t), CurrentSession: cur})
	mustErr(t, err, ErrLoginStale, "stale after reset")
}

func TestTokenListingAndOwnerScopedRevocation(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	a := mkUser(t, s, pool, "ann")
	b := mkUser(t, s, pool, "bob")
	aAPI, _ := s.CreateAPIToken(ctx, a.ID, "ann api", nil)
	aRSS, _ := s.CreateRSSToken(ctx, a.ID, "ann rss", nil)
	bAPI, _ := s.CreateAPIToken(ctx, b.ID, "bob api", nil)
	bRSS, _ := s.CreateRSSToken(ctx, b.ID, "bob rss", nil)
	exec(t, pool, `INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy, created_at) VALUES ($1,$2,true, now() - interval '30 days')`, hashToken("0123456789abcdef0123456789abcdef"), a.ID)

	list, err := s.ListRSSTokens(ctx, a.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("rss list = %+v, %v", list, err)
	}
	// newest id first: the legacy row was inserted last
	if !list[0].Legacy || list[1].Legacy || list[1].Label != "ann rss" {
		t.Fatalf("legacy flag/order wrong: %+v", list)
	}
	if l, _ := s.ListAPITokens(ctx, a.ID); len(l) != 1 || l[0].Label != "ann api" {
		t.Fatalf("api list = %+v", l)
	}

	// Another user's id is indistinguishable from an unknown one, and nothing changes.
	mustErr(t, s.RevokeAPIToken(ctx, a.ID, bAPI.ID), ErrNotFound, "foreign api token")
	mustErr(t, s.RevokeRSSToken(ctx, a.ID, bRSS.ID), ErrNotFound, "foreign rss token")
	mustErr(t, s.RevokeAPIToken(ctx, a.ID, 99999), ErrNotFound, "unknown token")
	if _, err := s.AuthenticateAPIToken(ctx, bAPI.Plaintext); err != nil {
		t.Fatal("foreign revoke took effect")
	}
	// An API id is never an RSS id.
	if aAPI.ID == aRSS.ID {
		mustErr(t, s.RevokeRSSToken(ctx, b.ID, aRSS.ID), ErrNotFound, "other user's rss id")
	}

	if err := s.RevokeRSSToken(ctx, a.ID, aRSS.ID); err != nil {
		t.Fatal(err)
	}
	mustErr(t, s.RevokeRSSToken(ctx, a.ID, aRSS.ID), ErrNotFound, "second revoke")
	if _, err := s.AuthenticateRSSToken(ctx, aRSS.Plaintext); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("revoked token still authenticates")
	}
	if l, _ := s.ListRSSTokens(ctx, a.ID); len(l) != 1 || !l[0].Legacy {
		t.Fatalf("revoked token still listed: %+v", l)
	}
}

func TestTokenCapCountsUnrevokedPerTypeAndRequiresActiveOwner(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "cap")
	var first CreatedToken
	for i := 0; i < MaxActiveTokens; i++ {
		tok, err := s.CreateAPIToken(ctx, u.ID, "", nil)
		if err != nil {
			t.Fatalf("token %d: %v", i, err)
		}
		if i == 0 {
			first = tok
		}
	}
	_, err := s.CreateAPIToken(ctx, u.ID, "", nil)
	mustErr(t, err, ErrTokenLimit, "51st API token")
	if _, err := s.CreateRSSToken(ctx, u.ID, "", nil); err != nil {
		t.Fatalf("the RSS count must be independent: %v", err)
	}
	if err := s.RevokeAPIToken(ctx, u.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAPIToken(ctx, u.ID, "", nil); err != nil {
		t.Fatalf("a revoked token must free its slot: %v", err)
	}
	if _, err := s.Disable(ctx, "cap"); err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateRSSToken(ctx, u.ID, "", nil)
	mustErr(t, err, ErrNotFound, "disabled owner")
	_, err = s.CreateAPIToken(ctx, 424242, "", nil)
	mustErr(t, err, ErrNotFound, "unknown owner")
}

func TestTokenTouchIsThrottled(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "tch")
	tok, _ := s.CreateAPIToken(ctx, u.ID, "", nil)
	id, err := s.AuthenticateAPIToken(ctx, tok.Plaintext)
	if err != nil || !id.TouchDue {
		t.Fatalf("first use: %+v, %v", id, err)
	}
	if err := s.TouchAPIToken(ctx, id.TokenID); err != nil {
		t.Fatal(err)
	}
	var t1 time.Time
	pool.QueryRow(ctx, `SELECT last_used_at FROM magnetico_web.api_tokens WHERE id = $1`, id.TokenID).Scan(&t1)
	if t1.IsZero() {
		t.Fatal("last_used_at not set")
	}
	id2, _ := s.AuthenticateAPIToken(ctx, tok.Plaintext)
	if id2.TouchDue {
		t.Fatal("touch due again immediately")
	}
	if err := s.TouchAPIToken(ctx, id.TokenID); err != nil {
		t.Fatal(err)
	}
	var t2 time.Time
	pool.QueryRow(ctx, `SELECT last_used_at FROM magnetico_web.api_tokens WHERE id = $1`, id.TokenID).Scan(&t2)
	if !t1.Equal(t2) {
		t.Fatal("a throttled touch wrote")
	}
	exec(t, pool, `UPDATE magnetico_web.api_tokens SET last_used_at = now() - interval '10 minutes' WHERE id = $1`, id.TokenID)
	if id3, _ := s.AuthenticateAPIToken(ctx, tok.Plaintext); !id3.TouchDue {
		t.Fatal("touch not due after the interval")
	}
	// RSS side shares the behaviour.
	rt, _ := s.CreateRSSToken(ctx, u.ID, "", nil)
	rid, _ := s.AuthenticateRSSToken(ctx, rt.Plaintext)
	if !rid.TouchDue {
		t.Fatal("RSS first use not due")
	}
	if err := s.TouchRSSToken(ctx, rid.TokenID); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerScopedInvitations(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	a := mkUser(t, s, pool, "ia")
	b := mkUser(t, s, pool, "ib")
	codesA, _ := s.CreateInvites(ctx, "ia", 2, nil)
	codesB, _ := s.CreateInvites(ctx, "ib", 1, nil)

	la, err := s.ListInvitesByOwner(ctx, a.ID)
	if err != nil || len(la) != 2 {
		t.Fatalf("list = %+v, %v", la, err)
	}
	for _, v := range la {
		if v.Code == codesB[0] {
			t.Fatal("another owner's code is listed")
		}
	}
	lb, _ := s.ListInvitesByOwner(ctx, b.ID)
	if len(lb) != 1 || lb[0].Code != codesB[0] {
		t.Fatalf("b list = %+v", lb)
	}

	_, err = s.RevokeOwnInvite(ctx, a.ID, lb[0].ID)
	mustErr(t, err, ErrNotFound, "revoke someone else's invite")
	_, err = s.RevokeOwnInvite(ctx, a.ID, 99999)
	mustErr(t, err, ErrNotFound, "unknown invite")
	if ok, _ := s.CheckInvite(ctx, codesB[0]); !ok {
		t.Fatal("foreign revoke took effect")
	}
	if out, err := s.RevokeOwnInvite(ctx, a.ID, la[0].ID); err != nil || out != InviteRevoked {
		t.Fatalf("own revoke = %v, %v", out, err)
	}
	if out, _ := s.RevokeOwnInvite(ctx, a.ID, la[0].ID); out != InviteAlreadyRevoked {
		t.Fatalf("second revoke = %v", out)
	}
	// A used invitation cannot be revoked and reports who used it.
	if _, err := s.RegisterWithInvite(ctx, RegisterParams{Code: codesB[0], Username: "newbie", Hash: testHash(t)}); err != nil {
		t.Fatal(err)
	}
	if out, _ := s.RevokeOwnInvite(ctx, b.ID, lb[0].ID); out != InviteAlreadyUsed {
		t.Fatalf("revoke used = %v", out)
	}
	if lb, _ = s.ListInvitesByOwner(ctx, b.ID); lb[0].UsedBy != "newbie" {
		t.Fatalf("used_by = %q", lb[0].UsedBy)
	}
	_ = codesA
}

func TestCheckInviteStates(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	o := mkUser(t, s, pool, "owner")
	codes, _ := s.CreateInvites(ctx, "owner", 5, nil)
	if ok, _ := s.CheckInvite(ctx, codes[0]); !ok {
		t.Fatal("fresh invite unusable")
	}
	exec(t, pool, `UPDATE magnetico_web.invites SET revoked_at = now() WHERE code = $1`, codes[1])
	exec(t, pool, `UPDATE magnetico_web.invites SET expires_at = now() - interval '1 second' WHERE code = $1`, codes[2])
	if _, err := s.RegisterWithInvite(ctx, RegisterParams{Code: codes[3], Username: "usedby", Hash: testHash(t)}); err != nil {
		t.Fatal(err)
	}
	for i, c := range codes[1:4] {
		if ok, err := s.CheckInvite(ctx, c); ok || err != nil {
			t.Errorf("state %d usable: %v %v", i, ok, err)
		}
	}
	if ok, _ := s.CheckInvite(ctx, "does-not-exist-0000000"); ok {
		t.Error("unknown code usable")
	}
	if ok, _ := s.CheckInvite(ctx, ""); ok {
		t.Error("empty code usable")
	}
	if _, err := s.Disable(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.CheckInvite(ctx, codes[0]); ok {
		t.Error("suspended owner's invite usable")
	}
	if err := s.Enable(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.CheckInvite(ctx, codes[0]); !ok {
		t.Error("enable did not restore the invite")
	}
	_ = o
}

func TestEmailVerificationFlow(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "eve")
	pw := u.PasswordHash

	l1, err := s.IssueEmailVerifyLink(ctx, u.ID, "eve@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.GetUserByID(ctx, u.ID); cur.Email != "" || cur.EmailVerified {
		t.Fatalf("issuing wrote the user: %+v", cur)
	}
	if e, ok, _ := s.PendingEmail(ctx, u.ID); !ok || e != "eve@example.org" {
		t.Fatalf("pending = %q %v", e, ok)
	}
	res, err := s.CompleteEmailVerify(ctx, l1.Token)
	if err != nil || res.Email != "eve@example.org" || res.PreviousEmail != "" {
		t.Fatalf("complete = %+v, %v", res, err)
	}
	mustErr(t, errOf(s.CompleteEmailVerify(ctx, l1.Token)), ErrLinkInvalid, "reuse")
	cur, _ := s.GetUserByID(ctx, u.ID)
	if cur.Email != "eve@example.org" || !cur.EmailVerified || cur.PasswordHash != pw || !cur.Active {
		t.Fatalf("after verify: %+v", cur)
	}

	// A change: the old verified address stays until the new one is confirmed.
	l2, err := s.IssueEmailVerifyLink(ctx, u.ID, "eve2@example.org")
	if err != nil {
		t.Fatal(err)
	}
	if cur, _ = s.GetUserByID(ctx, u.ID); cur.Email != "eve@example.org" || !cur.EmailVerified {
		t.Fatalf("old address lost before confirmation: %+v", cur)
	}
	if got, _ := s.IssueRecoveryLink(ctx, "eve@example.org"); got.Token == "" {
		t.Fatal("old verified address stopped working for recovery")
	}
	if _, err := s.IssueRecoveryLink(ctx, "eve2@example.org"); !errors.Is(err, ErrNotFound) {
		t.Fatal("pending address is usable for recovery")
	}
	// A second request replaces the first link.
	l3, _ := s.IssueEmailVerifyLink(ctx, u.ID, "eve3@example.org")
	mustErr(t, errOf(s.CompleteEmailVerify(ctx, l2.Token)), ErrLinkInvalid, "replaced link")
	res, err = s.CompleteEmailVerify(ctx, l3.Token)
	if err != nil || res.PreviousEmail != "eve@example.org" || res.Email != "eve3@example.org" {
		t.Fatalf("change = %+v, %v", res, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM magnetico_web.one_time_tokens WHERE purpose = 'email_verify'`); n != 0 {
		t.Fatalf("%d email links left", n)
	}
	if _, err := s.IssueEmailVerifyLink(ctx, u.ID, "not an address"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid address: %v", err)
	}
}

func altHash(t *testing.T) string {
	t.Helper()
	svc := password.NewService(password.NewLimiter(1, time.Minute), password.Params{Memory: 64, Time: 1, Threads: 1})
	h, err := svc.Hash(context.Background(), "another password 2")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func errOf[T any](_ T, err error) error { return err }

func TestEmailLinkNotConsumedForInactiveOrExpired(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "ivy")
	l, _ := s.IssueEmailVerifyLink(ctx, u.ID, "ivy@example.org")
	if _, err := s.Disable(ctx, "ivy"); err != nil {
		t.Fatal(err)
	}
	mustErr(t, errOf(s.CompleteEmailVerify(ctx, l.Token)), ErrLinkInvalid, "disabled owner")
	if _, err := s.IssueEmailVerifyLink(ctx, u.ID, "other@example.org"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("issuing for a disabled account: %v", err)
	}
	if n := count(t, pool, `SELECT count(*) FROM magnetico_web.one_time_tokens WHERE purpose = 'email_verify'`); n != 1 {
		t.Fatal("a disabled owner's link was consumed")
	}
	if err := s.Enable(ctx, "ivy"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteEmailVerify(ctx, l.Token); err != nil {
		t.Fatalf("link must work again after enable: %v", err)
	}
	l2, _ := s.IssueEmailVerifyLink(ctx, u.ID, "ivy2@example.org")
	exec(t, pool, `UPDATE magnetico_web.one_time_tokens SET created_at = now() - interval '2 days', expires_at = now() - interval '1 second' WHERE purpose = 'email_verify'`)
	mustErr(t, errOf(s.CompleteEmailVerify(ctx, l2.Token)), ErrLinkInvalid, "expired")
	for _, bad := range []string{"", strings.Repeat("a", 300), "nope"} {
		mustErr(t, errOf(s.CompleteEmailVerify(ctx, bad)), ErrLinkInvalid, "bad token")
	}
}

func TestEmailVerifyUniquenessConflictRollsBackAndKeepsLink(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	holder := mkUser(t, s, pool, "holder")
	lh, _ := s.IssueEmailVerifyLink(ctx, holder.ID, "shared@example.org")
	if _, err := s.CompleteEmailVerify(ctx, lh.Token); err != nil {
		t.Fatal(err)
	}
	other := mkUser(t, s, pool, "other")
	exec(t, pool, `UPDATE magnetico_web.users SET email = 'old@example.org', email_verified_at = now() WHERE id = $1`, other.ID)
	lo, _ := s.IssueEmailVerifyLink(ctx, other.ID, "SHARED@example.org")
	before := tableDump(t, pool, other.ID)
	for i := 0; i < 2; i++ {
		mustErr(t, errOf(s.CompleteEmailVerify(ctx, lo.Token)), ErrEmailUnavailable, "duplicate verified address")
		if after := tableDump(t, pool, other.ID); after != before {
			t.Fatalf("attempt %d changed state:\n%s\nvs\n%s", i, before, after)
		}
	}
	// The link is still usable once the other account releases the address.
	exec(t, pool, `UPDATE magnetico_web.users SET email = NULL, email_verified_at = NULL WHERE id = $1`, holder.ID)
	if _, err := s.CompleteEmailVerify(ctx, lo.Token); err != nil {
		t.Fatalf("link must still work after the conflict ends: %v", err)
	}
}

func TestConcurrentVerificationOfTheSameAddress(t *testing.T) {
	for round := 0; round < 12; round++ {
		s, pool := newStore(t)
		ctx := tctx(t)
		a := mkUser(t, s, pool, "ca")
		b := mkUser(t, s, pool, "cb")
		la, _ := s.IssueEmailVerifyLink(ctx, a.ID, "race@example.org")
		lb, _ := s.IssueEmailVerifyLink(ctx, b.ID, "RACE@example.org")

		// Overlap the two transactions: each waits (briefly) before COMMIT for
		// the other to have done its UPDATE. The second UPDATE blocks on the
		// first's uncommitted index entry, so the wait is bounded by a timeout.
		var mu sync.Mutex
		arrived := 0
		cond := make(chan struct{})
		s.beforeCommit = func(op string) error {
			if op != "complete-email-verify" {
				return nil
			}
			mu.Lock()
			arrived++
			if arrived == 2 {
				close(cond)
			}
			mu.Unlock()
			select {
			case <-cond:
			case <-time.After(150 * time.Millisecond):
			}
			return nil
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, l := range []Link{la, lb} {
			wg.Add(1)
			go func(i int, l Link) {
				defer wg.Done()
				_, errs[i] = s.CompleteEmailVerify(ctx, l.Token)
			}(i, l)
		}
		wg.Wait()
		s.beforeCommit = nil

		wins, losses := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrEmailUnavailable):
				losses++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if wins != 1 || losses != 1 {
			t.Fatalf("round %d: wins=%d losses=%d (%v)", round, wins, losses, errs)
		}
		if n := count(t, pool, `SELECT count(*) FROM magnetico_web.users WHERE lower(email) = 'race@example.org' AND email_verified_at IS NOT NULL`); n != 1 {
			t.Fatalf("round %d: %d accounts hold the address", round, n)
		}
		// The loser's link was not consumed and fails the same way again.
		if n := count(t, pool, `SELECT count(*) FROM magnetico_web.one_time_tokens WHERE purpose = 'email_verify'`); n != 1 {
			t.Fatalf("round %d: %d links left, want the loser's only", round, n)
		}
		loser := la
		if errs[0] == nil {
			loser = lb
		}
		mustErr(t, errOf(s.CompleteEmailVerify(ctx, loser.Token)), ErrEmailUnavailable, "loser retry")
	}
}

func TestRecoveryLinkEligibility(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "rec")
	// Unverified address: not eligible.
	exec(t, pool, `UPDATE magnetico_web.users SET email = 'rec@example.org' WHERE id = $1`, u.ID)
	if _, err := s.IssueRecoveryLink(ctx, "rec@example.org"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unverified: %v", err)
	}
	if _, err := s.IssueRecoveryLink(ctx, "nobody@example.org"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := s.IssueRecoveryLink(ctx, "not an address"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed: %v", err)
	}
	exec(t, pool, `UPDATE magnetico_web.users SET email_verified_at = now() WHERE id = $1`, u.ID)
	l, err := s.IssueRecoveryLink(ctx, "REC@example.org")
	if err != nil || l.Purpose != PurposeReset || l.Token == "" {
		t.Fatalf("verified: %+v, %v", l, err)
	}
	if l.Email != "rec@example.org" {
		t.Fatalf("link email = %q", l.Email)
	}
	// A different spelling locates the account but the result carries the
	// stored verified address.
	exec(t, pool, `UPDATE magnetico_web.users SET email = 'Rec.Mixed@Example.ORG' WHERE id = $1`, u.ID)
	for _, in := range []string{"rec.mixed@example.org", "REC.MIXED@EXAMPLE.ORG"} {
		ml, err := s.IssueRecoveryLink(ctx, in)
		if err != nil || ml.Email != "Rec.Mixed@Example.ORG" {
			t.Fatalf("submitted %q: email %q, %v; want the stored spelling", in, ml.Email, err)
		}
	}
	exec(t, pool, `UPDATE magnetico_web.users SET email = 'rec@example.org' WHERE id = $1`, u.ID)
	if l, err = s.IssueRecoveryLink(ctx, "rec@example.org"); err != nil {
		t.Fatal(err)
	}
	// It works as a reset link and revokes everything.
	api, _ := s.CreateAPIToken(ctx, u.ID, "", nil)
	if _, err := s.CompletePasswordLink(ctx, l.Token, PurposeReset, testHash(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateAPIToken(ctx, api.Plaintext); err == nil {
		t.Fatal("reset by email left an API token valid")
	}
	// No password yet: setup link.
	exec(t, pool, `UPDATE magnetico_web.users SET password_hash = NULL WHERE id = $1`, u.ID)
	if l, err = s.IssueRecoveryLink(ctx, "rec@example.org"); err != nil || l.Purpose != PurposeSetup {
		t.Fatalf("setup: %+v, %v", l, err)
	}
	// Disabled: not eligible, nothing changes.
	if _, err := s.Disable(ctx, "rec"); err != nil {
		t.Fatal(err)
	}
	before := tableDump(t, pool, u.ID)
	if _, err := s.IssueRecoveryLink(ctx, "rec@example.org"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled: %v", err)
	}
	if tableDump(t, pool, u.ID) != before {
		t.Fatal("a refused recovery wrote something")
	}
}

func TestDisableEnableLeavesTokenRowsByteIdentical(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "dis")
	api, _ := s.CreateAPIToken(ctx, u.ID, "a", nil)
	rss, _ := s.CreateRSSToken(ctx, u.ID, "r", nil)
	const legacy = "ffeeddccbbaa99887766554433221100"
	exec(t, pool, `INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy) VALUES ($1,$2,true)`, hashToken(legacy), u.ID)
	revoked, _ := s.CreateAPIToken(ctx, u.ID, "gone", nil)
	if err := s.RevokeAPIToken(ctx, u.ID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	before := tableDump(t, pool, u.ID)
	if _, err := s.Disable(ctx, "dis"); err != nil {
		t.Fatal(err)
	}
	for name, f := range map[string]func() error{
		"api":    func() error { _, e := s.AuthenticateAPIToken(ctx, api.Plaintext); return e },
		"rss":    func() error { _, e := s.AuthenticateRSSToken(ctx, rss.Plaintext); return e },
		"legacy": func() error { _, e := s.AuthenticateRSSToken(ctx, legacy); return e },
	} {
		if !errors.Is(f(), ErrUnauthenticated) {
			t.Errorf("%s works while disabled", name)
		}
	}
	if tableDump(t, pool, u.ID) != before {
		t.Fatal("disable touched token, invitation, link or user rows")
	}
	if err := s.Enable(ctx, "dis"); err != nil {
		t.Fatal(err)
	}
	if tableDump(t, pool, u.ID) != before {
		t.Fatal("enable touched token rows")
	}
	if _, err := s.AuthenticateAPIToken(ctx, api.Plaintext); err != nil {
		t.Errorf("api after enable: %v", err)
	}
	if _, err := s.AuthenticateRSSToken(ctx, legacy); err != nil {
		t.Errorf("legacy after enable: %v", err)
	}
	if _, err := s.AuthenticateAPIToken(ctx, revoked.Plaintext); err == nil {
		t.Error("explicitly revoked token revived by enable")
	}
}
