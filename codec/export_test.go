package codec

// Test-only hooks for the explicit benchmark-blob dumper (benchblobs_test.go).
// They expose existing internals unchanged; no encoder or decoder behaviour is
// altered.

// EncodeForced serializes files with exactly (ser, comp) and frames the result.
// Brotli uses quality 9 (the Compact profile), Zstd its default level. The
// canonicalization and limits checks are the encoder's own. A DirectoryTrie
// request for a list the trie cannot represent returns ErrEmptyPathComponent.
func EncodeForced(ser SerializerID, comp CompressorID, files []File, limits Limits) ([]byte, error) {
	sorted, err := canonicalize(files, limits)
	if err != nil {
		return nil, err
	}
	if ser == DirectoryTrie && (!trieEligible(sorted) || maxTrieDepth(sorted) > limits.MaxTrieDepth) {
		return nil, ErrEmptyPathComponent
	}
	payload, err := encodeRaw(ser, sorted)
	if err != nil {
		return nil, err
	}
	if len(payload) > limits.MaxUncompressedBytes {
		return nil, &LimitError{Limit: LimitUncompressedBytes, Actual: uint64(len(payload)), Allowed: uint64(limits.MaxUncompressedBytes)}
	}
	rawLen := uint64(len(payload))
	if comp != None {
		p := ProfileCompact
		if comp == Zstd {
			p = ProfileBalanced
		}
		payload, err = newCompressState(p).compress(comp, payload)
		if err != nil {
			return nil, err
		}
	}
	blob, err := buildEnvelope(ser, comp, rawLen, payload)
	if err != nil {
		return nil, err
	}
	if len(blob) > limits.MaxStoredBytes {
		return nil, &LimitError{Limit: LimitStoredBytes, Actual: uint64(len(blob)), Allowed: uint64(limits.MaxStoredBytes)}
	}
	return append([]byte(nil), blob...), nil
}
