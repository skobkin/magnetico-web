package codec

import "encoding/binary"

// appendUvarint appends v in the canonical minimal encoding.
func appendUvarint(b []byte, v uint64) []byte {
	return binary.AppendUvarint(b, v)
}

// uvarintLen returns the length of the minimal uvarint encoding of v.
func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// readUvarint reads one unsigned base-128 varint from the front of b and
// returns its value and consumed byte length. Truncated input, 64-bit
// overflow and nonminimal (overlong) encodings are rejected with *VarintError,
// as mandated by wire-format-v1 §2.
func readUvarint(b []byte) (uint64, int, error) {
	v, n := binary.Uvarint(b)
	switch {
	case n == 0:
		return 0, 0, &VarintError{Reason: VarintTruncated}
	case n < 0:
		return 0, 0, &VarintError{Reason: VarintOverflow}
	}
	if n != uvarintLen(v) {
		return 0, n, &VarintError{Reason: VarintNonminimal}
	}
	return v, n, nil
}
