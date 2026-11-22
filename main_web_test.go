package main

// Real-binary tests of `magnetico web` (milestone W1). They use only
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
		"missing public URL": {nil, nil, "public URL"},
		"unknown variable":   {[]string{"MAGNETICO_WEB_NOPE=1"}, []string{"--public-url", "https://m.example.org"}, "MAGNETICO_WEB_NOPE"},
		"config file":        {[]string{"MAGNETICO_CONFIG_FILE_PATH=/nonexistent.yml"}, []string{"--public-url", "https://m.example.org"}, "does not read a configuration file"},
	} {
		out, code := runWebBinary(t, bin, webEnv(c.env...), c.args...)
		if code == 0 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, output %q", name, code, out)
		}
	}
}

// The web service fails closed when the dedicated magnetico_web schema is
// absent — which is the state of every real database until W2 delivers the
// approved schema and its initialization command.
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

// The success path needs a web schema. The stand-in below exists only inside
// the disposable test database to exercise the process lifecycle; it is not
// the production DDL (milestone W2).
func TestWebBinaryServesAndStopsOnSIGTERM(t *testing.T) {
	bin := buildBinary(t)
	dsn := webTarget(t)
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(context.Background(),
		`CREATE SCHEMA magnetico_web; CREATE TABLE magnetico_web.schema_version (version int)`); err != nil {
		t.Fatal(err)
	}
	conn.Close(context.Background())

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

// TestWebBinaryStartsWithProductionWebSchema is the real-binary startup test
// against the approved production magnetico_web schema.
func TestWebBinaryStartsWithProductionWebSchema(t *testing.T) {
	t.Skip("blocked on milestone W2: the approved magnetico_web DDL and its explicit initialization command do not exist yet")
}
