// Package synth generates deterministic synthetic torrent file lists for
// tests and explicit benchmarks of the web service (plan 07E §8.1). It is
// test-support code: nothing in the serving path imports it. Data are
// invented from a seed; no real, production or network-sourced names appear.
//
// Every list returned by Generate is in canonical order (path bytes
// ascending, then size ascending), exactly what codec.Decode produces, and
// satisfies codec.DefaultLimits() (files, path length, aggregate path bytes).
package synth

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/skobkin/magnetico-web/codec"
)

// Shape selects a directory layout.
type Shape string

const (
	// Flat: every file in the root, no directories.
	Flat Shape = "flat"
	// Typical: a shallow-to-moderate tree (depth 1–5, ~25 files per directory).
	Typical Shape = "typical"
	// Deep: long chains of nested directories (depth up to ~24) with few files each.
	Deep Shape = "deep"
	// FlatMax: every file in the root with a ~252-byte, highly compressible name:
	// ~63 MB of path bytes (97% of the 64 MiB aggregate limit) in one directory,
	// the worst case for a copied-names (arena) index.
	FlatMax Shape = "flatmax"
	// DeepChain: a few chains of ~100 nested one- or two-letter directories with
	// many files at the bottom: the worst case for decoders that copy the file
	// list once per ancestor (the trie serializer).
	DeepChain Shape = "deepchain"
	// Wide: 80% of the files are immediate children of one directory.
	Wide Shape = "wide"
	// LongPath: a very long shared directory prefix and short names, so the
	// reconstructed path bytes dwarf the serialized payload (prefix-delta).
	LongPath Shape = "longpath"
	// Adversarial: duplicates, empty components, leading '/', "." and "..",
	// invalid UTF-8, NUL and bidi characters.
	Adversarial Shape = "adversarial"
	// LongName: components of 0.5–40 KiB.
	LongName Shape = "longname"
)

// Spec describes one synthetic torrent.
type Spec struct {
	Name    string
	Shape   Shape
	Files   int
	Seed    uint64
	AvgName int // average base-name length in bytes (0 = shape default)
}

// Stats summarises a generated list.
type Stats struct {
	Files     int
	PathBytes int // sum of full path lengths (the codec's aggregate)
	MaxPath   int
	MaxDepth  int // largest number of '/' in a path
	TotalSize int64
}

// Measure computes Stats of files.
func Measure(files []codec.File) Stats {
	var s Stats
	s.Files = len(files)
	for _, f := range files {
		s.PathBytes += len(f.Path)
		s.MaxPath = max(s.MaxPath, len(f.Path))
		s.MaxDepth = max(s.MaxDepth, strings.Count(f.Path, "/"))
		s.TotalSize += f.Size
	}
	return s
}

// Canonical sorts files into canonical order in place and returns them.
func Canonical(files []codec.File) []codec.File {
	slices.SortFunc(files, func(a, b codec.File) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		switch {
		case a.Size < b.Size:
			return -1
		case a.Size > b.Size:
			return 1
		}
		return 0
	})
	return files
}

// Corpus is the standard named set used by the C1 measurements. Sizes
// follow the codec defaults (250,000 files; 64 MiB aggregate path bytes).
func Corpus() []Spec {
	return []Spec{
		{Name: "typical-100", Shape: Typical, Files: 100, Seed: 1},
		{Name: "typical-1k", Shape: Typical, Files: 1_000, Seed: 2},
		{Name: "typical-10k", Shape: Typical, Files: 10_000, Seed: 3},
		{Name: "typical-250k", Shape: Typical, Files: 250_000, Seed: 4},
		{Name: "flat-250k", Shape: Flat, Files: 250_000, Seed: 5},
		{Name: "deep-250k", Shape: Deep, Files: 250_000, Seed: 6},
		{Name: "flatmax-250k", Shape: FlatMax, Files: 250_000, Seed: 12},
		{Name: "deepchain-250k", Shape: DeepChain, Files: 250_000, Seed: 11},
		{Name: "wide-250k", Shape: Wide, Files: 250_000, Seed: 7},
		{Name: "longpath-250k", Shape: LongPath, Files: 250_000, Seed: 8},
		{Name: "adversarial-50k", Shape: Adversarial, Files: 50_000, Seed: 9},
		{Name: "longname-1500", Shape: LongName, Files: 1_500, Seed: 10},
	}
}

// Torrents derives n independent torrents from base: same shape, different
// seeds and names, with the file count varied by up to ±50% so a mixed
// workload is not n copies of one blob. Deterministic in (base, n).
func Torrents(base Spec, n int) []Spec {
	rng := rand.New(rand.NewPCG(base.Seed, 0x7011))
	out := make([]Spec, n)
	for i := range out {
		s := base
		s.Seed = base.Seed*1_000_003 + uint64(i) + 1
		s.Name = fmt.Sprintf("%s#%d", base.Name, i)
		if base.Files > 1 {
			s.Files = max(1, base.Files/2+rng.IntN(base.Files+1))
		}
		out[i] = s
	}
	return out
}

const nameAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789-_. "

func word(rng *rand.Rand, n int) string {
	if n < 1 {
		n = 1
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = nameAlphabet[rng.IntN(len(nameAlphabet)-1)] // no trailing-space ambiguity
	}
	return string(b)
}

// jitter returns avg varied by roughly ±40%.
func jitter(rng *rand.Rand, avg int) int {
	lo := avg * 6 / 10
	return max(1, lo+rng.IntN(avg*8/10+1))
}

func size(rng *rand.Rand) int64 { return rng.Int64N(1 << 34) }

// Generate builds the canonical list for s. It panics on an unknown shape.
func Generate(s Spec) []codec.File {
	rng := rand.New(rand.NewPCG(s.Seed, 0xc0ffee))
	var files []codec.File
	switch s.Shape {
	case Flat:
		files = genFlat(rng, s)
	case Typical:
		files = genTree(rng, s, 4, 25, 12)
	case Deep:
		files = genDeep(rng, s)
	case FlatMax:
		files = genFlatMax(rng, s)
	case DeepChain:
		files = genDeepChain(rng, s)
	case Wide:
		files = genWide(rng, s)
	case LongPath:
		files = genLongPath(rng, s)
	case Adversarial:
		files = genAdversarial(rng, s)
	case LongName:
		files = genLongName(rng, s)
	default:
		panic("synth: unknown shape " + string(s.Shape))
	}
	return Canonical(files)
}

func avgName(s Spec, def int) int {
	if s.AvgName > 0 {
		return s.AvgName
	}
	return def
}

func genFlat(rng *rand.Rand, s Spec) []codec.File {
	avg := avgName(s, 40)
	files := make([]codec.File, s.Files)
	for i := range files {
		files[i] = codec.File{Path: fmt.Sprintf("%s-%07d.bin", word(rng, jitter(rng, avg)), i), Size: size(rng)}
	}
	return files
}

// dirPool builds directory paths: each new directory extends a random earlier
// one (or the root), up to maxDepth components.
func dirPool(rng *rand.Rand, count, maxDepth, nameLen int, recentBias bool) []string {
	pool := make([]string, 0, count+1)
	pool = append(pool, "")
	depth := []int{0}
	for len(pool) <= count {
		var p int
		if recentBias && rng.IntN(10) < 8 {
			p = max(0, len(pool)-1-rng.IntN(min(len(pool), 3)))
		} else {
			p = rng.IntN(len(pool))
		}
		if depth[p] >= maxDepth {
			p = 0
		}
		pool = append(pool, pool[p]+word(rng, jitter(rng, nameLen))+"/")
		depth = append(depth, depth[p]+1)
	}
	return pool
}

func genTree(rng *rand.Rand, s Spec, maxDepth, perDir, dirName int) []codec.File {
	avg := avgName(s, 40)
	nd := max(1, s.Files/perDir)
	pool := dirPool(rng, nd, maxDepth, dirName, false)
	files := make([]codec.File, s.Files)
	for i := range files {
		d := pool[rng.IntN(len(pool))]
		files[i] = codec.File{Path: fmt.Sprintf("%s%s-%07d.mkv", d, word(rng, jitter(rng, avg)), i), Size: size(rng)}
	}
	return files
}

func genDeep(rng *rand.Rand, s Spec) []codec.File {
	avg := avgName(s, 30)
	nd := max(1, s.Files/8)
	pool := dirPool(rng, nd, 24, 8, true)
	files := make([]codec.File, s.Files)
	for i := range files {
		d := pool[rng.IntN(len(pool))]
		files[i] = codec.File{Path: fmt.Sprintf("%s%s-%07d", d, word(rng, jitter(rng, avg)), i), Size: size(rng)}
	}
	return files
}

func genWide(rng *rand.Rand, s Spec) []codec.File {
	avg := avgName(s, 40)
	wide := s.Files * 8 / 10
	rest := genTree(rng, Spec{Files: s.Files - wide, AvgName: avg}, 3, 25, 10)
	files := make([]codec.File, 0, s.Files)
	for i := 0; i < wide; i++ {
		files = append(files, codec.File{Path: fmt.Sprintf("the-big-directory/%s-%07d.dat", word(rng, jitter(rng, avg)), i), Size: size(rng)})
	}
	return append(files, rest...)
}

func genLongPath(rng *rand.Rand, s Spec) []codec.File {
	var prefix strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&prefix, "level%02d-%s/", i, word(rng, 3))
	}
	pre := prefix.String() // ~200 bytes
	files := make([]codec.File, s.Files)
	for i := range files {
		files[i] = codec.File{Path: fmt.Sprintf("%sg%03d/%s%07d", pre, i/2500, word(rng, 2), i), Size: size(rng)}
	}
	return files
}

func genAdversarial(rng *rand.Rand, s Spec) []codec.File {
	pieces := []string{"", ".", "..", "a", "b b", "\xff\xfe", "\x00", "x\xc3", "é", "\u202e", "�", "100%", "a+b&c#d?e", "\\x41"}
	files := make([]codec.File, 0, s.Files+s.Files/10)
	for i := 0; len(files) < s.Files; i++ {
		depth := rng.IntN(5)
		parts := make([]string, 0, depth+1)
		for d := 0; d <= depth; d++ {
			if rng.IntN(3) == 0 {
				parts = append(parts, pieces[rng.IntN(len(pieces))])
			} else {
				parts = append(parts, word(rng, 1+rng.IntN(6)))
			}
		}
		p := strings.Join(parts, "/")
		switch i % 29 {
		case 0:
			p = "/" + p // leading separator
		case 1:
			p += "/" // trailing separator: an empty file name
		}
		f := codec.File{Path: p, Size: size(rng)}
		files = append(files, f)
		switch {
		case i%17 == 0:
			files = append(files, f) // identical (path, size)
		case i%31 == 0:
			files = append(files, codec.File{Path: p, Size: f.Size + 1}) // same path, other size
		}
	}
	return files
}

func genLongName(rng *rand.Rand, s Spec) []codec.File {
	files := make([]codec.File, s.Files)
	for i := range files {
		n := 500 + rng.IntN(39_500)
		files[i] = codec.File{Path: fmt.Sprintf("long/%04d/%s", i%50, word(rng, n)), Size: size(rng)}
	}
	return files
}

func genDeepChain(rng *rand.Rand, s Spec) []codec.File {
	const chains, depth = 25, 100
	prefixes := make([]string, chains)
	for c := range prefixes {
		var b strings.Builder
		for d := 0; d < depth; d++ {
			b.WriteString(word(rng, 1+rng.IntN(2)))
			b.WriteByte('/')
		}
		prefixes[c] = b.String()
	}
	files := make([]codec.File, s.Files)
	for i := range files {
		files[i] = codec.File{Path: fmt.Sprintf("%s%07d-%s", prefixes[i%chains], i, word(rng, 4)), Size: size(rng)}
	}
	return files
}

func genFlatMax(rng *rand.Rand, s Spec) []codec.File {
	pad := strings.Repeat("x", 245)
	files := make([]codec.File, s.Files)
	for i := range files {
		files[i] = codec.File{Path: fmt.Sprintf("%07d%s", i, pad), Size: size(rng)}
	}
	return files
}
