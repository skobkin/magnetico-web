package main

import (
	"bytes"
	"strings"
	"testing"
)

// event builds one test2json line.
func ev(action, pkg, test, output string) string {
	var b strings.Builder
	b.WriteString(`{"Action":"` + action + `","Package":"` + pkg + `"`)
	if test != "" {
		b.WriteString(`,"Test":"` + test + `"`)
	}
	if output != "" {
		b.WriteString(`,"Output":` + quote(output))
	}
	b.WriteString("}\n")
	return b.String()
}

func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

const allow = `# test-regexp<TAB>reason-regexp<TAB>why
^TestWebDemoServer$	set MAGNETICO	manual demo
^TestLoad$	MAGNETICO_BENCH_LARGE	heavy harness
`

func skipOf(pkg, test, reason string) string {
	return ev("run", pkg, test, "") +
		ev("output", pkg, test, "=== RUN   "+test+"\n") +
		ev("output", pkg, test, "    x_test.go:12: "+reason+"\n") +
		ev("output", pkg, test, "--- SKIP: "+test+" (0.00s)\n") +
		ev("skip", pkg, test, "")
}

func census(t *testing.T, stream, allowlist string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := run(strings.NewReader(stream), strings.NewReader(allowlist), &out)
	return code, out.String()
}

func TestAllowedSkipsPass(t *testing.T) {
	stream := ev("run", "p", "TestA", "") + ev("pass", "p", "TestA", "") +
		skipOf("p", "TestWebDemoServer", "set MAGNETICO_WEB_DEMO=1 to start it")
	code, out := census(t, stream, allow)
	if code != 0 || !strings.Contains(out, "TestWebDemoServer") || !strings.Contains(out, "1 skipped") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestUnexpectedSkipFails(t *testing.T) {
	stream := skipOf("p", "TestPostgresThing", "PostgreSQL integration harness unavailable: no server")
	code, out := census(t, stream, allow)
	if code == 0 || !strings.Contains(out, "UNEXPECTED") || !strings.Contains(out, "TestPostgresThing") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestNameMatchesButReasonDoesNot(t *testing.T) {
	// The allowed test skipping for a different reason (say, a missing database)
	// must not be waved through.
	code, out := census(t, skipOf("p", "TestLoad", "PostgreSQL integration harness unavailable"), allow)
	if code == 0 {
		t.Fatalf("the reason must match too:\n%s", out)
	}
}

func TestSubtestSkipIsReported(t *testing.T) {
	code, out := census(t, skipOf("p", "TestOuter/case_1", "needs docker"), allow)
	if code == 0 || !strings.Contains(out, "TestOuter/case_1") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestPackagesWithoutTestsAreNotSkips(t *testing.T) {
	stream := ev("skip", "pkg/without/tests", "", "") + ev("pass", "p", "TestA", "")
	if code, out := census(t, stream, allow); code != 0 {
		t.Fatalf("a package-level skip ([no test files]) is not a skipped test: %s", out)
	}
}

func TestSkipWithoutReasonLineFails(t *testing.T) {
	stream := ev("run", "p", "TestMute", "") + ev("skip", "p", "TestMute", "")
	if code, out := census(t, stream, allow); code == 0 {
		t.Fatalf("a skip whose reason cannot be established is unexpected: %s", out)
	}
}

func TestZeroTestsIsAnError(t *testing.T) {
	if code, out := census(t, "", allow); code == 0 {
		t.Fatalf("an empty stream means the suite did not run: %s", out)
	}
}

func TestMalformedLineIsAnError(t *testing.T) {
	if code, _ := census(t, "not json\n", allow); code == 0 {
		t.Fatal("garbage input must fail")
	}
}

func TestSummaryCountsAndStaleAllowEntries(t *testing.T) {
	stream := ev("pass", "p", "TestA", "") + ev("fail", "p", "TestB", "") + skipOf("p", "TestWebDemoServer", "set MAGNETICO_X")
	code, out := census(t, stream, allow)
	if !strings.Contains(out, "1 passed, 1 failed, 1 skipped") {
		t.Fatalf("summary missing:\n%s", out)
	}
	if !strings.Contains(out, "unused allow-list entry") || !strings.Contains(out, "TestLoad") {
		t.Fatalf("the stale entry TestLoad should be reported (as a warning):\n%s", out)
	}
	_ = code
}

func TestFailuresFlagPrintsTheOutputOfFailedTestsAndBrokenPackages(t *testing.T) {
	stream := ev("run", "p", "TestBroken", "") +
		ev("output", "p", "TestBroken", "=== RUN   TestBroken\n") +
		ev("output", "p", "TestBroken", "    thing_test.go:42: got 1, want 2\n") +
		ev("output", "p", "TestBroken", "--- FAIL: TestBroken (0.00s)\n") +
		ev("fail", "p", "TestBroken", "") +
		ev("output", "q", "", "# q [q.test]\n") +
		ev("output", "q", "", "q/x.go:3:1: undefined: y\n") +
		ev("fail", "q", "", "") +
		ev("pass", "p", "TestFine", "")
	var out bytes.Buffer
	code := runFailures(strings.NewReader(stream), &out)
	got := out.String()
	for _, want := range []string{"FAIL p TestBroken", "got 1, want 2", "FAIL package q", "undefined: y"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "TestFine") {
		t.Errorf("a passing test must not be listed:\n%s", got)
	}
	if code != 0 {
		t.Errorf("--failures only reports; the exit status of the run decides (got %d)", code)
	}
}
