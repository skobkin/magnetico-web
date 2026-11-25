package pgtest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestHelperLeftover is only a body for the subprocesses below: it opens
// connections to its test database and closes, leaks or declares them, or
// makes the leftover inspection itself fail or hang, depending on the mode.
func TestHelperLeftover(t *testing.T) {
	mode := os.Getenv("PGTEST_LEFTOVER_HELPER")
	if mode == "" {
		t.Skip("helper of TestTestDatabaseFailsATestThatLeavesConnectionsOpen")
	}
	leftoverGrace = 500 * time.Millisecond // nothing here is waiting to close
	dsn := IntegrationCluster(t).TestDatabase(t)
	t.Logf("test database: %s", dsn[strings.LastIndex(dsn, "/")+1:])
	open := func(marker string) *pgx.Conn {
		conn, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		var one int
		if err := conn.QueryRow(context.Background(), `SELECT 1 /* `+marker+` */`).Scan(&one); err != nil {
			t.Fatal(err)
		}
		return conn
	}

	switch mode {
	case "close":
		open("closed-marker").Close(context.Background())
	case "leak":
		open("leftover-marker")
	case "allowed":
		open("waived-marker")
		if n := AllowConnectionsOpenNow(t, dsn); n != 1 {
			t.Fatalf("waived %d connections, want 1", n)
		}
	case "allowed-then-leak":
		open("waived-marker")
		AllowConnectionsOpenNow(t, dsn)
		open("leftover-marker") // opened after the declaration: not waived
	case "inspect-fails":
		inspectLeftovers = func(context.Context, *pgx.Conn, string, time.Duration, map[int32]bool) ([]leftoverBackend, error) {
			return nil, errors.New("injected inspection failure")
		}
		open("leftover-marker")
	case "inspect-hangs":
		leftoverInspectTimeout = 300 * time.Millisecond
		inspectLeftovers = func(ctx context.Context, _ *pgx.Conn, _ string, _ time.Duration, _ map[int32]bool) ([]leftoverBackend, error) {
			<-ctx.Done() // uses up the whole inspection budget
			return nil, ctx.Err()
		}
		open("leftover-marker")
	default:
		t.Fatalf("unknown mode %q", mode)
	}
}

// The database drop is forced (WITH (FORCE)), so a pool a test forgot to close
// would otherwise vanish silently. TestDatabase's cleanup reports every
// PostgreSQL connection still open as a failure, and drops the database even
// when that inspection fails or runs out of time.
func TestTestDatabaseFailsATestThatLeavesConnectionsOpen(t *testing.T) {
	c := IntegrationCluster(t) // skip or fail exactly like the integration tests
	run := func(t *testing.T, mode string) (string, error) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run", "^TestHelperLeftover$", "-test.v", "-test.count=1")
		// The subprocess uses this process's server, whichever way the harness
		// obtained it, and must not skip for lack of one.
		var env []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "MAGNETICO_TEST_") && !strings.HasPrefix(kv, "PGTEST_") {
				env = append(env, kv)
			}
		}
		cmd.Env = append(env, "PGTEST_LEFTOVER_HELPER="+mode,
			"MAGNETICO_TEST_POSTGRES_DSN="+c.AdminDSN, RequireEnv+"=1")
		out, err := cmd.CombinedOutput()
		if !strings.Contains(string(out), "=== RUN   TestHelperLeftover") || strings.Contains(string(out), "--- SKIP") {
			t.Fatalf("the helper did not run in mode %s:\n%s", mode, out)
		}
		return string(out), err
	}
	dropped := func(t *testing.T, out string) {
		t.Helper()
		m := regexp.MustCompile(`test database: (magnetico_test_[0-9a-f]+)`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("the helper did not report its database:\n%s", out)
		}
		ctx, cancel := context.WithTimeout(context.Background(), DBOpTimeout)
		defer cancel()
		conn, err := pgx.Connect(ctx, c.AdminDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, m[1]).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("test database %s was not dropped:\n%s", m[1], out)
		}
	}

	t.Run("closed connection passes", func(t *testing.T) {
		out, err := run(t, "close")
		if err != nil {
			t.Fatalf("a test that closed its connection failed: %v\n%s", err, out)
		}
		dropped(t, out)
	})
	t.Run("leftover connection fails", func(t *testing.T) {
		out, err := run(t, "leak")
		if err == nil {
			t.Fatalf("a test that left a connection open passed:\n%s", out)
		}
		if !strings.Contains(out, "left open") || !strings.Contains(out, "leftover-marker") {
			t.Fatalf("the failure does not report the leftover connection and its last query:\n%s", out)
		}
		dropped(t, out)
	})
	t.Run("declared connection passes", func(t *testing.T) {
		out, err := run(t, "allowed")
		if err != nil {
			t.Fatalf("a declared connection failed the test: %v\n%s", err, out)
		}
		dropped(t, out)
	})
	t.Run("connection opened after the declaration fails", func(t *testing.T) {
		out, err := run(t, "allowed-then-leak")
		if err == nil {
			t.Fatalf("a connection opened after the declaration was waived:\n%s", out)
		}
		if !strings.Contains(out, "leftover-marker") || strings.Contains(out, "waived-marker") {
			t.Fatalf("the failure must name the later connection only:\n%s", out)
		}
		dropped(t, out)
	})
	for _, mode := range []string{"inspect-fails", "inspect-hangs"} {
		t.Run("database dropped when the inspection "+strings.TrimPrefix(mode, "inspect-"), func(t *testing.T) {
			out, err := run(t, mode)
			if err == nil || !strings.Contains(out, "inspect its connections") {
				t.Fatalf("an inspection that could not complete did not fail the test:\n%s", out)
			}
			dropped(t, out)
		})
	}
}
