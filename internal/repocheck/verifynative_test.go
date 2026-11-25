package repocheck

import (
	"crypto/sha256"
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

// archiveBytes are the (fake) bytes of the qualified archive of a platform.
func archiveBytes(tag, arch string) []byte {
	return []byte("qualified " + arch + " archive of " + tag + "\n")
}

// qualifiedFor is the checksums.txt the release gate qualified: the real SHA-256
// of the qualified archives' bytes.
func qualifiedFor(tag string) string {
	var b strings.Builder
	for _, arch := range []string{"amd64", "arm64"} {
		sum := sha256.Sum256(archiveBytes(tag, arch))
		fmt.Fprintf(&b, "%x  magnetico-web_%s_linux_%s.tar.gz\n", sum, tag, arch)
	}
	return b.String()
}

type fakeRelease struct {
	tag        string
	prerelease bool
	draft      bool
	assets     []string
	checksums  string
	status     int
	// files are the bytes served for each published asset name; a name that is
	// absent answers 404.
	files map[string][]byte
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
	for name, data := range f.files {
		mux.HandleFunc("/skobkin/magnetico-web/releases/download/"+f.tag+"/"+name, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(data)
		})
	}
	return httptest.NewServer(mux)
}

func goodRelease(tag string) fakeRelease {
	return fakeRelease{
		tag: tag, prerelease: strings.Contains(tag, "-rc."),
		assets:    []string{"magnetico-web_" + tag + "_linux_amd64.tar.gz", "magnetico-web_" + tag + "_linux_arm64.tar.gz", "checksums.txt"},
		checksums: qualifiedFor(tag),
		files: map[string][]byte{
			"magnetico-web_" + tag + "_linux_amd64.tar.gz": archiveBytes(tag, "amd64"),
			"magnetico-web_" + tag + "_linux_arm64.tar.gz": archiveBytes(tag, "arm64"),
		},
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
	if out, code := verifyNative(t, goodRelease("0.8.0-rc.0"), qualifiedFor("0.8.0-rc.0")); code != 0 {
		t.Fatalf("rc: exit %d:\n%s", code, out)
	}
	if out, code := verifyNative(t, goodRelease("0.8.0"), qualifiedFor("0.8.0")); code != 0 {
		t.Fatalf("stable: exit %d:\n%s", code, out)
	}
}

func TestVerifyNativeRequiresPrereleaseExactlyForReleaseCandidateTags(t *testing.T) {
	rc := goodRelease("0.8.0-rc.0")
	rc.prerelease = false
	if out, code := verifyNative(t, rc, qualifiedFor("0.8.0-rc.0")); code == 0 || !strings.Contains(out, "prerelease") {
		t.Fatalf("an rc tag published as a stable release must fail (exit %d):\n%s", code, out)
	}
	stable := goodRelease("0.8.0")
	stable.prerelease = true
	if out, code := verifyNative(t, stable, qualifiedFor("0.8.0")); code == 0 {
		t.Fatalf("a stable tag published as a prerelease must fail:\n%s", out)
	}
}

func TestVerifyNativeFailsWhenThePublishedChecksumsAreNotTheQualifiedOnes(t *testing.T) {
	rel := goodRelease("0.8.0-rc.0")
	rel.checksums = strings.Replace(rel.checksums, rel.checksums[:4], "cccc", 1)
	if out, code := verifyNative(t, rel, qualifiedFor("0.8.0-rc.0")); code == 0 || !strings.Contains(out, "checksums") {
		t.Fatalf("different bytes must fail (exit %d):\n%s", code, out)
	}
}

func TestVerifyNativeFailsOnMissingAssetsDraftsAndAPIErrors(t *testing.T) {
	missing := goodRelease("0.8.0-rc.0")
	missing.assets = missing.assets[:2]
	if out, code := verifyNative(t, missing, qualifiedFor("0.8.0-rc.0")); code == 0 {
		t.Fatalf("a missing checksums.txt asset must fail:\n%s", out)
	}
	noArm := goodRelease("0.8.0-rc.0")
	noArm.assets = []string{noArm.assets[0], noArm.assets[2]}
	if out, code := verifyNative(t, noArm, qualifiedFor("0.8.0-rc.0")); code == 0 {
		t.Fatalf("a missing arm64 archive must fail:\n%s", out)
	}
	draft := goodRelease("0.8.0-rc.0")
	draft.draft = true
	if out, code := verifyNative(t, draft, qualifiedFor("0.8.0-rc.0")); code == 0 {
		t.Fatalf("a draft release is not published:\n%s", out)
	}
	gone := goodRelease("0.8.0-rc.0")
	gone.status = http.StatusNotFound
	if out, code := verifyNative(t, gone, qualifiedFor("0.8.0-rc.0")); code == 0 {
		t.Fatalf("an unreadable release must fail:\n%s", out)
	}
}

func TestVerifyNativeRefusesMalformedTags(t *testing.T) {
	for _, tag := range []string{"latest", "0.8", "v0.8.0", "0.8.0-rc1"} {
		rel := goodRelease("0.8.0-rc.0")
		rel.tag = tag
		if out, code := verifyNative(t, rel, qualifiedFor("0.8.0-rc.0")); code == 0 {
			t.Fatalf("tag %q must be refused:\n%s", tag, out)
		}
	}
}

// TestVerifyNativeChecksTheBytesOfThePublishedArchives (Task 09A): the published
// checksums.txt can be byte-identical to the qualified one while the attached
// archives are something else (the file is only a list); the archives themselves
// are downloaded and hashed against the qualified checksums.
func TestVerifyNativeChecksTheBytesOfThePublishedArchives(t *testing.T) {
	tag := "0.8.0-rc.0"
	tampered := goodRelease(tag)
	name := "magnetico-web_" + tag + "_linux_amd64.tar.gz"
	tampered.files[name] = []byte("a different archive\n")
	out, code := verifyNative(t, tampered, qualifiedFor(tag))
	if code == 0 || !strings.Contains(out, name) || !strings.Contains(out, "differs") {
		t.Fatalf("a tampered archive behind an honest checksums.txt must fail naming the archive (exit %d):\n%s", code, out)
	}
	arm := goodRelease(tag)
	armName := "magnetico-web_" + tag + "_linux_arm64.tar.gz"
	arm.files[armName] = append(arm.files[armName], 'x')
	if out, code := verifyNative(t, arm, qualifiedFor(tag)); code == 0 || !strings.Contains(out, armName) {
		t.Fatalf("one extra byte in the arm64 archive must fail (exit %d):\n%s", code, out)
	}
	gone := goodRelease(tag)
	delete(gone.files, name)
	if out, code := verifyNative(t, gone, qualifiedFor(tag)); code == 0 || !strings.Contains(out, name) {
		t.Fatalf("an archive that cannot be downloaded must fail (exit %d):\n%s", code, out)
	}
	// The qualified file must itself list both archives.
	short := strings.SplitN(qualifiedFor(tag), "\n", 2)[0] + "\n"
	if out, code := verifyNative(t, goodRelease(tag), short); code == 0 {
		t.Fatalf("a qualified file without the arm64 archive must fail:\n%s", out)
	}
}
