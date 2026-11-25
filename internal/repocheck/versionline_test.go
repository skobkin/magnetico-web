package repocheck

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

// The container smoke test (ci/image-smoke.sh, run locally before a tag: see
// docs/release-engineering.md) judges the `--version` line of the image.

func smokeLine(t *testing.T, line string, expect ...string) (string, int) {
	t.Helper()
	args := append([]string{filepath.Join(testkit.ModuleRoot(t), "ci", "image-smoke.sh"), "--check-version-line", line}, expect...)
	out, err := exec.Command("sh", args...).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

const goodLine = "magnetico 0.8.0-rc.0 (commit 0123456789ab, go1.27.4, linux/amd64)"

func TestVersionLineCheckAcceptsAWellFormedLine(t *testing.T) {
	if out, code := smokeLine(t, goodLine); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if out, code := smokeLine(t, goodLine, "0.8.0-rc.0", "0123456789abcdef0123456789abcdef01234567"); code != 0 {
		t.Fatalf("the expected version and the commit prefix match: exit %d:\n%s", code, out)
	}
	if out, code := smokeLine(t, "magnetico dev (commit unknown, go1.28.0, linux/arm64)"); code != 0 {
		t.Fatalf("an unknown commit is fine when none is expected (exit %d):\n%s", code, out)
	}
}

func TestVersionLineCheckRejectsMalformedLinesAndTrailingJunk(t *testing.T) {
	for _, line := range []string{
		"", "garbage", goodLine + " extra", "x " + goodLine,
		"magnetico 0.8.0 (commit 0123456789ab", // the old alternation matched a prefix
		"magnetico 0.8.0 (commit 0123456789ab, go1.27.4)",
		"magnetico  (commit 0123456789ab, go1.27.4, linux/amd64)", // an empty version
		"magnetico 0.8.0 (commit nothex, go1.27.4, linux/amd64)",
	} {
		if out, code := smokeLine(t, line); code == 0 {
			t.Errorf("line %q must be rejected:\n%s", line, out)
		}
	}
}

func TestVersionLineCheckEnforcesTheGoFloorAndTheExpectedIdentity(t *testing.T) {
	if out, code := smokeLine(t, "magnetico 0.8.0 (commit 0123456789ab, go1.27.3, linux/amd64)"); code == 0 || !strings.Contains(out, "1.27.4") {
		t.Fatalf("go1.27.3 is below the floor (exit %d):\n%s", code, out)
	}
	if out, code := smokeLine(t, "magnetico 0.8.0 (commit 0123456789ab, go1.26.9, linux/amd64)"); code == 0 {
		t.Fatalf("go1.26.9 is below the floor:\n%s", out)
	}
	if out, code := smokeLine(t, goodLine, "0.8.0"); code == 0 {
		t.Fatalf("the image reports 0.8.0-rc.0, not 0.8.0:\n%s", out)
	}
	if out, code := smokeLine(t, goodLine, "0.8.0-rc.0", "ffffffffffffffffffffffffffffffffffffffff"); code == 0 {
		t.Fatalf("a different commit must fail:\n%s", out)
	}
	if out, code := smokeLine(t, "magnetico 0.8.0-rc.0 (commit unknown, go1.27.4, linux/amd64)", "0.8.0-rc.0", "0123456789abcdef0123456789abcdef01234567"); code == 0 {
		t.Fatalf("an unknown commit cannot satisfy an expected commit:\n%s", out)
	}
}
