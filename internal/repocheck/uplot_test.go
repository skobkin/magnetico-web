package repocheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readRepo(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The licence of the vendored charting library travels with every distribution.

func TestDockerfileShipsTheUPlotLicence(t *testing.T) {
	df := readRepo(t, "Dockerfile")
	want := "COPY third_party/uplot-1.6.32/LICENSE /usr/share/licenses/magnetico/uplot-LICENSE"
	if !strings.Contains(df, want) {
		t.Fatalf("the Dockerfile does not copy the uPlot licence into the image (expected %q)", want)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "third_party", "uplot-1.6.32", "LICENSE")); err != nil {
		t.Fatalf("the licence file the Dockerfile copies is missing: %v", err)
	}
}

func TestGoReleaserArchivesShipTheUPlotLicence(t *testing.T) {
	gr := readRepo(t, ".goreleaser.yaml")
	re := regexp.MustCompile(`(?m)^\s*- src: third_party/uplot-1\.6\.32/LICENSE\n\s*dst: licenses/uplot-LICENSE\n`)
	if !re.MatchString(gr) {
		t.Fatal("the GoReleaser archive does not ship licenses/uplot-LICENSE from third_party/uplot-1.6.32/LICENSE")
	}
}

func TestNativeAndImageSmokeRequireTheUPlotLicence(t *testing.T) {
	if !strings.Contains(readRepo(t, "ci/native-smoke.sh"), "licenses/uplot-LICENSE") {
		t.Error("ci/native-smoke.sh does not require licenses/uplot-LICENSE in the archives")
	}
	if !strings.Contains(readRepo(t, "ci/image-smoke.sh"), "/usr/share/licenses/magnetico/uplot-LICENSE") {
		t.Error("ci/image-smoke.sh does not require the uPlot licence in the image")
	}
	// A native archive without the licence must fail the smoke test (the behaviour, not only the text).
	if out, code := nativeSmoke(t, makeDist(t, "0.8.0-rc.0", distOpts{omitLicence: "licenses/uplot-LICENSE"}), "0.8.0-rc.0"); code == 0 {
		t.Fatalf("an archive without the uPlot licence passed the native smoke test:\n%s", out)
	}
}
