package codec

import (
	"cmp"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
)

// trieEligible reports whether every path splits on literal '/' into
// non-empty components and rejoins exactly (wire-format-v1 §3.3). Empty
// paths, leading or trailing separators and empty components ("//") are not
// eligible; such inputs fall back to another serializer without altering the
// path. Components like "." and ".." are literal names, not normalized.
func trieEligible(files []File) bool {
	for _, f := range files {
		if f.Path == "" || f.Path[0] == '/' || f.Path[len(f.Path)-1] == '/' ||
			strings.Contains(f.Path, "//") {
			return false
		}
	}
	return true
}

// maxTrieDepth returns the maximum number of nested directories the trie
// would need: the largest separator count over all paths.
func maxTrieDepth(files []File) int {
	max := 0
	for _, f := range files {
		if n := strings.Count(f.Path, "/"); n > max {
			max = n
		}
	}
	return max
}

// trieNode is one directory of the trie. Subdirectories are keyed by name
// while building and emitted in sorted component-byte order; the local files
// keep the canonical input order, which within one directory equals
// (basename bytes, size) order.
type trieNode struct {
	dirs  map[string]*trieNode
	files []File
}

func buildTrie(files []File) *trieNode {
	root := &trieNode{dirs: map[string]*trieNode{}}
	for _, f := range files {
		node := root
		components := strings.Split(f.Path, "/")
		for _, comp := range components[:len(components)-1] {
			child, ok := node.dirs[comp]
			if !ok {
				child = &trieNode{dirs: map[string]*trieNode{}}
				node.dirs[comp] = child
			}
			node = child
		}
		node.files = append(node.files, f)
	}
	return root
}

// encodeTrie serializes a canonically sorted, trie-eligible list
// (wire-format-v1 §3.3): a leading global file count followed by the root
// node, each node encoding its subdirectories (sorted by component bytes)
// before its local files (sorted by basename bytes, then size).
func encodeTrie(files []File) []byte {
	root := buildTrie(files)
	buf := appendUvarint(make([]byte, 0, len(files)*8), uint64(len(files)))
	return appendTrieNode(buf, root)
}

func appendTrieNode(dst []byte, n *trieNode) []byte {
	names := slices.Sorted(maps.Keys(n.dirs))
	dst = appendUvarint(dst, uint64(len(names)))
	for _, name := range names {
		dst = appendUvarint(dst, uint64(len(name)))
		dst = append(dst, name...)
		dst = appendTrieNode(dst, n.dirs[name])
	}
	dst = appendUvarint(dst, uint64(len(n.files)))
	for _, f := range n.files {
		base := f.Path
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		dst = appendUvarint(dst, uint64(len(base)))
		dst = append(dst, base...)
		dst = appendUvarint(dst, uint64(f.Size))
	}
	return dst
}

// trieDecoder streams a trie payload, enforcing the global file count, all
// limits and the structural rules while records stream in.
type trieDecoder struct {
	data      []byte
	limits    Limits
	remaining uint64 // global files still expected
	aggregate uint64 // reconstructed full-path bytes so far
}

func (d *trieDecoder) uvarint(what string) (uint64, error) {
	v, n, err := readUvarint(d.data)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	d.data = d.data[n:]
	return v, nil
}

func (d *trieDecoder) take(what string, n uint64) ([]byte, error) {
	if n > uint64(len(d.data)) {
		return nil, fmt.Errorf("%s: %w", what, ErrTruncated)
	}
	b := d.data[:n]
	d.data = d.data[n:]
	return b, nil
}

// node decodes one directory node; dirPath is the full directory prefix of
// its files ("" at the root) and depth the node's nesting level (root = 0).
// Local ordering is validated here; the emitted depth-first order may differ
// from the global canonical order and is sorted by the caller.
func (d *trieDecoder) node(dirPath string, depth int) ([]File, error) {
	if depth > d.limits.MaxTrieDepth {
		return nil, &LimitError{
			Limit: LimitTrieDepth, Actual: uint64(depth), Allowed: uint64(d.limits.MaxTrieDepth),
		}
	}

	var files []File

	dirCount, err := d.uvarint("subdirectory count")
	if err != nil {
		return nil, err
	}
	prevDir := ""
	for i := uint64(0); i < dirCount; i++ {
		nameLen, err := d.uvarint("subdirectory name length")
		if err != nil {
			return nil, err
		}
		if nameLen == 0 {
			return nil, fmt.Errorf("subdirectory %d: %w", i, ErrEmptyPathComponent)
		}
		if nameLen > uint64(d.limits.MaxPathBytes) {
			return nil, &LimitError{
				Limit: LimitPathBytes, Actual: nameLen, Allowed: uint64(d.limits.MaxPathBytes),
			}
		}
		nameBytes, err := d.take("subdirectory name", nameLen)
		if err != nil {
			return nil, err
		}
		name := string(nameBytes)
		switch {
		case name == prevDir:
			return nil, fmt.Errorf("subdirectory %d: %w", i, ErrDuplicateDirName)
		case name < prevDir:
			return nil, fmt.Errorf("subdirectory %d: %w", i, ErrNonCanonicalOrder)
		}
		prevDir = name

		// Every path in this subtree carries the child prefix, so charge the
		// aggregate budget once per directory.
		childPath := dirPath + name + "/"
		d.aggregate += uint64(len(childPath))
		if d.aggregate > uint64(d.limits.MaxAggregatePathBytes) {
			return nil, &LimitError{
				Limit: LimitAggregatePathBytes, Actual: d.aggregate, Allowed: uint64(d.limits.MaxAggregatePathBytes),
			}
		}
		childFiles, err := d.node(childPath, depth+1)
		if err != nil {
			return nil, err
		}
		files = append(files, childFiles...)
	}

	fileCount, err := d.uvarint("local file count")
	if err != nil {
		return nil, err
	}
	if fileCount > d.remaining {
		return nil, fmt.Errorf("local file count %d exceeds %d remaining: %w",
			fileCount, d.remaining, ErrInvalidCount)
	}
	prevBase := ""
	for i := uint64(0); i < fileCount; i++ {
		baseLen, err := d.uvarint("file name length")
		if err != nil {
			return nil, err
		}
		if baseLen == 0 {
			return nil, fmt.Errorf("file %d: %w", i, ErrEmptyPathComponent)
		}
		if baseLen > uint64(d.limits.MaxPathBytes) {
			return nil, &LimitError{
				Limit: LimitPathBytes, Actual: baseLen, Allowed: uint64(d.limits.MaxPathBytes),
			}
		}
		baseBytes, err := d.take("file name", baseLen)
		if err != nil {
			return nil, err
		}
		base := string(baseBytes)
		if base < prevBase {
			return nil, fmt.Errorf("file %d: %w", i, ErrNonCanonicalOrder)
		}
		prevBase = base

		sizeU, err := d.uvarint("file size")
		if err != nil {
			return nil, err
		}
		if sizeU > math.MaxInt64 {
			return nil, fmt.Errorf("file %d size: %w", i, ErrOverflow)
		}

		path := dirPath + base
		d.aggregate += uint64(len(path))
		if d.aggregate > uint64(d.limits.MaxAggregatePathBytes) {
			return nil, &LimitError{
				Limit: LimitAggregatePathBytes, Actual: d.aggregate, Allowed: uint64(d.limits.MaxAggregatePathBytes),
			}
		}

		d.remaining--
		files = append(files, File{Path: path, Size: int64(sizeU)})
	}
	return files, nil
}

// decodeTrie parses a directory-trie payload. The global file count must be
// exact, all limits are enforced while decoding progresses, and the returned
// list is sorted into canonical order (tree depth-first traversal differs
// from global sorting — vector G).
func decodeTrie(payload []byte, limits Limits) ([]File, error) {
	d := &trieDecoder{data: payload, limits: limits}
	count, err := d.uvarint("file count")
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, fmt.Errorf("trie payload: %w", ErrInvalidCount)
	}
	if count > uint64(limits.MaxFiles) {
		return nil, &LimitError{Limit: LimitFiles, Actual: count, Allowed: uint64(limits.MaxFiles)}
	}
	d.remaining = count

	files, err := d.node("", 0)
	if err != nil {
		return nil, err
	}
	if d.remaining != 0 {
		return nil, fmt.Errorf("file count %d, decoded %d: %w",
			count, count-d.remaining, ErrInvalidCount)
	}
	if len(d.data) != 0 {
		return nil, ErrTrailingBytes
	}

	slices.SortFunc(files, func(a, b File) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return cmp.Compare(a.Size, b.Size)
	})
	return files, nil
}
