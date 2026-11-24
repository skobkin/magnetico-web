package repocheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

const (
	digestA = "sha256:0db622202d0bb715dbf0a844a7228e9f8903b2502f705d3c1779ceae2c079f3d"
	digestB = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// stubCrane writes a fake `crane` that records every call and answers like the
// real one for the three subcommands the scripts use.
func stubCrane(t *testing.T, dir string) (path, logFile string) {
	t.Helper()
	logFile = filepath.Join(dir, "crane.log")
	path = filepath.Join(dir, "crane")
	script := `#!/bin/sh
echo "$@ [DOCKER_CONFIG=${DOCKER_CONFIG:-}]" >> "` + logFile + `"
case "$1" in
push)
	repo="${3%:*}"
	v="STUB_DIGEST_FOR_$(echo "$repo" | tr -c 'A-Za-z0-9\n' '_')"
	eval "d=\${$v:-}"
	[ -n "$d" ] || d="$STUB_DIGEST"
	echo "$repo@$d"
	;;
digest)
	repo="${2%:*}"
	v="STUB_DIGEST_FOR_$(echo "$repo" | tr -c 'A-Za-z0-9\n' '_')"
	eval "d=\${$v:-}"
	[ -n "$d" ] || d="$STUB_DIGEST"
	echo "$d"
	;;
manifest)
	cat "$STUB_MANIFEST"
	;;
*) echo "stub: unexpected $*" >&2; exit 9 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, logFile
}

func layout(t *testing.T, dir string, digests ...string) string {
	t.Helper()
	l := filepath.Join(dir, "layout")
	if err := os.MkdirAll(l, 0o755); err != nil {
		t.Fatal(err)
	}
	var ms []string
	for _, d := range digests {
		ms = append(ms, `{"mediaType":"application/vnd.oci.image.index.v1+json","digest":"`+d+`","size":1611}`)
	}
	idx := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` + strings.Join(ms, ",") + `]}`
	if err := os.WriteFile(filepath.Join(l, "index.json"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	return l
}

func script(t *testing.T, name string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(testkit.ModuleRoot(t), "ci", name)}, args...)...)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v", name, err)
	}
	return string(out), code
}

func rehearsal(crane, digest string, extra ...string) []string {
	return append([]string{
		"CRANE=" + crane, "STUB_DIGEST=" + digest,
		"MAGNETICO_PROMOTE_REHEARSAL=1", "MAGNETICO_PROMOTE_REPOS=r1.invalid/mag r2.invalid/mag",
	}, extra...)
}

func readLog(t *testing.T, p string) string {
	t.Helper()
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestPromoteRefusesLatestAliasesAndMalformedTags(t *testing.T) {
	dir := t.TempDir()
	crane, log := stubCrane(t, dir)
	l := layout(t, dir, digestA)
	for _, tag := range []string{"latest", "0.8", "stable", "edge", "v0.8.0", "0.8.0-rc1", "0.8.0-beta.1", "", "0.8.0 latest"} {
		if out, code := script(t, "promote-image.sh", rehearsal(crane, digestA), l, tag); code == 0 {
			t.Fatalf("tag %q must be refused:\n%s", tag, out)
		}
	}
	if got := readLog(t, log); got != "" {
		t.Fatalf("a refused tag must not reach the registry tool:\n%s", got)
	}
}

func TestPromotePushesTheLayoutToBothRepositoriesWithOnlyTheReleaseTag(t *testing.T) {
	dir := t.TempDir()
	crane, log := stubCrane(t, dir)
	l := layout(t, dir, digestA)
	out, code := script(t, "promote-image.sh", rehearsal(crane, digestA), l, "0.8.0-rc.0")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	pushes := 0
	for _, line := range strings.Split(strings.TrimSpace(readLog(t, log)), "\n") {
		if !strings.HasPrefix(line, "push ") {
			continue
		}
		pushes++
		if !strings.Contains(line, " "+l+" ") {
			t.Errorf("push of something other than the qualified layout: %s", line)
		}
		if !strings.Contains(line, "r1.invalid/mag:0.8.0-rc.0") && !strings.Contains(line, "r2.invalid/mag:0.8.0-rc.0") {
			t.Errorf("unexpected destination: %s", line)
		}
	}
	if pushes != 2 {
		t.Fatalf("%d pushes, want one per repository:\n%s", pushes, readLog(t, log))
	}
	if strings.Contains(readLog(t, log), "latest") {
		t.Fatal("the latest tag must never be applied")
	}
}

func TestPromoteFailsWhenARepositoryStoresAnotherDigest(t *testing.T) {
	dir := t.TempDir()
	crane, _ := stubCrane(t, dir)
	l := layout(t, dir, digestA)
	env := rehearsal(crane, digestA, "STUB_DIGEST_FOR_r2_invalid_mag="+digestB)
	if out, code := script(t, "promote-image.sh", env, l, "0.8.0"); code == 0 || !strings.Contains(out, "digest") {
		t.Fatalf("a digest that is not the qualified one must fail (exit %d):\n%s", code, out)
	}
}

func TestPromoteNeedsASingleTopLevelIndex(t *testing.T) {
	dir := t.TempDir()
	crane, log := stubCrane(t, dir)
	for name, l := range map[string]string{
		"no layout":    filepath.Join(dir, "nothing"),
		"two entries":  layout(t, t.TempDir(), digestA, digestB),
		"empty layout": layout(t, t.TempDir()),
	} {
		if out, code := script(t, "promote-image.sh", rehearsal(crane, digestA), l, "0.8.0"); code == 0 {
			t.Errorf("%s must be refused:\n%s", name, out)
		}
	}
	if readLog(t, log) != "" {
		t.Fatal("nothing may be pushed from a layout that is not exactly one index")
	}
}

func TestPromoteIgnoresRepositoryOverrideWithoutTheRehearsalFlag(t *testing.T) {
	dir := t.TempDir()
	crane, log := stubCrane(t, dir)
	l := layout(t, dir, digestA)
	env := []string{"CRANE=" + crane, "STUB_DIGEST=" + digestA, "MAGNETICO_PROMOTE_REPOS=evil.invalid/mag"}
	out, code := script(t, "promote-image.sh", env, l, "0.8.0")
	if strings.Contains(readLog(t, log), "evil.invalid") || strings.Contains(out, "evil.invalid") {
		t.Fatalf("the override must be ignored outside a rehearsal:\n%s", out)
	}
	if code == 0 {
		t.Fatalf("without credentials the real repositories must be refused:\n%s", out)
	}
}

func TestPromoteRealRepositoriesNeedAllCredentialsAndNeverPrintThem(t *testing.T) {
	dir := t.TempDir()
	crane, log := stubCrane(t, dir)
	l := layout(t, dir, digestA)
	base := []string{"CRANE=" + crane, "STUB_DIGEST=" + digestA}
	if out, code := script(t, "promote-image.sh", base, l, "0.8.0"); code == 0 {
		t.Fatalf("no credentials: must fail:\n%s", out)
	}
	if out, code := script(t, "promote-image.sh", append(base, "DOCKER_LOGIN=u", "DOCKER_TOKEN=t"), l, "0.8.0"); code == 0 {
		t.Fatalf("missing the GHCR credentials: must fail:\n%s", out)
	}
	if readLog(t, log) != "" {
		t.Fatal("nothing may be pushed with incomplete credentials")
	}
	env := append(base, "DOCKER_LOGIN=dockeruser", "DOCKER_TOKEN=SECRET-DOCKER-TOKEN", "GHCR_LOGIN=ghuser", "GHCR_TOKEN=SECRET-GHCR-TOKEN")
	out, code := script(t, "promote-image.sh", env, l, "0.8.0")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "SECRET-") || strings.Contains(readLog(t, log), "SECRET-") {
		t.Fatalf("a token leaked:\n%s\n%s", out, readLog(t, log))
	}
	for _, repo := range []string{"docker.io/skobkin/magnetico-web:0.8.0", "ghcr.io/skobkin/magnetico-web:0.8.0"} {
		if !strings.Contains(readLog(t, log), repo) {
			t.Errorf("no push to %s:\n%s", repo, readLog(t, log))
		}
	}
	// The registry credentials live in a private temporary DOCKER_CONFIG that is
	// removed when the script ends.
	for _, line := range strings.Split(readLog(t, log), "\n") {
		if i := strings.Index(line, "DOCKER_CONFIG="); i >= 0 {
			cfg := strings.TrimSuffix(line[i+len("DOCKER_CONFIG="):], "]")
			if cfg == "" {
				t.Errorf("crane ran without a DOCKER_CONFIG: %s", line)
			} else if _, err := os.Stat(cfg); err == nil {
				t.Errorf("the credential directory %s still exists after the run", cfg)
			}
		}
	}
}

const manifestOK = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
 {"digest":"sha256:a","platform":{"architecture":"amd64","os":"linux"}},
 {"digest":"sha256:b","platform":{"architecture":"arm64","os":"linux"}},
 {"digest":"sha256:c","platform":{"architecture":"unknown","os":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest"}},
 {"digest":"sha256:d","platform":{"architecture":"unknown","os":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest"}}]}`

func verify(t *testing.T, manifest string, extra ...string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	crane, _ := stubCrane(t, dir)
	mf := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(mf, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append([]string{"CRANE=" + crane, "STUB_DIGEST=" + digestA, "STUB_MANIFEST=" + mf,
		"MAGNETICO_PROMOTE_REHEARSAL=1", "MAGNETICO_PROMOTE_REPOS=r1.invalid/mag r2.invalid/mag"}, extra...)
	return script(t, "verify-published.sh", env, digestA, "0.8.0-rc.0")
}

func TestVerifyPublishedPassesWhenBothRegistriesHoldTheQualifiedDigest(t *testing.T) {
	if out, code := verify(t, manifestOK); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestVerifyPublishedFailsOnADifferentDigestInEitherRegistry(t *testing.T) {
	if out, code := verify(t, manifestOK, "STUB_DIGEST_FOR_r2_invalid_mag="+digestB); code == 0 {
		t.Fatalf("a differing registry must fail:\n%s", out)
	}
}

func TestVerifyPublishedFailsWithoutBothPlatformsOrAttestations(t *testing.T) {
	noArm := strings.Replace(manifestOK, `"architecture":"arm64"`, `"architecture":"s390x"`, 1)
	if out, code := verify(t, noArm); code == 0 {
		t.Fatalf("a missing arm64 platform must fail:\n%s", out)
	}
	oneAtt := strings.Replace(manifestOK, `"vnd.docker.reference.type":"attestation-manifest"}},
 {"digest":"sha256:d"`, `"x":"y"}},
 {"digest":"sha256:d"`, 1)
	if out, code := verify(t, oneAtt); code == 0 {
		t.Fatalf("a missing attestation manifest must fail:\n%s", out)
	}
}
