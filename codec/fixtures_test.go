package codec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// manifest is the fixture manifest committed to testdata/. Fixtures are
// generated once by an external program (not this package's encoder) and
// never regenerated; the pinned bytes pin decode compatibility, not
// recompression determinism.
type manifest struct {
	WireFormat    int               `json:"wire_format"`
	GeneratedWith map[string]string `json:"generated_with"`
	PayloadHex    string            `json:"payload_hex"`
	PayloadFiles  []File            `json:"payload_files"`
	Fixtures      []struct {
		File       string `json:"file"`
		Compressor int    `json:"compressor"`
		Serializer int    `json:"serializer"`
		Outcome    string `json:"outcome"`
		SHA256     string `json:"sha256"`
		Note       string `json:"note"`
	} `json:"fixtures"`
}

func loadManifest(t *testing.T) manifest {
	t.Helper()
	raw, err := os.ReadFile("testdata/manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return m
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	blob, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("fixture %s: bad hex: %v", name, err)
	}
	return blob
}

// TestCompressedFixtures pins the decode behavior of the pinned library
// versions against externally generated, outcome-verified fixtures.
func TestCompressedFixtures(t *testing.T) {
	t.Parallel()

	m := loadManifest(t)
	if m.WireFormat != int(FormatVersion) {
		t.Fatalf("manifest wire format %d, want %d", m.WireFormat, FormatVersion)
	}

	for _, f := range m.Fixtures {
		t.Run(f.File, func(t *testing.T) {
			t.Parallel()

			blob := readFixture(t, f.File)
			sum := sha256.Sum256(blob)
			if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
				t.Fatalf("fixture %s sha256 mismatch: %s != %s (testdata replaced?)", f.File, got, f.SHA256)
			}

			files, err := Decode(blob, DefaultLimits())
			switch f.Outcome {
			case "decode":
				if err != nil {
					t.Fatalf("Decode() error = %v, want the manifest payload", err)
				}
				want := m.PayloadFiles
				if !reflect.DeepEqual(files, want) {
					t.Errorf("Decode() = %#v, want %#v", files, want)
				}
			case "reject-truncated":
				if err == nil || !errors.Is(err, ErrTruncated) {
					t.Errorf("Decode() = %#v, %v; want a truncation rejection", files, err)
				}
			case "reject-corrupt", "reject-trailing":
				// The precise typed error depends on where the decoder
				// notices the defect; any typed rejection is contract-
				// compliant, success is not.
				if err == nil {
					t.Errorf("Decode() succeeded on %s (%s), want rejection", f.File, f.Note)
				}
			default:
				t.Fatalf("unknown outcome %q", f.Outcome)
			}
		})
	}
}
