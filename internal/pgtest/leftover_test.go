package pgtest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestHelperLeftover is only a body for the subprocesses below: it opens a
// connection to its test database and, depending on the mode, closes it,
// leaves it open, or leaves it open after declaring that deliberate.
func TestHelperLeftover(t *testing.T) {
	mode := os.Getenv("PGTEST_LEFTOVER_HELPER")
	if mode == "" {
		t.Skip("helper of TestTestDatabaseFailsATestThatLeavesConnectionsOpen")
	}
	dsn := IntegrationCluster(t).TestDatabase(t)
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	var one int
	if err := conn.QueryRow(context.Background(), `SELECT 1 /* leftover-marker */`).Scan(&one); err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "close":
		conn.Close(context.Background())
	case "leak":
	case "allowed":
		AllowLeftoverConnections(t, dsn)
	}
}

// The database drop is forced (WITH (FORCE)), so a pool a test forgot to close
// would otherwise vanish silently, and with it the evidence of a worker or
// pool outliving its test. TestDatabase's cleanup reports it as a failure.
func TestTestDatabaseFailsATestThatLeavesConnectionsOpen(t *testing.T) {
	IntegrationCluster(t) // skip or fail exactly like the integration tests
	run := func(mode string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestHelperLeftover$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), "PGTEST_LEFTOVER_HELPER="+mode)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run("close"); err != nil {
		t.Fatalf("a test that closed its connection failed: %v\n%s", err, out)
	}
	out, err := run("leak")
	if err == nil {
		t.Fatalf("a test that left a connection open passed:\n%s", out)
	}
	if !strings.Contains(out, "left open") || !strings.Contains(out, "leftover-marker") {
		t.Fatalf("the failure does not report the leftover connection and its last query:\n%s", out)
	}
	if out, err := run("allowed"); err != nil {
		t.Fatalf("a declared leftover connection failed the test: %v\n%s", err, out)
	}
}
