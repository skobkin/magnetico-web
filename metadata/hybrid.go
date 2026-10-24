package metadata

// BEP 52 hybrid torrent support (v1 fields plus a v2 file tree).
//
// The pipeline verifies a hybrid's identity through its v1 layer only: the
// SHA-1 infohash the DHT metadata exchange checks is the v1 infohash, and
// the v1 piece hashes cover the padded v1 payload. The v2 per-file piece
// roots ("pieces root") are NOT verified — hybrid ingestion must not be
// presented as complete v2 hash-layer verification.

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/skobkin/magnetico-web/metainfo"
)

// errHybridMismatch reports a hybrid whose v1 layer and v2 file tree carry
// conflicting file information.
var errHybridMismatch = errors.New("v1 file list and v2 file tree disagree")

// isPaddingFile reports whether a v1 file entry is a synthetic BEP 52
// padding file. Padding is identified by the "p" file attribute (BEP 47)
// only — never by its file name, so real files called ".padding" or
// ".pad/123" remain real files.
func isPaddingFile(fi *metainfo.FileInfo) bool {
	return strings.ContainsRune(fi.Attr, 'p')
}

// validateHybrid validates a hybrid info dictionary against its authoritative
// v1 layer:
//
//  1. the v1 layout (padding entries included) must accumulate (checked) to
//     exactly the v1 piece count len(info.Pieces)/20 — the piece hashes the
//     v1 layer actually ships;
//  2. the v1 real files (padding excluded) must be exactly the v2 file
//     tree's files — same paths, sizes and order;
//  3. every real file must start at a piece boundary — including the last;
//     a BEP 47 padding entry is accepted only where a filler is required:
//     directly after a real file that ended mid-piece, filling the exact
//     remainder of that piece.
//
// The v2 tree walk (NumPieces) is deliberately not used to validate v1
// piece hashes: it would count per-file piece spans and reject any layout
// that needs padding.
func validateHybrid(info *metainfo.Info) error {
	var total int64
	if len(info.Files) == 0 {
		// Single-file hybrid: the v1 payload is info.Length.
		if info.Length < 0 {
			return errors.New("negative v1 file length")
		}
		total = info.Length
	}
	for i := range info.Files {
		f := &info.Files[i]
		if f.Length < 0 {
			return errors.New("negative v1 file length")
		}
		if math.MaxInt64-total < f.Length {
			return fmt.Errorf("hybrid v1 file lengths: %w", ErrTotalSizeOverflow)
		}
		total += f.Length
	}

	var expected int64
	if total > 0 {
		expected = (total-1)/info.PieceLength + 1
	}
	if expected != int64(len(info.Pieces)/20) {
		return errors.New("piece count and v1 file lengths are at odds")
	}

	return validateHybridTree(info)
}

func validateHybridTree(info *metainfo.Info) error {
	var tree []metainfo.FileInfo
	appendTreeFiles(&info.FileTree, nil, &tree)

	var real []metainfo.FileInfo
	for i := range info.Files {
		if !isPaddingFile(&info.Files[i]) {
			real = append(real, info.Files[i])
		}
	}

	// Single-file hybrids carry no v1 Files entries; the v1 file is
	// info.Length. The tree must hold exactly that one file.
	if len(real) == 0 {
		if len(tree) != 1 || tree[0].Length != info.Length {
			return errHybridMismatch
		}
		return nil
	}

	if len(real) != len(tree) {
		return errHybridMismatch
	}
	for i := range real {
		if real[i].Length != tree[i].Length || !slices.Equal(real[i].BestPath(), tree[i].Path) {
			return errHybridMismatch
		}
	}

	// Alignment and padding placement (BEP 52 with BEP 47 padding): every
	// real file — including the last — must begin on a piece boundary. A
	// padding entry is valid only as the exact filler directly after a real
	// file that ended mid-piece; the final entry may end unaligned without
	// a trailing filler.
	filled := false // the previous real file ended mid-piece, no filler yet
	var offset int64
	for i := range info.Files {
		f := &info.Files[i]
		rem := offset % info.PieceLength
		if isPaddingFile(f) {
			if !filled || f.Length != info.PieceLength-rem {
				return errHybridMismatch
			}
			filled = false
		} else {
			if rem != 0 {
				return errHybridMismatch
			}
			filled = f.Length%info.PieceLength != 0
		}
		if math.MaxInt64-offset < f.Length {
			return fmt.Errorf("hybrid v1 file lengths: %w", ErrTotalSizeOverflow)
		}
		offset += f.Length
	}
	return nil
}

// appendTreeFiles collects the v2 file tree's files depth-first in component
// byte order — the order BEP 52 requires the v1 layer's real files to
// mirror. The properties entry ("") of a file node is the file itself.
func appendTreeFiles(ft *metainfo.FileTree, prefix []string, out *[]metainfo.FileInfo) {
	if !ft.IsDir() {
		*out = append(*out, metainfo.FileInfo{Length: ft.File.Length, Path: prefix})
		return
	}
	for _, key := range slices.Sorted(maps.Keys(ft.Dir)) {
		if key == metainfo.FileTreePropertiesKey {
			continue
		}
		sub := ft.Dir[key]
		path := make([]string, len(prefix)+1)
		copy(path, prefix)
		path[len(prefix)] = key
		appendTreeFiles(&sub, path, out)
	}
}
