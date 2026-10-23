package codec

import (
	"fmt"
	"math"
)

// commonPrefixLen returns the length of the maximal common byte prefix of a
// and b. It may stop in the middle of a multi-byte UTF-8 code point; that is
// valid because paths are reconstructed by concatenating bytes.
func commonPrefixLen(a, b string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// encodePrefix serializes a canonically sorted list as byte-wise prefix
// deltas (wire-format-v1 §3.2).
func encodePrefix(files []File) ([]byte, error) {
	buf := make([]byte, 0, len(files)*8)
	buf = appendUvarint(buf, uint64(len(files)))
	prev := ""
	for _, f := range files {
		shared := commonPrefixLen(prev, f.Path)
		suffix := f.Path[shared:]
		buf = appendUvarint(buf, uint64(shared))
		buf = appendUvarint(buf, uint64(len(suffix)))
		buf = append(buf, suffix...)
		buf = appendUvarint(buf, uint64(f.Size))
		prev = f.Path
	}
	return buf, nil
}

// decodePrefix parses a prefix-delta payload. `shared` must be the maximal
// common byte prefix with the previously reconstructed path, canonical
// ordering and all limits are enforced while records stream in.
func decodePrefix(payload []byte, limits Limits) ([]File, error) {
	count, n, err := readUvarint(payload)
	if err != nil {
		return nil, fmt.Errorf("file count: %w", err)
	}
	if count == 0 {
		return nil, fmt.Errorf("prefix payload: %w", ErrInvalidCount)
	}
	if count > uint64(limits.MaxFiles) {
		return nil, &LimitError{Limit: LimitFiles, Actual: count, Allowed: uint64(limits.MaxFiles)}
	}

	files := make([]File, 0, min(count, uint64(4096)))
	rest := payload[n:]
	prev := ""
	var prevSize int64
	var aggregate uint64
	for i := uint64(0); i < count; i++ {
		shared, n, err := readUvarint(rest)
		if err != nil {
			return nil, fmt.Errorf("file %d shared prefix length: %w", i, err)
		}
		rest = rest[n:]
		if shared > uint64(len(prev)) {
			return nil, fmt.Errorf("file %d: shared prefix %d exceeds previous path length %d: %w",
				i, shared, len(prev), ErrPrefixOutOfRange)
		}

		suffixLen, n, err := readUvarint(rest)
		if err != nil {
			return nil, fmt.Errorf("file %d suffix length: %w", i, err)
		}
		rest = rest[n:]
		if suffixLen > uint64(len(rest)) {
			return nil, fmt.Errorf("file %d suffix: %w", i, ErrTruncated)
		}
		suffix := string(rest[:suffixLen])
		rest = rest[suffixLen:]

		// Maximality (wire-format-v1 §3.2): when the path grows beyond the
		// previous path, the first new byte must differ from the byte it
		// skips over, else `shared` stopped short of the maximal common
		// prefix. An empty suffix (shared == len(prev), or a shorter
		// shared) is maximal by construction.
		if len(suffix) > 0 && int(shared) < len(prev) && prev[shared] == suffix[0] {
			return nil, fmt.Errorf("file %d: %w", i, ErrNonMaximalPrefix)
		}

		sizeU, n, err := readUvarint(rest)
		if err != nil {
			return nil, fmt.Errorf("file %d size: %w", i, err)
		}
		rest = rest[n:]
		if sizeU > math.MaxInt64 {
			return nil, fmt.Errorf("file %d size: %w", i, ErrOverflow)
		}

		path := prev[:shared] + suffix
		aggregate += uint64(len(path))
		if aggregate > uint64(limits.MaxAggregatePathBytes) {
			return nil, &LimitError{
				Limit: LimitAggregatePathBytes, Actual: aggregate, Allowed: uint64(limits.MaxAggregatePathBytes),
			}
		}

		f := File{Path: path, Size: int64(sizeU)}
		if !canonicalAfter(File{Path: prev, Size: prevSize}, f) {
			return nil, fmt.Errorf("file %d: %w", i, ErrNonCanonicalOrder)
		}
		prev, prevSize = path, f.Size
		files = append(files, f)
	}

	if len(rest) != 0 {
		return nil, ErrTrailingBytes
	}
	return files, nil
}
