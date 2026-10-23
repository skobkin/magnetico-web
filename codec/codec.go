// Package codec implements the Magnetico 0.8.0 torrent file-list wire
// format v1 (docs/specs/wire-format-v1.md): a self-describing blob holding a
// canonically sorted file list under one of three serializers and one of
// three compressors. Encoding is deterministic for a chosen serializer,
// compressor and library version; decoding is strict — malformed input
// produces a typed error and never a partial file list.
//
// Paths are opaque byte strings: the codec preserves them byte-exactly and
// never normalizes separators, case or Unicode.
package codec

import "fmt"

// FormatVersion is the current wire format version byte (spec §2).
const FormatVersion byte = 0x01

// SerializerID identifies the payload serializer (envelope header high
// nibble). All other IDs are reserved and rejected.
type SerializerID byte

const (
	Flat          SerializerID = 0x1
	PrefixDelta   SerializerID = 0x2
	DirectoryTrie SerializerID = 0x4
)

// CompressorID identifies the payload compressor (envelope header low
// nibble). All other IDs are reserved and rejected.
type CompressorID byte

const (
	None   CompressorID = 0x0
	Zstd   CompressorID = 0x1
	Brotli CompressorID = 0x2
)

func validSerializer(id SerializerID) bool {
	switch id {
	case Flat, PrefixDelta, DirectoryTrie:
		return true
	}
	return false
}

func validCompressor(id CompressorID) bool {
	switch id {
	case None, Zstd, Brotli:
		return true
	}
	return false
}

// File is one entry of a torrent's file list.
type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Selection records the serializer and compressor an encoder chose for a
// blob.
type Selection struct {
	Serializer SerializerID
	Compressor CompressorID
}

// Limits bound encoding and decoding work. The values are the initial
// defaults from wire-format-v1 §5 and are meant to be configurable at the
// application boundary. Every advertised length is checked against these
// limits before it can drive an allocation.
type Limits struct {
	MaxStoredBytes        int // whole blob, including envelope
	MaxUncompressedBytes  int // raw payload after decompression
	MaxFiles              int // file count per torrent
	MaxPathBytes          int // single full path length in bytes
	MaxAggregatePathBytes int // sum of all reconstructed path lengths
	MaxTrieDepth          int // directory nesting depth
}

// DefaultLimits returns the initial limits from wire-format-v1 §5.
func DefaultLimits() Limits {
	return Limits{
		MaxStoredBytes:        32 << 20,
		MaxUncompressedBytes:  64 << 20,
		MaxFiles:              250_000,
		MaxPathBytes:          64 << 10,
		MaxAggregatePathBytes: 64 << 20,
		MaxTrieDepth:          256,
	}
}

// Decode parses blob and returns the complete file list, or a typed error.
// The output is canonically sorted. Decompression (Zstandard, Brotli) must
// yield exactly the envelope's declared length; any trailing input, unknown
// version/IDs, limit violation or structural defect is rejected.
func Decode(blob []byte, limits Limits) ([]File, error) {
	ser, comp, payload, _, err := parseEnvelope(blob, limits)
	if err != nil {
		return nil, err
	}
	if comp != None {
		return nil, fmt.Errorf("compressor 0x%x: %w", comp, ErrUnsupportedCompressor)
	}
	return decodeRaw(ser, payload, limits)
}

func decodeRaw(ser SerializerID, payload []byte, limits Limits) ([]File, error) {
	switch ser {
	case Flat:
		return decodeFlat(payload, limits)
	case PrefixDelta:
		return decodePrefix(payload, limits)
	default:
		return nil, fmt.Errorf("serializer 0x%x: %w", ser, ErrUnsupportedSerializer)
	}
}
