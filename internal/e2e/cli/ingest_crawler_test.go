package cli

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// runIngestCrawlerBinary runs `magnetico ingest crawler …` through the real
// binary against dsn.
func runIngestCrawlerBinary(t *testing.T, bin, dsn string, extraEnv []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"ingest", "crawler"}, args...)...)
	cmd.Env = webEnv(append([]string{"MAGNETICO_INGEST_DATABASE=" + dsn}, extraEnv...)...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return so.String(), se.String(), 0
	case errors.As(err, &ee):
		return so.String(), se.String(), ee.ExitCode()
	}
	t.Fatalf("run: %v", err)
	return
}

// registerCrawler adds a crawler through the real binary and returns its token.
func registerCrawler(t *testing.T, bin, dsn, name string) string {
	t.Helper()
	out, errOut, code := runIngestCrawlerBinary(t, bin, dsn, nil, "add", name)
	if code != 0 {
		t.Fatalf("ingest crawler add %s: exit %d\n%s%s", name, code, out, errOut)
	}
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(l, "token: "); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no token in the output of ingest crawler add:\n%s", out)
	return ""
}

// TestIngestCrawlerCommandsBinary drives the operator commands through the real
// binary: results on stdout, failures controlled on stderr, the token exactly
// once, no DSN or secret echoed.
func TestIngestCrawlerCommandsBinary(t *testing.T) {
	bin := buildBinary(t)
	secret := "db-secret-pw-for-test"
	dsn := withPassword(t, webTarget(t), secret)
	if out, code := runWebSchemaInit(t, bin, dsn, "ingest-schema-init"); code != 0 {
		t.Fatalf("ingest-schema-init: %d %s", code, out)
	}
	run := func(args ...string) (string, string, int) {
		return runIngestCrawlerBinary(t, bin, dsn, nil, args...)
	}
	noLeak := func(what, out, errOut string) {
		t.Helper()
		for _, bad := range []string{secret, "postgres://"} {
			if strings.Contains(out+errOut, bad) {
				t.Fatalf("%s leaks %q:\nstdout: %s\nstderr: %s", what, bad, out, errOut)
			}
		}
	}

	out, errOut, code := run("add", "alpha")
	noLeak("add", out, errOut)
	if code != 0 || !strings.Contains(out, "token: mgc_") || strings.Count(out, "mgc_") != 1 {
		t.Fatalf("add: exit %d\n%s%s", code, out, errOut)
	}
	token := strings.TrimSpace(strings.TrimPrefix(out[strings.Index(out, "token: "):strings.Index(out, "token: ")+80], "token: "))
	token = strings.Fields(token)[0]
	for _, name := range []string{"alpha", "ALPHA"} {
		out, errOut, code = run("add", name)
		noLeak("duplicate add", out, errOut)
		if code != 1 || !strings.Contains(errOut, "already exists") {
			t.Fatalf("add %s again: exit %d\n%s%s", name, code, out, errOut)
		}
	}
	if out, errOut, code = run("add", "bad name!"); code != 1 || !strings.Contains(errOut, "invalid crawler name") {
		t.Fatalf("invalid name: %d %s%s", code, out, errOut)
	}
	out, errOut, code = run("list")
	noLeak("list", out, errOut)
	if code != 0 || !strings.Contains(out, "alpha") || strings.Contains(out, "mgc_") || strings.Contains(out, token) {
		t.Fatalf("list: exit %d\n%s%s", code, out, errOut)
	}
	out, errOut, code = run("rotate-token", "alpha")
	noLeak("rotate", out, errOut)
	if code != 0 || !strings.Contains(out, "token: mgc_") || strings.Contains(out, token) || !strings.Contains(out, "60 seconds") {
		t.Fatalf("rotate: exit %d\n%s%s", code, out, errOut)
	}
	for _, c := range [][]string{{"disable", "alpha"}, {"enable", "alpha"}} {
		out, errOut, code = run(c...)
		noLeak(c[0], out, errOut)
		if code != 0 || !strings.Contains(out, "60 seconds") {
			t.Fatalf("%v: exit %d\n%s%s", c, code, out, errOut)
		}
	}
	for _, c := range [][]string{{"disable", "nobody"}, {"enable", "nobody"}, {"rotate-token", "nobody"}} {
		out, errOut, code = run(c...)
		noLeak(c[0], out, errOut)
		if code != 1 || !strings.Contains(errOut, "crawler not found") {
			t.Fatalf("%v: exit %d\n%s%s", c, code, out, errOut)
		}
	}
}

// An older development schema (without crawlers) is refused with the
// reinitialisation steps and left exactly as it was, by the commands and by the
// service; a role without privileges gets a controlled error.
func TestIngestCrawlerCommandsRefuseAnIncompatibleSchemaAndMissingPrivileges(t *testing.T) {
	bin := buildBinary(t)
	secret := "db-secret-pw-for-test"
	admin := webTarget(t)
	if out, code := runWebSchemaInit(t, bin, admin, "ingest-schema-init"); code != 0 {
		t.Fatalf("ingest-schema-init: %d %s", code, out)
	}
	dsn := withPassword(t, admin, secret)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// Missing privileges first: a role that can read the schema but not write crawlers.
	cl := pgtest.IntegrationCluster(t)
	role := "crawler_ro_" + pgtest.RandomSuffix()
	for _, q := range []string{
		`CREATE ROLE ` + pgtest.QuoteIdent(role) + ` LOGIN`,
		`GRANT USAGE ON SCHEMA magnetico_ingest TO ` + pgtest.QuoteIdent(role),
		`GRANT SELECT ON ALL TABLES IN SCHEMA magnetico_ingest TO ` + pgtest.QuoteIdent(role),
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), `DROP OWNED BY `+pgtest.QuoteIdent(role))
			c.Close(context.Background())
		}
		if a, err := pgx.Connect(context.Background(), cl.AdminDSN); err == nil {
			_, _ = a.Exec(context.Background(), `DROP ROLE IF EXISTS `+pgtest.QuoteIdent(role))
			a.Close(context.Background())
		}
	})
	u, _ := url.Parse(admin)
	u.User = url.UserPassword(role, secret)
	out, errOut, code := runIngestCrawlerBinary(t, bin, u.String(), nil, "add", "alpha")
	if code != 1 || !strings.Contains(errOut, "lacks a privilege on magnetico_ingest.crawlers") || strings.Contains(out+errOut, secret) {
		t.Fatalf("add without privileges: exit %d\n%s%s", code, out, errOut)
	}

	// An older development schema.
	if _, err := conn.Exec(ctx, `DROP TABLE magnetico_ingest.crawlers`); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"list"}, {"add", "alpha"}} {
		out, errOut, code = runIngestCrawlerBinary(t, bin, dsn, nil, args...)
		for _, want := range []string{"missing table crawlers", "DROP SCHEMA magnetico_ingest CASCADE", "UPGRADE.md"} {
			if code != 1 || !strings.Contains(errOut, want) {
				t.Fatalf("%v: exit %d, stderr lacks %q:\n%s%s", args, code, want, out, errOut)
			}
		}
		if strings.Contains(out+errOut, secret) {
			t.Fatalf("%v leaks the password", args)
		}
	}
	// The service refuses it too, and neither run recreated the table.
	addr := freeLoopbackAddr(t)
	cmd := exec.Command(bin, "ingest")
	cmd.Env = webEnv("MAGNETICO_INGEST_DATABASE="+dsn, "MAGNETICO_INGEST_LISTEN="+addr)
	b, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(b), "missing table crawlers") || strings.Contains(string(b), secret) {
		t.Fatalf("ingest over the old schema: %v\n%s", err, b)
	}
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('magnetico_ingest.crawlers') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("a refused command recreated crawlers (%v)", err)
	}
	// And the removed shared token is refused with a message.
	cmd = exec.Command(bin, "ingest")
	cmd.Env = webEnv("MAGNETICO_INGEST_TOKEN=whatever-value", "MAGNETICO_INGEST_DATABASE="+dsn)
	b, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(b), "ingest crawler add") || strings.Contains(string(b), "whatever-value") {
		t.Fatalf("shared token variable: %v\n%s", err, b)
	}
}
