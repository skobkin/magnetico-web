package metadata

// BEP 52 v2/hybrid handling (task 03). The original audit established
// empirically, before any fix, and is documented in
// docs/wip/storage-implementation-report.md:
//
//   - v2-only metadata used to slip through validateInfo and persist as a
//     zero-size single file (total=0, files=[{0, name}]) — silent corruption,
//     now rejected with ErrV2OnlyUnsupported.
//   - Multi-file hybrids needing BEP 52 padding were rejected by the
//     piece-count comparison (NumPieces() walked the v2 tree while
//     TotalLength() summed real file lengths). On the owner's decision
//     (stop-gate answer, 2026-10-24) hybrids are now supported through
//     v1-authoritative validation; see metadata/hybrid.go.
//
// Hybrid ingestion verifies the v1 SHA-1 identity only; the v2 per-file
// piece roots are not verified and support must not be presented as
// complete v2 hash-layer verification.

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
			// A file node carries its properties in File; Dir stays empty.
			FileTree: metainfo.FileTree{File: metainfo.FileTreeFile{Length: 16384}},
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
	// multi-file hybrids. Supported via v1-authoritative validation: the
	// padding entries are excluded from the extracted list, file count and
	// total size.
	t.Run("padded multi-file extracts real files without padding", func(t *testing.T) {
		t.Parallel()

		raw := auditInfo(t, &metainfo.Info{
			PieceLength: 16384, Pieces: make([]byte, 40), Name: "hybridpad", MetaVersion: 2,
			Files: []metainfo.FileInfo{
				{Length: 1000, Path: []string{"a"}},
				// Fills the piece remainder after "a" exactly (BEP 47).
				{Length: 15384, Path: []string{".padding"},
					ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}},
				{Length: 2000, Path: []string{"b"}},
				// Trailing filler for the remainder left by "b".
				{Length: 14384, Path: []string{".padding"},
					ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}},
			},
			FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
				"a": {File: metainfo.FileTreeFile{Length: 1000}},
				"b": {File: metainfo.FileTreeFile{Length: 2000}},
			}},
		})

		md, err := auditExtract(t, raw)
		if err != nil {
			t.Fatalf("extractMetadata() error = %v", err)
		}
		if md.TotalSize != 3000 || !reflect.DeepEqual(md.Files, []persistence.File{
			{Size: 1000, Path: "a"}, {Size: 2000, Path: "b"},
		}) {
			t.Errorf("extractMetadata() = total %d files %#v", md.TotalSize, md.Files)
		}
	})

	// The final real file may end unaligned without a trailing filler: BEP 47
	// does not require padding the last piece.
	t.Run("unaligned last file without trailing filler is accepted", func(t *testing.T) {
		t.Parallel()

		raw := auditInfo(t, &metainfo.Info{
			PieceLength: 16384, Pieces: make([]byte, 40), Name: "hybridtail", MetaVersion: 2,
			Files: []metainfo.FileInfo{
				{Length: 1000, Path: []string{"a"}},
				{Length: 15384, Path: []string{".padding"},
					ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}},
				{Length: 2000, Path: []string{"b"}},
			},
			FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
				"a": {File: metainfo.FileTreeFile{Length: 1000}},
				"b": {File: metainfo.FileTreeFile{Length: 2000}},
			}},
		})

		md, err := auditExtract(t, raw)
		if err != nil {
			t.Fatalf("extractMetadata() error = %v", err)
		}
		if md.TotalSize != 3000 || !reflect.DeepEqual(md.Files, []persistence.File{
			{Size: 1000, Path: "a"}, {Size: 2000, Path: "b"},
		}) {
			t.Errorf("extractMetadata() = total %d files %#v", md.TotalSize, md.Files)
		}
	})

	// Padding is identified by the attr, never the name: a real file named
	// ".padding" persists, and an attr-p file under an ordinary name is
	// excluded. The attr-p entry sits where BEP 47 requires a filler: after
	// the unaligned-ending ".padding" file.
	t.Run("padding identified by attr not name", func(t *testing.T) {
		t.Parallel()

		raw := auditInfo(t, &metainfo.Info{
			PieceLength: 16384, Pieces: make([]byte, 40), Name: "attrpadding", MetaVersion: 2,
			Files: []metainfo.FileInfo{
				{Length: 1000, Path: []string{".padding"}}, // real file, no attr
				{Length: 15384, Path: []string{"hide.bin"},
					ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}}, // padding by attr
				{Length: 16384, Path: []string{"b"}},
			},
			FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
				".padding": {File: metainfo.FileTreeFile{Length: 1000}},
				"b":        {File: metainfo.FileTreeFile{Length: 16384}},
			}},
		})

		md, err := auditExtract(t, raw)
		if err != nil {
			t.Fatalf("extractMetadata() error = %v", err)
		}
		if md.TotalSize != 17384 || !reflect.DeepEqual(md.Files, []persistence.File{
			{Size: 1000, Path: ".padding"}, {Size: 16384, Path: "b"},
		}) {
			t.Errorf("extractMetadata() = total %d files %#v", md.TotalSize, md.Files)
		}
	})

	// Wrong v1 piece count for the padded v1 layout: the v1 layer is
	// authoritative, so this is rejected even though the v2 tree looks
	// consistent.
	t.Run("padded multi-file with wrong v1 piece count rejected", func(t *testing.T) {
		t.Parallel()

		raw := auditInfo(t, &metainfo.Info{
			PieceLength: 16384, Pieces: make([]byte, 20), Name: "hybridpad", MetaVersion: 2,
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
			t.Fatal("extractMetadata() succeeded with a wrong v1 piece count")
		}
	})
}

// TestAuditHybridRejections pins the malformed-hybrid rejections: the v1
// layer and the v2 file tree must agree, and alignment must hold.
func TestAuditHybridRejections(t *testing.T) {
	t.Parallel()

	padding := func(length int64) metainfo.FileInfo {
		return metainfo.FileInfo{Length: length, Path: []string{".padding"},
			ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}}
	}
	tree := func(files map[string]int64) metainfo.FileTree {
		dir := make(map[string]metainfo.FileTree, len(files))
		for name, length := range files {
			dir[name] = metainfo.FileTree{File: metainfo.FileTreeFile{Length: length}}
		}
		return metainfo.FileTree{Dir: dir}
	}

	tests := []struct {
		name   string
		pieces int // v1 piece count matching the declared v1 layout
		files  []metainfo.FileInfo
		tree   metainfo.FileTree
	}{
		{
			name:   "real file size differs from the tree",
			pieces: 2,
			files: []metainfo.FileInfo{
				{Length: 1000, Path: []string{"a"}},
				padding(15384),
				{Length: 2000, Path: []string{"b"}},
			},
			tree: tree(map[string]int64{"a": 1000, "b": 3000}),
		},
		{
			name:   "real file path missing from the tree",
			pieces: 2,
			files: []metainfo.FileInfo{
				{Length: 1000, Path: []string{"a"}},
				padding(15384),
				{Length: 2000, Path: []string{"c"}},
			},
			tree: tree(map[string]int64{"a": 1000, "b": 2000}),
		},
		{
			name:   "real file count differs from the tree",
			pieces: 3,
			files: []metainfo.FileInfo{
				{Length: 16384, Path: []string{"a"}},
				{Length: 16384, Path: []string{"b"}},
				{Length: 16384, Path: []string{"c"}},
			},
			tree: tree(map[string]int64{"a": 16384, "b": 16384}),
		},
		{
			name:   "v1 real files out of tree order",
			pieces: 3,
			files: []metainfo.FileInfo{
				{Length: 16384, Path: []string{"b"}},
				padding(16384),
				{Length: 16384, Path: []string{"a"}},
			},
			tree: tree(map[string]int64{"a": 16384, "b": 16384}),
		},
		{
			name:   "malformed padding leaves a later real file misaligned",
			pieces: 3,
			files: []metainfo.FileInfo{
				{Length: 1000, Path: []string{"a"}},
				padding(100),                        // must fill to the next piece boundary
				{Length: 2000, Path: []string{"b"}}, // starts at 1100: misaligned
				padding(29668),
				{Length: 16384, Path: []string{"c"}},
			},
			tree: tree(map[string]int64{
				"a": 1000, "b": 2000, "c": 16384,
			}),
		},
		{
			// The review's fixture: a second unaligned real file with the
			// required filler missing — the last real file is not exempt
			// from start alignment.
			name:   "second real file misaligned without padding",
			pieces: 1,
			files: []metainfo.FileInfo{
				{Length: 1000, Path: []string{"a"}},
				{Length: 2000, Path: []string{"b"}}, // starts at 1000: misaligned
			},
			tree: tree(map[string]int64{"a": 1000, "b": 2000}),
		},
		{
			name:   "padding entry at an aligned position",
			pieces: 3,
			files: []metainfo.FileInfo{
				{Length: 16384, Path: []string{"a"}}, // ends aligned: no filler needed
				padding(16384),
				{Length: 16384, Path: []string{"b"}},
			},
			tree: tree(map[string]int64{"a": 16384, "b": 16384}),
		},
		{
			name:   "consecutive padding entries",
			pieces: 3,
			files: []metainfo.FileInfo{
				{Length: 1000, Path: []string{"a"}},
				padding(15384), // exact filler
				padding(16384), // at an aligned position: misplaced
				{Length: 16384, Path: []string{"b"}},
			},
			tree: tree(map[string]int64{"a": 1000, "b": 16384}),
		},
		{
			name:   "trailing filler of the wrong size",
			pieces: 2,
			files: []metainfo.FileInfo{
				{Length: 1000, Path: []string{"a"}},
				padding(15384),
				{Length: 2000, Path: []string{"b"}}, // leaves a 14384-byte remainder
				padding(100),
			},
			tree: tree(map[string]int64{"a": 1000, "b": 2000}),
		},
		{
			name:   "leading padding entry",
			pieces: 2,
			files: []metainfo.FileInfo{
				padding(16384), // nothing precedes it to fill
				{Length: 16384, Path: []string{"b"}},
			},
			tree: tree(map[string]int64{"b": 16384}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// The declared v1 piece count matches the declared v1 layout, so
			// each case exercises its intended tree/alignment check rather
			// than failing the piece-count precondition.
			raw := auditInfo(t, &metainfo.Info{
				PieceLength: 16384, Pieces: make([]byte, 20*tt.pieces), Name: "badhybrid", MetaVersion: 2,
				Files: tt.files, FileTree: tt.tree,
			})
			if _, err := auditExtract(t, raw); err == nil {
				t.Error("extractMetadata() succeeded for a malformed hybrid, want rejection")
			}
		})
	}
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
