package users

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const dsnPassword = "SUPERSECRETDSNPASSWORD"

// withPassword appends a password to a trust-authenticated test DSN (the
// server ignores it); it must never appear in any output.
func withPassword(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "password=" + dsnPassword
}

// TestNoSecretsInReportsErrorsOrTargetTokens drives success, dry-run, refusal
// and failure paths and scans every report and error text for any source
// credential. It also checks that the target token table holds no plaintext key.
func TestNoSecretsInReportsErrorsOrTargetTokens(t *testing.T) {
	src, d := seeded(t)
	execSQL(t, src,
		`UPDATE users.users SET password = '$2a$10$`+strings.Repeat("a", 53)+`' WHERE id = 4`,
		`INSERT INTO users.api_tokens (key, user_id, created_at) VALUES ('d0000000000000000000000000000001', 9, now())`)
	d.Tokens = append(d.Tokens, sToken{Key: "d0000000000000000000000000000001"})
	secrets := append(d.secrets(), dsnPassword, "operator", "op@example.org")

	var outputs []string
	collect := func(rep *Report, err error) {
		if rep != nil {
			outputs = append(outputs, reportJSON(t, rep))
		}
		if err != nil {
			outputs = append(outputs, err.Error())
			if e, ok := err.(*Error); ok {
				outputs = append(outputs, e.Class, e.Message, strings.Join(e.Details, "|"))
			}
		}
	}

	tgt := targetDB(t)
	s, g := withPassword(src), withPassword(tgt)

	cfg := cfgFor(s, g)
	cfg.DryRun = true
	collect(Run(tctx(t), cfg)) // needs-reset unacknowledged
	cfg.AcceptNeedsReset = 1
	collect(Run(tctx(t), cfg)) // ok
	cfg.DryRun = false
	// injected storage failures
	testHook = func(stage string, tx pgx.Tx) error {
		if stage == "after-invites" {
			_, err := tx.Exec(tctx(t), `INSERT INTO magnetico_web.invites (id, owner_id, code) OVERRIDING SYSTEM VALUE VALUES (3, 1, 'DUPLICATEDUPLICATEDUPLI')`)
			return err
		}
		return nil
	}
	collect(Run(tctx(t), cfg))
	testHook = func(string, pgx.Tx) error { return errors.New("boom " + d.Users[0].Email + " " + d.Users[0].Hash) }
	collect(Run(tctx(t), cfg))
	testHook = nil
	collect(Run(tctx(t), cfg)) // success
	collect(Verify(tctx(t), cfg))
	collect(Run(tctx(t), cfg)) // already migrated
	// populated target refusals
	execSQL(t, tgt, `INSERT INTO magnetico_web.users (id, username, email) OVERRIDING SYSTEM VALUE VALUES (50, 'operator', 'op@example.org')`)
	collect(Run(tctx(t), cfg))
	cfg.ReplaceUsers, cfg.ConfirmPlan = true, strings.Repeat("0", 64)
	collect(Run(tctx(t), cfg))
	cfg.ConfirmPlan = ""
	collect(Run(tctx(t), cfg))
	// connection and configuration failures carry the password in the DSN
	bad := cfgFor("postgres://127.0.0.1:1/x?password="+dsnPassword, g)
	bad.DryRun = true
	collect(Run(tctx(t), bad))
	collect(Run(tctx(t), Config{SourceDSN: "::not a url::" + dsnPassword, TargetDSN: g, SourceTimezone: testZone, DryRun: true}))

	if len(outputs) < 20 {
		t.Fatalf("only %d outputs collected", len(outputs))
	}
	all := strings.Join(outputs, "\n")
	for _, secret := range secrets {
		if strings.Contains(all, secret) {
			t.Errorf("a report or error contains %q", secret)
		}
	}
	// "boom" proves the injected error text is dropped, not echoed.
	if strings.Contains(all, "boom") {
		t.Error("an arbitrary error string was echoed")
	}

	// No plaintext legacy key reaches the target token tables.
	for _, tbl := range []string{"rss_tokens", "api_tokens"} {
		dump := scalar[string](t, tgt, `SELECT COALESCE(string_agg(t::text, E'\n'), '') FROM magnetico_web.`+tbl+` t`)
		for _, k := range d.Tokens {
			if strings.Contains(dump, k.Key) {
				t.Errorf("%s contains a plaintext legacy token", tbl)
			}
		}
	}
}
