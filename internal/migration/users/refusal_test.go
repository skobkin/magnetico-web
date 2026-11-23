package users

import (
	"strings"
	"testing"
)

func TestFinalRunRequiresWritesStoppedAssertion(t *testing.T) {
	cfg := cfgFor("postgres://127.0.0.1:1/src", "postgres://127.0.0.1:1/tgt")
	cfg.SourceWritesStopped = false
	_, err := Run(tctx(t), cfg)
	wantClass(t, err, ClassSourceWritesNotAssert)
	cfg.DryRun = true // a dry-run needs no assertion; it fails later, on the unreachable source
	_, err = Run(tctx(t), cfg)
	wantClass(t, err, ClassStorage)
}

func TestConfigurationErrors(t *testing.T) {
	base := cfgFor("postgres://x/src", "postgres://x/tgt")
	for _, mod := range map[string]func(*Config){
		"no source":                      func(c *Config) { c.SourceDSN = "" },
		"no target":                      func(c *Config) { c.TargetDSN = "" },
		"no timezone":                    func(c *Config) { c.SourceTimezone = "" },
		"unknown timezone":               func(c *Config) { c.SourceTimezone = "Mars/Olympus" },
		"Local timezone":                 func(c *Config) { c.SourceTimezone = "Local" },
		"negative acknowledgement":       func(c *Config) { c.AcceptNeedsReset = -1 },
		"unmatched ids without replace":  func(c *Config) { c.DeleteUnmatchedIDs = []int64{1} },
		"replace without confirm (real)": func(c *Config) { c.ReplaceUsers = true },
	} {
		cfg := base
		mod(&cfg)
		_, err := Run(tctx(t), cfg)
		wantClass(t, err, ClassConfig)
	}
	if _, err := ParseIDList("3, 1,2"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"1,1", "a", "0", "-2", "1,,2"} {
		if _, err := ParseIDList(bad); err == nil {
			t.Errorf("ParseIDList(%q) accepted", bad)
		}
	}
}

func TestNeedsResetMustBeAcknowledgedExactly(t *testing.T) {
	src, d := seeded(t)
	// Two accounts get formats the verifier does not support.
	execSQL(t, src,
		`UPDATE users.users SET password = '$2a$10$`+strings.Repeat("a", 53)+`' WHERE id = 1`,
		`UPDATE users.users SET password = '{SSHA}opaque' WHERE id = 2`)
	tgt := targetDB(t)
	before := userState(t, tgt)

	for _, n := range []int{0, 1, 3} {
		cfg := cfgFor(src, tgt)
		cfg.AcceptNeedsReset = n
		rep, err := Run(tctx(t), cfg)
		wantClass(t, err, ClassNeedsResetUnack)
		if rep.NeedsReset != 2 {
			t.Errorf("needs_reset=%d", rep.NeedsReset)
		}
		if userState(t, tgt) != before {
			t.Fatal("a refused run wrote")
		}
	}
	cfg := cfgFor(src, tgt)
	cfg.AcceptNeedsReset = 2
	cfg.DryRun = true
	if rep := mustRun(t, cfg); rep.Result != "ok" || rep.NeedsReset != 2 {
		t.Fatalf("dry-run: %+v", rep)
	}
	cfg.DryRun = false
	mustRun(t, cfg)
	// Imported verbatim, never transformed.
	if got := scalar[string](t, tgt, `SELECT password_hash FROM magnetico_web.users WHERE id = 2`); got != "{SSHA}opaque" {
		t.Errorf("unsupported hash was changed")
	}
	_ = d
}

func TestUnvettedHashFormatBlocksTheFinalRun(t *testing.T) {
	src, _ := seeded(t)
	execSQL(t, src, `UPDATE users.users SET password = '$2y$12$`+strings.Repeat("a", 53)+`' WHERE id = 4`)
	tgt := targetDB(t)
	before := userState(t, tgt)

	cfg := cfgFor(src, tgt)
	cfg.DryRun = true
	rep, err := Run(tctx(t), cfg)
	wantClass(t, err, ClassUnvettedHash)
	if rep.Unvetted != 1 || rep.NeedsReset != 0 || len(rep.WouldRefuse) != 1 || rep.WouldRefuse[0] != ClassUnvettedHash {
		t.Fatalf("dry-run report: %+v", rep)
	}
	cfg.DryRun = false
	_, err = Run(tctx(t), cfg)
	wantClass(t, err, ClassUnvettedHash)
	if userState(t, tgt) != before {
		t.Fatal("a refused run wrote")
	}
}

func TestMalformedSourceDataIsBlockedAndEverythingIsReported(t *testing.T) {
	src, _ := seeded(t)
	// Drop the legacy constraints so inconsistent rows can exist at all.
	execSQL(t, src,
		`ALTER TABLE users.invites DROP CONSTRAINT FK_93848FA8A76ED395, DROP CONSTRAINT FK_93848FA84C2B72A8`,
		`ALTER TABLE users.api_tokens DROP CONSTRAINT FK_C46A74A9A76ED395`,
		`DROP INDEX users.UNIQ_338ADFC4F85E0677`,
		`INSERT INTO users.users (id, username, password, email, roles, created_at) VALUES
            (20, 'alice', '', 'xy', '[]', now()), (21, '', 'whatever', 'ok@example.org', '[]', now())`,
		`INSERT INTO users.invites (id, user_id, used_by_id, code) VALUES (30, 999, 998, 'tooshort')`,
		`INSERT INTO users.api_tokens (key, user_id, created_at) VALUES ('', 997, now())`)
	tgt := targetDB(t)
	before := userState(t, tgt)
	cfg := cfgFor(src, tgt)
	rep, err := Run(tctx(t), cfg)
	e := wantClass(t, err, ClassDataBlocked)
	got := map[string]bool{}
	for _, f := range rep.Blocking {
		got[f.Code] = true
	}
	for _, c := range []string{"username-duplicate", "password-empty", "email-length", "username-length",
		"invite-orphan-owner", "invite-orphan-used-by", "invite-code-format", "token-key-empty", "token-orphan-owner"} {
		if !got[c] {
			t.Errorf("finding %s not reported (got %v)", c, rep.Blocking)
		}
	}
	for _, line := range e.Details {
		if strings.Contains(line, "tooshort") || strings.Contains(line, "whatever") {
			t.Errorf("error detail leaks data: %q", line)
		}
	}
	if userState(t, tgt) != before {
		t.Fatal("blocked source data led to a target write")
	}
}

func TestEmptySourceIsRefused(t *testing.T) {
	src := legacyDB(t)
	tgt := targetDB(t)
	_, err := Run(tctx(t), cfgFor(src, tgt))
	wantClass(t, err, ClassSourceEmpty)
}

func TestWarningsDoNotBlockAndAllTokensAreImported(t *testing.T) {
	src, _ := seeded(t)
	// 51 extra tokens for one user (over the 50-unrevoked cap) and a non-hex
	// key: all warnings, none blocking. Every token is imported (plan D5).
	execSQL(t, src,
		`INSERT INTO users.api_tokens (key, user_id, created_at)
            SELECT 'f' || lpad(g::text, 31, '0'), 6, timestamp '2022-12-02 00:00:00' + g * interval '1 minute' FROM generate_series(1, 51) g`,
		`INSERT INTO users.api_tokens (key, user_id, created_at) VALUES ('NOT-HEX-KEY', 6, '2022-12-03 00:00:00')`)
	tgt := targetDB(t)
	rep := mustRun(t, cfgFor(src, tgt))
	warn := map[string]int{}
	for _, f := range rep.Warnings {
		warn[f.Code] = f.Count
	}
	for _, c := range []string{"email-case-duplicate", "roles-not-plain-user", "tokens-over-cap", "token-key-not-32-hex"} {
		if warn[c] == 0 {
			t.Errorf("warning %s missing: %v", c, rep.Warnings)
		}
	}
	if n := scalar[int](t, tgt, `SELECT count(*) FROM magnetico_web.rss_tokens WHERE user_id = 6`); n != 53 {
		t.Errorf("user 6 has %d migrated tokens, want 53", n)
	}
	if n := scalar[int](t, tgt, `SELECT count(*) FROM magnetico_web.rss_tokens`); n != 4+52 {
		t.Errorf("%d tokens imported", n)
	}
	// No administrator is inferred from the legacy roles.
	if n := scalar[int](t, tgt, `SELECT count(*) FROM magnetico_web.users WHERE role <> 'user'`); n != 0 {
		t.Errorf("%d accounts are not plain users", n)
	}
}

func TestSourceCanBeSelectOnlyRoleAndStaysUntouched(t *testing.T) {
	ro := newRole(t)
	src, _ := seeded(t)
	tgt := targetDB(t)
	grantReadSource(t, src, ro)
	before := sourceState(t, src)
	cfg := cfgFor(asRole(t, src, ro), tgt)
	rep := mustRun(t, cfg)
	for _, n := range rep.Notes {
		if strings.Contains(n, "source-role-has-write-privileges") {
			t.Error("a SELECT-only role was reported as writable")
		}
	}
	if sourceState(t, src) != before {
		t.Fatal("the source changed")
	}
}

func TestSourceAndTargetInOneDatabase(t *testing.T) {
	both := targetDB(t)
	sql := scalar[string](t, both, `SELECT 'x'`) // keep the helper import honest
	_ = sql
	execLegacy(t, both)
	d := stdData(t)
	d.load(t, both)
	rep := mustRun(t, cfgFor(both, both))
	if !rep.SameDatabase || rep.Result != "ok" {
		t.Fatalf("%+v", rep)
	}
}

func TestPrivilegeAndOwnershipFailuresNameObjectsOnly(t *testing.T) {
	rw := newRole(t)
	src, _ := seeded(t)
	tgt := targetDB(t)
	grantReadSource(t, src, rw)
	grantReadTarget(t, tgt, rw)
	execSQL(t, tgt, `GRANT INSERT ON magnetico_web.users, magnetico_web.invites, magnetico_web.rss_tokens TO `+quoteRole(rw))
	before := userState(t, tgt)
	_, err := Run(tctx(t), cfgFor(asRole(t, src, rw), asRole(t, tgt, rw)))
	e := wantClass(t, err, ClassPrivileges)
	if !strings.Contains(strings.Join(e.Details, " "), "ownership") {
		t.Errorf("an INSERT-only role must be refused for lacking table ownership: %v", e.Details)
	}
	if userState(t, tgt) != before {
		t.Fatal("a privilege refusal wrote")
	}
}

func TestLockTimeoutWhenAUserTableIsBusy(t *testing.T) {
	src, _ := seeded(t)
	tgt := targetDB(t)
	before := userState(t, tgt)
	old := lockTimeout
	lockTimeout = "300ms"
	defer func() { lockTimeout = old }()
	holder := admin(t, tgt)
	tx, err := holder.Begin(tctx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(tctx(t)) }()
	if _, err := tx.Exec(tctx(t), `LOCK TABLE magnetico_web.users IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	_, err = Run(tctx(t), cfgFor(src, tgt))
	wantClass(t, err, ClassLockTimeout)
	_ = tx.Rollback(tctx(t))
	if userState(t, tgt) != before {
		t.Fatal("a lock timeout left changes")
	}
}
