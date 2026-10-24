package codec

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// compareCanonical orders two files in canonical order: path bytes
// ascending, then size ascending.
func compareCanonical(a, b File) int {
	if c := strings.Compare(a.Path, b.Path); c != 0 {
		return c
	}
	return cmp.Compare(a.Size, b.Size)
}

// canonicalize validates files against the limits and returns a canonically
// sorted copy (path bytes ascending, then size ascending). The caller's
// slice is never modified.
func canonicalize(files []File, limits Limits) ([]File, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("empty input: %w", ErrInvalidCount)
	}
	if len(files) > limits.MaxFiles {
		return nil, &LimitError{Limit: LimitFiles, Actual: uint64(len(files)), Allowed: uint64(limits.MaxFiles)}
	}

	var aggregate uint64
	for _, f := range files {
		if f.Size < 0 {
			return nil, fmt.Errorf("file size %d: %w", f.Size, ErrNegativeSize)
		}
		if len(f.Path) > limits.MaxPathBytes {
			return nil, &LimitError{Limit: LimitPathBytes, Actual: uint64(len(f.Path)), Allowed: uint64(limits.MaxPathBytes)}
		}
		aggregate += uint64(len(f.Path))
		if aggregate > uint64(limits.MaxAggregatePathBytes) {
			return nil, &LimitError{
				Limit: LimitAggregatePathBytes, Actual: aggregate, Allowed: uint64(limits.MaxAggregatePathBytes),
			}
		}
	}

	sorted := make([]File, len(files))
	copy(sorted, files)
	slices.SortFunc(sorted, compareCanonical)
	return sorted, nil
}

// canonicalAfter reports whether cur may follow prev in canonical order:
// strictly increasing path bytes, or an identical path with a non-decreasing
// size (wire-format-v1 §1). Identical (path, size) pairs are allowed.
func canonicalAfter(prev, cur File) bool {
	if cur.Path != prev.Path {
		return cur.Path > prev.Path
	}
	return cur.Size >= prev.Size
}

// encodeRaw serializes an already canonical list without the envelope.
func encodeRaw(ser SerializerID, files []File) ([]byte, error) {
	switch ser {
	case Flat:
		return encodeFlat(files)
	case PrefixDelta:
		return encodePrefix(files)
	case DirectoryTrie:
		return encodeTrie(files), nil
	default:
		return nil, fmt.Errorf("serializer 0x%x: %w", ser, ErrUnsupportedSerializer)
	}
}

// Encode serializes files under the profile's candidate set and returns the
// smallest complete framed blob, breaking ties deterministically
// (uncompressed first, then prefix-delta, then trie, then flat). The input
// is canonically sorted on a copy and never mutated.
func Encode(files []File, profile Profile, limits Limits) ([]byte, Selection, error) {
	if !profile.valid() {
		return nil, Selection{}, fmt.Errorf("profile %d: %w", profile, ErrUnsupportedSerializer)
	}
	sorted, err := canonicalize(files, limits)
	if err != nil {
		return nil, Selection{}, err
	}
	return encodeSelected(nil, profile, sorted, limits)
}

// encodeSelected encodes an already canonical list across the profile's
// candidates. st may be nil (one-shot compressors are created per call).
func encodeSelected(st *compressState, profile Profile, sorted []File, limits Limits) ([]byte, Selection, error) {
	var (
		bestBlob []byte
		bestSel  Selection
		bestRank int
		haveBest bool
	)
	for _, c := range profile.candidates() {
		// Trie candidates are skipped — never an error — when the input's
		// paths cannot be split and rejoined exactly (spec §3.3) or would
		// exceed the trie depth limit; another candidate represents the
		// list instead. The depth limit still rejects trie-encoded blobs on
		// decode.
		if c.ser == DirectoryTrie &&
			(!trieEligible(sorted) || maxTrieDepth(sorted) > limits.MaxTrieDepth) {
			continue
		}
		payload, err := encodeRaw(c.ser, sorted)
		if err != nil {
			return nil, Selection{}, err
		}
		if len(payload) > limits.MaxUncompressedBytes {
			return nil, Selection{}, &LimitError{
				Limit: LimitUncompressedBytes, Actual: uint64(len(payload)), Allowed: uint64(limits.MaxUncompressedBytes),
			}
		}
		rawLen := uint64(len(payload))

		if c.comp != None {
			if st == nil {
				tmp := newCompressState(profile)
				payload, err = tmp.compress(c.comp, payload)
			} else {
				payload, err = st.compress(c.comp, payload)
			}
			if err != nil {
				return nil, Selection{}, err
			}
		}

		blob, err := buildEnvelope(c.ser, c.comp, rawLen, payload)
		if err != nil {
			return nil, Selection{}, err
		}
		if len(blob) > limits.MaxStoredBytes {
			return nil, Selection{}, &LimitError{
				Limit: LimitStoredBytes, Actual: uint64(len(blob)), Allowed: uint64(limits.MaxStoredBytes),
			}
		}

		r := rank(c)
		if !haveBest || better(len(blob), r, len(bestBlob), bestRank) {
			// Copy: compressed payloads alias a reusable buffer.
			bestBlob = append([]byte(nil), blob...)
			bestSel = Selection{Serializer: c.ser, Compressor: c.comp}
			bestRank = r
			haveBest = true
		}
	}
	return bestBlob, bestSel, nil
}

// encodeWith serializes files under a fixed serializer with no compression
// and returns the framed blob plus the selection it used. It is the building
// block for the profile-based Encode and the direct entry point for the
// golden-vector tests.
func encodeWith(ser SerializerID, files []File, limits Limits) ([]byte, Selection, error) {
	sorted, err := canonicalize(files, limits)
	if err != nil {
		return nil, Selection{}, err
	}
	payload, err := encodeRaw(ser, sorted)
	if err != nil {
		return nil, Selection{}, err
	}
	if len(payload) > limits.MaxUncompressedBytes {
		return nil, Selection{}, &LimitError{
			Limit: LimitUncompressedBytes, Actual: uint64(len(payload)), Allowed: uint64(limits.MaxUncompressedBytes),
		}
	}
	blob, err := buildEnvelope(ser, None, uint64(len(payload)), payload)
	if err != nil {
		return nil, Selection{}, err
	}
	if len(blob) > limits.MaxStoredBytes {
		return nil, Selection{}, &LimitError{
			Limit: LimitStoredBytes, Actual: uint64(len(blob)), Allowed: uint64(limits.MaxStoredBytes),
		}
	}
	return blob, Selection{Serializer: ser, Compressor: None}, nil
}
