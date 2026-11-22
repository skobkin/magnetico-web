package main

// Real-binary tests of `magnetico web` and `magnetico migrate web-schema-init`. They use only
// disposable PostgreSQL databases from the shared harness and bind loopback
// ports; no BitTorrent or DHT code runs in the `web` command.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/internal/web/webschema"
	"github.com/skobkin/magnetico-web/persistence"
)

// webEnv is a clean MAGNETICO_* environment plus the given variables.
func webEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MAGNETICO_") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// webTarget creates a disposable database holding the 0.8.0 torrent schema.
func webTarget(t *testing.T) string {
	t.Helper()
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := persistence.InitSchema(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func runWebBinary(t *testing.T, bin string, env []string, args ...string) (stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"web"}, args...)...)
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stderr, cmd.Stdout = &buf, &buf
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return buf.String(), 0
	case errors.As(err, &ee):
		return buf.String(), ee.ExitCode()
	}
	t.Fatalf("run: %v", err)
	return
}

func TestWebBinaryRejectsBadConfiguration(t *testing.T) {
	bin := buildBinary(t)
	for name, c := range map[string]struct {
		env  []string
		args []string
		want string
	}{
		"missing public URL":  {nil, nil, "public URL"},
		"unknown variable":    {[]string{"MAGNETICO_WEB_NOPE=1"}, []string{"--public-url", "https://m.example.org"}, "MAGNETICO_WEB_NOPE"},
		"missing config file": {[]string{"MAGNETICO_CONFIG_FILE_PATH=/nonexistent.yml"}, []string{"--public-url", "https://m.example.org"}, "nonexistent.yml"},
	} {
		out, code := runWebBinary(t, bin, webEnv(c.env...), c.args...)
		if code == 0 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, output %q", name, code, out)
		}
	}
}

// The web service fails closed when the dedicated magnetico_web schema is
// absent (it is created only by `migrate web-schema-init`, never by `web`).
func TestWebBinaryFailsClosedWithoutWebSchema(t *testing.T) {
	bin := buildBinary(t)
	dsn := webTarget(t)
	addr := freeLoopbackAddr(t)
	env := webEnv("MAGNETICO_WEB_DATABASE="+dsn, "MAGNETICO_WEB_LISTEN="+addr, "MAGNETICO_WEB_PUBLIC_URL=http://"+addr)

	out, code := runWebBinary(t, bin, env)
	if code != 1 || !strings.Contains(out, "magnetico_web schema is missing") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" && strings.Contains(out, pw) {
			t.Fatalf("database password leaked: %q", out)
		}
	}
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("something listens although startup verification failed")
	}
}

// The success path runs against the production magnetico_web schema, created
// by the same explicit initialization the operator uses.
func TestWebBinaryServesAndStopsOnSIGTERM(t *testing.T) {
	bin := buildBinary(t)
	dsn := webTarget(t)
	initWebSchema(t, dsn)

	addr := freeLoopbackAddr(t)
	cmd := exec.Command(bin, "web")
	cmd.Env = webEnv("MAGNETICO_WEB_DATABASE="+dsn, "MAGNETICO_WEB_LISTEN="+addr,
		"MAGNETICO_WEB_PUBLIC_URL=http://"+addr, "MAGNETICO_WEB_LOG_LEVEL=debug")
	var out bytes.Buffer
	cmd.Stderr, cmd.Stdout = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()

	get := func(path string) (*http.Response, string) {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			return nil, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if resp, _ := get("/readyz"); resp != nil && resp.StatusCode == 200 {
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("web exited early: %v\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready in time:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp, body := get("/robots.txt"); resp.StatusCode != 200 || body != "User-agent: *\nDisallow: /\n" ||
		resp.Header.Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("robots: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, _ := get("/healthz"); resp.StatusCode != 200 {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
	if resp, _ := get("/torrents"); resp.StatusCode != 404 {
		t.Fatalf("no data route may exist in W1, got %d", resp.StatusCode)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit after SIGTERM: %v\n%s", err, out.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no exit after SIGTERM:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "stopped cleanly") {
		t.Fatalf("log lacks the clean-stop line:\n%s", out.String())
	}
}

// A request that is still being sent when SIGTERM arrives keeps the server
// busy; with a 1 s grace period the shutdown is forced and the process must
// exit with status 3 (the incomplete-shutdown convention).
func TestWebBinaryExitsWithStatus3WhenGraceExpires(t *testing.T) {
	bin := buildBinary(t)
	dsn := webTarget(t)
	initWebSchema(t, dsn)

	addr := freeLoopbackAddr(t)
	cmd := exec.Command(bin, "web")
	cmd.Env = webEnv("MAGNETICO_WEB_DATABASE="+dsn, "MAGNETICO_WEB_LISTEN="+addr,
		"MAGNETICO_WEB_PUBLIC_URL=http://"+addr, "MAGNETICO_WEB_SHUTDOWN_TIMEOUT=1s")
	var out bytes.Buffer
	cmd.Stderr, cmd.Stdout = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()

	deadline := time.Now().Add(20 * time.Second)
	for {
		if resp, err := http.Get("http://" + addr + "/readyz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Half a request: the connection stays busy until the header timeout.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("GET /healthz HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 3 {
			t.Fatalf("exit = %v, want status 3\n%s", err, out.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no exit after SIGTERM:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "shutdown incomplete") {
		t.Fatalf("log lacks the incomplete-shutdown line:\n%s", out.String())
	}
}

func initWebSchema(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := webschema.Init(ctx, dsn); err != nil {
		t.Fatal(err)
	}
}

// runWebSchemaInit runs `magnetico migrate <args…>` with the target database
// in MAGNETICO_MIGRATE_DATABASE (the existing migrate convention).
func runWebSchemaInit(t *testing.T, bin, dsn string, args ...string) (output string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"migrate"}, args...)...)
	cmd.Env = webEnv("MAGNETICO_MIGRATE_DATABASE=" + dsn)
	var buf bytes.Buffer
	cmd.Stderr, cmd.Stdout = &buf, &buf
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return buf.String(), 0
	case errors.As(err, &ee):
		return buf.String(), ee.ExitCode()
	}
	t.Fatalf("run: %v", err)
	return
}

func withPassword(t *testing.T, dsn, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(u.User.Username(), password)
	return u.String()
}

func TestMigrateWebSchemaInitBinary(t *testing.T) {
	bin := buildBinary(t)

	t.Run("fresh, repeat and refusal", func(t *testing.T) {
		dsn := webTarget(t)
		secret := "s3cretpw-for-test"
		dsn = withPassword(t, dsn, secret)

		out, code := runWebSchemaInit(t, bin, dsn, "web-schema-init")
		if code != 0 || !strings.Contains(out, "created now") || !strings.Contains(out, "seeded 12") {
			t.Fatalf("first run: exit %d: %s", code, out)
		}
		out, code = runWebSchemaInit(t, bin, dsn, "web-schema-init")
		if code != 0 || !strings.Contains(out, "already up to date") || !strings.Contains(out, "already seeded") {
			t.Fatalf("second run: exit %d: %s", code, out)
		}

		conn, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(context.Background(), `ALTER TABLE magnetico_web.users DROP CONSTRAINT users_role_check`); err != nil {
			t.Fatal(err)
		}
		out, code = runWebSchemaInit(t, bin, dsn, "web-schema-init")
		if code != 1 || !strings.Contains(out, "not the supported version") || !strings.Contains(out, "users_role_check") {
			t.Fatalf("damaged schema: exit %d: %s", code, out)
		}
		// Refused means untouched: the constraint was not recreated.
		var n int
		if err := conn.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_constraint WHERE conname = 'users_role_check'`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("the refused init repaired the schema (n=%d, err=%v)", n, err)
		}
		if strings.Contains(out, secret) {
			t.Fatalf("password leaked: %s", out)
		}
	})

	t.Run("requires the torrent schema", func(t *testing.T) {
		dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
		out, code := runWebSchemaInit(t, bin, dsn, "web-schema-init")
		if code != 1 {
			t.Fatalf("exit %d: %s", code, out)
		}
		conn, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		var exists bool
		if err := conn.QueryRow(context.Background(), `SELECT to_regnamespace('magnetico_web') IS NOT NULL`).Scan(&exists); err != nil || exists {
			t.Fatalf("magnetico_web was created without the torrent schema (exists=%v, err=%v)", exists, err)
		}
	})
}

// docsGrants returns the documented minimum GRANT block, so the test runs
// exactly what operators are told to run.
func docsGrants(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("docs/web-deployment.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	a, b := strings.Index(s, "<!-- grants:begin -->"), strings.Index(s, "<!-- grants:end -->")
	if a < 0 || b < a {
		t.Fatal("grants block missing")
	}
	block := s[a:b]
	i, j := strings.Index(block, "```sql"), strings.LastIndex(block, "```")
	if i < 0 || j <= i {
		t.Fatal("sql fence missing")
	}
	return block[i+len("```sql") : j]
}

// TestWebBinaryStartsWithProductionWebSchema starts the real service, as a
// role holding exactly the documented minimum privileges, against the
// production magnetico_web schema (created by the real init command), and
// then proves that removing the SELECT on magneticod.schema_version makes
// startup fail in a controlled way.
func TestWebBinaryStartsWithProductionWebSchema(t *testing.T) {
	bin := buildBinary(t)
	cl := pgtest.IntegrationCluster(t)
	dsn := webTarget(t)
	if out, code := runWebSchemaInit(t, bin, dsn, "web-schema-init"); code != 0 {
		t.Fatalf("web-schema-init: exit %d: %s", code, out)
	}

	suffix := pgtest.RandomSuffix()
	rt, op := "wrt_"+suffix, "wop_"+suffix
	admin, err := pgx.Connect(context.Background(), cl.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	for _, r := range []string{rt, op} {
		if _, err := admin.Exec(context.Background(), "CREATE ROLE "+pgtest.QuoteIdent(r)+" LOGIN"); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := url.Parse(dsn)
	dbName := strings.TrimPrefix(u.Path, "/")
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), cl.AdminDSN)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgtest.QuoteIdent(dbName)+" WITH (FORCE)")
		for _, r := range []string{op, rt} {
			_, _ = c.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgtest.QuoteIdent(r))
		}
	})
	dbConn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer dbConn.Close(context.Background())
	grants := strings.NewReplacer("magnetico_web_rt", pgtest.QuoteIdent(rt), "magnetico_web_op", pgtest.QuoteIdent(op)).Replace(docsGrants(t))
	if _, err := dbConn.Exec(context.Background(), grants); err != nil {
		t.Fatalf("documented grants: %v", err)
	}
	ru := *u
	ru.User = url.UserPassword(rt, "rt-secret-pw")
	roleDSN := ru.String()

	start := func(extraEnv ...string) (*exec.Cmd, chan error, *bytes.Buffer, string) {
		addr := freeLoopbackAddr(t)
		cmd := exec.Command(bin, "web")
		cmd.Env = webEnv(append([]string{"MAGNETICO_WEB_DATABASE=" + roleDSN, "MAGNETICO_WEB_LISTEN=" + addr,
			"MAGNETICO_WEB_PUBLIC_URL=http://" + addr}, extraEnv...)...)
		var out bytes.Buffer
		cmd.Stderr, cmd.Stdout = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, exited, &out, addr
	}

	// 1. With the documented minimum privileges the service starts and stops.
	cmd, exited, out, addr := start()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if resp, err := http.Get("http://" + addr + "/readyz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		select {
		case err := <-exited:
			t.Fatalf("web exited early as the restricted role: %v\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("exit after SIGTERM: %v\n%s", err, out.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no exit after SIGTERM:\n%s", out.String())
	}

	// 2. Without SELECT on magneticod.schema_version startup fails in a
	// controlled way: exit 1, the missing privilege is named, no listener,
	// no secrets.
	if _, err := dbConn.Exec(context.Background(),
		"REVOKE SELECT ON magneticod.schema_version FROM "+pgtest.QuoteIdent(rt)); err != nil {
		t.Fatal(err)
	}
	_, exited, out, addr = start()
	select {
	case err := <-exited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("exit = %v, want status 1\n%s", err, out.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the service did not fail startup:\n%s", out.String())
	}
	msg := out.String()
	if !strings.Contains(msg, "lacks privileges") || !strings.Contains(msg, "magneticod.schema_version: SELECT") {
		t.Fatalf("startup failure is not controlled/actionable:\n%s", msg)
	}
	if strings.Contains(msg, "rt-secret-pw") || strings.Contains(msg, "SQLSTATE") {
		t.Fatalf("secret or raw driver error in output:\n%s", msg)
	}
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("something listens although startup verification failed")
	}
}

// Structural diagnostics reach the operator and startup logs, so they must
// name the damaged object only and never echo catalog contents such as a
// column default.
func TestSchemaDiagnosticsDoNotLeakCatalogContentsBinary(t *testing.T) {
	bin := buildBinary(t)
	dsn := webTarget(t)
	const secret = "s3cr3t-DEFAULT-canary-7f3a"
	if out, code := runWebSchemaInit(t, bin, dsn, "web-schema-init"); code != 0 {
		t.Fatalf("web-schema-init: exit %d: %s", code, out)
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(),
		`ALTER TABLE magnetico_web.users ALTER COLUMN role SET DEFAULT '`+secret+`'`); err != nil {
		t.Fatal(err)
	}

	out, code := runWebSchemaInit(t, bin, dsn, "web-schema-init")
	if code != 1 || !strings.Contains(out, "column magnetico_web.users.role differs") {
		t.Fatalf("init: exit %d: %s", code, out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("init output leaks catalog contents: %s", out)
	}

	addr := freeLoopbackAddr(t)
	env := webEnv("MAGNETICO_WEB_DATABASE="+dsn, "MAGNETICO_WEB_LISTEN="+addr, "MAGNETICO_WEB_PUBLIC_URL=http://"+addr)
	out, code = runWebBinary(t, bin, env)
	if code != 1 || !strings.Contains(out, "column magnetico_web.users.role differs") {
		t.Fatalf("web: exit %d: %s", code, out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("startup output leaks catalog contents: %s", out)
	}
}
