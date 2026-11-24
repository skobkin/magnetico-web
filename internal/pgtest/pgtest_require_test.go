package pgtest

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHelperNeedsDB is only a body for the subprocess below: it asks for the
// cluster exactly like every integration test does.
func TestHelperNeedsDB(t *testing.T) {
	if os.Getenv("PGTEST_SELF_HELPER") != "1" {
		t.Skip("helper of TestRequirePostgresFailsInsteadOfSkipping")
	}
	IntegrationCluster(t)
}

// A CI run that is required to exercise PostgreSQL must not turn green because
// the database was unreachable and every integration test skipped.
func TestRequirePostgresFailsInsteadOfSkipping(t *testing.T) {
	run := func(extraEnv ...string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestHelperNeedsDB$", "-test.v", "-test.count=1")
		var env []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "MAGNETICO_TEST_") {
				env = append(env, kv)
			}
		}
		cmd.Env = append(env, "PGTEST_SELF_HELPER=1",
			// An explicit but unreachable server: bootstrap fails without any
			// fallback to docker or local binaries.
			"MAGNETICO_TEST_POSTGRES_DSN=postgres://127.0.0.1:1/postgres?connect_timeout=1")
		cmd.Env = append(cmd.Env, extraEnv...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run("MAGNETICO_TEST_REQUIRE_POSTGRES=1")
	if err == nil || !strings.Contains(out, "--- FAIL") || strings.Contains(out, "--- SKIP") {
		t.Fatalf("with the requirement set the test must fail, not skip (err=%v):\n%s", err, out)
	}
	if !strings.Contains(out, "MAGNETICO_TEST_POSTGRES_DSN unreachable") {
		t.Errorf("the failure must carry the actionable reason:\n%s", out)
	}

	out, err = run()
	if err != nil || !strings.Contains(out, "--- SKIP") {
		t.Fatalf("without the requirement an unavailable database still skips (err=%v):\n%s", err, out)
	}

	out, err = run("MAGNETICO_TEST_REQUIRE_POSTGRES=0")
	if err != nil || !strings.Contains(out, "--- SKIP") {
		t.Fatalf("only the value 1 enables the requirement (err=%v):\n%s", err, out)
	}
}
