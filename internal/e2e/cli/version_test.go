package cli

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

var versionLine = regexp.MustCompile(`^magnetico dev \(commit (unknown|[0-9a-f]{12}(\+dirty)?), go[0-9][0-9.a-z]*, [a-z0-9]+/[a-z0-9]+\)$`)

func TestVersionFlagPrintsBuildInfo(t *testing.T) {
	res := testkit.Run(t, testkit.Binary(t), testkit.RunOptions{Args: []string{"--version"}})
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, output %q", res.ExitCode, res.Output)
	}
	if res.Stderr != "" {
		t.Errorf("stderr = %q, want empty", res.Stderr)
	}
	if line := strings.TrimSuffix(res.Stdout, "\n"); !versionLine.MatchString(line) || strings.Contains(line, "\n") {
		t.Errorf("stdout = %q, want one line like `magnetico dev (commit 0123456789ab, go1.27.4, linux/amd64)`", res.Stdout)
	}
}

// Version output needs no configuration file, no database and no network.
func TestVersionFlagNeedsNoConfigNetworkOrDatabase(t *testing.T) {
	start := time.Now()
	res := testkit.Run(t, testkit.Binary(t), testkit.RunOptions{
		Args: []string{"--version"},
		Env: []string{
			"MAGNETICO_CONFIG_FILE_PATH=/nonexistent/magnetico.yaml",
			"MAGNETICO_WEB_DATABASE=postgres://127.0.0.1:1/unreachable",
			"MAGNETICO_INGEST_DATABASE=postgres://127.0.0.1:1/unreachable",
			"MAGNETICO_CRAWL_INGEST_URL=http://127.0.0.1:1",
			"MAGNETICO_WEB_NOPE=1", // an unknown variable would fail a real command
		},
		Timeout: 10 * time.Second,
	})
	if res.ExitCode != 0 || !versionLine.MatchString(strings.TrimSpace(res.Stdout)) {
		t.Fatalf("exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("--version took %v: it must not touch the network", d)
	}
}

// The linker values are the single mechanism shared by the Dockerfile and
// GoReleaser; this builds with exactly their -X pair.
func TestVersionLdflagsAreReflected(t *testing.T) {
	const (
		version = "0.8.0-rc.0"
		commit  = "0123456789abcdef0123456789abcdef01234567"
		pkg     = "github.com/skobkin/magnetico-web/internal/buildinfo"
	)
	bin := filepath.Join(t.TempDir(), "magnetico")
	cmd := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-s -w -X "+pkg+".Version="+version+" -X "+pkg+".Commit="+commit,
		"-o", bin, "./cmd/magnetico")
	cmd.Dir = testkit.ModuleRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	res := testkit.Run(t, bin, testkit.RunOptions{Args: []string{"--version"}})
	want := "magnetico " + version + " (commit 0123456789ab, " + runtime.Version() + ", " + runtime.GOOS + "/" + runtime.GOARCH + ")"
	if got := strings.TrimSpace(res.Stdout); res.ExitCode != 0 || got != want {
		t.Fatalf("got %q (exit %d), want %q", got, res.ExitCode, want)
	}
}

// --version is the sole-argument form only; help and the other commands are
// untouched by it.
func TestVersionDoesNotChangeHelpOrCommands(t *testing.T) {
	bin := testkit.Binary(t)
	help := testkit.Run(t, bin, testkit.RunOptions{Args: []string{"--help"}})
	if help.ExitCode != 0 || !strings.Contains(help.Output, "Available commands:") || strings.Contains(help.Output, "--version") {
		t.Errorf("--help changed: exit=%d\n%s", help.ExitCode, help.Output)
	}
	none := testkit.Run(t, bin, testkit.RunOptions{})
	if none.ExitCode != 1 || !strings.Contains(none.Output, "Please specify one command of: crawl, dev, ingest, migrate or web") {
		t.Errorf("no-argument behaviour changed: exit=%d %q", none.ExitCode, none.Output)
	}
	combined := testkit.Run(t, bin, testkit.RunOptions{Args: []string{"web", "--version"}})
	if combined.ExitCode == 0 {
		t.Errorf("`web --version` must not be treated as the version flag: %q", combined.Output)
	}
}
