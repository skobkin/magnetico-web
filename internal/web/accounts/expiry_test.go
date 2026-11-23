package accounts

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// holdUserLock takes the user row FOR UPDATE in a separate transaction and
// returns the function that releases it. The release is idempotent and is also
// registered as a cleanup, so a test that fails (or is aborted by Fatal) while
// holding the lock can never leave the transaction open: a blocked operation
// would otherwise keep the pool busy and deadlock pool.Close at cleanup.
func holdUserLock(t *testing.T, pool *pgxpool.Pool, userID int64) (release func()) {
	t.Helper()
	tx, err := pool.Begin(tctx(t))
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { tx.Rollback(context.Background()) }) }
	t.Cleanup(release)
	if _, err := tx.Exec(tctx(t), `SELECT 1 FROM magnetico_web.users WHERE id = $1 FOR UPDATE`, userID); err != nil {
		t.Fatal(err)
	}
	return release
}

// blockWait bounds every wait for an expected lock wait.
const blockWait = 20 * time.Second

// waitBlocked waits until some session of this database waits on a lock; it
// fails the test after blockWait.
func waitBlocked(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	waitBlockedOr(t, pool, nil)
}

// waitBlockedOr is waitBlocked that also fails promptly when done is closed
// first: the operation that was expected to block has already finished, so the
// lock wait did not occur and waiting longer cannot help.
func waitBlockedOr(t *testing.T, pool *pgxpool.Pool, done <-chan struct{}) {
	t.Helper()
	for deadline := time.Now().Add(blockWait); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		select {
		case <-done:
			t.Fatal("the operation finished without ever blocking on the lock")
		default:
		}
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
// The expiry has to come from the clock (editing the row afterwards would not
// reproduce the bug: now() would already be later than the edited value). The
// window starts short and doubles whenever a slow machine lets the link expire
// before the operation blocked, so a loaded CI retries instead of failing.
func TestLinkExpiringDuringLockWaitIsRefused(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "lena")
	s.CreateAPIToken(ctx, u.ID, "", nil)

	for window := 2 * time.Second; window <= 32*time.Second; window *= 2 {
		link, err := s.IssuePasswordLink(ctx, "lena")
		if err != nil {
			t.Fatal(err)
		}
		exec(t, pool, `UPDATE magnetico_web.one_time_tokens SET expires_at = clock_timestamp() + make_interval(secs => $2) WHERE user_id = $1`, u.ID, window.Seconds())
		before := tableDump(t, pool, u.ID)

		release := holdUserLock(t, pool, u.ID)
		res := make(chan error, 1)
		go func() {
			_, err := s.CompletePasswordLink(ctx, link.Token, PurposeReset, testHash(t))
			res <- err
		}()
		waitBlocked(t, pool)
		if count(t, pool, `SELECT count(*) FROM magnetico_web.one_time_tokens WHERE user_id = $1 AND expires_at > clock_timestamp()`, u.ID) != 1 {
			// Too slow for this window: widen it and try again.
			release()
			<-res
			continue
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
		return
	}
	t.Fatal("the machine was too slow even for a 32 s window")
}

// An invitation that expired while the registration waited for the owner's
// row lock must be refused, not consumed, and create no user. The registration
// holds the invitation row lock while it waits, so the invitation cannot be
// edited meanwhile; the expiry must come from the clock. The window starts
// short and doubles whenever a slow machine lets the invitation expire before
// the registration blocked, so a loaded CI retries instead of failing.
func TestInvitationExpiringDuringLockWaitIsRefused(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	owner := mkUser(t, s, pool, "omar")
	usersBefore := count(t, pool, `SELECT count(*) FROM magnetico_web.users`)

	for window := 2 * time.Second; window <= 32*time.Second; window *= 2 {
		codes, err := s.CreateInvites(ctx, "omar", 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		code := codes[0]
		exec(t, pool, `UPDATE magnetico_web.invites SET expires_at = clock_timestamp() + make_interval(secs => $2) WHERE code = $1`, code, window.Seconds())
		invitesBefore := count(t, pool, `SELECT count(*) FROM magnetico_web.invites`)

		release := holdUserLock(t, pool, owner.ID)
		res := make(chan error, 1)
		go func() {
			_, err := s.RegisterWithInvite(ctx, RegisterParams{Code: code, Username: "latecomer", Hash: testHash(t), Quota: 2})
			res <- err
		}()
		waitBlocked(t, pool)
		if count(t, pool, `SELECT count(*) FROM magnetico_web.invites WHERE code = $1 AND expires_at > clock_timestamp()`, code) != 1 {
			// Too slow for this window: the registration may have started
			// after expiry. Discard this attempt and widen the window.
			release()
			<-res
			exec(t, pool, `DELETE FROM magnetico_web.invites WHERE code = $1`, code)
			continue
		}
		sleepPast(t, pool, "the invitation", "invites", `code = $1`, code)
		release()

		if err := <-res; !errors.Is(err, ErrInvitationUnavailable) {
			t.Fatalf("expired-while-waiting invitation: err = %v, want ErrInvitationUnavailable", err)
		}
		if count(t, pool, `SELECT count(*) FROM magnetico_web.invites WHERE code = $1 AND used_by IS NULL AND used_at IS NULL`, code) != 1 {
			t.Fatal("the invitation was consumed")
		}
		if count(t, pool, `SELECT count(*) FROM magnetico_web.users`) != usersBefore ||
			count(t, pool, `SELECT count(*) FROM magnetico_web.invites`) != invitesBefore {
			t.Fatal("a refused registration left rows behind")
		}
		return
	}
	t.Fatal("the machine was too slow even for a 32 s window")
}
