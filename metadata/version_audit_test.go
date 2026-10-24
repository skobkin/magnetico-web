package metadata

// BEP 52 v2/hybrid audit (task 03). The outcomes pinned here were
// established empirically against the actual pipeline before the guard was
// added, and are documented in docs/wip/storage-implementation-report.md:
//
//   - v2-only metadata used to slip through validateInfo and persist as a
//     zero-size single file (total=0, files=[{0, name}]) — silent corruption,
//     now rejected with ErrV2OnlyUnsupported.
//   - Multi-file hybrids whose files need BEP 52 padding (the standard layout
//     for unaligned files) are rejected by the piece-count comparison in
//     validateInfo: NumPieces() sums per-file piece counts from the v2 tree
//     while TotalLength() sums the real file lengths, so any required padding
//     breaks the equality. Aligned multi-file hybrids and hybrid single files
//     pass and extract correctly. This is a recorded limitation, reported to
//     the owner before the persistence store was built on top.
//   - extractFiles does not filter v1 padding entries (attr "p"); in the
//     layouts that pass validation none are present.

import (
	"crypto/sha1"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/bencode"
	"github.com/skobkin/magnetico-web/metainfo"
	"github.com/skobkin/magnetico-web/persistence"
)

// auditInfo builds one of the bencode-marshalled audit fixtures. Fixtures
// use piece length 16384 and only zero or piece-aligned lengths where the
// layout under test requires alignment.
func auditInfo(t *testing.T, info *metainfo.Info) []byte {
	t.Helper()
	raw, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("bencode.Marshal() error = %v", err)
	}
	return raw
}

func auditExtract(t *testing.T, raw []byte) (*Metadata, error) {
	t.Helper()
	return extractMetadata(raw, sha1.Sum(raw), time.Unix(0, 0))
}

func TestAuditV2OnlyRejected(t *testing.T) {
	t.Parallel()

	single := auditInfo(t, &metainfo.Info{
		PieceLength: 16384, Name: "v2single", MetaVersion: 2,
		FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
			"one.bin": {File: metainfo.FileTreeFile{Length: 16384}},
		}},
	})
	multi := auditInfo(t, &metainfo.Info{
		PieceLength: 16384, Name: "v2multi", MetaVersion: 2,
		FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
			"d": {Dir: map[string]metainfo.FileTree{
				// Aligned lengths: without the guard this variant passed
				// validation and still persisted a zero-size file list.
				"a": {File: metainfo.FileTreeFile{Length: 16384}},
				"b": {File: metainfo.FileTreeFile{Length: 16384}},
			}},
		}},
	})

	for name, raw := range map[string][]byte{"single-file": single, "multi-file": multi} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			md, err := auditExtract(t, raw)
			if !errors.Is(err, ErrV2OnlyUnsupported) {
				t.Errorf("extractMetadata() = %+v, %v; want ErrV2OnlyUnsupported", md, err)
			}
		})
	}
}

func TestAuditHybridLayouts(t *testing.T) {
	t.Parallel()

	t.Run("aligned multi-file extracts real files", func(t *testing.T) {
		t.Parallel()

		raw := auditInfo(t, &metainfo.Info{
			PieceLength: 16384, Pieces: make([]byte, 40), Name: "hybridaligned", MetaVersion: 2,
			Files: []metainfo.FileInfo{
				{Length: 16384, Path: []string{"a"}},
				{Length: 16384, Path: []string{"b"}},
			},
			FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
				"a": {File: metainfo.FileTreeFile{Length: 16384}},
				"b": {File: metainfo.FileTreeFile{Length: 16384}},
			}},
		})

		md, err := auditExtract(t, raw)
		if err != nil {
			t.Fatalf("extractMetadata() error = %v", err)
		}
		if md.TotalSize != 32768 || !reflect.DeepEqual(md.Files, []persistence.File{
			{Size: 16384, Path: "a"}, {Size: 16384, Path: "b"},
		}) {
			t.Errorf("extractMetadata() = total %d files %#v", md.TotalSize, md.Files)
		}
	})

	t.Run("single file extracts correctly", func(t *testing.T) {
		t.Parallel()

		raw := auditInfo(t, &metainfo.Info{
			PieceLength: 16384, Pieces: make([]byte, 20), Name: "hybridsingle.bin",
			Length: 16384, MetaVersion: 2,
			FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
				"": {File: metainfo.FileTreeFile{Length: 16384}},
			}},
		})

		md, err := auditExtract(t, raw)
		if err != nil {
			t.Fatalf("extractMetadata() error = %v", err)
		}
		if md.TotalSize != 16384 || !reflect.DeepEqual(md.Files, []persistence.File{
			{Size: 16384, Path: "hybridsingle.bin"},
		}) {
			t.Errorf("extractMetadata() = total %d files %#v", md.TotalSize, md.Files)
		}
	})

	// Padded layouts are the standard BEP 52 representation for unaligned
	// multi-file hybrids. They are rejected by the piece-count comparison —
	// a recorded limitation (stop-gate), not silently mis-extracted. If this
	// test starts failing because validation changed, the owner-approved
	// decision recorded in the implementation report has been superseded and
	// must be revisited.
	t.Run("padded multi-file rejected as a recorded limitation", func(t *testing.T) {
		t.Parallel()

		raw := auditInfo(t, &metainfo.Info{
			PieceLength: 16384, Pieces: make([]byte, 40), Name: "hybridpad", MetaVersion: 2,
			Files: []metainfo.FileInfo{
				{Length: 1000, Path: []string{"a"}},
				{Length: 15362, Path: []string{".padding"},
					ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}},
				{Length: 2000, Path: []string{"b"}},
				{Length: 14384, Path: []string{".padding"},
					ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}},
			},
			FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
				"a": {File: metainfo.FileTreeFile{Length: 1000}},
				"b": {File: metainfo.FileTreeFile{Length: 2000}},
			}},
		})

		if _, err := auditExtract(t, raw); err == nil {
			t.Fatal("extractMetadata() succeeded for a padded hybrid, want the recorded rejection")
		}
	})
}

func TestAuditV1MultiFileRegression(t *testing.T) {
	t.Parallel()

	raw := auditInfo(t, &metainfo.Info{
		PieceLength: 16384, Pieces: make([]byte, 40), Name: "v1multi",
		Files: []metainfo.FileInfo{
			{Length: 16384, Path: []string{"a"}},
			{Length: 16384, Path: []string{"b"}},
		},
	})

	md, err := auditExtract(t, raw)
	if err != nil {
		t.Fatalf("extractMetadata() error = %v", err)
	}
	if md.TotalSize != 32768 || len(md.Files) != 2 {
		t.Errorf("extractMetadata() = total %d files %#v", md.TotalSize, md.Files)
	}
}

func TestTotalSizeCheckedAccumulation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		files   []persistence.File
		want    uint64
		wantErr error
	}{
		{
			name:  "two MaxInt64 files sum just below the uint64 ceiling",
			files: []persistence.File{{Size: math.MaxInt64}, {Size: math.MaxInt64}},
			want:  2*math.MaxUint64/2 - 1, // 2^64 - 2
		},
		{
			name:    "three MaxInt64 files overflow uint64",
			files:   []persistence.File{{Size: math.MaxInt64}, {Size: math.MaxInt64}, {Size: math.MaxInt64}},
			wantErr: ErrTotalSizeOverflow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := totalSize(tt.files)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("totalSize() error = %v, want %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("totalSize() = %d, want %d", got, tt.want)
			}
		})
	}
}
