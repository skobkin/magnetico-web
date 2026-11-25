package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ci/go-test.sh is how the CI steps run the suites: live progress in the log, the full
// JSON kept for the census, and go test's exit status, although its output goes through a
// pipe. These run the real script, under the sh of the machine (busybox ash in the Alpine
// CI images, dash in the Debian ones), on a throwaway module.
func runGoTestScript(t *testing.T, files map[string]string) (string, string, int) {
	t.Helper()
	script, err := filepath.Abs("../go-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	census := filepath.Join(work, "skipcensus")
	if out, err := exec.Command("go", "build", "-o", census, ".").CombinedOutput(); err != nil {
		t.Fatalf("build skipcensus: %v\n%s", err, out)
	}
	mod := filepath.Join(work, "mod")
	files["go.mod"] = "module m\n\ngo 1.24\n"
	for name, body := range files {
		p := filepath.Join(mod, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", script, "suite", "-count=1", "./...")
	cmd.Dir = mod
	cmd.Env = append(os.Environ(), "GOTEST_DIR="+work, "SKIPCENSUS="+census, "GOFLAGS=-buildvcs=false")
	out, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run the script: %v", err)
	}
	json, err := os.ReadFile(filepath.Join(work, "suite.json"))
	if err != nil {
		t.Fatalf("the JSON stream was not kept: %v\n%s", err, out)
	}
	return string(out), string(json), code
}

const passingTest = "package a\nimport \"testing\"\nfunc TestOK(t *testing.T) { t.Log(\"quiet when passing\") }\n"

func TestGoTestScriptPassingRun(t *testing.T) {
	out, json, code := runGoTestScript(t, map[string]string{"a/a_test.go": passingTest, "c/c.go": "package c\n"})
	if code != 0 {
		t.Fatalf("exit %d for a passing run:\n%s", code, out)
	}
	if !regexp.MustCompile(`(?m)^ok +m/a +\d+\.\ds$`).MatchString(out) || !strings.Contains(out, "[no test files]") {
		t.Errorf("no live line per package:\n%s", out)
	}
	if strings.Contains(out, "quiet when passing") || strings.Contains(out, "go test failed") {
		t.Errorf("a passing run printed test output or a failure:\n%s", out)
	}
	if !strings.Contains(json, `"Action":"pass","Package":"m/a","Test":"TestOK"`) {
		t.Errorf("the kept JSON stream is incomplete:\n%s", json)
	}
}

func TestGoTestScriptFailingTest(t *testing.T) {
	out, json, code := runGoTestScript(t, map[string]string{
		"a/a_test.go": passingTest,
		"b/b_test.go": "package b\nimport \"testing\"\nfunc TestBad(t *testing.T) { t.Log(\"context line\"); t.Fatal(\"boom\") }\n",
	})
	if code != 1 {
		t.Fatalf("exit %d for a failing test, want 1:\n%s", code, out)
	}
	live := strings.Index(out, "--- FAIL m/b TestBad")
	summary := strings.Index(out, "go test failed (status 1); failures:")
	if live < 0 || summary < 0 || live > summary {
		t.Fatalf("want the failed test live, then the closing summary:\n%s", out)
	}
	for _, want := range []string{"context line", "boom", "FAIL m/b TestBad"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !regexp.MustCompile(`(?m)^ok +m/a`).MatchString(out) || !regexp.MustCompile(`(?m)^FAIL +m/b`).MatchString(out) {
		t.Errorf("package lines missing:\n%s", out)
	}
	if !strings.Contains(json, `"Action":"fail","Package":"m/b","Test":"TestBad"`) {
		t.Errorf("the kept JSON stream lacks the failure:\n%s", json)
	}
}

func TestGoTestScriptBuildFailure(t *testing.T) {
	out, _, code := runGoTestScript(t, map[string]string{
		"b/b_test.go": "package b\nimport \"testing\"\nfunc TestX(t *testing.T) { undefinedName() }\n",
	})
	if code != 1 {
		t.Fatalf("exit %d for a package that does not compile, want 1:\n%s", code, out)
	}
	if strings.Count(out, "undefined: undefinedName") < 2 {
		t.Errorf("the compile error must be shown live and in the summary:\n%s", out)
	}
}
