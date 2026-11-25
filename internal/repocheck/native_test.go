package repocheck

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

// fakeBinaries are tiny real Go programs that print what `magnetico --version`
// prints, so the smoke script's `go version -m` checks see genuine build info.
var (
	fakeOnce sync.Once
	fakeDir  string
	fakeErr  error
)

func fakeBinary(t *testing.T, name string) string {
	t.Helper()
	fakeOnce.Do(func() {
		fakeDir, fakeErr = os.MkdirTemp("", "native-fakes-")
		if fakeErr != nil {
			return
		}
		src := filepath.Join(fakeDir, "src")
		_ = os.MkdirAll(src, 0o755)
		_ = os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fake\n\ngo 1.24\n"), 0o644)
		_ = os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nimport \"fmt\"\n\nvar line = \"x\"\n\nfunc main() { fmt.Println(line) }\n"), 0o644)
		build := func(out, arch, line string, extra ...string) {
			if fakeErr != nil {
				return
			}
			args := append([]string{"build", "-ldflags", "-X 'main.line=" + line + "'"}, extra...)
			args = append(args, "-o", filepath.Join(fakeDir, out), ".")
			cmd := exec.Command("go", args...)
			cmd.Dir = src
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch, "GOFLAGS=-mod=mod")
			if b, err := cmd.CombinedOutput(); err != nil {
				fakeErr = fmt.Errorf("build %s: %v\n%s", out, err, b)
			}
		}
		good := func(arch string) string {
			return "magnetico 0.8.0-rc.0 (commit 0123456789ab, go1.27.4, linux/" + arch + ")"
		}
		build("amd64", "amd64", good("amd64"))
		build("arm64", "arm64", good("arm64"))
		build("amd64-oldgo", "amd64", "magnetico 0.8.0-rc.0 (commit 0123456789ab, go1.27.3, linux/amd64)")
		build("amd64-tags", "amd64", good("amd64"), "-tags", "diag")
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return filepath.Join(fakeDir, name)
}

type distOpts struct {
	amd64, arm64     string // fixture names
	omitLicence      string
	omitArm64Archive bool
	tamper           bool
}

func writeArchive(t *testing.T, path, binary string, omit string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	add := func(name string, body []byte, mode int64) {
		if name == omit {
			return
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(body)
	}
	bin, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	add("magnetico", bin, 0o755)
	for _, n := range []string{"COPYING", "README.md", "UPGRADE.md", "licenses/swagger-ui-LICENSE", "licenses/swagger-ui-NOTICE", "licenses/swagger-ui-bundle.js.LICENSE.txt", "licenses/uplot-LICENSE"} {
		add(n, []byte("text of "+n), 0o644)
	}
	_ = tw.Close()
	_ = gz.Close()
}

func makeDist(t *testing.T, version string, o distOpts) string {
	t.Helper()
	if o.amd64 == "" {
		o.amd64 = "amd64"
	}
	if o.arm64 == "" {
		o.arm64 = "arm64"
	}
	dir := t.TempDir()
	var sums strings.Builder
	for _, a := range []struct{ arch, fixture string }{{"amd64", o.amd64}, {"arm64", o.arm64}} {
		if a.arch == "arm64" && o.omitArm64Archive {
			continue
		}
		name := "magnetico-web_" + version + "_linux_" + a.arch + ".tar.gz"
		writeArchive(t, filepath.Join(dir, name), fakeBinary(t, a.fixture), o.omitLicence)
		b, _ := os.ReadFile(filepath.Join(dir, name))
		sum := sha256.Sum256(b)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if o.tamper {
		// A perfectly readable archive whose recorded digest is wrong: only the
		// checksum verification can notice (a corrupted archive would also fail
		// to unpack and hide a missing checksum check).
		p := filepath.Join(dir, "checksums.txt")
		b, _ := os.ReadFile(p)
		flipped := []byte(string(b))
		if flipped[0] == '0' {
			flipped[0] = '1'
		} else {
			flipped[0] = '0'
		}
		_ = os.WriteFile(p, flipped, 0o644)
	}
	return dir
}

func nativeSmoke(t *testing.T, dist string, args ...string) (string, int) {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		t.Skip("the fake release binary that gets executed is built for linux/amd64")
	}
	cmd := exec.Command("sh", append([]string{filepath.Join(testkit.ModuleRoot(t), "ci", "native-smoke.sh"), dist}, args...)...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

func TestNativeSmokePassesForAQualifiedDist(t *testing.T) {
	if out, code := nativeSmoke(t, makeDist(t, "0.8.0-rc.0", distOpts{}), "0.8.0-rc.0"); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestNativeSmokeInfersTheVersionOfASnapshotDist(t *testing.T) {
	dir := t.TempDir()
	_ = dir
	d := makeDist(t, "0.8.0-rc.0", distOpts{})
	if out, code := nativeSmoke(t, d); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestNativeSmokeFailsOnAChecksumMismatch(t *testing.T) {
	if out, code := nativeSmoke(t, makeDist(t, "0.8.0-rc.0", distOpts{tamper: true}), "0.8.0-rc.0"); code == 0 {
		t.Fatalf("a tampered archive must fail:\n%s", out)
	}
}

func TestNativeSmokeFailsWhenALicenceFileIsMissing(t *testing.T) {
	for _, f := range []string{"COPYING", "licenses/swagger-ui-NOTICE", "licenses/uplot-LICENSE", "UPGRADE.md"} {
		if out, code := nativeSmoke(t, makeDist(t, "0.8.0-rc.0", distOpts{omitLicence: f}), "0.8.0-rc.0"); code == 0 {
			t.Fatalf("missing %s must fail:\n%s", f, out)
		}
	}
}

func TestNativeSmokeFailsOnAWrongVersion(t *testing.T) {
	if out, code := nativeSmoke(t, makeDist(t, "0.8.0-rc.0", distOpts{}), "0.8.0"); code == 0 {
		t.Fatalf("the binary reports 0.8.0-rc.0, not 0.8.0:\n%s", out)
	}
}

func TestNativeSmokeFailsForGoBelowTheSecurityFloor(t *testing.T) {
	if out, code := nativeSmoke(t, makeDist(t, "0.8.0-rc.0", distOpts{amd64: "amd64-oldgo"}), "0.8.0-rc.0"); code == 0 || !strings.Contains(out, "1.27.4") {
		t.Fatalf("go1.27.3 must fail (exit %d):\n%s", code, out)
	}
}

func TestNativeSmokeFailsForABuildWithTags(t *testing.T) {
	if out, code := nativeSmoke(t, makeDist(t, "0.8.0-rc.0", distOpts{amd64: "amd64-tags"}), "0.8.0-rc.0"); code == 0 || !strings.Contains(out, "tags") {
		t.Fatalf("a release binary built with tags must fail (exit %d):\n%s", code, out)
	}
}

func TestNativeSmokeFailsWithoutBothArchives(t *testing.T) {
	if out, code := nativeSmoke(t, makeDist(t, "0.8.0-rc.0", distOpts{omitArm64Archive: true}), "0.8.0-rc.0"); code == 0 {
		t.Fatalf("a dist without the arm64 archive must fail:\n%s", out)
	}
}
