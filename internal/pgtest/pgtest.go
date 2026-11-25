// Package pgtest provides the shared disposable-PostgreSQL integration
// harness used by every package that needs real PostgreSQL in its tests
// (persistence, internal/migration). It is not part of the production
// program; only test files import it.
//
// The cluster is bootstrapped lazily on the first integration test (never in
// TestMain, never for unit-only runs). Sources, in order:
//
//  1. MAGNETICO_TEST_POSTGRES_DSN — explicit operator authorization for a
//     DISPOSABLE server. Must be a postgres:// URL. The harness creates and
//     drops only `magnetico_test_<random>` databases on it and refuses to
//     touch anything else.
//  2. Docker with a locally cached postgres:17 image (never pulled, never
//     bound beyond 127.0.0.1), removed at teardown.
//  3. initdb/postgres binaries on PATH into a temporary directory.
//  4. Otherwise every integration test skips with an actionable message.
//
// An external server must use trust authentication: tests derive role DSNs
// without a password, so a password-protected server fails them.
//
// Set MAGNETICO_TEST_REQUIRE_POSTGRES=1 (CI does) to fail instead of skip when
// no server can be obtained.
//
// The harness never probes well-known ports or auto-discovers real servers.
// Each package's test binary bootstraps its own cluster; the per-test
// database names are globally unique, so concurrent package runs are safe.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	dockerImage      = "postgres:17"
	testDBPrefix     = "magnetico_test_"
	bootstrapTimeout = 60 * time.Second

	// DBOpTimeout bounds a single administrative operation (create/drop test
	// database, role setup) inside a test. Exported so harness shims can
	// reuse the same bound.
	DBOpTimeout = 20 * time.Second
)

// Cluster is one disposable PostgreSQL server. AdminDSN connects to its
// maintenance database ("postgres"); External reports a server the operator
// pointed at (never stopped); Stop, when non-nil, tears the server down.
// SocketDir is the Unix-socket directory of a locally started cluster
// (empty for Docker/external servers).
type Cluster struct {
	AdminDSN  string
	External  bool
	SocketDir string
	Stop      func() // nil for external servers
}

var (
	clusterOnce sync.Once
	cluster     *Cluster
	clusterErr  error
)

// RequireEnv, when set to 1, turns "no disposable PostgreSQL available" from a
// skip into a failure. CI sets it so that a pipeline cannot go green because
// every integration test silently skipped.
const RequireEnv = "MAGNETICO_TEST_REQUIRE_POSTGRES"

// IntegrationCluster returns the shared disposable cluster. When none can be
// obtained the test skips, or fails when RequireEnv is 1.
func IntegrationCluster(t *testing.T) *Cluster {
	t.Helper()
	clusterOnce.Do(func() {
		cluster, clusterErr = bootstrapCluster()
		if clusterErr != nil {
			cluster = nil
		}
	})
	if clusterErr != nil {
		if os.Getenv(RequireEnv) == "1" {
			t.Fatalf("PostgreSQL integration harness unavailable although %s=1: %v", RequireEnv, clusterErr)
		}
		t.Skipf("PostgreSQL integration harness unavailable: %v", clusterErr)
	}
	return cluster
}

// RunTests is the TestMain body for packages that use the PostgreSQL
// integration harness: it runs the package tests, then stops this process's
// bootstrapped cluster (external servers are never stopped). Without it,
// every `go test ./...` package process leaked one disposable PostgreSQL
// container for the lifetime of the Docker host — dozens of idle servers
// across a full battery. The cluster is bootstrapped lazily, so packages
// that end up touching no database pay nothing and leave nothing behind.
func RunTests(m *testing.M) int {
	code := m.Run()
	// m.Run has returned: no test goroutines remain, so the once-guarded
	// cluster needs no synchronization here.
	if cluster != nil && cluster.Stop != nil {
		cluster.Stop()
	}
	return code
}

func bootstrapCluster() (*Cluster, error) {
	if raw := os.Getenv("MAGNETICO_TEST_POSTGRES_DSN"); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" ||
			parsed.Path != "/postgres" {
			return nil, errors.New(
				"MAGNETICO_TEST_POSTGRES_DSN must be a postgres:// URL of a DISPOSABLE server ending in /postgres")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, raw)
		if err != nil {
			return nil, fmt.Errorf("MAGNETICO_TEST_POSTGRES_DSN unreachable: %w", err)
		}
		conn.Close(context.Background())
		return &Cluster{AdminDSN: raw, External: true}, nil
	}

	if c, err := bootstrapDocker(); err == nil {
		return c, nil
	}
	if c, err := bootstrapLocalBinaries(); err == nil {
		return c, nil
	}
	return nil, errors.New(
		"no disposable PostgreSQL available; set MAGNETICO_TEST_POSTGRES_DSN, " +
			"or install docker with the locally cached " + dockerImage + " image, " +
			"or provide initdb/postgres on PATH")
}

func bootstrapDocker() (*Cluster, error) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return nil, err
	}
	// Only a locally cached image is used; never a network pull.
	if err := exec.Command(docker, "image", "inspect", dockerImage).Run(); err != nil {
		return nil, fmt.Errorf("%s not cached locally", dockerImage)
	}

	out, err := exec.Command(docker, "run", "-d", "--rm",
		"-p", "127.0.0.1::5432",
		"-e", "POSTGRES_HOST_AUTH_METHOD=trust",
		dockerImage,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("docker run: %w", err)
	}
	containerID := strings.TrimSpace(string(out))

	port, err := func() (string, error) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			out, err := exec.Command(docker, "port", containerID, "5432").Output()
			if err == nil {
				line := strings.TrimSpace(string(out))
				// "127.0.0.1:34568"
				if idx := strings.LastIndex(line, ":"); idx >= 0 {
					return line, nil
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		return "", errors.New("docker port lookup timed out")
	}()
	if err != nil {
		_ = exec.Command(docker, "stop", containerID).Run()
		return nil, err
	}

	adminDSN := fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=disable", port)
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapTimeout)
	defer cancel()
	if err := waitForPostgres(ctx, adminDSN); err != nil {
		_ = exec.Command(docker, "stop", containerID).Run()
		return nil, err
	}

	return &Cluster{
		AdminDSN: adminDSN,
		Stop:     func() { _ = exec.Command(docker, "stop", containerID).Run() },
	}, nil
}

func bootstrapLocalBinaries() (*Cluster, error) {
	initdb, err := exec.LookPath("initdb")
	if err != nil {
		return nil, err
	}
	postgresBin, err := exec.LookPath("postgres")
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "magnetico-pgtest-")
	if err != nil {
		return nil, err
	}
	if out, err := exec.Command(initdb, "-D", dir, "--auth=trust", "-U", "postgres").CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("initdb: %v: %s", err, out)
	}

	socketDir := filepath.Join(dir, "sock")
	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	// PostgreSQL does not accept port 0; claim a free port and hand it over.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	// Small race between Close and the server binding — acceptable for a
	// disposable test cluster.
	listener.Close()

	cmd := exec.Command(postgresBin, "-D", dir, "-p", strconv.Itoa(port),
		"-c", "listen_addresses=127.0.0.1",
		"-k", socketDir,
	)
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	stop := func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
		os.RemoveAll(dir)
	}

	adminDSN := fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port)

	ctx, cancel := context.WithTimeout(context.Background(), bootstrapTimeout)
	defer cancel()
	if err := waitForPostgres(ctx, adminDSN); err != nil {
		stop()
		return nil, err
	}
	return &Cluster{AdminDSN: adminDSN, SocketDir: socketDir, Stop: stop}, nil
}

func waitForPostgres(ctx context.Context, dsn string) error {
	deadline := time.Now().Add(bootstrapTimeout)
	for time.Now().Before(deadline) {
		connCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		conn, err := pgx.Connect(connCtx, dsn)
		cancel()
		if err == nil {
			conn.Close(context.Background())
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("PostgreSQL readiness timeout")
}

// TestDatabase creates an isolated database for one test and registers its
// drop with t.Cleanup. Only magnetico_test_<random> databases are ever
// created or dropped. The returned DSN is the admin DSN with its path
// swapped to the new database.
func (c *Cluster) TestDatabase(t *testing.T) string {
	t.Helper()
	name := testDBPrefix + RandomSuffix()

	ctx, cancel := context.WithTimeout(context.Background(), DBOpTimeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, c.AdminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, QuoteIdent(name))); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		// Two independent budgets, each with its own connection: inspecting the
		// leftover connections may fail or use up its whole budget, and the
		// drop is still attempted with a fresh one.
		c.reportLeftoverConnections(t, name)
		c.dropTestDatabase(t, name)
	})

	parsed, err := url.Parse(c.AdminDSN)
	if err != nil {
		t.Fatalf("parse admin DSN: %v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}

// dropTestDatabase drops the test database with its own connection and
// budget. The drop is forced: connections still open never block it.
func (c *Cluster) dropTestDatabase(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, c.AdminDSN)
	if err != nil {
		t.Errorf("drop test database %s: connect: %v", name, err)
		return
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx,
		fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, QuoteIdent(name))); err != nil {
		t.Errorf("drop test database %s: %v", name, err)
	}
}

// leftoverGrace bounds how long the cleanup waits for the server to retire the
// connections of a test that closed its pools (a backend exits shortly after
// its client's Terminate) before it reports the rest as left open.
// leftoverInspectTimeout bounds the whole inspection, connecting included.
// Variables only so the harness's own tests can shorten them.
var (
	leftoverGrace          = 10 * time.Second
	leftoverInspectTimeout = leftoverGrace + 10*time.Second
)

// inspectLeftovers is leftoverBackends; the harness's own tests replace it to
// make the inspection fail or hang.
var inspectLeftovers = leftoverBackends

// leftoverWaived holds, per test database, the backend pids a test declared
// with AllowConnectionsOpenNow.
var leftoverWaived sync.Map // database name -> map[int32]bool

// reportLeftoverConnections fails the test for every PostgreSQL client
// connection still open to its database once the test has ended, except the
// declared ones. It sees connections only: a goroutine or worker that holds
// none is invisible to it.
func (c *Cluster) reportLeftoverConnections(t *testing.T, name string) {
	t.Helper()
	waivedAny, _ := leftoverWaived.LoadAndDelete(name)
	waived, _ := waivedAny.(map[int32]bool)

	ctx, cancel := context.WithTimeout(context.Background(), leftoverInspectTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, c.AdminDSN)
	if err != nil {
		t.Errorf("test database %s: inspect its connections: connect: %v", name, err)
		return
	}
	defer conn.Close(context.Background())
	left, err := inspectLeftovers(ctx, conn, name, leftoverGrace, waived)
	if err != nil {
		t.Errorf("test database %s: inspect its connections: %v", name, err)
		return
	}
	for _, b := range left {
		t.Errorf("test database %s: a PostgreSQL connection was left open after the test (a pool or connection not closed): %s", name, b.desc)
	}
}

// AllowConnectionsOpenNow declares that the PostgreSQL connections open to the
// test database at this moment may outlive the test: typically the pools of
// an instance the test proves is abandoned with its pools left open. Only
// those connections are waived; one opened later and left open still fails
// the test. The forced drop ends them all. It returns how many it waived.
func AllowConnectionsOpenNow(t *testing.T, dsn string) int {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test database DSN: %v", err)
	}
	name := strings.TrimPrefix(parsed.Path, "/")
	ctx, cancel := context.WithTimeout(context.Background(), DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, IntegrationCluster(t).AdminDSN)
	if err != nil {
		t.Fatalf("waive connections: connect admin: %v", err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `SELECT pid FROM pg_stat_activity
        WHERE datname = $1 AND pid <> pg_backend_pid() AND backend_type = 'client backend'`, name)
	if err != nil {
		t.Fatalf("waive connections: %v", err)
	}
	pids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		t.Fatalf("waive connections: %v", err)
	}
	waived := map[int32]bool{}
	if prev, ok := leftoverWaived.Load(name); ok {
		for pid := range prev.(map[int32]bool) {
			waived[pid] = true
		}
	}
	for _, pid := range pids {
		waived[pid] = true
	}
	leftoverWaived.Store(name, waived)
	return len(pids)
}

// leftoverBackend is one client connection still open to a test database.
type leftoverBackend struct {
	pid  int32
	desc string // pid, state, application name and last statement
}

// leftoverBackends lists the client connections still open to the database
// and not waived, after waiting up to grace for them to disappear.
func leftoverBackends(ctx context.Context, conn *pgx.Conn, db string, grace time.Duration, waived map[int32]bool) ([]leftoverBackend, error) {
	deadline := time.Now().Add(grace)
	for {
		rows, err := conn.Query(ctx, `SELECT pid, format('pid %s, %s, application %L, last query: %s',
                pid, COALESCE(state, '?'), application_name,
                left(regexp_replace(COALESCE(query, ''), '\s+', ' ', 'g'), 200))
            FROM pg_stat_activity
            WHERE datname = $1 AND pid <> pg_backend_pid() AND backend_type = 'client backend'
            ORDER BY pid`, db)
		if err != nil {
			return nil, err
		}
		all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (leftoverBackend, error) {
			var b leftoverBackend
			err := r.Scan(&b.pid, &b.desc)
			return b, err
		})
		if err != nil {
			return nil, err
		}
		var left []leftoverBackend
		for _, b := range all {
			if !waived[b.pid] {
				left = append(left, b)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("still waiting for %d connection(s) to close: %w", len(left), ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// RandomSuffix is a collision-safe per-test suffix.
func RandomSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// QuoteIdent safely quotes a PostgreSQL identifier we generated ourselves.
func QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// QuoteLiteral renders a Go string as a PostgreSQL string literal (fixture
// seed data only — never production SQL paths).
func QuoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
