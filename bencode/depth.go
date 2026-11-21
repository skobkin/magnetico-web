package bencode

import (
	"bytes"
	"errors"
)

// ErrNestingTooDeep reports bencode whose list/dictionary nesting exceeds the
// limit given to CheckDepth.
var ErrNestingTooDeep = errors.New("bencode: nesting too deep")

// CheckDepth scans the first bencode value in b and reports
// ErrNestingTooDeep if lists/dictionaries nest deeper than maxDepth.
//
// The decoder recurses once per nesting level and exhausting the Go stack is
// a fatal, unrecoverable error, so untrusted input (peer-supplied torrent
// metadata) must be checked before Unmarshal. The scan is iterative, uses
// O(1) memory and runs in O(len(b)): strings are skipped by their declared
// length, so their content cannot be mistaken for structure. Input that is
// malformed in any other way returns nil; the decoder reports those errors.
func CheckDepth(b []byte, maxDepth int) error {
	depth := 0
	for i := 0; i < len(b); {
		switch c := b[i]; {
		case c == 'l' || c == 'd':
			depth++
			if depth > maxDepth {
				return ErrNestingTooDeep
			}
			i++
		case c == 'e':
			depth--
			if depth <= 0 {
				return nil // first value complete (or malformed: decoder's job)
			}
			i++
		case c == 'i':
			end := bytes.IndexByte(b[i:], 'e')
			if end < 0 {
				return nil
			}
			i += end + 1
			if depth == 0 {
				return nil
			}
		case c >= '0' && c <= '9':
			n, j := 0, i
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				if n > (len(b)-j)/10+1 { // already longer than the input: malformed
					return nil
				}
				n = n*10 + int(b[j]-'0')
				j++
			}
			if j >= len(b) || b[j] != ':' || n > len(b)-j-1 {
				return nil
			}
			i = j + 1 + n
			if depth == 0 {
				return nil
			}
		default:
			return nil
		}
	}
	return nil
}
