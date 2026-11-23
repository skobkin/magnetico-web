package fileview

import (
	"encoding/base64"
	"strconv"
	"unicode/utf8"
)

// jsonBuf is a size-capped JSON builder. Once an append would exceed max it
// stops writing and records the overflow; callers check over at loop
// boundaries and discard the buffer, so an oversized document is never
// produced or sent.
type jsonBuf struct {
	b    []byte
	max  int
	over bool
}

func (w *jsonBuf) room(n int) bool {
	if w.over {
		return false
	}
	if len(w.b)+n > w.max {
		w.over = true
		return false
	}
	return true
}

func (w *jsonBuf) raw(s string) {
	if w.room(len(s)) {
		w.b = append(w.b, s...)
	}
}

func (w *jsonBuf) int(v int64) {
	if w.room(20) {
		w.b = strconv.AppendInt(w.b, v, 10)
	}
}

func (w *jsonBuf) bool(v bool) {
	if v {
		w.raw("true")
	} else {
		w.raw("false")
	}
}

const hexLower = "0123456789abcdef"

// str appends s as an RFC 8259 string. The escaping follows encoding/json
// (including <, >, & and U+2028/2029), never Go literal escaping. s must be
// valid UTF-8; invalid names are sent through name_base64 instead.
func (w *jsonBuf) str(s string) {
	// Worst case: six output bytes per input byte, plus the quotes.
	if !w.room(2 + len(s)) { // cheap early reject
		return
	}
	w.b = append(w.b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&' {
				i++
				continue
			}
			w.b = append(w.b, s[start:i]...)
			switch c {
			case '"', '\\':
				w.b = append(w.b, '\\', c)
			case '\n':
				w.b = append(w.b, '\\', 'n')
			case '\r':
				w.b = append(w.b, '\\', 'r')
			case '\t':
				w.b = append(w.b, '\\', 't')
			case '\b':
				w.b = append(w.b, '\\', 'b')
			case '\f':
				w.b = append(w.b, '\\', 'f')
			default:
				w.b = append(w.b, '\\', 'u', '0', '0', hexLower[c>>4], hexLower[c&0xf])
			}
			i++
			start = i
		} else {
			r, n := utf8.DecodeRuneInString(s[i:])
			if r == ' ' || r == ' ' {
				w.b = append(w.b, s[start:i]...)
				w.b = append(w.b, '\\', 'u', '2', '0', '2', hexLower[r&0xf])
				i += n
				start = i
				continue
			}
			i += n
		}
		if len(w.b) > w.max {
			w.over = true
			return
		}
	}
	w.b = append(w.b, s[start:]...)
	w.b = append(w.b, '"')
	if len(w.b) > w.max {
		w.over = true
	}
}

// name appends the name member(s) of an entry: "name" for valid UTF-8, or
// "name":null plus "name_base64" (standard alphabet, padded) for any other
// bytes, so arbitrary bytes round-trip without U+FFFD substitution.
func (w *jsonBuf) name(s string) {
	if utf8.ValidString(s) {
		w.raw(`"name":`)
		w.str(s)
		return
	}
	w.raw(`"name":null,"name_base64":"`)
	n := base64.StdEncoding.EncodedLen(len(s))
	if w.room(n + 1) {
		w.b = base64.StdEncoding.AppendEncode(w.b, []byte(s))
		w.b = append(w.b, '"')
	}
}
