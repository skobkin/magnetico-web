package codec

import "fmt"

// PeekHeaderLen is the number of leading blob bytes PeekDecodedSize needs:
// version, the serializer/compressor byte and a uvarint (at most 10 bytes).
const PeekHeaderLen = 12

// PeekDecodedSize reads only the envelope header of a stored blob and reports
// the serializer and compressor and the uncompressed payload length the full
// decode would allocate, so a caller can admit memory before reading the
// whole blob. header is at least the first min(PeekHeaderLen, storedLen)
// bytes; storedLen is the full stored length.
//
// It applies the same envelope checks and limits as Decode (stored length,
// version, IDs, minimal varint, MaxUncompressedBytes) and changes no decode
// behaviour; it does not validate the payload.
func PeekDecodedSize(header []byte, storedLen int, limits Limits) (Selection, uint64, error) {
	if storedLen > limits.MaxStoredBytes {
		return Selection{}, 0, &LimitError{
			Limit: LimitStoredBytes, Actual: uint64(storedLen), Allowed: uint64(limits.MaxStoredBytes),
		}
	}
	if storedLen < 2 || len(header) < 2 {
		return Selection{}, 0, ErrTruncated
	}
	if header[0] != FormatVersion {
		return Selection{}, 0, fmt.Errorf("version 0x%02x: %w", header[0], ErrUnsupportedVersion)
	}
	sel := Selection{Serializer: SerializerID(header[1] >> 4), Compressor: CompressorID(header[1] & 0x0f)}
	if !validSerializer(sel.Serializer) {
		return Selection{}, 0, fmt.Errorf("serializer 0x%x: %w", sel.Serializer, ErrUnsupportedSerializer)
	}
	if !validCompressor(sel.Compressor) {
		return Selection{}, 0, fmt.Errorf("compressor 0x%x: %w", sel.Compressor, ErrUnsupportedCompressor)
	}
	if sel.Compressor == None {
		declared := uint64(storedLen - 2)
		if declared > uint64(limits.MaxUncompressedBytes) {
			return Selection{}, 0, &LimitError{
				Limit: LimitUncompressedBytes, Actual: declared, Allowed: uint64(limits.MaxUncompressedBytes),
			}
		}
		return sel, declared, nil
	}
	declared, _, err := readUvarint(header[2:])
	if err != nil {
		return Selection{}, 0, fmt.Errorf("declared uncompressed length: %w", err)
	}
	if declared > uint64(limits.MaxUncompressedBytes) {
		return Selection{}, 0, &LimitError{
			Limit: LimitUncompressedBytes, Actual: declared, Allowed: uint64(limits.MaxUncompressedBytes),
		}
	}
	return sel, declared, nil
}
