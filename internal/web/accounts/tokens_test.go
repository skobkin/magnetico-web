package accounts

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenSeparationAndStorage(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "rita")
	api, _ := s.CreateAPIToken(ctx, u.ID, "a", nil)
	rss, _ := s.CreateRSSToken(ctx, u.ID, "r", nil)
	const legacy = "fedcba9876543210fedcba9876543210"
	exec(t, pool, `INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy) VALUES ($1,$2,true)`, hashToken(legacy), u.ID)

	if !strings.HasPrefix(api.Plaintext, "mgt_") || !strings.HasPrefix(rss.Plaintext, "mgf_") {
		t.Fatalf("prefixes: %q %q", api.Plaintext, rss.Plaintext)
	}
	if _, err := s.AuthenticateRSSToken(ctx, api.Plaintext); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("an API token authenticates as RSS")
	}
	if _, err := s.AuthenticateAPIToken(ctx, rss.Plaintext); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("an RSS token authenticates as API")
	}
	if _, err := s.AuthenticateAPIToken(ctx, legacy); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("a legacy RSS token authenticates as API")
	}
	if id, err := s.AuthenticateAPIToken(ctx, api.Plaintext); err != nil || id.UserID != u.ID || id.Username != "rita" || id.TokenID != api.ID {
		t.Fatalf("api identity = %+v, %v", id, err)
	}
	for _, bad := range []string{"", "mgt_", strings.Repeat("x", 300)} {
		if _, err := s.AuthenticateAPIToken(ctx, bad); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// Only SHA-256 is stored: no plaintext (or its base64 body) in any row.
	body := strings.TrimPrefix(api.Plaintext, "mgt_")
	for _, tbl := range []string{"api_tokens", "rss_tokens", "one_time_tokens", "invites", "users"} {
		if n := count(t, pool, `SELECT count(*) FROM magnetico_web.`+tbl+` x WHERE x::text LIKE '%'||$1||'%'`, body); n != 0 {
			t.Errorf("token plaintext found in %s", tbl)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM magnetico_web.api_tokens WHERE token_hash = $1 AND octet_length(token_hash) = 32`, hashToken(api.Plaintext)); n != 1 {
		t.Fatal("stored hash is not SHA-256 of the presented token")
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "sam")
	past := time.Now().Add(-time.Hour)
	tok, _ := s.CreateAPIToken(ctx, u.ID, "", &past)
	if _, err := s.AuthenticateAPIToken(ctx, tok.Plaintext); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired: %v", err)
	}
	future := time.Now().Add(time.Hour)
	tok, _ = s.CreateAPIToken(ctx, u.ID, "", &future)
	if _, err := s.AuthenticateAPIToken(ctx, tok.Plaintext); err != nil {
		t.Fatal(err)
	}
}

func TestInvitationRules(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	owner := mkUser(t, s, pool, "tess")
	reg := func(code, name string) error {
		_, err := s.RegisterWithInvite(ctx, RegisterParams{Code: code, Username: name, Hash: testHash(t), Quota: 1})
		return err
	}
	codes, _ := s.CreateInvites(ctx, "tess", 8, nil)

	// Unknown, used, revoked and expired invitations give the same error.
	mustErr(t, reg("unknown-code-unknown-code", "u1"), ErrInvitationUnavailable, "unknown")
	if err := reg(codes[0], "user-one"); err != nil {
		t.Fatal(err)
	}
	mustErr(t, reg(codes[0], "user-two"), ErrInvitationUnavailable, "used")
	invs, _ := s.ListInvites(ctx, "tess")
	var revokeID int64
	for _, v := range invs {
		if v.Code == codes[1] {
			revokeID = v.ID
		}
	}
	if o, err := s.RevokeInvite(ctx, revokeID); err != nil || o != InviteRevoked {
		t.Fatalf("revoke = %v, %v", o, err)
	}
	if o, _ := s.RevokeInvite(ctx, revokeID); o != InviteAlreadyRevoked {
		t.Fatalf("second revoke = %v", o)
	}
	mustErr(t, reg(codes[1], "user-three"), ErrInvitationUnavailable, "revoked")
	exec(t, pool, `UPDATE magnetico_web.invites SET expires_at = now() - interval '1 minute' WHERE code = $1`, codes[2])
	mustErr(t, reg(codes[2], "user-four"), ErrInvitationUnavailable, "expired")
	var usedID int64
	for _, v := range invs {
		if v.Code == codes[0] {
			usedID = v.ID
		}
	}
	if o, _ := s.RevokeInvite(ctx, usedID); o != InviteAlreadyUsed {
		t.Fatalf("revoke of a used invite = %v", o)
	}
	if _, err := s.RevokeInvite(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}

	// A duplicate username does not consume the invitation.
	mustErr(t, reg(codes[3], "tess"), ErrUsernameTaken, "duplicate username")
	if err := reg(codes[3], "user-five"); err != nil {
		t.Fatalf("invitation was consumed by a failed registration: %v", err)
	}

	// Disabled owner: generic error, invitation unchanged; enable restores it.
	if _, err := s.Disable(ctx, "tess"); err != nil {
		t.Fatal(err)
	}
	mustErr(t, reg(codes[4], "user-six"), ErrInvitationUnavailable, "disabled owner")
	if count(t, pool, `SELECT count(*) FROM magnetico_web.invites WHERE code = $1 AND used_by IS NULL AND revoked_at IS NULL`, codes[4]) != 1 {
		t.Fatal("a refused registration changed the invitation")
	}
	if err := s.Enable(ctx, "tess"); err != nil {
		t.Fatal(err)
	}
	if err := reg(codes[4], "user-six"); err != nil {
		t.Fatalf("after enable: %v", err)
	}
	_ = owner
}

// Disable versus registration, deterministic in both orders.
func TestDisableVersusRegistrationDeterministic(t *testing.T) {
	t.Run("disable holds the lock first", func(t *testing.T) {
		s, pool := newStore(t)
		ctx := tctx(t)
		mkUser(t, s, pool, "uma")
		codes, _ := s.CreateInvites(ctx, "uma", 1, nil)
		inCommit, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		s.beforeCommit = func(op string) error {
			if op == "disable" {
				once.Do(func() { close(inCommit) })
				<-release
			}
			return nil
		}
		go func() { _, _ = s.Disable(ctx, "uma") }()
		<-inCommit
		res := make(chan error, 1)
		go func() {
			_, err := s.RegisterWithInvite(ctx, RegisterParams{Code: codes[0], Username: "vera", Hash: testHash(t)})
			res <- err
		}()
		select {
		case err := <-res:
			t.Fatalf("registration did not wait for the disable: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		close(release)
		mustErr(t, <-res, ErrInvitationUnavailable, "registration after disable")
		if count(t, pool, `SELECT count(*) FROM magnetico_web.invites WHERE code = $1 AND used_by IS NULL`, codes[0]) != 1 {
			t.Fatal("invitation consumed")
		}
	})
	t.Run("registration holds the locks first", func(t *testing.T) {
		s, pool := newStore(t)
		ctx := tctx(t)
		mkUser(t, s, pool, "walt")
		codes, _ := s.CreateInvites(ctx, "walt", 1, nil)
		inCommit, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		s.beforeCommit = func(op string) error {
			if op == "register" {
				once.Do(func() { close(inCommit) })
				<-release
			}
			return nil
		}
		reg := make(chan error, 1)
		go func() {
			_, err := s.RegisterWithInvite(ctx, RegisterParams{Code: codes[0], Username: "xena", Hash: testHash(t)})
			reg <- err
		}()
		<-inCommit
		dis := make(chan error, 1)
		go func() { _, err := s.Disable(ctx, "walt"); dis <- err }()
		select {
		case err := <-dis:
			t.Fatalf("disable did not wait for the registration (returned %v)", err)
		case <-time.After(150 * time.Millisecond):
		}
		close(release)
		if err := <-reg; err != nil {
			t.Fatal(err)
		}
		if err := <-dis; err != nil {
			t.Fatal(err)
		}
		if u, _ := s.GetUser(ctx, "walt"); u.Active {
			t.Fatal("owner is still active")
		}
	})
}

func TestDisableVersusRegistrationStress(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	mkUser(t, s, pool, "yan")
	for i := 0; i < 100; i++ {
		codes, err := s.CreateInvites(ctx, "yan", 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Enable(ctx, "yan"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var regErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, regErr = s.RegisterWithInvite(ctx, RegisterParams{Code: codes[0], Username: "stress-" + codes[0][:10], Hash: testHash(t)})
		}()
		go func() { defer wg.Done(); _, _ = s.Disable(ctx, "yan") }() // racing the registration: either outcome is legitimate
		wg.Wait()
		used := count(t, pool, `SELECT count(*) FROM magnetico_web.invites WHERE code = $1 AND used_by IS NOT NULL`, codes[0])
		switch {
		case regErr == nil && used != 1, regErr != nil && used != 0:
			t.Fatalf("iteration %d: registration err = %v but invite used = %d", i, regErr, used)
		case regErr != nil && !errors.Is(regErr, ErrInvitationUnavailable):
			t.Fatalf("iteration %d: %v", i, regErr)
		}
		// Anything started after Disable returned must fail.
		codes2, _ := s.CreateInvites(ctx, "yan", 1, nil)
		if _, err := s.RegisterWithInvite(ctx, RegisterParams{Code: codes2[0], Username: "late-" + codes2[0][:10], Hash: testHash(t)}); !errors.Is(err, ErrInvitationUnavailable) {
			t.Fatalf("iteration %d: registration after disable: %v", i, err)
		}
	}
}

// Authentication loops against toggling disable/enable and revoke.
//
// The expectations are judged against a transition record, not a bare flag: every change of
// state publishes a new value that carries a generation number, and a call is judged only when
// the value read after it is the very value read before it. With a flag (0 = disabled, 1 =
// enabled) a goroutine that was starved for more than a whole disable/enable cycle read "0"
// before, "0" after, and judged an authentication that ran in the enabled phase as one after
// Disable returned (found at default parallelism under CPU load, Task 09B; reproduced with
// -race -count=60 on 12 busy CPUs, issue #53).
func TestAuthenticationVersusDisableAndRevoke(t *testing.T) {
	s, pool := newStore(t)
	ctx := tctx(t)
	u := mkUser(t, s, pool, "zed")
	api, _ := s.CreateAPIToken(ctx, u.ID, "", nil)
	rss, _ := s.CreateRSSToken(ctx, u.ID, "", nil)

	const (
		transition = 0 // a Disable or Enable call is in progress
		disabled   = 1 // Disable returned
		enabled    = 2 // Enable returned
	)
	var stop atomic.Bool
	var cell atomic.Uint64 // generation<<2 | phase; every store has a new generation
	var gen uint64         // only the toggling goroutine writes it
	publish := func(phase uint64) { gen++; cell.Store(gen<<2 | phase) }
	publish(enabled)
	var wg sync.WaitGroup
	for _, f := range []func() error{
		func() error { _, e := s.AuthenticateAPIToken(ctx, api.Plaintext); return e },
		func() error { _, e := s.AuthenticateRSSToken(ctx, rss.Plaintext); return e },
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				before := cell.Load()
				err := f()
				if err != nil && !errors.Is(err, ErrUnauthenticated) {
					t.Errorf("auth: %v", err)
					return
				}
				// No transition began, let alone finished, while the call ran: the phase is certain.
				if cell.Load() != before {
					continue
				}
				if before&3 == disabled && err == nil {
					t.Error("authenticated after Disable returned")
					return
				}
				if before&3 == enabled && err != nil {
					t.Error("rejected after Enable returned")
					return
				}
			}
		}()
	}
	for i := 0; i < 60; i++ {
		publish(transition)
		if _, err := s.Disable(ctx, "zed"); err != nil {
			t.Fatal(err)
		}
		publish(disabled)
		time.Sleep(3 * time.Millisecond)
		publish(transition)
		if err := s.Enable(ctx, "zed"); err != nil {
			t.Fatal(err)
		}
		publish(enabled)
		time.Sleep(3 * time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()

	// Revoke: once RevokeAllTokens returns, no authentication may succeed.
	var revoked atomic.Bool
	stop.Store(false)
	for _, f := range []func() error{
		func() error { _, e := s.AuthenticateAPIToken(ctx, api.Plaintext); return e },
		func() error { _, e := s.AuthenticateRSSToken(ctx, rss.Plaintext); return e },
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				was := revoked.Load()
				if err := f(); was && err == nil {
					t.Error("authenticated after RevokeAllTokens returned")
					return
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	if _, _, err := s.RevokeAllTokens(ctx, "zed"); err != nil {
		t.Fatal(err)
	}
	revoked.Store(true)
	time.Sleep(50 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
}
