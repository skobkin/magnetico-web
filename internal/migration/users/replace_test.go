package users

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestRerunIsIdempotent(t *testing.T) {
	src, _ := seeded(t)
	tgt := targetDB(t)
	mustRun(t, cfgFor(src, tgt))
	after := userState(t, tgt)

	rep := mustRun(t, cfgFor(src, tgt))
	if rep.Result != "already-migrated" || rep.TargetState != "identical" {
		t.Fatalf("second run: %+v", rep)
	}
	if userState(t, tgt) != after {
		t.Fatal("a repeated run changed the target")
	}
	cfg := cfgFor(src, tgt)
	cfg.DryRun = true
	if rep := mustRun(t, cfg); rep.Result != "already-migrated" {
		t.Fatalf("dry-run: %+v", rep)
	}
}

func TestPopulatedTargetIsNeverReplacedWithoutTheFlag(t *testing.T) {
	src, d := seeded(t)
	tgt := targetDB(t)
	execSQL(t, tgt, `INSERT INTO magnetico_web.users (username, email) VALUES ('operator', 'op@example.org')`)
	before := userState(t, tgt)
	rep, err := Run(tctx(t), cfgFor(src, tgt))
	e := wantClass(t, err, ClassTargetNotEmpty)
	if rep.TargetState != "diverged" {
		t.Errorf("state %q", rep.TargetState)
	}
	for _, line := range e.Details {
		for _, s := range append(d.secrets(), "operator", "op@example.org") {
			if strings.Contains(line, s) {
				t.Errorf("detail %q leaks %q", line, s)
			}
		}
	}
	if userState(t, tgt) != before {
		t.Fatal("a refused run wrote")
	}
	// A leftover of a previous import that was modified is diverged as well.
	tgt2 := targetDB(t)
	mustRun(t, cfgFor(src, tgt2))
	execSQL(t, tgt2, `UPDATE magnetico_web.users SET password_hash = 'rehashed' WHERE id = 1`)
	_, err = Run(tctx(t), cfgFor(src, tgt2))
	wantClass(t, err, ClassTargetNotEmpty)
}

// populate turns a fresh import into a realistic "live" state that
// --replace-users must discard: an administrator, a verified email, a changed
// hash, a disabled account, a session, an API token, a non-legacy RSS token, a
// one-time link, a new invitation, and advanced identities.
func populate(t *testing.T, tgt string) {
	t.Helper()
	execSQL(t, tgt,
		`UPDATE magnetico_web.users SET role = 'admin', email_verified_at = now(), password_hash = 'rehashed' WHERE id = 1`,
		`UPDATE magnetico_web.users SET active = false WHERE id = 4`,
		`INSERT INTO magnetico_web.sessions (token_hash, user_id, expires_at, remember) VALUES ('\x0000000000000000000000000000000000000000000000000000000000000003', 1, now() + interval '1 hour', false)`,
		`INSERT INTO magnetico_web.api_tokens (token_hash, user_id) VALUES ('\x0000000000000000000000000000000000000000000000000000000000000001', 1)`,
		`INSERT INTO magnetico_web.rss_tokens (token_hash, user_id) VALUES ('\x0000000000000000000000000000000000000000000000000000000000000002', 1)`,
		`INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, expires_at) VALUES ('\x0000000000000000000000000000000000000000000000000000000000000004', 2, 'reset', now() + interval '1 hour')`,
		`INSERT INTO magnetico_web.invites (owner_id, code) VALUES (1, 'NEWNEWNEWNEWNEWNEWNEWNEW')`,
		`INSERT INTO magnetico_web.users (username) VALUES ('t1'), ('t2'), ('t3')`,
		`DELETE FROM magnetico_web.users WHERE username IN ('t1','t2','t3')`)
}

func TestReplaceUsersGuardedAndAtomic(t *testing.T) {
	src, _ := seeded(t)
	tgt := targetDB(t)
	mustRun(t, cfgFor(src, tgt))
	populate(t, tgt)
	unrelated, srcBefore := unrelatedState(t, tgt), sourceState(t, src)
	before := userState(t, tgt)
	// 1. The preview needs no digest, writes nothing, reports what would be discarded.
	prev := cfgFor(src, tgt)
	prev.DryRun, prev.ReplaceUsers = true, true
	rep := mustRun(t, prev)
	if rep.TargetState != "replace" || rep.PlanDigest == "" || rep.Discarded == nil {
		t.Fatalf("preview: %+v", rep)
	}
	d := rep.Discarded
	if d.Users != 6 || d.Invites != 6 || d.Sessions != 1 || d.APITokens != 1 || d.RSSTokens != 5 || d.NonLegacyRSSTokens != 1 ||
		d.OneTimeLinks != 1 || d.DiffersFromSource != 2 || d.AdminRoles != 1 || d.DisabledAccounts != 1 || d.VerifiedEmails != 1 || d.InvitesNotInSource != 1 {
		t.Fatalf("discard counts: %+v", *d)
	}
	if userState(t, tgt) != before {
		t.Fatal("the preview changed the target")
	}
	digest := rep.PlanDigest

	// 2. A real run needs --confirm-plan, and an old/wrong digest is refused.
	real := cfgFor(src, tgt)
	real.ReplaceUsers = true
	_, err := Run(tctx(t), real)
	wantClass(t, err, ClassConfig)
	real.ConfirmPlan = strings.Repeat("0", 64)
	_, err = Run(tctx(t), real)
	wantClass(t, err, ClassPlanMismatch)
	if userState(t, tgt) != before {
		t.Fatal("a refused replace changed the target")
	}

	// 3. The confirmed run replaces atomically.
	real.ConfirmPlan = digest
	rep = mustRun(t, real)
	if rep.Result != "ok" || rep.Discarded == nil || rep.ImportedUsers != 6 {
		t.Fatalf("replace: %+v", rep)
	}
	if v, err := Verify(tctx(t), cfgFor(src, tgt)); err != nil || v.Result != "verified" {
		t.Fatalf("verify after replace: %v %+v", err, v)
	}
	after := userState(t, tgt)
	for _, gone := range []string{"rehashed", "NEWNEWNEWNEW", "admin"} {
		if strings.Contains(after, gone) {
			t.Errorf("%q survived the replace", gone)
		}
	}
	// Identities never move backward: users next stays 13, invites 14, and the
	// new rss ids continue after the discarded non-legacy token (5 -> ids 6..9).
	for _, want := range []string{"seq users last=13 called=false", "seq invites last=14 called=false", "seq rss_tokens last=10 called=false"} {
		if !strings.Contains(after, want) {
			t.Errorf("after replace missing %q in\n%s", want, after[strings.Index(after, "seq users"):])
		}
	}
	if min := scalar[int64](t, tgt, `SELECT min(id) FROM magnetico_web.rss_tokens`); min != 6 {
		t.Errorf("first rss id %d, want 6", min)
	}
	if unrelatedState(t, tgt) != unrelated {
		t.Error("trackers, settings, web_version or torrent data changed")
	}
	if sourceState(t, src) != srcBefore {
		t.Error("the source changed")
	}
}

func TestReplaceFailureIsFullyRolledBack(t *testing.T) {
	src, _ := seeded(t)
	tgt := targetDB(t)
	mustRun(t, cfgFor(src, tgt))
	populate(t, tgt)
	before := userState(t, tgt)
	prev := cfgFor(src, tgt)
	prev.DryRun, prev.ReplaceUsers = true, true
	digest := mustRun(t, prev).PlanDigest
	for _, stage := range []string{"after-delete", "after-users", "after-restart"} {
		testHook = func(s string, tx pgx.Tx) error {
			if s == stage {
				return fmt.Errorf("injected")
			}
			return nil
		}
		real := cfgFor(src, tgt)
		real.ReplaceUsers, real.ConfirmPlan = true, digest
		_, err := Run(tctx(t), real)
		testHook = nil
		wantClass(t, err, ClassStorage)
		if userState(t, tgt) != before {
			t.Fatalf("a failed replace at %s left changes (rows or sequences)", stage)
		}
	}
}

func TestUnmatchedTargetAccountsAreProtected(t *testing.T) {
	src, _ := seeded(t)
	tgt := targetDB(t)
	mustRun(t, cfgFor(src, tgt))
	execSQL(t, tgt,
		`INSERT INTO magnetico_web.users (id, username) OVERRIDING SYSTEM VALUE VALUES (50, 'operator')`,
		`UPDATE magnetico_web.users SET username = 'Bobby' WHERE id = 2`) // same id, different username: unmatched
	before := userState(t, tgt)

	dry := func(list ...int64) (*Report, error) {
		cfg := cfgFor(src, tgt)
		cfg.DryRun, cfg.ReplaceUsers, cfg.DeleteUnmatchedIDs = true, true, list
		return Run(tctx(t), cfg)
	}
	rep, err := dry()
	e := wantClass(t, err, ClassUnmatchedTarget)
	if got := fmt.Sprint(rep.UnmatchedTargetUserIDs); got != "[2 50]" {
		t.Errorf("unmatched ids %s", got)
	}
	if !strings.Contains(strings.Join(e.Details, " "), "[2,50]") {
		t.Errorf("details %v", e.Details)
	}
	for _, list := range [][]int64{{50}, {2}, {2, 50, 51}, {2, 49}} {
		if _, err := dry(list...); err == nil {
			t.Errorf("list %v accepted", list)
		} else {
			wantClass(t, err, ClassUnmatchedTarget)
		}
	}
	rep, err = dry(2, 50)
	if err != nil || rep.Result != "ok" {
		t.Fatalf("exact list: %v %+v", err, rep)
	}
	digest := rep.PlanDigest

	// The digest binds the deletion scope, the timezone and the operation.
	other, _ := dry(50, 2)
	if other.PlanDigest != digest {
		t.Error("the digest depends on the order of the id list")
	}
	cfg := cfgFor(src, tgt)
	cfg.DryRun, cfg.ReplaceUsers, cfg.DeleteUnmatchedIDs = true, true, []int64{2, 50}
	cfg.SourceTimezone = "Europe/Berlin"
	if rep2 := mustRun(t, cfg); rep2.PlanDigest == digest {
		t.Error("the digest does not bind the source timezone")
	}
	cfg.SourceTimezone, cfg.AcceptNeedsReset = testZone, 0
	cfg.ReplaceUsers, cfg.DeleteUnmatchedIDs = false, nil
	rep3, err := Run(tctx(t), cfg)
	wantClass(t, err, ClassTargetNotEmpty)
	if rep3.PlanDigest == digest {
		t.Error("the digest does not bind the destructive operation")
	}

	// Real runs: a different timezone or list invalidates the confirmation.
	real := cfgFor(src, tgt)
	real.ReplaceUsers, real.DeleteUnmatchedIDs, real.ConfirmPlan = true, []int64{2, 50}, digest
	real.SourceTimezone = "Europe/Berlin"
	_, err = Run(tctx(t), real)
	wantClass(t, err, ClassPlanMismatch)
	real.SourceTimezone = testZone
	real.DeleteUnmatchedIDs = []int64{50, 2, 3}
	_, err = Run(tctx(t), real)
	wantClass(t, err, ClassUnmatchedTarget)
	if userState(t, tgt) != before {
		t.Fatal("a refused run wrote")
	}
	real.DeleteUnmatchedIDs = []int64{2, 50}
	mustRun(t, real)
	if n := scalar[int](t, tgt, `SELECT count(*) FROM magnetico_web.users WHERE username IN ('operator','Bobby')`); n != 0 {
		t.Errorf("%d unmatched accounts survived", n)
	}
	if v, err := Verify(tctx(t), cfgFor(src, tgt)); err != nil || v.Result != "verified" {
		t.Fatalf("verify: %v", err)
	}
}

func TestConfirmationIsInvalidatedByDataChanges(t *testing.T) {
	src, _ := seeded(t)
	tgt := targetDB(t)
	dry := cfgFor(src, tgt)
	dry.DryRun = true
	digest := mustRun(t, dry).PlanDigest

	// A source change after the preview invalidates the confirmation.
	execSQL(t, src, `UPDATE users.users SET email = 'changed@example.org' WHERE id = 1`)
	real := cfgFor(src, tgt)
	real.ConfirmPlan = digest
	_, err := Run(tctx(t), real)
	wantClass(t, err, ClassPlanMismatch)
	if n := scalar[int](t, tgt, `SELECT count(*) FROM magnetico_web.users`); n != 0 {
		t.Fatal("a refused run wrote")
	}
	// The matching digest of the new state is accepted (optional on a non-replace run).
	dry2 := mustRun(t, withDry(real))
	real.ConfirmPlan = dry2.PlanDigest
	mustRun(t, real)
}

func withDry(c Config) Config { c.DryRun, c.ConfirmPlan = true, ""; return c }

func TestSourceChangeDuringTheRunRollsBack(t *testing.T) {
	src, _ := seeded(t)
	tgt := targetDB(t)
	before := userState(t, tgt)
	testHook = func(stage string, tx pgx.Tx) error {
		if stage == "before-drift-check" {
			_, err := admin(t, src).Exec(tctx(t), `UPDATE users.users SET email = 'late@example.org' WHERE id = 2`)
			return err
		}
		return nil
	}
	defer func() { testHook = nil }()
	_, err := Run(tctx(t), cfgFor(src, tgt))
	wantClass(t, err, ClassSourceChanged)
	if userState(t, tgt) != before {
		t.Fatal("source drift left changes in the target (rows or sequences)")
	}
}

// A target with the right rows but an identity behind its maximum id is not
// "already migrated": the next insert would collide.
func TestIdentityBehindItsMaximumIsNotIdentical(t *testing.T) {
	for _, tbl := range []string{"users", "invites", "rss_tokens"} {
		t.Run(tbl, func(t *testing.T) {
			src, _ := seeded(t)
			tgt := targetDB(t)
			mustRun(t, cfgFor(src, tgt))
			execSQL(t, tgt, `ALTER TABLE magnetico_web.`+tbl+` ALTER COLUMN id RESTART WITH 1`)
			before := userState(t, tgt) + seqState(t, tgt)

			for _, dry := range []bool{true, false} {
				cfg := cfgFor(src, tgt)
				cfg.DryRun = dry
				rep, err := Run(tctx(t), cfg)
				wantClass(t, err, ClassTargetNotEmpty)
				if rep != nil && (rep.Result == "already-migrated" || rep.TargetState == "identical") {
					t.Fatalf("dry=%v: reported %q/%q", dry, rep.Result, rep.TargetState)
				}
			}
			if userState(t, tgt)+seqState(t, tgt) != before {
				t.Fatal("a refused run changed the target")
			}

			// --replace-users repairs it and leaves a usable identity.
			prev := cfgFor(src, tgt)
			prev.DryRun, prev.ReplaceUsers = true, true
			real := cfgFor(src, tgt)
			real.ReplaceUsers = true
			real.ConfirmPlan = mustRun(t, prev).PlanDigest
			mustRun(t, real)
			if rep := mustRun(t, cfgFor(src, tgt)); rep.Result != "already-migrated" {
				t.Fatalf("after the replace: %+v", rep)
			}
		})
	}
}

// The identities' current next values are part of the plan: moving one
// between the preview and the real run invalidates the confirmation even
// though no row changed.
func TestConfirmationIsInvalidatedByIdentityChanges(t *testing.T) {
	for _, tbl := range []string{"users", "invites", "rss_tokens"} {
		t.Run(tbl, func(t *testing.T) {
			src, _ := seeded(t)
			tgt := targetDB(t)
			mustRun(t, cfgFor(src, tgt))
			populate(t, tgt)
			prev := cfgFor(src, tgt)
			prev.DryRun, prev.ReplaceUsers = true, true
			digest := mustRun(t, prev).PlanDigest

			execSQL(t, tgt, `ALTER TABLE magnetico_web.`+tbl+` ALTER COLUMN id RESTART WITH 5000`)
			rows := userState(t, tgt)

			real := prev
			real.DryRun, real.ConfirmPlan = false, digest
			_, err := Run(tctx(t), real)
			wantClass(t, err, ClassPlanMismatch)
			if userState(t, tgt) != rows {
				t.Fatal("a refused run changed the rows")
			}
			if again := mustRun(t, withDry(prev)).PlanDigest; again == digest {
				t.Fatal("the plan digest ignores the identity next value")
			}
		})
	}
}
