package main

// Real-binary test of W8 (`migrate users`): the legacy PHP accounts are
// imported from a synthetic legacy database into a disposable magnetico_web
// schema, and the migrated accounts then work in the real `magnetico web`
// service. Disposable PostgreSQL only; synthetic data only; loopback only.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/pgtest"
)

type usersFixture struct{ name, hash, pw, email string }

// usersFixtureHash returns a PHP-generated hash of the family prefix with a
// short ordinary password.
func usersFixtureHash(t *testing.T, params string) (hash, pw string) {
	t.Helper()
	raw, err := os.ReadFile("internal/web/password/testdata/php/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			Hash        string `json:"hash"`
			PasswordB64 string `json:"password_b64"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Cases {
		if !strings.Contains(c.Hash, params) {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(c.PasswordB64)
		if err == nil && len(b) >= 8 && len(b) < 72 {
			return c.Hash, string(b)
		}
	}
	t.Fatalf("no fixture for %s", params)
	return "", ""
}

var planDigestRe = regexp.MustCompile(`plan digest: ([0-9a-f]{64})`)

func TestMigrateUsersBinaryEndToEnd(t *testing.T) {
	bin := buildBinary(t)
	cl := pgtest.IntegrationCluster(t)

	// Synthetic legacy database (the five Doctrine migrations replayed).
	legacy := cl.TestDatabase(t)
	ddl, err := os.ReadFile("internal/migration/users/testdata/legacy_users.sql")
	if err != nil {
		t.Fatal(err)
	}
	execSmokeSQL(t, legacy, string(ddl))
	sodium, sodiumPW := usersFixtureHash(t, "$argon2id$v=19$m=65536,t=4,p=1$")
	bcr, bcrPW := usersFixtureHash(t, "$2y$10$")
	users := []usersFixture{{"alice", sodium, sodiumPW, "alice@example.org"}, {"Bob", bcr, bcrPW, "bob@example.org"}}
	const (
		aliceKey = "a0000000000000000000000000000001"
		bobKey   = "b0000000000000000000000000000001"
		code     = "0123456789abcdef0123456789abcdef"
	)
	execSmokeSQL(t, legacy, `
INSERT INTO users.users (id, username, password, email, roles, created_at) VALUES
  (1, 'alice', '`+sodium+`', 'alice@example.org', '["ROLE_USER"]', '2021-01-15 10:00:00'),
  (2, 'Bob',   '`+bcr+`',    'bob@example.org',   '["ROLE_USER"]', '2021-07-15 10:00:00');
INSERT INTO users.invites (id, user_id, used_by_id, code) VALUES (5, 1, NULL, '`+code+`');
INSERT INTO users.api_tokens (key, user_id, created_at) VALUES
  ('`+aliceKey+`', 1, '2021-02-01 00:00:00'), ('`+bobKey+`', 2, '2021-03-01 00:00:00');`)

	tgt := webTarget(t)
	if out, c := runWebSchemaInit(t, bin, tgt, "web-schema-init"); c != 0 {
		t.Fatalf("web-schema-init: %d %s", c, out)
	}
	rtDSN, _ := webAuthRoles(t, tgt)

	const dsnSecret = "legacy-dsn-secret-pw"
	srcDSN := withPassword(t, legacy, dsnSecret)
	tgtDSN := withPassword(t, tgt, dsnSecret)
	run := func(args ...string) (string, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append([]string{"migrate", "users"}, args...)...)
		cmd.Env = webEnv("MAGNETICO_MIGRATE_DATABASE="+tgtDSN, "MAGNETICO_MIGRATION_USERS_SOURCE_DSN="+srcDSN)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		if err == nil {
			return out.String(), 0
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return out.String(), ee.ExitCode()
		}
		t.Fatal(err)
		return "", -1
	}
	var outputs []string
	check := func(out string) string { outputs = append(outputs, out); return out }

	// 1. Refusals: no zone; no offline assertion.
	if out, c := run(); c != 1 || !strings.Contains(check(out), "[config]") || !strings.Contains(out, "--source-timezone") {
		t.Fatalf("no timezone: %d %s", c, out)
	}
	if out, c := run("--source-timezone", "America/New_York"); c != 1 || !strings.Contains(check(out), "[source-writes-not-asserted]") {
		t.Fatalf("no assertion: %d %s", c, out)
	}
	// 2. Read-only dry-run prints the plan digest and writes nothing.
	out, c := run("--source-timezone", "America/New_York", "--dry-run")
	if c != 0 {
		t.Fatalf("dry-run: %d %s", c, out)
	}
	check(out)
	m := planDigestRe.FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "source: users=2 invites=1 (used 0) legacy_api_tokens=2") {
		t.Fatalf("dry-run output:\n%s", out)
	}
	if n := sqlCount(t, tgt, `SELECT count(*) FROM magnetico_web.users`); n != 0 {
		t.Fatalf("a dry-run wrote %d users", n)
	}
	// 3. The final run, with the confirmed digest and a 0600 report.
	report := t.TempDir() + "/report.json"
	out, c = run("--source-timezone", "America/New_York", "--source-writes-stopped", "--confirm-plan", m[1], "--report-json", report)
	if c != 0 || !strings.Contains(check(out), "result=ok") {
		t.Fatalf("final run: %d %s", c, out)
	}
	st, err := os.Stat(report)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("report file: %v %v", err, st)
	}
	raw, _ := os.ReadFile(report)
	check(string(raw))
	// The report file is never overwritten.
	before := string(raw)
	if _, c := run("--source-timezone", "America/New_York", "--dry-run", "--report-json", report); c == 0 {
		t.Error("an existing report file was overwritten")
	}
	if after, _ := os.ReadFile(report); string(after) != before {
		t.Error("the report file changed")
	}
	// 4. verify reconciles; a repeated run is a no-op.
	vout, vc := runVerify(t, bin, tgtDSN, srcDSN, "America/New_York")
	if vc != 0 || !strings.Contains(check(vout), "result=verified") {
		t.Fatalf("verify: %d %s", vc, vout)
	}
	if out, c := run("--source-timezone", "America/New_York", "--source-writes-stopped"); c != 0 || !strings.Contains(check(out), "already-migrated") {
		t.Fatalf("repeat: %d %s", c, out)
	}
	if n := sqlCount(t, tgt, `SELECT count(*) FROM magnetico_web.api_tokens`); n != 0 {
		t.Errorf("%d api_tokens", n)
	}
	if n := sqlCount(t, tgt, `SELECT count(*) FROM magnetico_web.rss_tokens WHERE legacy`); n != 2 {
		t.Errorf("%d legacy rss tokens", n)
	}

	// 5. The migrated accounts work in the real service.
	w := startWeb(t, bin, rtDSN, "http")
	base := "http://" + w.addr
	for _, u := range users {
		b := newBrowser(t, w.addr)
		if resp := b.login(u.name, "definitely not it"); resp.StatusCode == 303 {
			t.Fatalf("%s: a wrong password logged in", u.name)
		}
		b = newBrowser(t, w.addr)
		if resp := b.login(u.name, u.pw); resp.StatusCode != 303 {
			t.Fatalf("%s: legacy login: %d\n%s", u.name, resp.StatusCode, b.body)
		}
	}
	// A migrated historical address is unverified. The service here has no SMTP,
	// so the page says recovery is unavailable and that the address cannot be
	// verified (never the generic "verify your address" instruction), offers no
	// verification action and still lets the user remove the address (W9).
	ab := newBrowser(t, w.addr)
	if resp := ab.login("alice", sodiumPW); resp.StatusCode != 303 {
		t.Fatalf("alice login: %d", resp.StatusCode)
	}
	ab.get("/account?tab=profile")
	for _, want := range []string{"alice@example.org", "Unverified", "Password recovery is unavailable.", "cannot currently send email, so this address cannot be verified", `action="/account/email/remove"`} {
		if !strings.Contains(ab.body, want) {
			t.Errorf("migrated profile page lacks %q", want)
		}
	}
	for _, bad := range []string{"Verify your email address", `action="/account/email/resend"`, `action="/account/email"`, "Send verification email"} {
		if strings.Contains(ab.body, bad) {
			t.Errorf("migrated profile page without SMTP contains %q", bad)
		}
	}
	if resp := ab.post("/account/email/remove", url.Values{"current_password": {"not the password"}, "csrf_token": {ab.token()}}); resp.StatusCode != 400 {
		t.Errorf("removal with a wrong password: %d", resp.StatusCode)
	}
	if resp := ab.post("/account/email/remove", url.Values{"current_password": {sodiumPW}, "csrf_token": {ab.token()}}); resp.StatusCode != 303 {
		t.Errorf("removal: %d\n%s", resp.StatusCode, ab.body)
	}
	if n := sqlCount(t, tgt, `SELECT count(*) FROM magnetico_web.users WHERE username = 'alice' AND email IS NULL`); n != 1 {
		t.Errorf("alice's address was not removed")
	}
	if n := sqlCount(t, tgt, `SELECT count(*) FROM magnetico_web.users WHERE username = 'Bob' AND email = 'bob@example.org'`); n != 1 {
		t.Errorf("Bob's address changed")
	}
	if n := sqlCount(t, tgt, `SELECT count(*) FROM magnetico_web.users WHERE password_hash LIKE '$argon2id$v=19$m=65536,t=3,p=4$%'`); n != 2 {
		t.Errorf("%d accounts were rehashed at login, want 2", n)
	}
	for _, key := range []string{aliceKey, bobKey} {
		for name, hdr := range map[string]map[string]string{"header": {"api-token": key}, "cookie": {"Cookie": "api-token=" + key}} {
			if code, _, body := plain(t, base, "/api/v1/feed/rss/last", hdr); code != 200 || !strings.Contains(body, "<rss") {
				t.Errorf("legacy token via %s on the v1 feed: %d", name, code)
			}
		}
		if code, _, _ := plain(t, base, "/api/v1/feed/rss/last?api-token="+key, nil); code != 200 {
			t.Errorf("legacy token via query on the v1 feed: %d", code)
		}
		if code, _, _ := plain(t, base, "/api/v2/torrents", map[string]string{"Authorization": "Bearer " + key}); code != http.StatusUnauthorized {
			t.Errorf("a legacy token on the JSON API: %d, want 401", code)
		}
	}

	// 6. Nothing secret reached any migrate output or report.
	all := strings.Join(outputs, "\n")
	secrets := []string{dsnSecret, sodium, bcr, aliceKey, bobKey, code, "alice@example.org", "bob@example.org", "alice", "Bob"}
	for _, s := range secrets {
		if strings.Contains(all, s) {
			t.Errorf("migrate output contains %q", s)
		}
	}
}

func runVerify(t *testing.T, bin, tgtDSN, srcDSN, zone string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "migrate", "users", "verify", "--source-timezone", zone)
	cmd.Env = webEnv("MAGNETICO_MIGRATE_DATABASE="+tgtDSN, "MAGNETICO_MIGRATION_USERS_SOURCE_DSN="+srcDSN)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return out.String(), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return out.String(), ee.ExitCode()
	}
	t.Fatal(err)
	return "", -1
}
