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
type Cluster struct {
	AdminDSN string
	External bool
	Stop     func() // nil for external servers
}

var (
	clusterOnce sync.Once
	cluster     *Cluster
	clusterErr  error
)

// IntegrationCluster returns the shared disposable cluster or skips the test.
func IntegrationCluster(t *testing.T) *Cluster {
	t.Helper()
	clusterOnce.Do(func() {
		cluster, clusterErr = bootstrapCluster()
		if clusterErr != nil {
			cluster = nil
		}
	})
	if clusterErr != nil {
		t.Skipf("PostgreSQL integration harness unavailable: %v", clusterErr)
	}
	return cluster
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
	return &Cluster{AdminDSN: adminDSN, Stop: stop}, nil
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
		dropCtx, dropCancel := context.WithTimeout(context.Background(), DBOpTimeout)
		defer dropCancel()
		dropConn, err := pgx.Connect(dropCtx, c.AdminDSN)
		if err != nil {
			t.Errorf("drop test database %s: connect: %v", name, err)
			return
		}
		defer dropConn.Close(context.Background())
		if _, err := dropConn.Exec(dropCtx,
			fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, QuoteIdent(name))); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})

	parsed, err := url.Parse(c.AdminDSN)
	if err != nil {
		t.Fatalf("parse admin DSN: %v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
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
