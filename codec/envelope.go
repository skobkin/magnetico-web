package codec

import "fmt"

// buildEnvelope frames a raw payload: version byte, serializer/compressor
// nibble byte and — for compressed payloads only — the minimal uvarint of the
// exact uncompressed payload length (wire-format-v1 §2). Uncompressed
// payloads carry no length field; they extend to the end of the blob.
func buildEnvelope(ser SerializerID, comp CompressorID, payload []byte) ([]byte, error) {
	if !validSerializer(ser) {
		return nil, fmt.Errorf("serializer 0x%x: %w", ser, ErrUnsupportedSerializer)
	}
	if !validCompressor(comp) {
		return nil, fmt.Errorf("compressor 0x%x: %w", comp, ErrUnsupportedCompressor)
	}
	out := make([]byte, 2, 2+uvarintLen(uint64(len(payload)))+len(payload))
	out[0] = FormatVersion
	out[1] = byte(ser)<<4 | byte(comp)
	if comp != None {
		out = appendUvarint(out, uint64(len(payload)))
	}
	return append(out, payload...), nil
}

// parseEnvelope validates the envelope of blob and returns the serializer,
// compressor and payload slice. For compressed payloads, declared is the
// exact uncompressed length the caller must honor; for None it equals
// len(payload).
func parseEnvelope(blob []byte, limits Limits) (ser SerializerID, comp CompressorID, payload []byte, declared uint64, err error) {
	if len(blob) > limits.MaxStoredBytes {
		return 0, 0, nil, 0, &LimitError{
			Limit: LimitStoredBytes, Actual: uint64(len(blob)), Allowed: uint64(limits.MaxStoredBytes),
		}
	}
	if len(blob) < 2 {
		return 0, 0, nil, 0, ErrTruncated
	}
	if blob[0] != FormatVersion {
		return 0, 0, nil, 0, fmt.Errorf("version 0x%02x: %w", blob[0], ErrUnsupportedVersion)
	}
	ser = SerializerID(blob[1] >> 4)
	comp = CompressorID(blob[1] & 0x0f)
	if !validSerializer(ser) {
		return 0, 0, nil, 0, fmt.Errorf("serializer 0x%x: %w", ser, ErrUnsupportedSerializer)
	}
	if !validCompressor(comp) {
		return 0, 0, nil, 0, fmt.Errorf("compressor 0x%x: %w", comp, ErrUnsupportedCompressor)
	}

	rest := blob[2:]
	if comp == None {
		if len(rest) > limits.MaxUncompressedBytes {
			return 0, 0, nil, 0, &LimitError{
				Limit: LimitUncompressedBytes, Actual: uint64(len(rest)), Allowed: uint64(limits.MaxUncompressedBytes),
			}
		}
		return ser, comp, rest, uint64(len(rest)), nil
	}

	declared, n, err := readUvarint(rest)
	if err != nil {
		return 0, 0, nil, 0, fmt.Errorf("declared uncompressed length: %w", err)
	}
	if declared > uint64(limits.MaxUncompressedBytes) {
		return 0, 0, nil, 0, &LimitError{
			Limit: LimitUncompressedBytes, Actual: declared, Allowed: uint64(limits.MaxUncompressedBytes),
		}
	}
	return ser, comp, rest[n:], declared, nil
}
