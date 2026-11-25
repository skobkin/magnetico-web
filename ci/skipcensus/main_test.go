package main

import (
	"bytes"
	"regexp"
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

// ---- --progress: the live view of a running suite ----------------------------------------------

func progress(t *testing.T, stream string) string {
	t.Helper()
	var out bytes.Buffer
	if code := runProgress(strings.NewReader(stream), &out); code != 0 {
		t.Fatalf("progress exit = %d: it only reports, the go test status decides", code)
	}
	return out.String()
}

func pkgEnd(action, pkg string, elapsed string) string {
	return `{"Action":"` + action + `","Package":"` + pkg + `","Elapsed":` + elapsed + "}\n"
}

func TestProgressReportsEachPackageWithResultAndElapsedTime(t *testing.T) {
	stream := ev("start", "m/a", "", "") +
		ev("run", "m/a", "TestOK", "") +
		ev("output", "m/a", "TestOK", "=== RUN   TestOK\n") +
		ev("output", "m/a", "TestOK", "    a_test.go:3: noisy but passing\n") +
		ev("pass", "m/a", "TestOK", "") +
		ev("output", "m/a", "", "ok  \tm/a\t1.523s\n") +
		pkgEnd("pass", "m/a", "1.523") +
		ev("output", "m/b", "", "?   \tm/b\t[no test files]\n") +
		pkgEnd("skip", "m/b", "0")
	got := progress(t, stream)
	if !regexp.MustCompile(`(?m)^ok +m/a +1\.5s$`).MatchString(got) {
		t.Errorf("no result line for m/a:\n%s", got)
	}
	if !regexp.MustCompile(`(?m)^\? +m/b +\[no test files\]$`).MatchString(got) {
		t.Errorf("no line for the package without tests:\n%s", got)
	}
	if strings.Contains(got, "noisy but passing") {
		t.Errorf("the output of a passing test was printed:\n%s", got)
	}
	if !strings.Contains(got, "2 packages: 1 ok, 0 failed, 1 without tests") {
		t.Errorf("no closing count:\n%s", got)
	}
}

func TestProgressPrintsAFailedTestsOutputWhenItFails(t *testing.T) {
	stream := ev("run", "m/a", "TestBad", "") +
		ev("output", "m/a", "TestBad", "=== RUN   TestBad\n") +
		ev("output", "m/a", "TestBad", "    a_test.go:9: context line\n") +
		ev("output", "m/a", "TestBad", "    a_test.go:10: boom\n") +
		ev("output", "m/a", "TestBad", "--- FAIL: TestBad (0.20s)\n") +
		`{"Action":"fail","Package":"m/a","Test":"TestBad","Elapsed":0.2}` + "\n" +
		ev("run", "m/a", "TestLater", "") +
		ev("pass", "m/a", "TestLater", "") +
		ev("output", "m/a", "", "FAIL\tm/a\t3.000s\n") +
		pkgEnd("fail", "m/a", "3")
	got := progress(t, stream)
	fail := strings.Index(got, "--- FAIL m/a TestBad")
	pkg := regexp.MustCompile(`(?m)^FAIL +m/a +3\.0s$`).FindStringIndex(got)
	if fail < 0 || pkg == nil {
		t.Fatalf("no failed-test block or package result:\n%s", got)
	}
	if fail > pkg[0] {
		t.Errorf("the failed test is reported only after its package:\n%s", got)
	}
	for _, want := range []string{"context line", "boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("the failed test's output lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "1 packages: 0 ok, 1 failed") {
		t.Errorf("no closing count:\n%s", got)
	}
}

func TestProgressReportsTestsCutShortByAPackageFailure(t *testing.T) {
	// A panic or a -timeout kills the test binary: the running test gets no
	// result of its own, only the package fails.
	stream := ev("run", "m/a", "TestHangs", "") +
		ev("output", "m/a", "TestHangs", "=== RUN   TestHangs\n") +
		ev("output", "m/a", "TestHangs", "    a_test.go:5: waiting for the server\n") +
		ev("output", "m/a", "", "panic: test timed out after 20m0s\n") +
		ev("output", "m/a", "", "FAIL\tm/a\t1200.000s\n") +
		pkgEnd("fail", "m/a", "1200")
	got := progress(t, stream)
	for _, want := range []string{"TestHangs", "waiting for the server", "panic: test timed out", "FAIL"} {
		if !strings.Contains(got, want) {
			t.Errorf("the package failure lacks %q:\n%s", want, got)
		}
	}
}

func TestProgressReportsBuildFailures(t *testing.T) {
	got := progress(t, buildFailure)
	if !strings.Contains(got, "undefined: undefined") || !regexp.MustCompile(`(?m)^FAIL +m/b`).MatchString(got) {
		t.Errorf("the build failure is not reported with the compiler's output:\n%s", got)
	}
}

func TestProgressPassesOtherLinesThrough(t *testing.T) {
	got := progress(t, "go: downloading example.com/x v1.0.0\n"+ev("output", "m/a", "", "ok\n")+pkgEnd("pass", "m/a", "0.1"))
	if !strings.Contains(got, "go: downloading example.com/x v1.0.0") {
		t.Errorf("a line that is not test2json was dropped:\n%s", got)
	}
}

const buildFailure = `{"ImportPath":"m/b [m/b.test]","Action":"build-output","Output":"# m/b [m/b.test]\n"}
{"ImportPath":"m/b [m/b.test]","Action":"build-output","Output":"b/b_test.go:3:27: undefined: undefined\n"}
{"ImportPath":"m/b [m/b.test]","Action":"build-fail"}
{"Action":"start","Package":"m/b"}
{"Action":"fail","Package":"m/b","Elapsed":0,"FailedBuild":"m/b [m/b.test]"}
`

// Since Go 1.24 a compile error is reported as build-output events of the
// import path, not as package output: the closing summary must print them.
func TestFailuresFlagPrintsCompileErrors(t *testing.T) {
	var out bytes.Buffer
	runFailures(strings.NewReader(buildFailure), &out)
	if !strings.Contains(out.String(), "FAIL package m/b") || !strings.Contains(out.String(), "undefined: undefined") {
		t.Errorf("compile error missing from the failure summary:\n%s", out.String())
	}
}
