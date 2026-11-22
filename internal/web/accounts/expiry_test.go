package accounts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// holdUserLock takes the user row FOR UPDATE in a separate transaction and
// returns the function that releases it.
func holdUserLock(t *testing.T, pool *pgxpool.Pool, userID int64) (release func()) {
	t.Helper()
	tx, err := pool.Begin(tctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(tctx(t), `SELECT 1 FROM magnetico_web.users WHERE id = $1 FOR UPDATE`, userID); err != nil {
		t.Fatal(err)
	}
	var done bool
	return func() {
		if !done {
			done = true
			tx.Rollback(context.Background())
		}
	}
}

// waitBlocked waits until some session of this database waits on a lock.
func waitBlocked(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if count(t, pool, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) > 0 {
			return
		}
	}
	t.Fatal("the operation never blocked on the lock")
}

// sleepPast blocks until the database clock is past ts.
func sleepPast(t *testing.T, pool *pgxpool.Pool, ts string, table string, where string, args ...any) {
	t.Helper()
	exec(t, pool, `SELECT pg_sleep(GREATEST(0, EXTRACT(EPOCH FROM (max(expires_at) - clock_timestamp())) + 0.3)) FROM magnetico_web.`+table+` WHERE `+where, args...)
	if count(t, pool, `SELECT count(*) FROM magnetico_web.`+table+` WHERE `+where+` AND expires_at > clock_timestamp()`, args...) != 0 {
		t.Fatalf("%s is still unexpired", ts)
	}
}

// A link that was valid when the completing transaction started, but expired
// while it waited for the user row lock, must be refused and not consumed.
func TestLinkExpiringDuringLockWaitIsRefused(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "lena")
	s.CreateAPIToken(ctx, u.ID, "", nil)
	link, err := s.IssuePasswordLink(ctx, "lena")
	if err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `UPDATE magnetico_web.one_time_tokens SET expires_at = clock_timestamp() + interval '2 seconds' WHERE user_id = $1`, u.ID)
	before := tableDump(t, pool, u.ID)

	release := holdUserLock(t, pool, u.ID)
	defer release()
	res := make(chan error, 1)
	go func() {
		_, err := s.CompletePasswordLink(ctx, link.Token, PurposeReset, testHash(t))
		res <- err
	}()
	waitBlocked(t, pool)
	if count(t, pool, `SELECT count(*) FROM magnetico_web.one_time_tokens WHERE user_id = $1 AND expires_at > clock_timestamp()`, u.ID) != 1 {
		t.Fatal("test too slow: the link expired before the transaction was blocked")
	}
	sleepPast(t, pool, "the link", "one_time_tokens", `user_id = $1`, u.ID)
	release()

	if err := <-res; !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("expired-while-waiting link: err = %v, want ErrLinkInvalid", err)
	}
	if got := tableDump(t, pool, u.ID); got != before {
		t.Fatalf("the refused completion changed state:\n--- before\n%s--- after\n%s", before, got)
	}
	if count(t, pool, `SELECT count(*) FROM magnetico_web.api_tokens WHERE user_id = $1 AND revoked_at IS NULL`, u.ID) != 1 {
		t.Fatal("tokens were revoked by a refused completion")
	}
	if linkCount(t, s, u.ID, `'setup','reset'`) != 1 {
		t.Fatal("the link was consumed")
	}
}

// An invitation that expired while the registration waited for the owner's
// row lock must be refused, not consumed, and create no user.
func TestInvitationExpiringDuringLockWaitIsRefused(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	owner := mkUser(t, s, pool, "omar")
	codes, err := s.CreateInvites(ctx, "omar", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `UPDATE magnetico_web.invites SET expires_at = clock_timestamp() + interval '2 seconds' WHERE code = $1`, codes[0])
	usersBefore := count(t, pool, `SELECT count(*) FROM magnetico_web.users`)
	invitesBefore := count(t, pool, `SELECT count(*) FROM magnetico_web.invites`)

	release := holdUserLock(t, pool, owner.ID)
	defer release()
	res := make(chan error, 1)
	go func() {
		_, err := s.RegisterWithInvite(ctx, RegisterParams{Code: codes[0], Username: "latecomer", Hash: testHash(t), Quota: 2})
		res <- err
	}()
	waitBlocked(t, pool)
	if count(t, pool, `SELECT count(*) FROM magnetico_web.invites WHERE code = $1 AND expires_at > clock_timestamp()`, codes[0]) != 1 {
		t.Fatal("test too slow: the invitation expired before the transaction was blocked")
	}
	sleepPast(t, pool, "the invitation", "invites", `code = $1`, codes[0])
	release()

	if err := <-res; !errors.Is(err, ErrInvitationUnavailable) {
		t.Fatalf("expired-while-waiting invitation: err = %v, want ErrInvitationUnavailable", err)
	}
	if count(t, pool, `SELECT count(*) FROM magnetico_web.invites WHERE code = $1 AND used_by IS NULL AND used_at IS NULL`, codes[0]) != 1 {
		t.Fatal("the invitation was consumed")
	}
	if count(t, pool, `SELECT count(*) FROM magnetico_web.users`) != usersBefore ||
		count(t, pool, `SELECT count(*) FROM magnetico_web.invites`) != invitesBefore {
		t.Fatal("a refused registration left rows behind")
	}
}
