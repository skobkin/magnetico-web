package filetree

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/codec"
)

func canon(files []codec.File) []codec.File {
	out := slices.Clone(files)
	slices.SortFunc(out, func(a, b codec.File) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return int(a.Size - b.Size)
	})
	return out
}

// ---- reference model (maps; independent of the index code) ------------------

type refDir struct {
	subdirs map[string]bool
	files   []uint32
	nfiles  uint32
	size    int64
}

func reference(files []codec.File) map[string]*refDir {
	m := map[string]*refDir{}
	get := func(k string) *refDir {
		d := m[k]
		if d == nil {
			d = &refDir{subdirs: map[string]bool{}}
			m[k] = d
		}
		return d
	}
	get("")
	for i, f := range files {
		c := strings.Split(f.Path, "/")
		key := func(k int) string { // directory made of the first k components
			if k == 0 {
				return ""
			}
			return strings.Join(c[:k], "/") + "/"
		}
		for k := 0; k < len(c)-1; k++ {
			get(key(k)).subdirs[c[k]] = true
		}
		for k := 0; k < len(c); k++ { // every ancestor, root included
			d := get(key(k))
			d.nfiles++
			d.size += f.Size
		}
		d := get(key(len(c) - 1))
		d.files = append(d.files, uint32(i))
	}
	return m
}

func expectedRows(files []codec.File, m map[string]*refDir, dir string) []Entry {
	d := m[dir]
	var names []string
	for n := range d.subdirs {
		names = append(names, n)
	}
	slices.Sort(names)
	var rows []Entry
	for _, n := range names {
		sub := m[dir+n+"/"]
		rows = append(rows, Entry{Name: n, Dir: true, Size: sub.size, Files: sub.nfiles})
	}
	for j, pos := range d.files {
		f := files[pos]
		name := f.Path[strings.LastIndexByte(f.Path, '/')+1:]
		e := Entry{Name: name, Pos: pos, Size: f.Size}
		if j > 0 {
			pf := files[d.files[j-1]]
			e.Dup = pf.Path[strings.LastIndexByte(pf.Path, '/')+1:] == name
		}
		rows = append(rows, e)
	}
	return rows
}

type builder struct {
	name  string
	build func(files []codec.File) (Index, error)
}

var builders = []builder{
	{"arena", func(f []codec.File) (Index, error) { return BuildArena(context.Background(), f) }},
	{"ref", func(f []codec.File) (Index, error) { return BuildRef(context.Background(), f) }},
}

// verify compares every directory of the reference against the index, paging
// with several page sizes.
func verify(t *testing.T, ix Index, files []codec.File) {
	t.Helper()
	m := reference(files)
	if ix.Files() != len(files) || ix.Dirs() != len(m) {
		t.Fatalf("counts: files %d/%d dirs %d/%d", ix.Files(), len(files), ix.Dirs(), len(m))
	}
	for dir, rd := range m {
		id, ok := ix.Lookup(dir)
		if !ok {
			t.Fatalf("Lookup(%q) not found", dir)
		}
		info := ix.Info(id)
		if info.Files != rd.nfiles || info.Size != rd.size || int(info.NFiles) != len(rd.files) || int(info.NDirs) != len(rd.subdirs) {
			t.Fatalf("Info(%q) = %+v, ref files=%d size=%d nfiles=%d ndirs=%d", dir, info, rd.nfiles, rd.size, len(rd.files), len(rd.subdirs))
		}
		want := expectedRows(files, m, dir)
		for _, ps := range []int{1, 7, 500, 1 << 20} {
			var got []Entry
			for from := 0; from < info.Total(); from += ps {
				got = ix.Page(id, from, ps, got)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("dir %q page size %d:\n got %v\nwant %v", dir, ps, got, want)
			}
		}
	}
	// Prefixes that are not directories.
	for _, bad := range []string{"nope/", "x", "\x00/"} {
		if _, isDir := m[bad]; !isDir {
			if _, ok := ix.Lookup(bad); ok {
				t.Fatalf("Lookup(%q) found a nonexistent directory", bad)
			}
		}
	}
}

func forEach(t *testing.T, files []codec.File, fn func(t *testing.T, ix Index)) {
	t.Helper()
	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			ix, err := b.build(slices.Clone(files))
			if err != nil {
				t.Fatal(err)
			}
			fn(t, ix)
		})
	}
}

func TestSiblingOrderAndFileVsDirectory(t *testing.T) {
	// Canonical order interleaves "a-b/", "a.txt" and "a/": a directory and a
	// file of the same name are distinct rows, and sibling directories sort by
	// name bytes, not by first appearance.
	files := canon([]codec.File{
		{Path: "a", Size: 1}, {Path: "a/x", Size: 2}, {Path: "a.txt", Size: 3}, {Path: "a-b/x", Size: 4},
		{Path: "a-b/y/z", Size: 5}, {Path: "b/a", Size: 6}, {Path: "b/a/q", Size: 7},
	})
	forEach(t, files, func(t *testing.T, ix Index) {
		verify(t, ix, files)
		root, _ := ix.Lookup("")
		rows := ix.Page(root, 0, 100, nil)
		var names []string
		for _, r := range rows {
			names = append(names, fmt.Sprintf("%s:%v", r.Name, r.Dir))
		}
		want := []string{"a:true", "a-b:true", "b:true", "a:false", "a.txt:false"}
		if !slices.Equal(names, want) {
			t.Fatalf("root rows %v, want %v", names, want)
		}
	})
}

func TestUnusualPaths(t *testing.T) {
	files := canon([]codec.File{
		{Path: "/lead", Size: 1}, {Path: "//two", Size: 2}, {Path: "a//b", Size: 3}, {Path: "a//b", Size: 3},
		{Path: "a//b", Size: 9}, {Path: "a/", Size: 4}, {Path: "./x", Size: 5}, {Path: "../y", Size: 6},
		{Path: "../../z", Size: 7}, {Path: ".", Size: 8}, {Path: "..", Size: 9}, {Path: "", Size: 10},
		{Path: "d/\xff\xfe", Size: 11}, {Path: "d/\xff\xfe/n", Size: 12}, {Path: "d/\x00", Size: 13},
		{Path: strings.Repeat("L", 60000) + "/" + strings.Repeat("M", 4000), Size: 14},
		{Path: "dup", Size: 1}, {Path: "dup", Size: 1},
	})
	forEach(t, files, func(t *testing.T, ix Index) {
		verify(t, ix, files)
		// The root directory and "/" (a directory with an empty name) differ.
		r, _ := ix.Lookup("")
		s, ok := ix.Lookup("/")
		if !ok || r == s {
			t.Fatalf("\"/\" must be its own directory: %v %v", ok, r == s)
		}
		// "." and ".." are ordinary names.
		for _, d := range []string{"./", "../", "../../", "a//", "//", "d/\xff\xfe/"} {
			if _, ok := ix.Lookup(d); !ok {
				t.Fatalf("Lookup(%q)", d)
			}
		}
		// Duplicates are separate rows and marked.
		d, _ := ix.Lookup("")
		var dups []Entry
		for _, e := range ix.Page(d, 0, 1000, nil) {
			if e.Name == "dup" {
				dups = append(dups, e)
			}
		}
		if len(dups) != 2 || dups[0].Dup || !dups[1].Dup || dups[0].Pos == dups[1].Pos {
			t.Fatalf("duplicates: %+v", dups)
		}
		ab, _ := ix.Lookup("a//")
		var bs []Entry
		for _, e := range ix.Page(ab, 0, 10, nil) {
			bs = append(bs, e)
		}
		if len(bs) != 3 || bs[0].Size != 3 || bs[1].Size != 3 || bs[2].Size != 9 || !bs[1].Dup || !bs[2].Dup {
			t.Fatalf("a//b entries: %+v", bs)
		}
	})
}

func TestDupMarkerAcrossPageBoundary(t *testing.T) {
	files := canon([]codec.File{{Path: "q", Size: 1}, {Path: "q", Size: 1}, {Path: "q", Size: 2}, {Path: "r", Size: 1}})
	forEach(t, files, func(t *testing.T, ix Index) {
		root, _ := ix.Lookup("")
		p2 := ix.Page(root, 1, 1, nil) // the second q alone
		if len(p2) != 1 || !p2[0].Dup || p2[0].Pos != 1 {
			t.Fatalf("%+v", p2)
		}
	})
}

func TestEmptyAndSingle(t *testing.T) {
	forEach(t, nil, func(t *testing.T, ix Index) {
		verify(t, ix, nil)
	})
	one := []codec.File{{Path: "only.bin", Size: 5}}
	forEach(t, one, func(t *testing.T, ix Index) { verify(t, ix, one) })
}

func TestPageBounds(t *testing.T) {
	files := canon([]codec.File{{Path: "a/b", Size: 1}, {Path: "c", Size: 2}})
	forEach(t, files, func(t *testing.T, ix Index) {
		root, _ := ix.Lookup("")
		if got := ix.Page(root, 5, 10, nil); len(got) != 0 {
			t.Fatal("from past the end")
		}
		if got := ix.Page(root, -3, 10, nil); len(got) != 2 {
			t.Fatalf("negative from clamps to 0: %d", len(got))
		}
		if got := ix.Page(root, 0, 0, nil); len(got) != 0 {
			t.Fatal("n=0")
		}
		pre := []Entry{{Name: "keep"}}
		if got := ix.Page(root, 1, 1, pre); len(got) != 2 || got[0].Name != "keep" {
			t.Fatal("append semantics")
		}
		if _, ok := ix.Lookup("a"); ok {
			t.Fatal("prefix without trailing slash must not resolve")
		}
	})
}

func TestWideDirectory(t *testing.T) {
	var files []codec.File
	for i := 0; i < 100_000; i++ {
		files = append(files, codec.File{Path: fmt.Sprintf("wide/f%06d", i), Size: int64(i)})
		if i%1000 == 0 {
			files = append(files, codec.File{Path: fmt.Sprintf("wide/d%03d/x", i/1000), Size: 1})
		}
	}
	files = canon(files)
	forEach(t, files, func(t *testing.T, ix Index) {
		id, ok := ix.Lookup("wide/")
		if !ok {
			t.Fatal("wide/")
		}
		info := ix.Info(id)
		if info.NFiles != 100_000 || info.NDirs != 100 {
			t.Fatalf("%+v", info)
		}
		pg := ix.Page(id, 100+99_990, 500, nil) // last ten files
		if len(pg) != 10 || pg[9].Name != "f099999" {
			t.Fatalf("tail page: %d %v", len(pg), pg)
		}
	})
}

func TestRandomAgainstReference(t *testing.T) {
	alphabet := []string{"a", "b", "ab", "a-b", "a.b", ".", "..", "", "x", "\xff", "é", "%", "a b"}
	for seed := uint64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewPCG(seed, 99))
		n := 1 + rng.IntN(200)
		var files []codec.File
		for i := 0; i < n; i++ {
			depth := rng.IntN(5)
			parts := make([]string, 0, depth+1)
			for d := 0; d <= depth; d++ {
				parts = append(parts, alphabet[rng.IntN(len(alphabet))])
			}
			files = append(files, codec.File{Path: strings.Join(parts, "/"), Size: int64(rng.IntN(50))})
		}
		files = canon(files)
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			forEach(t, files, func(t *testing.T, ix Index) { verify(t, ix, files) })
		})
	}
}

func TestBuildErrors(t *testing.T) {
	for _, b := range builders {
		t.Run(b.name, func(t *testing.T) {
			if _, err := b.build([]codec.File{{Path: "b", Size: 1}, {Path: "a", Size: 1}}); !errors.Is(err, ErrNotCanonical) {
				t.Fatalf("unsorted: %v", err)
			}
			if _, err := b.build([]codec.File{{Path: "a", Size: 2}, {Path: "a", Size: 1}}); !errors.Is(err, ErrNotCanonical) {
				t.Fatalf("size order: %v", err)
			}
			if _, err := b.build([]codec.File{{Path: "a", Size: -1}}); !errors.Is(err, ErrNegativeSize) {
				t.Fatalf("negative: %v", err)
			}
			if _, err := b.build([]codec.File{{Path: "d/a", Size: math.MaxInt64}, {Path: "d/b", Size: 1}}); !errors.Is(err, ErrOverflow) {
				t.Fatalf("overflow in directory: %v", err)
			}
			if _, err := b.build([]codec.File{{Path: "a/x", Size: math.MaxInt64}, {Path: "b/x", Size: 1}}); !errors.Is(err, ErrOverflow) {
				t.Fatalf("overflow at the root: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var err error
			if b.name == "arena" {
				_, err = BuildArena(ctx, []codec.File{{Path: "a", Size: 1}})
			} else {
				_, err = BuildRef(ctx, []codec.File{{Path: "a", Size: 1}})
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled build: %v", err)
			}
		})
	}
}

// The arena index must not retain the decoded list; the ref index must keep it
// readable. Also checks Bytes() is a sane positive accounting.
func TestBytesAccounting(t *testing.T) {
	var files []codec.File
	for i := 0; i < 1000; i++ {
		files = append(files, codec.File{Path: fmt.Sprintf("d%d/file-%04d.bin", i%10, i), Size: 1})
	}
	files = canon(files)
	a, _ := BuildArena(context.Background(), files)
	r, _ := BuildRef(context.Background(), files)
	var pathBytes int
	for _, f := range files {
		pathBytes += len(f.Path)
	}
	if a.Bytes() <= 0 || a.Bytes() >= r.Bytes() {
		t.Fatalf("arena %d, ref %d, path bytes %d", a.Bytes(), r.Bytes(), pathBytes)
	}
	if r.Bytes() < pathBytes+1000*fileHeaderSize {
		t.Fatalf("ref accounting below the retained list: %d", r.Bytes())
	}
}

// Concurrent readers over an immutable index (run with -race).
func TestConcurrentReaders(t *testing.T) {
	var files []codec.File
	for i := 0; i < 5000; i++ {
		files = append(files, codec.File{Path: fmt.Sprintf("d%d/e%d/f%d", i%7, i%13, i), Size: int64(i)})
	}
	files = canon(files)
	forEach(t, files, func(t *testing.T, ix Index) {
		done := make(chan struct{})
		for w := 0; w < 8; w++ {
			go func() {
				defer func() { done <- struct{}{} }()
				var buf []Entry
				for i := 0; i < 200; i++ {
					id, ok := ix.Lookup(fmt.Sprintf("d%d/e%d/", i%7, i%13))
					if !ok {
						return
					}
					buf = ix.Page(id, 0, 50, buf[:0])
				}
			}()
		}
		for w := 0; w < 8; w++ {
			<-done
		}
	})
}
