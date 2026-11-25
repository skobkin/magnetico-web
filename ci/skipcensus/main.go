// Command skipcensus reads a `go test -json` stream and makes a green CI run
// impossible when integration tests were silently skipped.
//
//	go test -json ./... | skipcensus --allow ci/skip-allowlist.txt
//
// Two reporting modes serve the CI steps around the census: --progress is the
// live view of a running suite (one line per package as it finishes, a failed
// test's output as soon as it fails), --failures the closing summary of every
// failed test and package. Neither decides the outcome: go test's exit status
// does.
//
// Every skipped test is listed with its reason. A skip passes only when the
// allow-list has an entry whose test-name pattern and reason pattern both
// match (the expected skips are the explicit opt-in harnesses); anything else,
// for example "PostgreSQL integration harness unavailable", fails the run. An
// empty stream, unreadable input or a skip whose reason cannot be established
// also fail. It is CI tooling and uses only the standard library.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

type event struct {
	Action      string
	Package     string
	Test        string
	Output      string
	Elapsed     float64
	ImportPath  string // build-output and build-fail events (Go 1.24+)
	FailedBuild string // set on the package's fail event when it did not compile
}

type allowEntry struct {
	test, reason *regexp.Regexp
	raw          string
	used         bool
}

func main() {
	allowPath := flag.String("allow", "", "allow-list file: test-regexp<TAB>reason-regexp<TAB>why, one per line")
	failures := flag.Bool("failures", false, "instead of the census, print the output of every failed test and broken package")
	progress := flag.Bool("progress", false, "instead of the census, report each package as it finishes and each failed test as it fails")
	flag.Parse()
	if *failures {
		os.Exit(runFailures(os.Stdin, os.Stdout))
	}
	if *progress {
		os.Exit(runProgress(os.Stdin, os.Stdout))
	}
	var allowR io.Reader = strings.NewReader("")
	if *allowPath != "" {
		f, err := os.Open(*allowPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "skipcensus:", err)
			os.Exit(2)
		}
		defer f.Close()
		allowR = f
	}
	os.Exit(run(os.Stdin, allowR, os.Stdout))
}

func parseAllow(r io.Reader) ([]*allowEntry, error) {
	var out []*allowEntry
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			return nil, fmt.Errorf("allow-list line needs test<TAB>reason: %q", line)
		}
		tr, err := regexp.Compile(parts[0])
		if err != nil {
			return nil, err
		}
		rr, err := regexp.Compile(parts[1])
		if err != nil {
			return nil, err
		}
		out = append(out, &allowEntry{test: tr, reason: rr, raw: parts[0]})
	}
	return out, sc.Err()
}

var reasonLine = regexp.MustCompile(`^\s+\S+\.go:\d+: (.*)$`)

func run(stream, allowlist io.Reader, out io.Writer) int {
	allow, err := parseAllow(allowlist)
	if err != nil {
		fmt.Fprintln(out, "skipcensus:", err)
		return 2
	}
	type key struct{ pkg, test string }
	outputs := map[key][]string{}
	var passed, failed, skipped int
	type skip struct{ pkg, test, reason string }
	var skips []skip

	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			fmt.Fprintf(out, "skipcensus: not a test2json line: %.80q\n", line)
			return 2
		}
		if e.Test == "" {
			continue // package-level events (including [no test files])
		}
		k := key{e.Package, e.Test}
		switch e.Action {
		case "output":
			outputs[k] = append(outputs[k], strings.TrimRight(e.Output, "\n"))
		case "pass":
			passed++
		case "fail":
			failed++
		case "skip":
			skipped++
			reason := ""
			for _, l := range outputs[k] {
				if m := reasonLine.FindStringSubmatch(l); m != nil {
					reason = m[1]
				}
			}
			skips = append(skips, skip{e.Package, e.Test, reason})
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(out, "skipcensus:", err)
		return 2
	}
	if passed+failed+skipped == 0 {
		fmt.Fprintln(out, "skipcensus: no test result in the stream: the suite did not run")
		return 1
	}

	sort.Slice(skips, func(i, j int) bool {
		if skips[i].pkg != skips[j].pkg {
			return skips[i].pkg < skips[j].pkg
		}
		return skips[i].test < skips[j].test
	})
	unexpected := 0
	for _, s := range skips {
		ok := false
		for _, a := range allow {
			if a.test.MatchString(s.test) && a.reason.MatchString(s.reason) {
				a.used, ok = true, true
				break
			}
		}
		tag := "allowed   "
		if !ok {
			tag = "UNEXPECTED"
			unexpected++
		}
		reason := s.reason
		if reason == "" {
			reason = "(no reason line found)"
		}
		fmt.Fprintf(out, "skip %s %s %s: %s\n", tag, s.pkg, s.test, reason)
	}
	for _, a := range allow {
		if !a.used {
			fmt.Fprintf(out, "warning: unused allow-list entry %q (nothing was skipped for that reason)\n", a.raw)
		}
	}
	fmt.Fprintf(out, "skipcensus: %d passed, %d failed, %d skipped (%d unexpected)\n", passed, failed, skipped, unexpected)
	if unexpected > 0 {
		fmt.Fprintln(out, "skipcensus: FAIL: tests were skipped for reasons that are not on the allow-list; a required database or tool may be missing")
		return 1
	}
	return 0
}

// runFailures prints the captured output of every failed test and of every
// failed package (build errors), which `go test -json > file` would otherwise
// hide in the file. It only reports: the exit status of the test run decides.
func runFailures(stream io.Reader, out io.Writer) int {
	type key struct{ pkg, test string }
	outputs := map[key][]string{}
	builds := map[string][]string{}
	var failed []key
	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e event
		if err := json.Unmarshal([]byte(strings.TrimSpace(sc.Text())), &e); err != nil {
			continue
		}
		k := key{e.Package, e.Test}
		switch e.Action {
		case "output":
			outputs[k] = append(outputs[k], strings.TrimRight(e.Output, "\n"))
		case "build-output":
			builds[e.ImportPath] = append(builds[e.ImportPath], strings.TrimRight(e.Output, "\n"))
		case "fail":
			if e.FailedBuild != "" {
				outputs[k] = append(builds[e.FailedBuild], outputs[k]...)
			}
			failed = append(failed, k)
		}
	}
	for _, k := range failed {
		if k.test == "" {
			fmt.Fprintf(out, "FAIL package %s\n", k.pkg)
		} else {
			fmt.Fprintf(out, "FAIL %s %s\n", k.pkg, k.test)
		}
		printIndented(out, truncated(outputs[k], 40))
	}
	return 0
}

// truncated keeps the first and last keep lines of a long output.
func truncated(lines []string, keep int) []string {
	if len(lines) <= 2*keep {
		return lines
	}
	return append(lines[:keep:keep], append([]string{"... (output truncated) ..."}, lines[len(lines)-keep:]...)...)
}

func printIndented(out io.Writer, lines []string) {
	for _, l := range lines {
		fmt.Fprintf(out, "    %s\n", l)
	}
}

// runProgress is the live view of a `go test -json` stream for a CI log: a
// line per package as it finishes (result and elapsed time), and the output
// of a failed test as soon as it fails, or of a package that failed without
// reporting its tests (build failure, panic, -timeout) together with the
// tests it cut short. Passing tests' output is dropped. Lines that are not
// test2json pass through. It only reports: go test's exit status decides.
func runProgress(stream io.Reader, out io.Writer) int {
	type key struct{ pkg, test string }
	outputs := map[key][]string{}           // output of running tests and of each package
	running := map[string]map[string]bool{} // package -> tests without a result yet
	builds := map[string][]string{}         // import path -> compiler output
	var ok, failed, noTests int

	sc := bufio.NewScanner(stream)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		raw := sc.Text()
		var e event
		if err := json.Unmarshal([]byte(raw), &e); err != nil || e.Action == "" {
			fmt.Fprintln(out, raw)
			continue
		}
		k := key{e.Package, e.Test}
		done := func() {
			delete(outputs, k)
			delete(running[e.Package], e.Test)
		}
		switch e.Action {
		case "build-output":
			builds[e.ImportPath] = append(builds[e.ImportPath], strings.TrimRight(e.Output, "\n"))
		case "run":
			if running[e.Package] == nil {
				running[e.Package] = map[string]bool{}
			}
			running[e.Package][e.Test] = true
		case "output":
			outputs[k] = append(outputs[k], strings.TrimRight(e.Output, "\n"))
		case "pass", "skip":
			if e.Test != "" {
				done()
				continue
			}
			if e.Action == "skip" {
				noTests++
				fmt.Fprintf(out, "?    %-60s [no test files]\n", e.Package)
			} else {
				ok++
				fmt.Fprintf(out, "ok   %-60s %.1fs\n", e.Package, e.Elapsed)
			}
			delete(outputs, k)
			delete(running, e.Package)
		case "fail":
			if e.Test != "" {
				fmt.Fprintf(out, "--- FAIL %s %s (%.1fs)\n", e.Package, e.Test, e.Elapsed)
				printIndented(out, truncated(outputs[k], 200))
				done()
				continue
			}
			failed++
			if e.FailedBuild != "" {
				printIndented(out, builds[e.FailedBuild])
			}
			var cut []string
			for test := range running[e.Package] {
				cut = append(cut, test)
			}
			sort.Strings(cut)
			for _, test := range cut {
				fmt.Fprintf(out, "--- %s %s: no result (cut short by the package failure)\n", e.Package, test)
				printIndented(out, truncated(outputs[key{e.Package, test}], 200))
				delete(outputs, key{e.Package, test})
			}
			printIndented(out, truncated(outputs[k], 200))
			fmt.Fprintf(out, "FAIL %-60s %.1fs\n", e.Package, e.Elapsed)
			delete(outputs, k)
			delete(running, e.Package)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(out, "progress:", err)
	}
	fmt.Fprintf(out, "progress: %d packages: %d ok, %d failed, %d without tests\n", ok+failed+noTests, ok, failed, noTests)
	return 0
}
