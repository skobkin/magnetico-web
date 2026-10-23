package codec

import (
	"fmt"
	"math"
)

// encodeFlat serializes a canonically sorted list (wire-format-v1 §3.1).
func encodeFlat(files []File) ([]byte, error) {
	buf := make([]byte, 0, len(files)*8)
	buf = appendUvarint(buf, uint64(len(files)))
	for _, f := range files {
		buf = appendUvarint(buf, uint64(len(f.Path)))
		buf = append(buf, f.Path...)
		buf = appendUvarint(buf, uint64(f.Size))
	}
	return buf, nil
}

// decodeFlat parses a flat payload. Canonical ordering and all limits are
// enforced while records stream in, so a malformed or hostile payload can
// neither allocate unboundedly nor yield a partial list.
func decodeFlat(payload []byte, limits Limits) ([]File, error) {
	count, n, err := readUvarint(payload)
	if err != nil {
		return nil, fmt.Errorf("file count: %w", err)
	}
	if count == 0 {
		return nil, fmt.Errorf("flat payload: %w", ErrInvalidCount)
	}
	if count > uint64(limits.MaxFiles) {
		return nil, &LimitError{Limit: LimitFiles, Actual: count, Allowed: uint64(limits.MaxFiles)}
	}

	files := make([]File, 0, min(count, uint64(4096)))
	rest := payload[n:]
	var aggregate uint64
	var prev File
	for i := uint64(0); i < count; i++ {
		pathLen, n, err := readUvarint(rest)
		if err != nil {
			return nil, fmt.Errorf("file %d path length: %w", i, err)
		}
		if pathLen > uint64(limits.MaxPathBytes) {
			return nil, &LimitError{Limit: LimitPathBytes, Actual: pathLen, Allowed: uint64(limits.MaxPathBytes)}
		}
		rest = rest[n:]
		if pathLen > uint64(len(rest)) {
			return nil, fmt.Errorf("file %d path: %w", i, ErrTruncated)
		}
		path := string(rest[:pathLen])
		rest = rest[pathLen:]

		sizeU, n, err := readUvarint(rest)
		if err != nil {
			return nil, fmt.Errorf("file %d size: %w", i, err)
		}
		rest = rest[n:]
		if sizeU > math.MaxInt64 {
			return nil, fmt.Errorf("file %d size: %w", i, ErrOverflow)
		}

		aggregate += pathLen
		if aggregate > uint64(limits.MaxAggregatePathBytes) {
			return nil, &LimitError{
				Limit: LimitAggregatePathBytes, Actual: aggregate, Allowed: uint64(limits.MaxAggregatePathBytes),
			}
		}

		f := File{Path: path, Size: int64(sizeU)}
		if !canonicalAfter(prev, f) {
			return nil, fmt.Errorf("file %d: %w", i, ErrNonCanonicalOrder)
		}
		prev = f
		files = append(files, f)
	}

	if len(rest) != 0 {
		return nil, ErrTrailingBytes
	}
	return files, nil
}
