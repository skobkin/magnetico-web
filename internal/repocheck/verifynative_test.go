package repocheck

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

const qualifiedChecksums = "aaaa  magnetico-web_0.8.0-rc.0_linux_amd64.tar.gz\nbbbb  magnetico-web_0.8.0-rc.0_linux_arm64.tar.gz\n"

type fakeRelease struct {
	tag        string
	prerelease bool
	draft      bool
	assets     []string
	checksums  string
	status     int
}

func (f fakeRelease) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/skobkin/magnetico-web/releases/tags/"+f.tag, func(w http.ResponseWriter, r *http.Request) {
		if f.status != 0 {
			http.Error(w, "no", f.status)
			return
		}
		var as []string
		for _, a := range f.assets {
			as = append(as, fmt.Sprintf(`{"id":1,"name":%q}`, a))
		}
		fmt.Fprintf(w, `{"tag_name":%q,"draft":%v,"prerelease":%v,"assets":[%s]}`, f.tag, f.draft, f.prerelease, strings.Join(as, ","))
	})
	mux.HandleFunc("/skobkin/magnetico-web/releases/download/"+f.tag+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, f.checksums)
	})
	return httptest.NewServer(mux)
}

func goodRelease(tag string) fakeRelease {
	return fakeRelease{
		tag: tag, prerelease: strings.Contains(tag, "-rc."),
		assets:    []string{"magnetico-web_" + tag + "_linux_amd64.tar.gz", "magnetico-web_" + tag + "_linux_arm64.tar.gz", "checksums.txt"},
		checksums: strings.ReplaceAll(qualifiedChecksums, "0.8.0-rc.0", tag),
	}
}

func verifyNative(t *testing.T, rel fakeRelease, qualified string) (string, int) {
	t.Helper()
	srv := rel.serve(t)
	defer srv.Close()
	q := filepath.Join(t.TempDir(), "qualified-checksums.txt")
	if err := os.WriteFile(q, []byte(qualified), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(testkit.ModuleRoot(t), "ci", "verify-native.sh"), rel.tag, q)
	cmd.Env = append(os.Environ(), "CI_FORGE_URL="+srv.URL, "CI_REPO=skobkin/magnetico-web")
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

func TestVerifyNativePassesForAPrereleaseAndAStableRelease(t *testing.T) {
	if out, code := verifyNative(t, goodRelease("0.8.0-rc.0"), qualifiedChecksums); code != 0 {
		t.Fatalf("rc: exit %d:\n%s", code, out)
	}
	stable := strings.ReplaceAll(qualifiedChecksums, "0.8.0-rc.0", "0.8.0")
	if out, code := verifyNative(t, goodRelease("0.8.0"), stable); code != 0 {
		t.Fatalf("stable: exit %d:\n%s", code, out)
	}
}

func TestVerifyNativeRequiresPrereleaseExactlyForReleaseCandidateTags(t *testing.T) {
	rc := goodRelease("0.8.0-rc.0")
	rc.prerelease = false
	if out, code := verifyNative(t, rc, qualifiedChecksums); code == 0 || !strings.Contains(out, "prerelease") {
		t.Fatalf("an rc tag published as a stable release must fail (exit %d):\n%s", code, out)
	}
	stable := goodRelease("0.8.0")
	stable.prerelease = true
	if out, code := verifyNative(t, stable, strings.ReplaceAll(qualifiedChecksums, "0.8.0-rc.0", "0.8.0")); code == 0 {
		t.Fatalf("a stable tag published as a prerelease must fail:\n%s", out)
	}
}

func TestVerifyNativeFailsWhenThePublishedChecksumsAreNotTheQualifiedOnes(t *testing.T) {
	rel := goodRelease("0.8.0-rc.0")
	rel.checksums = strings.Replace(rel.checksums, "aaaa", "cccc", 1)
	if out, code := verifyNative(t, rel, qualifiedChecksums); code == 0 || !strings.Contains(out, "checksums") {
		t.Fatalf("different bytes must fail (exit %d):\n%s", code, out)
	}
}

func TestVerifyNativeFailsOnMissingAssetsDraftsAndAPIErrors(t *testing.T) {
	missing := goodRelease("0.8.0-rc.0")
	missing.assets = missing.assets[:2]
	if out, code := verifyNative(t, missing, qualifiedChecksums); code == 0 {
		t.Fatalf("a missing checksums.txt asset must fail:\n%s", out)
	}
	noArm := goodRelease("0.8.0-rc.0")
	noArm.assets = []string{noArm.assets[0], noArm.assets[2]}
	if out, code := verifyNative(t, noArm, qualifiedChecksums); code == 0 {
		t.Fatalf("a missing arm64 archive must fail:\n%s", out)
	}
	draft := goodRelease("0.8.0-rc.0")
	draft.draft = true
	if out, code := verifyNative(t, draft, qualifiedChecksums); code == 0 {
		t.Fatalf("a draft release is not published:\n%s", out)
	}
	gone := goodRelease("0.8.0-rc.0")
	gone.status = http.StatusNotFound
	if out, code := verifyNative(t, gone, qualifiedChecksums); code == 0 {
		t.Fatalf("an unreadable release must fail:\n%s", out)
	}
}

func TestVerifyNativeRefusesMalformedTags(t *testing.T) {
	for _, tag := range []string{"latest", "0.8", "v0.8.0", "0.8.0-rc1"} {
		rel := goodRelease("0.8.0-rc.0")
		rel.tag = tag
		if out, code := verifyNative(t, rel, qualifiedChecksums); code == 0 {
			t.Fatalf("tag %q must be refused:\n%s", tag, out)
		}
	}
}
