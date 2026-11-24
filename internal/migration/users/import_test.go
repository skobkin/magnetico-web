package users

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func nyTime(t *testing.T, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(testZone)
	if err != nil {
		t.Fatal(err)
	}
	v, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestImportPreservesIdentitiesAndRelationships(t *testing.T) {
	src, d := seeded(t)
	tgt := targetDB(t)
	unrelated, srcBefore := unrelatedState(t, tgt), sourceState(t, src)

	rep := mustRun(t, cfgFor(src, tgt))
	if rep.Result != "ok" || rep.TargetState != "empty" || rep.PlanDigest == "" || rep.SourceDigest == "" {
		t.Fatalf("report: %+v", rep)
	}
	if rep.Source != (Counts{Users: 6, Invites: 5, UsedInvites: 2, Tokens: 4}) ||
		rep.ImportedUsers != 6 || rep.ImportedInvites != 5 || rep.ImportedTokens != 4 {
		t.Fatalf("counts: source=%+v imported=%d/%d/%d", rep.Source, rep.ImportedUsers, rep.ImportedInvites, rep.ImportedTokens)
	}

	// users: every id, username (case kept), email, hash verbatim; unverified,
	// active, role user; created_at converted from the stated zone.
	c := admin(t, tgt)
	rows, err := c.Query(tctx(t), `SELECT id, username, email, password_hash, role, active, email_verified_at IS NULL,
        password_changed_at IS NULL, last_login_at IS NULL, created_at FROM magnetico_web.users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var id int64
		var name, email, hash, role string
		var active, unverified, noPw, noLogin bool
		var created time.Time
		if err := rows.Scan(&id, &name, &email, &hash, &role, &active, &unverified, &noPw, &noLogin, &created); err != nil {
			t.Fatal(err)
		}
		u := d.Users[n]
		n++
		if id != u.ID || name != u.Name || email != u.Email || hash != u.Hash {
			t.Errorf("user %d not preserved verbatim", u.ID)
		}
		if role != "user" || !active || !unverified || !noPw || !noLogin {
			t.Errorf("user %d: role=%s active=%v unverified=%v", u.ID, role, active, unverified)
		}
		if !created.Equal(nyTime(t, u.Created)) {
			t.Errorf("user %d created_at %v, want %v", u.ID, created, nyTime(t, u.Created))
		}
	}
	rows.Close()
	if n != 6 {
		t.Fatalf("%d users imported", n)
	}
	// Unambiguous zone conversions, written out (winter EST, summer EDT).
	if got := scalar[time.Time](t, tgt, `SELECT created_at FROM magnetico_web.users WHERE id = 1`); !got.Equal(time.Date(2021, 1, 15, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("winter conversion: %v", got)
	}
	if got := scalar[time.Time](t, tgt, `SELECT created_at FROM magnetico_web.users WHERE id = 2`); !got.Equal(time.Date(2021, 7, 15, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("summer conversion: %v", got)
	}

	// invitations: ids, owners, plaintext codes, used_by; used_at NULL; created_at = owner's.
	for _, v := range d.Invites {
		var owner int64
		var code string
		var used *int64
		var usedAtNull, expNull, revNull bool
		var created time.Time
		if err := c.QueryRow(tctx(t), `SELECT owner_id, code, used_by, used_at IS NULL, expires_at IS NULL, revoked_at IS NULL, created_at
            FROM magnetico_web.invites WHERE id = $1`, v.ID).Scan(&owner, &code, &used, &usedAtNull, &expNull, &revNull, &created); err != nil {
			t.Fatalf("invite %d: %v", v.ID, err)
		}
		if owner != v.Owner || code != v.Code || (used == nil) != (v.Used == nil) || (used != nil && *used != *v.Used) {
			t.Errorf("invite %d relationships changed", v.ID)
		}
		if !usedAtNull || !expNull || !revNull {
			t.Errorf("invite %d: used_at/expires_at/revoked_at must be NULL", v.ID)
		}
		var ownerCreated string
		for _, u := range d.Users {
			if u.ID == v.Owner {
				ownerCreated = u.Created
			}
		}
		if !created.Equal(nyTime(t, ownerCreated)) {
			t.Errorf("invite %d created_at is not its owner's", v.ID)
		}
	}

	// legacy tokens: only rss_tokens, sha256 stored, legacy, original time,
	// ids consecutive in (created_at, owner, digest) order; no api_tokens.
	wantOrder := []string{d.Tokens[0].Key, d.Tokens[2].Key, d.Tokens[1].Key, d.Tokens[3].Key}
	for i, key := range wantOrder {
		sum := sha256.Sum256([]byte(key))
		var id, owner int64
		var legacy, labelNull, usedNull, expNull, revNull bool
		var created time.Time
		if err := c.QueryRow(tctx(t), `SELECT id, user_id, legacy, label IS NULL, last_used_at IS NULL, expires_at IS NULL, revoked_at IS NULL, created_at
            FROM magnetico_web.rss_tokens WHERE token_hash = $1`, sum[:]).Scan(&id, &owner, &legacy, &labelNull, &usedNull, &expNull, &revNull, &created); err != nil {
			t.Fatalf("token %d: %v", i, err)
		}
		var want sToken
		for _, k := range d.Tokens {
			if k.Key == key {
				want = k
			}
		}
		if id != int64(i+1) || owner != want.User || !legacy || !labelNull || !usedNull || !expNull || !revNull || !created.Equal(nyTime(t, want.Created)) {
			t.Errorf("token #%d: id=%d owner=%d legacy=%v", i, id, owner, legacy)
		}
	}
	for _, q := range []string{`api_tokens`, `sessions`, `one_time_tokens`} {
		if n := scalar[int](t, tgt, `SELECT count(*) FROM magnetico_web.`+q); n != 0 {
			t.Errorf("%s has %d rows, want 0", q, n)
		}
	}

	// sequences advanced past the maxima: the next inserts get max+1.
	if id := scalar[int64](t, tgt, `INSERT INTO magnetico_web.users (username) VALUES ('zed') RETURNING id`); id != 10 {
		t.Errorf("next user id %d, want 10", id)
	}
	if id := scalar[int64](t, tgt, `INSERT INTO magnetico_web.invites (owner_id, code) VALUES (1, 'ZZZZZZZZZZZZZZZZZZZZZZ') RETURNING id`); id != 13 {
		t.Errorf("next invite id %d, want 13", id)
	}
	if id := scalar[int64](t, tgt, `INSERT INTO magnetico_web.rss_tokens (token_hash, user_id) VALUES ('\x0000000000000000000000000000000000000000000000000000000000000009', 1) RETURNING id`); id != 5 {
		t.Errorf("next rss token id %d, want 5", id)
	}
	// cleanup of the three probe rows so Verify below sees the pure import
	execSQL(t, tgt, `DELETE FROM magnetico_web.rss_tokens WHERE id = 5`, `DELETE FROM magnetico_web.invites WHERE id = 13`, `DELETE FROM magnetico_web.users WHERE id = 10`)

	if unrelatedState(t, tgt) != unrelated {
		t.Error("trackers, settings, web_version or torrent data changed")
	}
	if sourceState(t, src) != srcBefore {
		t.Error("the source changed")
	}

	vcfg := cfgFor(src, tgt)
	vrep, err := Verify(tctx(t), vcfg)
	if err != nil || vrep.Result != "verified" {
		t.Fatalf("verify: %v %+v", err, vrep)
	}
	// A wrong stated zone is detected by verify (timestamps differ).
	vcfg.SourceTimezone = "Europe/Berlin"
	vrep, err = Verify(tctx(t), vcfg)
	_ = wantClass(t, err, ClassVerifyFailed) // the helper fails the test itself
	if vrep.MismatchCount == 0 || !strings.Contains(strings.Join(vrep.Mismatches, ","), "user-created-at") {
		t.Errorf("verify under a different zone: %+v", vrep.Mismatches)
	}
}

func TestVerifyDetectsDifferences(t *testing.T) {
	src, _ := seeded(t)
	tgt := targetDB(t)
	mustRun(t, cfgFor(src, tgt))
	for name, sql := range map[string]string{
		"email changed":      `UPDATE magnetico_web.users SET email = 'x@example.org' WHERE id = 1`,
		"hash changed":       `UPDATE magnetico_web.users SET password_hash = 'x' WHERE id = 2`,
		"extra account":      `INSERT INTO magnetico_web.users (username) VALUES ('extra')`,
		"api token present":  `INSERT INTO magnetico_web.api_tokens (token_hash, user_id) VALUES ('\x0000000000000000000000000000000000000000000000000000000000000001', 1)`,
		"token not legacy":   `UPDATE magnetico_web.rss_tokens SET legacy = false WHERE id = 1`,
		"invite code":        `UPDATE magnetico_web.invites SET code = 'QQQQQQQQQQQQQQQQQQQQQQ' WHERE id = 3`,
		"token removed":      `DELETE FROM magnetico_web.rss_tokens WHERE id = 1`,
		"verified email set": `UPDATE magnetico_web.users SET email_verified_at = now() WHERE id = 1`,
	} {
		t.Run(name, func(t *testing.T) {
			// Each case on its own copy of the state: apply, verify, undo is
			// brittle, so use a savepoint-free approach: a fresh import.
			src2, _ := seeded(t)
			tgt2 := targetDB(t)
			mustRun(t, cfgFor(src2, tgt2))
			execSQL(t, tgt2, sql)
			rep, err := Verify(tctx(t), cfgFor(src2, tgt2))
			_ = wantClass(t, err, ClassVerifyFailed) // the helper fails the test itself
			if rep.MismatchCount == 0 {
				t.Fatal("no mismatch reported")
			}
		})
	}
}

// ---- dry-run writes nothing ----

func TestDryRunWritesNothingAndNeedsOnlySelect(t *testing.T) {
	ro := newRole(t)
	src, _ := seeded(t)
	tgt := targetDB(t)
	grantReadSource(t, src, ro)
	grantReadTarget(t, tgt, ro)

	before, unrelated := userState(t, tgt), unrelatedState(t, tgt)
	cfg := cfgFor(asRole(t, src, ro), asRole(t, tgt, ro))
	cfg.DryRun = true
	rep := mustRun(t, cfg)
	if rep.Result != "ok" || rep.TargetState != "empty" || rep.PlanDigest == "" {
		t.Fatalf("dry-run: %+v", rep)
	}
	if userState(t, tgt) != before || unrelatedState(t, tgt) != unrelated {
		t.Fatal("a dry-run changed the target (rows or sequence state)")
	}
	// The same SELECT-only role cannot run the final import: privileges, not a partial write.
	cfg.DryRun = false
	_, err := Run(tctx(t), cfg)
	e := wantClass(t, err, ClassPrivileges)
	if len(e.Details) == 0 {
		t.Error("privilege failure names nothing")
	}
	if userState(t, tgt) != before {
		t.Fatal("a refused final run changed the target")
	}
}

func TestDryRunPopulatedTargetKeepsSequences(t *testing.T) {
	src, _ := seeded(t)
	tgt := targetDB(t)
	mustRun(t, cfgFor(src, tgt))
	execSQL(t, tgt, `INSERT INTO magnetico_web.users (username) VALUES ('operator')`)
	before := userState(t, tgt)
	for _, replace := range []bool{false, true} {
		cfg := cfgFor(src, tgt)
		cfg.DryRun, cfg.ReplaceUsers = true, replace
		cfg.DeleteUnmatchedIDs = nil
		_, _ = Run(tctx(t), cfg) // refusals are expected here; only "nothing written" matters
		if userState(t, tgt) != before {
			t.Fatalf("dry-run (replace=%v) changed the target", replace)
		}
	}
}

// ---- sequence state after dry-run, failed import and successful import ----

func TestSequenceStateAcrossFailuresAndSuccess(t *testing.T) {
	src, _ := seeded(t)
	tgt := targetDB(t)
	// Advance the identities first so "never backward" and "no gaps" are visible.
	execSQL(t, tgt, `INSERT INTO magnetico_web.users (username) VALUES ('probe')`, `DELETE FROM magnetico_web.users`)
	before := userState(t, tgt)
	if !strings.Contains(before, "seq users last=1 called=true") {
		t.Fatalf("setup: %s", before)
	}

	stages := []string{"after-users", "after-invites", "after-tokens", "after-restart", "before-drift-check"}
	for _, stage := range stages {
		t.Run("failure at "+stage, func(t *testing.T) {
			testHook = func(s string, tx pgx.Tx) error {
				if s == stage {
					return errors.New("injected")
				}
				return nil
			}
			defer func() { testHook = nil }()
			rep, err := Run(tctx(t), cfgFor(src, tgt))
			_ = wantClass(t, err, ClassStorage) // the helper fails the test itself
			if rep.Result != "failed" {
				t.Errorf("result %q", rep.Result)
			}
			if got := userState(t, tgt); got != before {
				t.Fatalf("failed import at %s left changes:\n%s\n--- before ---\n%s", stage, got, before)
			}
		})
	}

	t.Run("real constraint failure", func(t *testing.T) {
		testHook = func(s string, tx pgx.Tx) error {
			if s == "after-users" {
				_, err := tx.Exec(tctx(t), `INSERT INTO magnetico_web.users (id, username) OVERRIDING SYSTEM VALUE VALUES (1, 'dup')`)
				return err
			}
			return nil
		}
		defer func() { testHook = nil }()
		_, err := Run(tctx(t), cfgFor(src, tgt))
		e := wantClass(t, err, ClassStorage)
		if !strings.Contains(strings.Join(e.Details, " "), "constraint users_pkey") {
			t.Errorf("details %v", e.Details)
		}
		if userState(t, tgt) != before {
			t.Fatal("a constraint failure left changes")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := contextWithCancel(t)
		testHook = func(s string, tx pgx.Tx) error {
			if s == "after-invites" {
				cancel()
			}
			return nil
		}
		defer func() { testHook = nil }()
		_, err := Run(ctx, cfgFor(src, tgt))
		_ = wantClass(t, err, ClassStorage) // the helper fails the test itself
		if userState(t, tgt) != before {
			t.Fatal("an interrupted import left changes")
		}
	})

	// Successful import: users 10, invites 13, rss tokens next after the probe-free state.
	mustRun(t, cfgFor(src, tgt))
	after := userState(t, tgt)
	for _, want := range []string{"seq users last=10 called=false", "seq invites last=13 called=false", "seq rss_tokens last=5 called=false", "seq api_tokens last=1 called=false"} {
		if !strings.Contains(after, want) {
			t.Errorf("after the import missing %q in\n%s", want, after[strings.Index(after, "seq users"):])
		}
	}
}

// A second import over a database whose rss_tokens identity is already high
// must start its explicit ids there (never backward, never a reused id).
func TestRSSIdsStartAtCurrentIdentity(t *testing.T) {
	src, _ := seeded(t)
	tgt := targetDB(t)
	execSQL(t, tgt,
		`INSERT INTO magnetico_web.users (username) VALUES ('probe')`,
		`INSERT INTO magnetico_web.rss_tokens (token_hash, user_id) SELECT '\x0000000000000000000000000000000000000000000000000000000000000001'::bytea, id FROM magnetico_web.users`,
		`DELETE FROM magnetico_web.rss_tokens`, `DELETE FROM magnetico_web.users`)
	mustRun(t, cfgFor(src, tgt))
	if min := scalar[int64](t, tgt, `SELECT min(id) FROM magnetico_web.rss_tokens`); min != 2 {
		t.Errorf("first migrated rss token id %d, want 2 (identity was at 1)", min)
	}
	if next := scalar[int64](t, tgt, `INSERT INTO magnetico_web.rss_tokens (token_hash, user_id) VALUES ('\x0000000000000000000000000000000000000000000000000000000000000002', 1) RETURNING id`); next != 6 {
		t.Errorf("next rss token id %d, want 6", next)
	}
}
