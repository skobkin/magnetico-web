package codec

import (
	"errors"
	"strconv"
)

// Sentinel errors reported by Encode and Decode. Match with errors.Is;
// *LimitError and *VarintError carry structured detail via errors.As.
// Error text never contains file paths (wire-format-v1 §5).
var (
	ErrUnsupportedVersion    = errors.New("unsupported wire format version")
	ErrUnsupportedSerializer = errors.New("unsupported serializer ID")
	ErrUnsupportedCompressor = errors.New("unsupported compressor ID")
	ErrTruncated             = errors.New("truncated stream")
	ErrTrailingBytes         = errors.New("trailing bytes after end of stream")
	ErrNonCanonicalOrder     = errors.New("records are not in canonical order")
	ErrNonMaximalPrefix      = errors.New("shared prefix is not the maximal common prefix")
	ErrPrefixOutOfRange      = errors.New("shared prefix length exceeds the previous path length")
	ErrDuplicateDirName      = errors.New("duplicate subdirectory name")
	ErrEmptyPathComponent    = errors.New("empty path component")
	// ErrPathComponentSeparator reports a trie component carrying the path
	// separator: splitting on "/" would then not reverse exactly
	// (wire-format-v1 §3.3).
	ErrPathComponentSeparator = errors.New("path component contains the path separator")
	ErrInvalidCount           = errors.New("invalid file count")
	ErrCorruptCompressed      = errors.New("corrupt compressed stream")
	ErrOverflow               = errors.New("numeric value exceeds the target type range")
	ErrNegativeSize           = errors.New("file size is negative")
)

// Limit names reported by LimitError.Limit.
const (
	LimitStoredBytes        = "stored_bytes"
	LimitUncompressedBytes  = "uncompressed_bytes"
	LimitFiles              = "files"
	LimitPathBytes          = "path_bytes"
	LimitAggregatePathBytes = "aggregate_path_bytes"
	LimitTrieDepth          = "trie_depth"
)

// LimitError reports a configured resource limit violation. Actual and
// Allowed are byte or record counts; paths are never included.
type LimitError struct {
	Limit   string
	Actual  uint64
	Allowed uint64
}

func (e *LimitError) Error() string {
	return "limit exceeded (" + e.Limit + "): actual " +
		strconv.FormatUint(e.Actual, 10) + ", allowed " +
		strconv.FormatUint(e.Allowed, 10)
}

// VarintError reasons.
const (
	VarintTruncated  = "truncated"
	VarintNonminimal = "nonminimal"
	VarintOverflow   = "overflow"
)

// VarintError reports a malformed variable-length integer encoding.
type VarintError struct {
	Reason string // one of the Varint* constants
}

func (e *VarintError) Error() string {
	return "malformed varint: " + e.Reason
}

// Is lets a truncated varint classify as ErrTruncated and an overflowing
// varint as ErrOverflow under errors.Is, while keeping the structured
// reason available through errors.As.
func (e *VarintError) Is(target error) bool {
	switch e.Reason {
	case VarintTruncated:
		return target == ErrTruncated
	case VarintOverflow:
		return target == ErrOverflow
	}
	return false
}
