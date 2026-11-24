package repocheck

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

// layoutOpts describes the attestation manifests of a fake OCI layout.
type layoutOpts struct {
	attestations []string // the predicate annotations of each attestation manifest, one entry per manifest
}

const (
	spdxAnn  = `"in-toto.io/predicate-type": "https://spdx.dev/Document"`
	slsaAnn  = `"in-toto.io/predicate-type": "https://slsa.dev/provenance/v1"`
	twoLayer = spdxAnn + "," + slsaAnn
)

func blob(t *testing.T, root, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	h := hex.EncodeToString(sum[:])
	dir := filepath.Join(root, "blobs", "sha256")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, h), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return "sha256:" + h
}

// attestationLayout writes an OCI layout shaped like docker buildx's output:
// index.json -> one index -> two platform manifests + attestation manifests whose
// layers carry the in-toto predicate-type annotations.
func attestationLayout(t *testing.T, o layoutOpts) string {
	t.Helper()
	root := t.TempDir()
	var children []string
	for _, arch := range []string{"amd64", "arm64"} {
		d := blob(t, root, `{"platform-manifest":"`+arch+`"}`)
		children = append(children, `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"`+d+`","size":10,"platform":{"architecture":"`+arch+`","os":"linux"}}`)
	}
	for i, anns := range o.attestations {
		layers := ""
		if anns != "" {
			layers = `{"mediaType":"application/vnd.in-toto+json","digest":"sha256:` + strings.Repeat("a", 64) + `","size":1,"annotations":{` + anns + `}}`
		}
		d := blob(t, root, `{"schemaVersion":2,"n":`+string(rune('0'+i))+`,"layers":[`+layers+`]}`)
		children = append(children, `{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"`+d+`","size":10,"platform":{"architecture":"unknown","os":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest"}}`)
	}
	index := blob(t, root, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`+strings.Join(children, ",")+`]}`)
	idx := `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.index.v1+json","digest":"` + index + `","size":1}]}`
	if err := os.WriteFile(filepath.Join(root, "index.json"), []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func checkAttestations(t *testing.T, layout string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", filepath.Join(testkit.ModuleRoot(t), "ci", "check-attestations.sh"), layout)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

func TestAttestationsPassWithSBOMAndProvenanceForBothPlatforms(t *testing.T) {
	if out, code := checkAttestations(t, attestationLayout(t, layoutOpts{attestations: []string{twoLayer, twoLayer}})); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// Two attestation manifests exist with provenance alone too: counting them (as
// verify-published does) cannot show that an SBOM is present, this check can.
func TestAttestationsFailWithoutAnSBOMPredicate(t *testing.T) {
	o := layoutOpts{attestations: []string{slsaAnn, slsaAnn}}
	if out, code := checkAttestations(t, attestationLayout(t, o)); code == 0 || !strings.Contains(out, "SBOM") {
		t.Fatalf("a provenance-only image must fail (exit %d):\n%s", code, out)
	}
	o = layoutOpts{attestations: []string{twoLayer, slsaAnn}}
	if out, code := checkAttestations(t, attestationLayout(t, o)); code == 0 {
		t.Fatalf("one platform without an SBOM must fail:\n%s", out)
	}
}

func TestAttestationsFailWithoutProvenance(t *testing.T) {
	o := layoutOpts{attestations: []string{spdxAnn, spdxAnn}}
	if out, code := checkAttestations(t, attestationLayout(t, o)); code == 0 || !strings.Contains(out, "provenance") {
		t.Fatalf("an SBOM-only image must fail (exit %d):\n%s", code, out)
	}
}

func TestAttestationsFailWhenAnAttestationManifestIsMissingOrTheLayoutIsUnusable(t *testing.T) {
	if out, code := checkAttestations(t, attestationLayout(t, layoutOpts{attestations: []string{twoLayer}})); code == 0 {
		t.Fatalf("one attestation manifest for two platforms must fail:\n%s", out)
	}
	if out, code := checkAttestations(t, attestationLayout(t, layoutOpts{})); code == 0 {
		t.Fatalf("no attestation manifests must fail:\n%s", out)
	}
	if out, code := checkAttestations(t, filepath.Join(t.TempDir(), "nothing")); code == 0 {
		t.Fatalf("a missing layout must fail:\n%s", out)
	}
}

func smokeLine(t *testing.T, line string, expect ...string) (string, int) {
	t.Helper()
	args := append([]string{filepath.Join(testkit.ModuleRoot(t), "ci", "image-smoke.sh"), "--check-version-line", line}, expect...)
	out, err := exec.Command("sh", args...).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

const goodLine = "magnetico 0.8.0-rc.0 (commit 0123456789ab, go1.27.4, linux/amd64)"

func TestVersionLineCheckAcceptsAWellFormedLine(t *testing.T) {
	if out, code := smokeLine(t, goodLine); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if out, code := smokeLine(t, goodLine, "0.8.0-rc.0", "0123456789abcdef0123456789abcdef01234567"); code != 0 {
		t.Fatalf("the expected version and the commit prefix match: exit %d:\n%s", code, out)
	}
	if out, code := smokeLine(t, "magnetico dev (commit unknown, go1.28.0, linux/arm64)"); code != 0 {
		t.Fatalf("an unknown commit is fine when none is expected (exit %d):\n%s", code, out)
	}
}

func TestVersionLineCheckRejectsMalformedLinesAndTrailingJunk(t *testing.T) {
	for _, line := range []string{
		"", "garbage", goodLine + " extra", "x " + goodLine,
		"magnetico 0.8.0 (commit 0123456789ab", // the old alternation matched a prefix
		"magnetico 0.8.0 (commit 0123456789ab, go1.27.4)",
		"magnetico  (commit 0123456789ab, go1.27.4, linux/amd64)", // an empty version
		"magnetico 0.8.0 (commit nothex, go1.27.4, linux/amd64)",
	} {
		if out, code := smokeLine(t, line); code == 0 {
			t.Errorf("line %q must be rejected:\n%s", line, out)
		}
	}
}

func TestVersionLineCheckEnforcesTheGoFloorAndTheExpectedIdentity(t *testing.T) {
	if out, code := smokeLine(t, "magnetico 0.8.0 (commit 0123456789ab, go1.27.3, linux/amd64)"); code == 0 || !strings.Contains(out, "1.27.4") {
		t.Fatalf("go1.27.3 is below the floor (exit %d):\n%s", code, out)
	}
	if out, code := smokeLine(t, "magnetico 0.8.0 (commit 0123456789ab, go1.26.9, linux/amd64)"); code == 0 {
		t.Fatalf("go1.26.9 is below the floor:\n%s", out)
	}
	if out, code := smokeLine(t, goodLine, "0.8.0"); code == 0 {
		t.Fatalf("the image reports 0.8.0-rc.0, not 0.8.0:\n%s", out)
	}
	if out, code := smokeLine(t, goodLine, "0.8.0-rc.0", "ffffffffffffffffffffffffffffffffffffffff"); code == 0 {
		t.Fatalf("a different commit must fail:\n%s", out)
	}
	if out, code := smokeLine(t, "magnetico 0.8.0-rc.0 (commit unknown, go1.27.4, linux/amd64)", "0.8.0-rc.0", "0123456789abcdef0123456789abcdef01234567"); code == 0 {
		t.Fatalf("an unknown commit cannot satisfy an expected commit:\n%s", out)
	}
}
