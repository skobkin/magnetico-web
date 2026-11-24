package devseed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/filetree"
	"github.com/skobkin/magnetico-web/internal/web/fileview"
	"github.com/skobkin/magnetico-web/persistence"
)

// datasetDigest is SHA-256 over a canonical encoding of the whole dataset.
func datasetDigest(data []Fixture) string {
	h := sha256.New()
	w := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	num := func(v int64) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(v))
		h.Write(n[:])
	}
	for _, f := range data {
		w(f.Key)
		w(string(f.Kind))
		w(string(f.Torrent.InfoHash))
		w(f.Torrent.Name)
		num(f.Torrent.TotalSize)
		num(f.Torrent.DiscoveredOn)
		num(int64(len(f.Files)))
		for _, file := range f.Files {
			w(file.Path)
			num(file.Size)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// The dataset is pinned: a change to the generator must bump Version and
// update these values consciously.
const (
	pinnedDigest        = "12abf846828e6662d84dc1c44c85fc6d432ef5ebab65fcbf07f07c7e01f5cba7"
	pinnedFirstInfoHash = "59842347fe411e7b5672e64d25cadde5ec4b50d2"
)

func TestDatasetShape(t *testing.T) {
	data := Dataset()
	if len(data) != TotalTorrents || TotalTorrents != 101 {
		t.Fatalf("torrents = %d, want 101", len(data))
	}
	counts := map[Kind]int{}
	multiMin, multiMax := 1<<30, 0
	for _, f := range data {
		counts[f.Kind]++
		n := len(f.Files)
		switch f.Kind {
		case KindSingle:
			if n != 1 {
				t.Errorf("%s: %d files, want 1", f.Key, n)
			}
		case KindMulti:
			multiMin, multiMax = min(multiMin, n), max(multiMax, n)
			if n < 10 || n > 100 {
				t.Errorf("%s: %d files, want 10..100", f.Key, n)
			}
		case KindLarge:
			if n != 10_000 {
				t.Errorf("%s: %d files, want exactly 10000", f.Key, n)
			}
		}
	}
	if counts[KindSingle] != 40 || counts[KindMulti] != 60 || counts[KindLarge] != 1 {
		t.Errorf("kinds = %v, want 40/60/1", counts)
	}
	if multiMin != 10 || multiMax != 100 {
		t.Errorf("multi file counts span %d..%d, want the bounds 10 and 100 to occur", multiMin, multiMax)
	}
}

func TestDatasetIsDeterministicAndPinned(t *testing.T) {
	a, b := Dataset(), Dataset()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two calls returned different datasets")
	}
	if got := hex.EncodeToString(a[0].Torrent.InfoHash); got != pinnedFirstInfoHash {
		t.Errorf("first infohash = %s, want %s (bump Version when changing the dataset)", got, pinnedFirstInfoHash)
	}
	if got := datasetDigest(a); got != pinnedDigest {
		t.Errorf("dataset digest = %s, want %s (bump Version when changing the dataset)", got, pinnedDigest)
	}
}

func TestDatasetIsValidForProduction(t *testing.T) {
	limits := codec.DefaultLimits()
	enc := codec.NewEncoder(codec.DefaultProfile(), limits, 1)
	defer enc.Close()

	seenHash, seenName := map[string]string{}, map[string]string{}
	for _, f := range Dataset() {
		if err := persistence.ValidateIngest(f.Torrent, f.Files, limits); err != nil {
			t.Fatalf("%s: production validation: %v", f.Key, err)
		}
		if len(f.Torrent.InfoHash) != 20 {
			t.Fatalf("%s: infohash length %d", f.Key, len(f.Torrent.InfoHash))
		}
		if prev, ok := seenHash[string(f.Torrent.InfoHash)]; ok {
			t.Errorf("%s and %s share an infohash", f.Key, prev)
		}
		seenHash[string(f.Torrent.InfoHash)] = f.Key
		if prev, ok := seenName[f.Torrent.Name]; ok {
			t.Errorf("%s and %s share a name", f.Key, prev)
		}
		seenName[f.Torrent.Name] = f.Key

		if persistence.NoBlobEligible(f.Torrent, f.Files) {
			continue
		}
		blob, _, err := enc.Encode(f.Files)
		if err != nil {
			t.Fatalf("%s: encode: %v", f.Key, err)
		}
		got, err := codec.Decode(blob, limits)
		if err != nil {
			t.Fatalf("%s: decode: %v", f.Key, err)
		}
		if !sameMultiset(got, f.Files) {
			t.Errorf("%s: decoded file list differs from the input", f.Key)
		}
	}
}

func sameMultiset(a, b []codec.File) bool {
	key := func(l []codec.File) []string {
		out := make([]string, len(l))
		for i, f := range l {
			out[i] = f.Path + "\x00" + itoa(f.Size)
		}
		sort.Strings(out)
		return out
	}
	return reflect.DeepEqual(key(a), key(b))
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// TestDatasetCoverage asserts the manual-test coverage the dataset promises.
func TestDatasetCoverage(t *testing.T) {
	data := Dataset()
	var cyr, jpn, ascii, special, long int
	minSize, maxSize := int64(1<<62), int64(0)
	stamps := map[int64]int{}
	var minTS, maxTS int64 = 1 << 62, 0
	var atypicalSingle, noBlobSingle, dupPair, samePathDiffSize, fileDirClash, dotComp, leadSlash, trailSlash, dblSlash, zeroSize, depth15, longPath, longComp int

	for _, f := range data {
		n := f.Torrent.Name
		hasC, hasJ := false, false
		for _, r := range n {
			hasC = hasC || unicode.Is(unicode.Cyrillic, r)
			hasJ = hasJ || unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han)
		}
		switch {
		case hasC:
			cyr++
		case hasJ:
			jpn++
		default:
			ascii++
		}
		if strings.ContainsAny(n, `&<>"'%#?[]|;`) {
			special++
		}
		if len(n) > 250 {
			long++
		}
		minSize, maxSize = min(minSize, f.Torrent.TotalSize), max(maxSize, f.Torrent.TotalSize)
		minTS, maxTS = min(minTS, f.Torrent.DiscoveredOn), max(maxTS, f.Torrent.DiscoveredOn)
		stamps[f.Torrent.DiscoveredOn]++

		if f.Kind == KindSingle {
			if persistence.NoBlobEligible(f.Torrent, f.Files) {
				noBlobSingle++
			} else {
				atypicalSingle++
			}
		}
		type ps struct {
			p string
			s int64
		}
		pairs, paths := map[ps]int{}, map[string]map[int64]bool{}
		set := map[string]bool{}
		for _, file := range f.Files {
			set[file.Path] = true
			pairs[ps{file.Path, file.Size}]++
			if paths[file.Path] == nil {
				paths[file.Path] = map[int64]bool{}
			}
			paths[file.Path][file.Size] = true
			if file.Size == 0 {
				zeroSize++
			}
			if strings.HasPrefix(file.Path, "/") {
				leadSlash++
			}
			if strings.HasSuffix(file.Path, "/") {
				trailSlash++
			}
			if strings.Contains(file.Path, "//") {
				dblSlash++
			}
			for _, c := range strings.Split(file.Path, "/") {
				if c == "." || c == ".." {
					dotComp++
					break
				}
			}
			if strings.Count(file.Path, "/") >= 15 {
				depth15++
			}
			if len(file.Path) > 1000 {
				longPath++
			}
			for _, c := range strings.Split(file.Path, "/") {
				if len(c) > 200 {
					longComp++
					break
				}
			}
		}
		for _, c := range pairs {
			if c > 1 {
				dupPair++
			}
		}
		for _, sizes := range paths {
			if len(sizes) > 1 {
				samePathDiffSize++
			}
		}
		for p := range set {
			if len(set) > 200 {
				break // quadratic scan: the multi-file fixtures are small, the large one has no clashes by design
			}
			for q := range set {
				if strings.HasPrefix(q, p+"/") {
					fileDirClash++
					break
				}
			}
		}
	}

	for name, v := range map[string]int{
		"ascii names": ascii, "cyrillic names": cyr, "japanese names": jpn, "special-character names": special,
		"long names": long, "atypical single-file blobs": atypicalSingle, "no-blob singles": noBlobSingle,
		"duplicate (path,size) pairs": dupPair, "same path different size": samePathDiffSize,
		"file/directory name clashes": fileDirClash, "dot components": dotComp, "leading slash": leadSlash,
		"trailing slash": trailSlash, "double slash": dblSlash, "zero-size files": zeroSize,
		"deep paths": depth15, "long paths": longPath, "long components": longComp,
	} {
		if v == 0 {
			t.Errorf("no fixture covers: %s", name)
		}
	}
	if minSize != 1 || maxSize < 1<<40 {
		t.Errorf("sizes span %d..%d, want 1 byte up to at least 1 TiB", minSize, maxSize)
	}
	if maxTS-minTS < 5*365*86400 {
		t.Errorf("discovery times span only %d s", maxTS-minTS)
	}
	ties := 0
	for _, c := range stamps {
		if c > 1 {
			ties++
		}
	}
	if ties == 0 {
		t.Error("no two torrents share a discovery timestamp (ordering ties are not exercised)")
	}
}

// TestBothFileBrowserModes: the web UI serves a torrent with the complete
// tree up to fileview.DefaultMaxTreeFiles files and paginates beyond it; the
// dataset must hit both, and every list must build a web index.
func TestBothFileBrowserModes(t *testing.T) {
	var tree, paged int
	for _, f := range Dataset() {
		if f.Kind == KindSingle {
			continue
		}
		// The index requires canonical order; take it from the codec itself.
		blob, _, err := codec.NewEncoder(codec.DefaultProfile(), codec.DefaultLimits(), 1).Encode(f.Files)
		if err != nil {
			t.Fatal(err)
		}
		canon, err := codec.Decode(blob, codec.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := filetree.BuildArena(context.Background(), canon); err != nil {
			t.Errorf("%s: web file index: %v", f.Key, err)
		}
		if len(f.Files) > fileview.DefaultMaxTreeFiles {
			paged++
		} else {
			tree++
		}
	}
	if tree != 60 || paged != 1 {
		t.Errorf("tree-mode = %d, paginated-mode = %d, want 60 and 1", tree, paged)
	}
}

func TestInfoHashDependsOnVersionedKey(t *testing.T) {
	if bytes.Equal(InfoHash("a"), InfoHash("b")) {
		t.Fatal("different keys share a hash")
	}
	if len(InfoHash("a")) != 20 {
		t.Fatal("infohash is not 20 bytes")
	}
}

func TestSameFilesPreservesMultiplicity(t *testing.T) {
	a := []persistence.File{{Path: "a", Size: 1}, {Path: "a", Size: 1}, {Path: "b", Size: 2}}
	cases := []struct {
		name string
		b    []persistence.File
		want bool
	}{
		{"reordered", []persistence.File{{Path: "b", Size: 2}, {Path: "a", Size: 1}, {Path: "a", Size: 1}}, true},
		{"multiplicity moved", []persistence.File{{Path: "a", Size: 1}, {Path: "b", Size: 2}, {Path: "b", Size: 2}}, false},
		{"size differs", []persistence.File{{Path: "a", Size: 1}, {Path: "a", Size: 2}, {Path: "b", Size: 2}}, false},
		{"shorter", a[:2], false},
	}
	for _, c := range cases {
		if got := sameFiles(a, c.b); got != c.want {
			t.Errorf("%s: sameFiles = %v, want %v", c.name, got, c.want)
		}
	}
}
