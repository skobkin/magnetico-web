// Command skipcensus reads a `go test -json` stream and makes a green CI run
// impossible when integration tests were silently skipped.
//
//	go test -json ./... | skipcensus --allow ci/skip-allowlist.txt
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
	Action  string
	Package string
	Test    string
	Output  string
}

type allowEntry struct {
	test, reason *regexp.Regexp
	raw          string
	used         bool
}

func main() {
	allowPath := flag.String("allow", "", "allow-list file: test-regexp<TAB>reason-regexp<TAB>why, one per line")
	flag.Parse()
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
