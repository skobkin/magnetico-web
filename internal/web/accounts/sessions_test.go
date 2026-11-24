package accounts

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var lt = SessionLifetimes{Idle: 12 * time.Hour, Absolute: 168 * time.Hour, Remember: 2160 * time.Hour}

// login commits a login of u (whose hash is testHash) and returns the result.
func login(t *testing.T, s *Store, u User, remember bool) LoginResult {
	t.Helper()
	res, err := s.CompleteLogin(tctx(t), LoginParams{UserID: u.ID, VerifiedHash: testHash(t), Remember: remember, Lifetimes: lt})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func hashOf(t *testing.T, token string) []byte {
	t.Helper()
	h, ok := SessionTokenHash(token)
	if !ok {
		t.Fatalf("malformed token %q", token)
	}
	return h
}

// backdate moves a session's three timestamps into the past together, which
// keeps expires_at > created_at, so expiry tests need no sleeping.
func backdate(t *testing.T, pool *pgxpool.Pool, token string, d time.Duration) {
	t.Helper()
	exec(t, pool, `UPDATE magnetico_web.sessions SET created_at = created_at - make_interval(secs => $2),
        last_seen_at = last_seen_at - make_interval(secs => $2), expires_at = expires_at - make_interval(secs => $2)
        WHERE token_hash = $1`, hashOf(t, token), d.Seconds())
}

func idleBackdate(t *testing.T, pool *pgxpool.Pool, token string, d time.Duration) {
	t.Helper()
	exec(t, pool, `UPDATE magnetico_web.sessions SET last_seen_at = last_seen_at - make_interval(secs => $2) WHERE token_hash = $1`,
		hashOf(t, token), d.Seconds())
}

func TestSessionTokenHashIsStrict(t *testing.T) {
	tok, hash, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != SessionTokenLen || len(hash) != 32 {
		t.Fatalf("token len %d, hash len %d", len(tok), len(hash))
	}
	if h, ok := SessionTokenHash(tok); !ok || string(h) != string(hash) {
		t.Fatal("round trip failed")
	}
	// The last base64 character carries only 4 significant bits. Flip a bit
	// that lies in the padding: the decoded bytes stay the same, so a lax
	// decoder would accept a second spelling of the same token.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	idx := strings.IndexByte(alphabet, tok[len(tok)-1])
	if idx < 0 {
		t.Fatal("token not in the base64url alphabet")
	}
	if _, ok := SessionTokenHash(tok[:len(tok)-1] + string(alphabet[idx^1])); ok {
		t.Error("non-canonical encoding (non-zero padding bits) accepted")
	}
	for _, bad := range []string{"", "short", tok + "A", tok[:42], tok[:42] + "=", tok[:42] + "+", tok[:42] + "/", tok[:42] + "\x00", strings.Repeat("A", 44)} {
		if _, ok := SessionTokenHash(bad); ok {
			t.Errorf("malformed token %q accepted", bad)
		}
	}
}

func TestSessionCreateStoresOnlyTheHashAndFiniteExpiry(t *testing.T) {
	s, pool := newStore(t)
	u := mkUser(t, s, pool, "sam")
	for _, remember := range []bool{false, true} {
		res := login(t, s, u, remember)
		want := lt.Absolute
		if remember {
			want = lt.Remember
		}
		var secs float64
		var nulls int
		err := pool.QueryRow(tctx(t), `SELECT EXTRACT(EPOCH FROM expires_at - created_at), (expires_at IS NULL)::int
            FROM magnetico_web.sessions WHERE token_hash = $1`, hashOf(t, res.Token)).Scan(&secs, &nulls)
		if err != nil {
			t.Fatal(err)
		}
		if secs != want.Seconds() || nulls != 0 {
			t.Errorf("remember=%v: lifetime %vs (want %vs), null=%d", remember, secs, want.Seconds(), nulls)
		}
		if !res.ExpiresAt.After(res.Now) {
			t.Errorf("remember=%v: ExpiresAt %v not after Now %v", remember, res.ExpiresAt, res.Now)
		}
		if res.Remember != remember {
			t.Errorf("remember flag not reported")
		}
		// The raw token is stored nowhere.
		if n := count(t, pool, `SELECT count(*) FROM magnetico_web.sessions s WHERE s::text LIKE '%'||$1||'%'`, res.Token); n != 0 {
			t.Error("raw session token appears in a stored row")
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM magnetico_web.sessions WHERE expires_at IS NULL`); n != 0 {
		t.Error("a session without absolute expiry exists")
	}
}

func TestLookupLifecycle(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "tess")

	if _, err := s.LookupSession(ctx, make([]byte, 32), lt.Idle); err != ErrUnauthenticated {
		t.Fatalf("unknown hash: %v", err)
	}
	if _, err := s.LookupSession(ctx, []byte("short"), lt.Idle); err != ErrUnauthenticated {
		t.Fatalf("short hash: %v", err)
	}

	ord := login(t, s, u, false)
	got, err := s.LookupSession(ctx, hashOf(t, ord.Token), lt.Idle)
	if err != nil || got.UserID != u.ID || got.Username != "tess" || got.Role != "user" || got.Remember || got.TouchDue {
		t.Fatalf("lookup: %+v %v", got, err)
	}

	// Ordinary: idle expiry.
	idleBackdate(t, pool, ord.Token, 13*time.Hour)
	if _, err := s.LookupSession(ctx, hashOf(t, ord.Token), lt.Idle); err != ErrUnauthenticated {
		t.Fatalf("idle-expired ordinary session resolved: %v", err)
	}

	// Ordinary: absolute expiry even though recently active.
	abs := login(t, s, u, false)
	exec(t, pool, `UPDATE magnetico_web.sessions SET created_at = created_at - interval '169 hours', expires_at = expires_at - interval '169 hours' WHERE token_hash = $1`, hashOf(t, abs.Token))
	if _, err := s.LookupSession(ctx, hashOf(t, abs.Token), lt.Idle); err != ErrUnauthenticated {
		t.Fatalf("absolute-expired session resolved: %v", err)
	}

	// Remember: not idle-expired, but absolute-expired.
	rem := login(t, s, u, true)
	idleBackdate(t, pool, rem.Token, 200*time.Hour)
	if _, err := s.LookupSession(ctx, hashOf(t, rem.Token), lt.Idle); err != nil {
		t.Fatalf("remember session wrongly idle-expired: %v", err)
	}
	backdate(t, pool, rem.Token, 2161*time.Hour)
	if _, err := s.LookupSession(ctx, hashOf(t, rem.Token), lt.Idle); err != ErrUnauthenticated {
		t.Fatalf("absolute-expired remember session resolved: %v", err)
	}
}

func TestLookupReadsActiveAndRoleLive(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "uma")
	res := login(t, s, u, false)
	h := hashOf(t, res.Token)

	if err := s.SetRole(ctx, "uma", "admin"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LookupSession(ctx, h, lt.Idle); err != nil || got.Role != "admin" {
		t.Fatalf("role not read live: %+v %v", got, err)
	}

	if n, err := s.Disable(ctx, "uma"); err != nil || n != 1 {
		t.Fatalf("disable: %d %v", n, err)
	}
	if _, err := s.LookupSession(ctx, h, lt.Idle); err != ErrUnauthenticated {
		t.Fatalf("session of a disabled user resolved: %v", err)
	}
	if err := s.Enable(ctx, "uma"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupSession(ctx, h, lt.Idle); err != ErrUnauthenticated {
		t.Fatalf("enable restored a deleted session: %v", err)
	}

	// Even a session row that survived (inserted behind the store's back)
	// does not authenticate while the owner is inactive.
	res2 := login(t, s, u, false)
	exec(t, pool, `UPDATE magnetico_web.users SET active = false WHERE id = $1`, u.ID)
	if _, err := s.LookupSession(ctx, hashOf(t, res2.Token), lt.Idle); err != ErrUnauthenticated {
		t.Fatalf("surviving row of an inactive owner resolved: %v", err)
	}
}

func lastSeen(t *testing.T, pool *pgxpool.Pool, token string) time.Time {
	t.Helper()
	var ts time.Time
	if err := pool.QueryRow(tctx(t), `SELECT last_seen_at FROM magnetico_web.sessions WHERE token_hash = $1`, hashOf(t, token)).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestTouchIsThrottledAndConditional(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "vic")
	if Throttle(12*time.Hour) != 5*time.Minute || Throttle(20*time.Minute) != 2*time.Minute {
		t.Fatalf("throttle = min(5m, idle/10): %v %v", Throttle(12*time.Hour), Throttle(20*time.Minute))
	}

	res := login(t, s, u, false)
	h := hashOf(t, res.Token)
	if ok, err := s.TouchSession(ctx, h, lt.Idle); err != nil || ok {
		t.Fatalf("touch inside the throttle wrote: %v %v", ok, err)
	}
	idleBackdate(t, pool, res.Token, 6*time.Minute)
	sess, _ := s.LookupSession(ctx, h, lt.Idle)
	if !sess.TouchDue {
		t.Fatal("touch not due after the throttle interval")
	}
	before := lastSeen(t, pool, res.Token)
	if ok, err := s.TouchSession(ctx, h, lt.Idle); err != nil || !ok {
		t.Fatalf("due touch: %v %v", ok, err)
	}
	if !lastSeen(t, pool, res.Token).After(before) {
		t.Fatal("last_seen_at not advanced")
	}
	if ok, _ := s.TouchSession(ctx, h, lt.Idle); ok {
		t.Fatal("second touch inside the throttle wrote")
	}

	// A touch never revives an idle-expired session...
	idleBackdate(t, pool, res.Token, 13*time.Hour)
	stale := lastSeen(t, pool, res.Token)
	if ok, _ := s.TouchSession(ctx, h, lt.Idle); ok || !lastSeen(t, pool, res.Token).Equal(stale) {
		t.Fatal("touch revived an idle-expired session")
	}
	// ...nor an absolute-expired one.
	exp := login(t, s, u, false)
	backdate(t, pool, exp.Token, 169*time.Hour)
	if ok, _ := s.TouchSession(ctx, hashOf(t, exp.Token), lt.Idle); ok {
		t.Fatal("touch revived an absolute-expired session")
	}

	// Remember: last_seen_at moves, expires_at never does.
	rem := login(t, s, u, true)
	idleBackdate(t, pool, rem.Token, time.Hour)
	var e1, e2 time.Time
	if err := pool.QueryRow(ctx, `SELECT expires_at FROM magnetico_web.sessions WHERE token_hash = $1`, hashOf(t, rem.Token)).Scan(&e1); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.TouchSession(ctx, hashOf(t, rem.Token), lt.Idle); err != nil || !ok {
		t.Fatalf("remember touch: %v %v", ok, err)
	}
	if err := pool.QueryRow(ctx, `SELECT expires_at FROM magnetico_web.sessions WHERE token_hash = $1`, hashOf(t, rem.Token)).Scan(&e2); err != nil {
		t.Fatal(err)
	}
	if !e1.Equal(e2) {
		t.Fatal("activity extended a remember session")
	}
}

func TestConcurrentTouchesWriteOnce(t *testing.T) {
	s, pool := newStore(t)
	u := mkUser(t, s, pool, "wes")
	res := login(t, s, u, false)
	idleBackdate(t, pool, res.Token, 10*time.Minute)
	h := hashOf(t, res.Token)

	var wrote atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ok, err := s.TouchSession(tctx(t), h, lt.Idle); err != nil {
				t.Error(err)
			} else if ok {
				wrote.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wrote.Load() != 1 {
		t.Fatalf("%d concurrent touches wrote, want exactly 1", wrote.Load())
	}
}

func TestTouchAfterDeleteDoesNotResurrect(t *testing.T) {
	s, pool := newStore(t)
	u := mkUser(t, s, pool, "xan")
	res := login(t, s, u, false)
	h := hashOf(t, res.Token)
	idleBackdate(t, pool, res.Token, 10*time.Minute)
	if err := s.DeleteSession(tctx(t), h); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.TouchSession(tctx(t), h, lt.Idle); err != nil || ok {
		t.Fatalf("touch after delete: %v %v", ok, err)
	}
	if sessions(t, pool, u.ID) != 0 {
		t.Fatal("session resurrected")
	}
	// Logout replay is idempotent.
	if err := s.DeleteSession(tctx(t), h); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := s.LookupSession(tctx(t), h, lt.Idle); err != ErrUnauthenticated {
		t.Fatal("deleted session resolved")
	}
}

func TestPurgeSessions(t *testing.T) {
	s, pool := newStore(t)
	u := mkUser(t, s, pool, "yan")
	insert := func(n int, createdAgo, expiresIn, seenAgo string, remember bool) {
		exec(t, pool, fmt.Sprintf(`INSERT INTO magnetico_web.sessions (token_hash, user_id, created_at, last_seen_at, expires_at, remember)
            SELECT decode(md5($1::text || i::text) || md5($2::text || i::text), 'hex'), $3,
                   now() - interval '%s', now() - interval '%s', now() + interval '%s', $4
            FROM generate_series(1, $5) i`, createdAgo, seenAgo, expiresIn),
			fmt.Sprintf("%s%s%s%v", createdAgo, expiresIn, seenAgo, remember), "salt", u.ID, remember, n)
	}
	insert(1300, "200 hours", "-1 hour", "200 hours", false) // absolute-expired (> one batch)
	insert(1300, "100 hours", "1 hour", "13 hours", false)   // ordinary, idle-expired
	insert(7, "100 hours", "1 hour", "13 hours", true)       // remember, idle irrelevant: kept
	insert(11, "1 hour", "100 hours", "1 minute", false)     // valid: kept

	n, err := s.PurgeSessions(tctx(t), lt.Idle)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2600 {
		t.Fatalf("purged %d, want 2600", n)
	}
	if left := sessions(t, pool, u.ID); left != 18 {
		t.Fatalf("%d sessions left, want 18", left)
	}
	if n, _ := s.PurgeSessions(tctx(t), lt.Idle); n != 0 {
		t.Fatalf("second purge removed %d", n)
	}
}

func TestPurgeExpiredLinks(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	a, b, c := mkUser(t, s, pool, "ann"), mkUser(t, s, pool, "bea"), mkUser(t, s, pool, "cat")
	var live Link
	for i, u := range []User{a, b, c} {
		l, err := s.IssuePasswordLink(ctx, u.Username)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			live = l
		}
	}
	exec(t, pool, `UPDATE magnetico_web.one_time_tokens SET created_at = now() - interval '3 hours', expires_at = now() - interval '2 hours' WHERE user_id IN ($1, $2)`, a.ID, b.ID)

	n, err := s.PurgeExpiredLinks(ctx)
	if err != nil || n != 2 {
		t.Fatalf("purged %d (%v), want 2", n, err)
	}
	if count(t, pool, `SELECT count(*) FROM magnetico_web.one_time_tokens`) != 1 {
		t.Fatal("the live link was removed")
	}
	if _, err := s.CompletePasswordLink(ctx, live.Token, PurposeReset, testHash(t)); err != nil {
		t.Fatalf("live link no longer completes: %v", err)
	}
	// An expired link whose row is gone is still just an invalid link, and the
	// user can be issued a new one.
	if _, err := s.IssuePasswordLink(ctx, "ann"); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeExpiredLinksConcurrentWithIssuance(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "dov")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			exec(t, pool, `UPDATE magnetico_web.one_time_tokens SET created_at = now() - interval '3 hours', expires_at = now() - interval '2 hours' WHERE user_id = $1`, u.ID)
			if _, err := s.PurgeExpiredLinks(ctx); err != nil {
				t.Errorf("purge: %v", err)
				return
			}
		}
	}()
	for i := 0; i < 60; i++ {
		if _, err := s.IssuePasswordLink(ctx, "dov"); err != nil {
			t.Fatalf("issuance %d failed beside the purge: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	if n := count(t, pool, `SELECT count(*) FROM magnetico_web.one_time_tokens WHERE user_id = $1`, u.ID); n > 1 {
		t.Fatalf("%d outstanding links", n)
	}
}

func TestParallelLoginsGetDistinctSessions(t *testing.T) {
	s, pool := newStore(t)
	u := mkUser(t, s, pool, "eli")
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.CompleteLogin(tctx(t), LoginParams{UserID: u.ID, VerifiedHash: testHash(t), Lifetimes: lt})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			seen[res.Token] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 32 || sessions(t, pool, u.ID) != 32 {
		t.Fatalf("%d distinct tokens, %d rows, want 32/32", len(seen), sessions(t, pool, u.ID))
	}
}

func TestLookupVersusDisable(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	for round := 0; round < 25; round++ {
		name := fmt.Sprintf("flo%d", round)
		u := mkUser(t, s, pool, name)
		h := hashOf(t, login(t, s, u, false).Token)

		var disabled atomic.Bool
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for g := 0; g < 6; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					after := disabled.Load() // Disable had returned before this lookup started
					_, err := s.LookupSession(ctx, h, lt.Idle)
					if after && err == nil {
						t.Error("a lookup started after Disable returned still authenticated")
						return
					}
				}
			}()
		}
		time.Sleep(5 * time.Millisecond)
		if _, err := s.Disable(ctx, name); err != nil {
			t.Fatal(err)
		}
		disabled.Store(true)
		time.Sleep(5 * time.Millisecond)
		close(stop)
		wg.Wait()
		if t.Failed() {
			return
		}
	}
}
