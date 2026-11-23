// Package filetree is the pure directory index over a decoded torrent file
// list (web-v1 §6.4; plan 07E §6.2). It holds no SQL, no HTTP and no cache.
//
// Input is the canonical file list produced by codec.Decode (paths ascending
// by bytes, then size ascending). Paths are opaque byte strings: the index
// splits them on '/' for grouping only and never normalises, merges or
// drops anything. Empty components, leading '/', literal "." and "..",
// invalid UTF-8 and duplicate (path, size) entries all survive. A file's
// position in the input is its identity (Entry.Pos).
//
// Two candidate representations are built from the same scan:
//
//   - Arena (variant A): component bytes are copied into one string arena
//     and the decoded []File can be freed.
//   - Ref (variant B): the decoded []File is retained and the index only
//     references it.
//
// Both answer the same questions through the Index interface.
//
// Construction has two phases. Scan walks the canonical list once with a
// stack of open directories and records directories, per-file parents and
// checked recursive aggregates; it does not fill children, because a
// directory's children are discovered interleaved with its descendants'.
// Pack then assigns every directory one contiguous block of the shared kids
// array: its subdirectories sorted by name bytes, followed by its files in
// canonical order.
package filetree

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"

	"github.com/skobkin/magnetico-web/codec"
)

// DirID identifies a directory; the root is 0.
type DirID uint32

// Root is the id of the root directory (the empty dir string).
const Root DirID = 0

// Errors of the index builders.
var (
	ErrNotCanonical = errors.New("filetree: file list is not in canonical order")
	ErrNegativeSize = errors.New("filetree: negative file size")
	ErrOverflow     = errors.New("filetree: aggregate size or count overflows")
	ErrTooLarge     = errors.New("filetree: list exceeds the index's 32-bit capacity")
)

// Entry is one row of a directory listing.
type Entry struct {
	Name  string // the component or file name, exact bytes
	Dir   bool
	Pos   uint32 // files: canonical position in the decoded list
	Size  int64  // files: size; directories: recursive aggregate
	Files uint32 // directories: recursive file count
	// Dup marks a file whose name equals the previous file entry's name in the
	// same directory (an identical path, with the same or a different size).
	Dup bool
}

// DirInfo describes a directory.
type DirInfo struct {
	Files  uint32 // recursive file count
	Size   int64  // recursive size
	NDirs  uint32 // immediate subdirectories
	NFiles uint32 // immediate files
}

// Total is the number of listing rows (subdirectories first, then files).
func (d DirInfo) Total() int { return int(d.NDirs) + int(d.NFiles) }

// Index answers directory questions about one file list. Implementations are
// immutable after construction and safe for concurrent use.
type Index interface {
	// Lookup resolves an exact directory prefix: "" is the root; any other
	// directory is "comp/comp/" (note the trailing '/'); "/" is the directory
	// with an empty name directly under the root. It reports false for an
	// unknown directory or a prefix without the trailing separator.
	Lookup(dir string) (DirID, bool)
	Info(d DirID) DirInfo
	// Page appends rows [from, from+n) of directory d to dst (clamped to the
	// listing) and returns it.
	Page(d DirID, from, n int, dst []Entry) []Entry
	// Files is the total file count; Dirs the number of directories (incl. root).
	Files() int
	Dirs() int
	// Bytes is the accounted retained size of the index (see each variant).
	Bytes() int
}

// ---- shared core --------------------------------------------------------------

// dirCore is the per-directory record shared by both variants (24 bytes).
type dirCore struct {
	size   int64
	files  uint32 // recursive file count
	kidLo  uint32 // start of the directory's block in kids
	nDirs  uint32
	nFiles uint32
}

const dirCoreBytes = 24

// namer resolves names and sizes for the variant that owns the core.
type namer interface {
	dirName(DirID) string
	fileName(uint32) string
	fileSize(uint32) int64
}

type core struct {
	dirs  []dirCore
	kids  []uint32 // per directory: dir ids sorted by name, then file positions
	names namer
	nfile int
}

func (c *core) Files() int { return c.nfile }
func (c *core) Dirs() int  { return len(c.dirs) }

func (c *core) Info(d DirID) DirInfo {
	r := &c.dirs[d]
	return DirInfo{Files: r.files, Size: r.size, NDirs: r.nDirs, NFiles: r.nFiles}
}

func (c *core) Lookup(dir string) (DirID, bool) {
	if dir == "" {
		return Root, true
	}
	if dir[len(dir)-1] != '/' {
		return 0, false
	}
	s := dir[:len(dir)-1]
	cur := Root
	for {
		comp := s
		j := strings.IndexByte(s, '/')
		if j >= 0 {
			comp = s[:j]
		}
		r := &c.dirs[cur]
		block := c.kids[r.kidLo : r.kidLo+r.nDirs]
		i, found := slices.BinarySearchFunc(block, comp, func(id uint32, name string) int {
			return strings.Compare(c.names.dirName(DirID(id)), name)
		})
		if !found {
			return 0, false
		}
		cur = DirID(block[i])
		if j < 0 {
			return cur, true
		}
		s = s[j+1:]
	}
}

func (c *core) Page(d DirID, from, n int, dst []Entry) []Entry {
	r := &c.dirs[d]
	total := int(r.nDirs) + int(r.nFiles)
	if from < 0 {
		from = 0
	}
	if n <= 0 || from >= total {
		return dst
	}
	end := min(total, from+n)
	block := c.kids[r.kidLo : int(r.kidLo)+total]
	nd := int(r.nDirs)
	for i := from; i < end; i++ {
		id := block[i]
		if i < nd {
			sub := &c.dirs[id]
			dst = append(dst, Entry{Name: c.names.dirName(DirID(id)), Dir: true, Size: sub.size, Files: sub.files})
			continue
		}
		e := Entry{Name: c.names.fileName(id), Pos: id, Size: c.names.fileSize(id)}
		if i > nd { // the previous row is a file of this directory too
			e.Dup = c.names.fileName(block[i-1]) == e.Name
		}
		dst = append(dst, e)
	}
	return dst
}

// ---- scan and pack -----------------------------------------------------------

// tdir is the construction-time directory record: its name is
// files[first].Path[off:off+n].
type tdir struct {
	parent, first, off, n uint32
}

type skeleton struct {
	td         []tdir
	core       []dirCore // aggregates; kidLo is set by pack
	fileParent []uint32
}

func (s *skeleton) name(files []codec.File, id uint32) string {
	t := &s.td[id]
	return files[t.first].Path[t.off : t.off+t.n]
}

func addSize(a, b int64) (int64, bool) {
	if a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}

const ctxCheckEvery = 4096

// scan is phase 1.
func scan(ctx context.Context, files []codec.File) (*skeleton, error) {
	if len(files) > math.MaxInt32 {
		return nil, ErrTooLarge
	}
	s := &skeleton{
		td:         make([]tdir, 1, 64),
		core:       make([]dirCore, 1, 64),
		fileParent: make([]uint32, len(files)),
	}
	var stack []uint32 // open directories below the root, outermost first
	closeTo := func(depth int) error {
		for len(stack) > depth {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			p := s.td[id].parent
			sum, ok := addSize(s.core[p].size, s.core[id].size)
			if !ok || s.core[p].files > math.MaxUint32-s.core[id].files {
				return ErrOverflow
			}
			s.core[p].size = sum
			s.core[p].files += s.core[id].files
		}
		return nil
	}
	prev := ""
	for i := range files {
		if i&(ctxCheckEvery-1) == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		f := &files[i]
		if f.Size < 0 {
			return nil, ErrNegativeSize
		}
		if i > 0 && (f.Path < prev || (f.Path == prev && f.Size < files[i-1].Size)) {
			return nil, ErrNotCanonical
		}
		prev = f.Path

		path := f.Path
		pos, depth := 0, 0
		matching := true
		for {
			j := strings.IndexByte(path[pos:], '/')
			if j < 0 {
				break
			}
			comp := path[pos : pos+j]
			if matching && depth < len(stack) && s.name(files, stack[depth]) == comp {
				depth++
			} else {
				if matching {
					if err := closeTo(depth); err != nil {
						return nil, err
					}
					matching = false
				}
				parent := uint32(0)
				if depth > 0 {
					parent = stack[depth-1]
				}
				if len(s.td) >= math.MaxUint32/2 {
					return nil, ErrTooLarge
				}
				id := uint32(len(s.td))
				s.td = append(s.td, tdir{parent: parent, first: uint32(i), off: uint32(pos), n: uint32(j)})
				s.core = append(s.core, dirCore{})
				s.core[parent].nDirs++
				stack = append(stack, id)
				depth++
			}
			pos += j + 1
		}
		if matching {
			if err := closeTo(depth); err != nil {
				return nil, err
			}
		}
		parent := uint32(0)
		if depth > 0 {
			parent = stack[depth-1]
		}
		s.fileParent[i] = parent
		p := &s.core[parent]
		p.nFiles++
		p.files++
		sum, ok := addSize(p.size, f.Size)
		if !ok {
			return nil, ErrOverflow
		}
		p.size = sum
	}
	if err := closeTo(0); err != nil {
		return nil, err
	}
	return s, nil
}

// pack is phase 2: contiguous per-directory blocks. It returns the kids
// array and fills dirCore.kidLo.
func (s *skeleton) pack(files []codec.File) ([]uint32, error) {
	off := uint64(0)
	for d := range s.core {
		s.core[d].kidLo = uint32(off)
		off += uint64(s.core[d].nDirs) + uint64(s.core[d].nFiles)
		if off > math.MaxUint32 {
			return nil, ErrTooLarge
		}
	}
	kids := make([]uint32, off)
	cur := make([]uint32, len(s.core))
	for id := 1; id < len(s.td); id++ { // appearance order
		p := s.td[id].parent
		kids[s.core[p].kidLo+cur[p]] = uint32(id)
		cur[p]++
	}
	for d := range s.core {
		blk := kids[s.core[d].kidLo : s.core[d].kidLo+s.core[d].nDirs]
		if len(blk) > 1 && !slices.IsSortedFunc(blk, func(a, b uint32) int {
			return strings.Compare(s.name(files, a), s.name(files, b))
		}) {
			slices.SortFunc(blk, func(a, b uint32) int {
				return strings.Compare(s.name(files, a), s.name(files, b))
			})
		}
	}
	for i, p := range s.fileParent { // canonical order
		kids[s.core[p].kidLo+cur[p]] = uint32(i)
		cur[p]++
	}
	return kids, nil
}

func baseOffset(path string) int { return strings.LastIndexByte(path, '/') + 1 }

// ---- variant A: arena -------------------------------------------------------

type nameRef struct{ off, n uint32 }

type fileRecA struct {
	off, n uint32
	size   int64
}

// Arena is variant A: all names live in one string; the decoded list can be
// dropped after Build.
type Arena struct {
	core
	arena string
	dn    []nameRef
	fs    []fileRecA
}

const (
	nameRefBytes   = 8
	fileRecABytes  = 16
	fileHeaderSize = 24 // codec.File: string header + int64
)

// BuildArena builds variant A. files must be canonical and is not retained.
func BuildArena(ctx context.Context, files []codec.File) (*Arena, error) {
	s, err := scan(ctx, files)
	if err != nil {
		return nil, err
	}
	kids, err := s.pack(files)
	if err != nil {
		return nil, err
	}
	total := 0
	for id := 1; id < len(s.td); id++ {
		total += int(s.td[id].n)
	}
	for i := range files {
		total += len(files[i].Path) - baseOffset(files[i].Path)
	}
	if total > math.MaxUint32 {
		return nil, ErrTooLarge
	}
	var sb strings.Builder
	sb.Grow(total)
	a := &Arena{dn: make([]nameRef, len(s.td)), fs: make([]fileRecA, len(files))}
	for id := 1; id < len(s.td); id++ {
		a.dn[id] = nameRef{off: uint32(sb.Len()), n: s.td[id].n}
		sb.WriteString(s.name(files, uint32(id)))
	}
	for i := range files {
		b := files[i].Path[baseOffset(files[i].Path):]
		a.fs[i] = fileRecA{off: uint32(sb.Len()), n: uint32(len(b)), size: files[i].Size}
		sb.WriteString(b)
	}
	a.arena = sb.String()
	a.core = core{dirs: s.core, kids: kids, names: a, nfile: len(files)}
	return a, nil
}

func (a *Arena) dirName(id DirID) string {
	r := a.dn[id]
	return a.arena[r.off : r.off+r.n]
}
func (a *Arena) fileName(p uint32) string {
	r := &a.fs[p]
	return a.arena[r.off : r.off+r.n]
}
func (a *Arena) fileSize(p uint32) int64 { return a.fs[p].size }

// Bytes is the exact sum of the retained slice capacities and the arena.
func (a *Arena) Bytes() int {
	return len(a.arena) + cap(a.dn)*nameRefBytes + cap(a.fs)*fileRecABytes +
		cap(a.dirs)*dirCoreBytes + cap(a.kids)*4
}

// ---- variant B: references into the retained list ---------------------------

type dirRefB struct {
	first, off, n uint32
}

// Ref is variant B: it retains the decoded list and references its paths.
type Ref struct {
	core
	files []codec.File
	dr    []dirRefB
}

const dirRefBytes = 12

// BuildRef builds variant B. The caller hands over files: it must stay
// unmodified for the life of the index.
func BuildRef(ctx context.Context, files []codec.File) (*Ref, error) {
	s, err := scan(ctx, files)
	if err != nil {
		return nil, err
	}
	kids, err := s.pack(files)
	if err != nil {
		return nil, err
	}
	r := &Ref{files: files, dr: make([]dirRefB, len(s.td))}
	for id := 1; id < len(s.td); id++ {
		t := s.td[id]
		r.dr[id] = dirRefB{first: t.first, off: t.off, n: t.n}
	}
	r.core = core{dirs: s.core, kids: kids, names: r, nfile: len(files)}
	return r, nil
}

func (r *Ref) dirName(id DirID) string {
	d := r.dr[id]
	return r.files[d.first].Path[d.off : d.off+d.n]
}
func (r *Ref) fileName(p uint32) string {
	path := r.files[p].Path
	return path[baseOffset(path):]
}
func (r *Ref) fileSize(p uint32) int64 { return r.files[p].Size }

// Bytes counts the index arrays, the retained []File headers and the path
// string bytes. It does not include allocator size-class rounding of the
// individually allocated path strings, so it is a lower bound of the heap
// the retained list occupies.
func (r *Ref) Bytes() int {
	n := cap(r.dr)*dirRefBytes + cap(r.dirs)*dirCoreBytes + cap(r.kids)*4 + cap(r.files)*fileHeaderSize
	for i := range r.files {
		n += len(r.files[i].Path)
	}
	return n
}
