package repocheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

// pgStubs puts fake pg_isready and psql first on PATH. pg_isready fails the first
// `failures` times, psql answers with `answer`.
func pgStubs(t *testing.T, failures int, answer string, psqlExit int) []string {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("pg_isready", `n=$(cat "`+counter+`" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "`+counter+`"
[ "$n" -gt `+strconv.Itoa(failures)+` ]`)
	write("psql", `echo `+answer+`; exit `+strconv.Itoa(psqlExit))
	return []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "WAIT_POSTGRES_INTERVAL=0.02"}
}

func waitPG(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(testkit.ModuleRoot(t), "ci", "wait-postgres.sh")}, args...)...)
	cmd.Env = append([]string{"HOME=" + t.TempDir()}, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

func TestWaitPostgresWaitsThenSucceeds(t *testing.T) {
	if out, code := waitPG(t, pgStubs(t, 3, "1", 0), "postgres", "5432", "5"); code != 0 || !strings.Contains(out, "ready") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestWaitPostgresFailsAfterTheTimeout(t *testing.T) {
	if out, code := waitPG(t, pgStubs(t, 9, "1", 0), "postgres", "5432", "3"); code == 0 || !strings.Contains(out, "not ready") {
		t.Fatalf("a server that never answers must fail (exit %d):\n%s", code, out)
	}
}

func TestWaitPostgresFailsWhenTheServerAnswersButQueriesFail(t *testing.T) {
	if out, code := waitPG(t, pgStubs(t, 0, "2", 0), "postgres", "5432", "3"); code == 0 {
		t.Fatalf("an unexpected answer must fail:\n%s", out)
	}
	if out, code := waitPG(t, pgStubs(t, 0, "1", 2), "postgres", "5432", "3"); code == 0 {
		t.Fatalf("a failing psql must fail:\n%s", out)
	}
}
