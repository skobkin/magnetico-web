package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// cgroup v2 measurement of the hard-limited ingest container. Nothing here
// defaults to zero: an unreadable or malformed file is an error, so a run with
// a requested hard limit can never report "0 OOM kills / 0 peak" for lack of
// data.

const cgroupRoot = "/sys/fs/cgroup"

// parseCgroupV2Path extracts the unified-hierarchy path from /proc/<pid>/cgroup.
func parseCgroupV2Path(procCgroup string) (string, error) {
	for _, line := range strings.Split(procCgroup, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			if p == "" || p == "/" {
				return "", fmt.Errorf("process is in the root cgroup (%q)", line)
			}
			return p, nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 entry (\"0::\") in %q", strings.TrimSpace(procCgroup))
}

func parseCgroupInt(name, content string) (int, error) {
	s := strings.TrimSpace(content)
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: %q is not a byte count", name, s)
	}
	return n, nil
}

var oomKillRe = regexp.MustCompile(`(?m)^oom_kill (\d+)\s*$`)

// parseOOMKills reads the oom_kill counter of memory.events; a missing counter
// is an error (some kernels or layouts do not provide it).
func parseOOMKills(events string) (int, error) {
	m := oomKillRe.FindStringSubmatch(events)
	if m == nil {
		return 0, fmt.Errorf("memory.events has no oom_kill counter: %q", strings.TrimSpace(events))
	}
	return strconv.Atoi(m[1])
}

// resolveCgroupDir finds the container's cgroup directory. The process's own
// /proc/<pid>/cgroup is authoritative (it is correct for the systemd and the
// cgroupfs Docker drivers alike); the two well-known layouts are tried only as
// a fallback. A candidate must contain memory.events.
func resolveCgroupDir(root, procRoot string, pid int, id string) (string, error) {
	var tried []string
	try := func(dir string) bool {
		if _, err := os.Stat(filepath.Join(dir, "memory.events")); err == nil {
			return true
		}
		tried = append(tried, dir)
		return false
	}
	if pid > 0 {
		b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
		if err == nil {
			if p, perr := parseCgroupV2Path(string(b)); perr == nil {
				if dir := filepath.Join(root, p); try(dir) {
					return dir, nil
				}
			} else {
				tried = append(tried, "proc cgroup: "+perr.Error())
			}
		} else {
			tried = append(tried, "proc cgroup: "+err.Error())
		}
	}
	if id != "" {
		for _, dir := range []string{
			filepath.Join(root, "system.slice", "docker-"+id+".scope"),
			filepath.Join(root, "docker", id),
		} {
			if try(dir) {
				return dir, nil
			}
		}
	}
	return "", fmt.Errorf("cgroup v2 directory of the container not found (tried %s)", strings.Join(tried, "; "))
}

// readCgroup returns the container's current memory.peak and oom_kill counter
// and verifies that a hard limit is really set (memory.max is a number).
func readCgroup(dir string) (peak, oom int, err error) {
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("cannot read %s: %w", name, err)
		}
		return string(b), nil
	}
	s, err := read("memory.peak")
	if err != nil {
		return 0, 0, fmt.Errorf("%w (memory.peak needs kernel >= 6.5)", err)
	}
	if peak, err = parseCgroupInt("memory.peak", s); err != nil {
		return 0, 0, err
	}
	if s, err = read("memory.events"); err != nil {
		return 0, 0, err
	}
	if oom, err = parseOOMKills(s); err != nil {
		return 0, 0, err
	}
	if s, err = read("memory.max"); err != nil {
		return 0, 0, err
	}
	if _, err = parseCgroupInt("memory.max", s); err != nil {
		return 0, 0, fmt.Errorf("the container has no hard memory limit: %w", err)
	}
	return peak, oom, nil
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParseCgroupV2Path(t *testing.T) {
	if p, err := parseCgroupV2Path("0::/system.slice/docker-abc.scope\n"); err != nil || p != "/system.slice/docker-abc.scope" {
		t.Fatalf("got %q, %v", p, err)
	}
	if p, err := parseCgroupV2Path("12:memory:/x\n0::/docker/abc\n"); err != nil || p != "/docker/abc" {
		t.Fatalf("hybrid layout: got %q, %v", p, err)
	}
	for _, bad := range []string{"", "0::/\n", "1:memory:/docker/abc\n"} {
		if _, err := parseCgroupV2Path(bad); err == nil {
			t.Fatalf("parseCgroupV2Path(%q) accepted", bad)
		}
	}
}

func TestParseOOMKillsAndInts(t *testing.T) {
	ev := "low 0\nhigh 0\nmax 12\noom 2\noom_kill 1\noom_group_kill 0\n"
	if n, err := parseOOMKills(ev); err != nil || n != 1 {
		t.Fatalf("got %d, %v", n, err)
	}
	if _, err := parseOOMKills("low 0\nhigh 0\nmax 0\noom 0\n"); err == nil {
		t.Fatal("a memory.events without oom_kill must be an error, not zero")
	}
	for _, bad := range []string{"", "max", "abc", "-1"} {
		if _, err := parseCgroupInt("memory.peak", bad); err == nil {
			t.Fatalf("parseCgroupInt(%q) accepted", bad)
		}
	}
	if n, err := parseCgroupInt("memory.peak", "12345\n"); err != nil || n != 12345 {
		t.Fatalf("got %d, %v", n, err)
	}
}

func TestResolveCgroupDirLayouts(t *testing.T) {
	good := map[string]string{"memory.events": "oom_kill 0\n", "memory.peak": "1048576\n", "memory.max": "536870912\n"}
	id := strings.Repeat("a", 64)

	t.Run("via /proc (systemd driver)", func(t *testing.T) {
		root, proc := t.TempDir(), t.TempDir()
		writeFiles(t, filepath.Join(root, "system.slice", "docker-"+id+".scope"), good)
		writeFiles(t, filepath.Join(proc, "4242"), map[string]string{"cgroup": "0::/system.slice/docker-" + id + ".scope\n"})
		dir, err := resolveCgroupDir(root, proc, 4242, id)
		if err != nil || !strings.HasSuffix(dir, "docker-"+id+".scope") {
			t.Fatalf("got %q, %v", dir, err)
		}
	})
	t.Run("via /proc (cgroupfs driver, nested path)", func(t *testing.T) {
		root, proc := t.TempDir(), t.TempDir()
		writeFiles(t, filepath.Join(root, "docker", id), good)
		writeFiles(t, filepath.Join(proc, "7"), map[string]string{"cgroup": "0::/docker/" + id + "\n"})
		if dir, err := resolveCgroupDir(root, proc, 7, id); err != nil || !strings.HasSuffix(dir, "/docker/"+id) {
			t.Fatalf("got %q, %v", dir, err)
		}
	})
	t.Run("fallback to the cgroupfs layout without /proc", func(t *testing.T) {
		root := t.TempDir()
		writeFiles(t, filepath.Join(root, "docker", id), good)
		if dir, err := resolveCgroupDir(root, t.TempDir(), 99, id); err != nil || !strings.HasSuffix(dir, "/docker/"+id) {
			t.Fatalf("got %q, %v", dir, err)
		}
	})
	t.Run("nothing found is an error listing what was tried", func(t *testing.T) {
		_, err := resolveCgroupDir(t.TempDir(), t.TempDir(), 5, id)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestReadCgroupFailsExplicitly(t *testing.T) {
	good := map[string]string{"memory.events": "oom_kill 3\n", "memory.peak": "2097152\n", "memory.max": "1073741824\n"}
	dir := t.TempDir()
	writeFiles(t, dir, good)
	if peak, oom, err := readCgroup(dir); err != nil || peak != 2097152 || oom != 3 {
		t.Fatalf("got %d %d %v", peak, oom, err)
	}
	cases := map[string]map[string]string{
		"missing memory.peak":     {"memory.events": "oom_kill 0\n", "memory.max": "1\n"},
		"missing memory.events":   {"memory.peak": "1\n", "memory.max": "1\n"},
		"events without oom_kill": {"memory.events": "max 0\n", "memory.peak": "1\n", "memory.max": "1\n"},
		"unlimited memory.max":    {"memory.events": "oom_kill 0\n", "memory.peak": "1\n", "memory.max": "max\n"},
		"garbage memory.peak":     {"memory.events": "oom_kill 0\n", "memory.peak": "n/a\n", "memory.max": "1\n"},
	}
	for name, files := range cases {
		d := t.TempDir()
		writeFiles(t, d, files)
		if _, _, err := readCgroup(d); err == nil {
			t.Fatalf("%s: readCgroup returned no error", name)
		}
	}
}
