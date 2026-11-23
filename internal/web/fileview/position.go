package fileview

import (
	"errors"
	"strconv"
	"strings"

	"github.com/skobkin/magnetico-web/internal/web/filetree"
)

// Errors of the request grammar and of directory resolution.
var (
	// ErrBadPath: the positional path is malformed or deeper than the index
	// depth limit. HTTP 400.
	ErrBadPath = errors.New("fileview: malformed positional directory path")
	// ErrBadPage: offset or limit is malformed or out of range. HTTP 400.
	ErrBadPage = errors.New("fileview: malformed or out-of-range page parameters")
	// ErrNoSuchDir: a positional component does not name a subdirectory.
	// HTTP 404.
	ErrNoSuchDir = errors.New("fileview: no such directory")
)

const maxComponentDigits = 6

// ParsePath parses a positional directory path: "" is the root, otherwise
// decimal components joined by '_', each canonical (no sign, no leading
// zeros) and at most six digits. At most maxDepth components are allowed.
func ParsePath(s string, maxDepth int) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, "_")
	if len(parts) > maxDepth {
		return nil, ErrBadPath
	}
	out := make([]int, len(parts))
	for i, p := range parts {
		if p == "" || len(p) > maxComponentDigits || (len(p) > 1 && p[0] == '0') {
			return nil, ErrBadPath
		}
		for j := 0; j < len(p); j++ {
			if p[j] < '0' || p[j] > '9' {
				return nil, ErrBadPath
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, ErrBadPath
		}
		out[i] = n
	}
	return out, nil
}

// FormatPath is the inverse of ParsePath.
func FormatPath(comps []int) string {
	var b strings.Builder
	for i, c := range comps {
		if i > 0 {
			b.WriteByte('_')
		}
		b.WriteString(strconv.Itoa(c))
	}
	return b.String()
}

// resolve walks comps from the root. Only directories can be addressed, so
// every component must be below the current directory's subdirectory count.
func resolve(idx filetree.Index, comps []int) (filetree.DirID, error) {
	cur := filetree.Root
	for _, c := range comps {
		next, ok := idx.Child(cur, c)
		if !ok {
			return 0, ErrNoSuchDir
		}
		cur = next
	}
	return cur, nil
}
