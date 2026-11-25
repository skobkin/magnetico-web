package repocheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

// Tests of ci/release-tag.sh, ci/prepare-rebuild.sh and ci/scan-release-image.sh. They use
// a real git repository and the real jq, and stub the network (curl) and the scanner
// (trivy). The main test step's image has neither git nor jq, so there they skip (listed in
// ci/skip-allowlist.txt); workflow-invariants installs both and sets
// REPOCHECK_REQUIRE_TOOLS=1, so there a missing tool fails instead of skipping.
func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("REPOCHECK_REQUIRE_TOOLS") == "1" {
				t.Fatalf("%s is not on PATH and REPOCHECK_REQUIRE_TOOLS=1", tool)
			}
			t.Skipf("needs git and jq: %s is not on PATH", tool)
		}
	}
}

// releaseRepo is a git repository with the CI scripts and, in order of creation: a PHP-era
// release (no cmd/magnetico) and then Go-line commits tagged with tags (in that order).
func releaseRepo(t *testing.T, tags ...string) (dir string, commits map[string]string) {
	t.Helper()
	dir = t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	step := 0
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		d := base.Add(time.Duration(step) * time.Hour).Format(time.RFC3339)
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_AUTHOR_DATE="+d, "GIT_COMMITTER_DATE="+d)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "master")
	write("composer.json", "{}\n")
	for _, s := range []string{"release-tag.sh", "prepare-rebuild.sh"} {
		write("ci/"+s, readCI(t, s))
	}
	git("add", "-A")
	git("commit", "-q", "-m", "php")
	git("tag", "0.7.1")
	commits = map[string]string{}
	for i, tag := range tags {
		step++
		write("cmd/magnetico/main.go", "package main // "+strconv.Itoa(i)+"\n")
		git("add", "-A")
		git("commit", "-q", "-m", "go "+tag)
		git("tag", tag)
		commits[tag] = git("rev-parse", "HEAD")
	}
	step++
	write("README.md", "after the releases\n")
	git("add", "-A")
	git("commit", "-q", "-m", "later")
	return dir, commits
}

func runScript(t *testing.T, dir string, env []string, script string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1"), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func TestReleaseTagPicksTheNewestStableGoLineRelease(t *testing.T) {
	requireTools(t, "git")
	for _, c := range []struct {
		name string
		tags []string
		want string
	}{
		{"stable before a newer release candidate", []string{"0.8.0-rc1", "0.8.0", "0.8.1-rc1"}, "0.8.0"},
		{"the newest stable", []string{"0.8.0", "0.8.1", "0.9.0-rc1"}, "0.8.1"},
		{"only release candidates", []string{"0.8.0-rc1", "0.8.0-rc2"}, "0.8.0-rc2"},
		{"only the PHP era", nil, ""},
	} {
		dir, _ := releaseRepo(t, c.tags...)
		out, code := runScript(t, dir, nil, "ci/release-tag.sh")
		if code != 0 || strings.TrimSpace(out) != c.want {
			t.Errorf("%s: exit %d, output %q, want %q", c.name, code, out, c.want)
		}
	}
}

// registryStubs puts a fake curl first on PATH: tokens for both registries, and the tag
// lists given (nil: that registry's listing fails).
func registryStubs(t *testing.T, hubTags, ghcrTags []string) []string {
	t.Helper()
	dir := t.TempDir()
	list := func(name string, tags []string) {
		if tags == nil {
			return
		}
		q := make([]string, len(tags))
		for i, s := range tags {
			q[i] = strconv.Quote(s)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"name":"x","tags":[`+strings.Join(q, ",")+`]}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list("hub.json", hubTags)
	list("ghcr.json", ghcrTags)
	stub := `#!/bin/sh
for a; do url="$a"; done
case "$url" in
https://auth.test/*|https://ghcr.test/token*) echo '{"token":"t"}' ;;
https://hub.test/v2/skobkin/magnetico-web/tags/list*) cat "` + dir + `/hub.json" 2>/dev/null || exit 22 ;;
https://ghcr.test/v2/skobkin/magnetico-web/tags/list*) cat "` + dir + `/ghcr.json" 2>/dev/null || exit 22 ;;
*) echo "unexpected URL $url" >&2; exit 22 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + dir + ":" + os.Getenv("PATH"),
		"DOCKERHUB_REGISTRY=https://hub.test", "DOCKERHUB_AUTH=https://auth.test/token", "GHCR_REGISTRY=https://ghcr.test"}
}

func TestPrepareRebuildNumbersTheRebuildAndMovesTheReleaseTag(t *testing.T) {
	requireTools(t, "git", "jq")
	dir, commits := releaseRepo(t, "0.8.0-rc1", "0.8.0", "0.8.1-rc1")
	// r1 on Docker Hub and r2 on GHCR: the next is r3 on both. Neither the release candidate's
	// rebuilds nor a non-numeric suffix count.
	env := append(registryStubs(t,
		[]string{"0.8.0", "0.8.0-r1", "0.8.0-rc1", "0.8.0-rc1-r7", "0.8.0-rx"},
		[]string{"0.8.0", "0.8.0-r2"}), "FORCE_REBUILD=true")
	out, code := runScript(t, dir, env, "ci/prepare-rebuild.sh")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	tags, _ := os.ReadFile(filepath.Join(dir, ".tags"))
	if string(tags) != "0.8.0-r3\n0.8.0\n" {
		t.Errorf(".tags = %q, want the rebuild tag and the moved release tag", tags)
	}
	envFile, _ := os.ReadFile(filepath.Join(dir, ".rebuild.env"))
	if string(envFile) != "VERSION=0.8.0\nCOMMIT="+commits["0.8.0"]+"\n" {
		t.Errorf(".rebuild.env = %q", envFile)
	}
	head, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if strings.TrimSpace(string(head)) != commits["0.8.0"] {
		t.Errorf("HEAD = %s, want the release commit %s checked out", head, commits["0.8.0"])
	}
	if !strings.Contains(out, "published on docker.io ghcr.io") {
		t.Errorf("output does not say where the release is published:\n%s", out)
	}
}

func TestPrepareRebuildRefusals(t *testing.T) {
	requireTools(t, "git", "jq")
	published := []string{"0.8.0"}
	for _, c := range []struct {
		name      string
		env       []string
		hub, ghcr []string
		want      string
	}{
		{"no FORCE_REBUILD", nil, published, published, "FORCE_REBUILD is not set"},
		{"FORCE_REBUILD=false", []string{"FORCE_REBUILD=false"}, published, published, "FORCE_REBUILD is not set"},
		{"a PHP-era release", []string{"FORCE_REBUILD=true", "REBUILD_TAG=0.7.1"}, published, published, "not a release of the Go line"},
		{"a rebuild tag", []string{"FORCE_REBUILD=true", "REBUILD_TAG=0.8.0-r1"}, published, published, "is a rebuild tag"},
		{"no such tag", []string{"FORCE_REBUILD=true", "REBUILD_TAG=0.9.9"}, published, published, "there is no git tag 0.9.9"},
		{"not a release tag", []string{"FORCE_REBUILD=true", "REBUILD_TAG=v0.8.0"}, published, published, "is not a release tag"},
		{"never published", []string{"FORCE_REBUILD=true", "REBUILD_TAG=0.8.1-rc1"}, published, published, "neither registry has 0.8.1-rc1"},
		{"a registry cannot be listed", []string{"FORCE_REBUILD=true"}, published, nil, "cannot list the tags of ghcr.io"},
	} {
		dir, _ := releaseRepo(t, "0.8.0", "0.8.1-rc1")
		out, code := runScript(t, dir, append(registryStubs(t, c.hub, c.ghcr), c.env...), "ci/prepare-rebuild.sh")
		if code == 0 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want a refusal with %q:\n%s", c.name, code, c.want, out)
		}
		if _, err := os.Stat(filepath.Join(dir, ".tags")); err == nil {
			t.Errorf("%s: .tags was written although the rebuild was refused", c.name)
		}
	}
}

// trivyStub records its arguments and fails for the platforms listed in fail.
func trivyStub(t *testing.T, fail string) (env []string, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	stub := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$*" in *"--platform ` + fail + ` "*) exit 1 ;; esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "trivy"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + dir + ":" + os.Getenv("PATH")}, log
}

func TestScanReleaseImage(t *testing.T) {
	script := filepath.Join(testkit.ModuleRoot(t), "ci", "scan-release-image.sh")
	tagFile := func(tag string) string {
		p := filepath.Join(t.TempDir(), "tag")
		if err := os.WriteFile(p, []byte(tag), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	env, log := trivyStub(t, "none")
	if out, code := runScript(t, t.TempDir(), env, script, tagFile("")); code != 0 || !strings.Contains(out, "nothing to scan") {
		t.Errorf("no release yet: exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("the scanner ran although there is no release")
	}

	env, log = trivyStub(t, "none")
	if out, code := runScript(t, t.TempDir(), env, script, tagFile("0.8.0\n")); code != 0 {
		t.Errorf("a clean image: exit %d:\n%s", code, out)
	}
	calls, _ := os.ReadFile(log)
	for _, p := range []string{"linux/amd64", "linux/arm64"} {
		want := "image --image-src remote --platform " + p + " --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 --no-progress docker.io/skobkin/magnetico-web:0.8.0"
		if !strings.Contains(string(calls), want) {
			t.Errorf("trivy was not called with %q:\n%s", want, calls)
		}
	}

	env, log = trivyStub(t, "linux/amd64")
	out, code := runScript(t, t.TempDir(), env, script, tagFile("0.8.0"))
	if code == 0 || !strings.Contains(out, "rebuild it") {
		t.Errorf("a finding must fail the run and say what to do: exit %d:\n%s", code, out)
	}
	if calls, _ := os.ReadFile(log); strings.Count(string(calls), "\n") != 2 {
		t.Errorf("both platforms must be scanned even when the first has findings:\n%s", calls)
	}
}
