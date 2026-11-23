package search

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"slices"
	"strings"
	"unicode/utf8"
)

// ErrInvalidCursor: the cursor is malformed, corrupt, of another version, too
// large, or belongs to a different search (terms, sort, direction).
var ErrInvalidCursor = errors.New("invalid page cursor")

// ErrCursorExpired: an anchor-only cursor refers to a row that no longer
// exists. Never rendered as an empty page.
var ErrCursorExpired = errors.New("page cursor expired")

const (
	cursorVersion = 1
	// maxCursorName is the longest name carried inside a cursor; a boundary row
	// with a longer name gets an anchor-only cursor (kind F/B).
	maxCursorName = 4096
	// maxCursorLen bounds the encoded cursor accepted from a request.
	maxCursorLen = 8192
)

// Cursor kinds. The traversal direction is always explicit and checksummed:
// lower case carries the sort value, upper case only the anchor id.
const (
	kindForward        = 'f'
	kindBackward       = 'b'
	kindForwardAnchor  = 'F'
	kindBackwardAnchor = 'B'
)

// Cursor is a decoded keyset position (exclusive) in a traversal direction.
type Cursor struct {
	Backward bool
	ByID     bool // anchor-only: the sort value is read from the row
	ID       int32
	Int      int64  // discovered_on / total_size
	Name     string // name sort
}

// contextHash binds a cursor to its search: the usable terms (case-folded,
// sorted, unique), the sort and the direction. Page size is deliberately not
// part of it.
func contextHash(p Params) [8]byte {
	terms := make([]string, len(p.Query.Terms))
	for i, t := range p.Query.Terms {
		terms[i] = strings.ToLower(t)
	}
	slices.Sort(terms)
	h := sha256.New()
	h.Write([]byte("magnetico/cursor/v1\x00"))
	h.Write([]byte{byte(p.Sort)})
	if p.Desc {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	for _, t := range terms {
		h.Write([]byte{0})
		h.Write([]byte(t))
	}
	var out [8]byte
	copy(out[:], h.Sum(nil))
	return out
}

// EncodeCursor renders c for params p. A name longer than maxCursorName is
// encoded as an anchor-only cursor (same direction).
func EncodeCursor(p Params, c Cursor) string {
	if p.Sort == SortName && len(c.Name) > maxCursorName {
		c.ByID = true
	}
	var kind byte
	switch {
	case c.ByID && c.Backward:
		kind = kindBackwardAnchor
	case c.ByID:
		kind = kindForwardAnchor
	case c.Backward:
		kind = kindBackward
	default:
		kind = kindForward
	}
	h := contextHash(p)
	buf := []byte{cursorVersion, kind}
	buf = append(buf, h[:]...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(c.ID))
	if !c.ByID {
		if p.Sort == SortName {
			buf = binary.AppendUvarint(buf, uint64(len(c.Name)))
			buf = append(buf, c.Name...)
		} else {
			buf = binary.BigEndian.AppendUint64(buf, uint64(c.Int))
		}
	}
	buf = binary.BigEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))
	return base64.RawURLEncoding.EncodeToString(buf)
}

// DecodeCursor validates and decodes s for params p (whose Sort, Desc and
// terms must be the ones the cursor was issued for).
func DecodeCursor(p Params, s string) (Cursor, error) {
	if s == "" || len(s) > maxCursorLen {
		return Cursor{}, ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) < 2+8+4+4 {
		return Cursor{}, ErrInvalidCursor
	}
	body, sum := raw[:len(raw)-4], binary.BigEndian.Uint32(raw[len(raw)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return Cursor{}, ErrInvalidCursor
	}
	if body[0] != cursorVersion {
		return Cursor{}, ErrInvalidCursor
	}
	var c Cursor
	switch body[1] {
	case kindForward:
	case kindBackward:
		c.Backward = true
	case kindForwardAnchor:
		c.ByID = true
	case kindBackwardAnchor:
		c.ByID, c.Backward = true, true
	default:
		return Cursor{}, ErrInvalidCursor
	}
	want := contextHash(p)
	if !slices.Equal(body[2:10], want[:]) {
		return Cursor{}, ErrInvalidCursor
	}
	rest := body[10:]
	if len(rest) < 4 {
		return Cursor{}, ErrInvalidCursor
	}
	c.ID = int32(binary.BigEndian.Uint32(rest))
	if c.ID < 1 {
		return Cursor{}, ErrInvalidCursor
	}
	rest = rest[4:]
	switch {
	case c.ByID:
		if len(rest) != 0 {
			return Cursor{}, ErrInvalidCursor
		}
	case p.Sort == SortName:
		n, k := binary.Uvarint(rest)
		if k <= 0 || n > maxCursorName || uint64(len(rest)-k) != n {
			return Cursor{}, ErrInvalidCursor
		}
		c.Name = string(rest[k:])
		if !utf8.ValidString(c.Name) || strings.ContainsRune(c.Name, 0) {
			return Cursor{}, ErrInvalidCursor
		}
	default:
		if len(rest) != 8 {
			return Cursor{}, ErrInvalidCursor
		}
		c.Int = int64(binary.BigEndian.Uint64(rest))
	}
	return c, nil
}
