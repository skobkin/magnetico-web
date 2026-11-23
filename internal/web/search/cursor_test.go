package search

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"strings"
	"testing"
)

func params(t *testing.T, q, sort, order string) Params {
	t.Helper()
	p, err := ParseParams(q, sort, order, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCursorRoundTrip(t *testing.T) {
	for _, sort := range []string{"discovered", "size", "name"} {
		for _, order := range []string{"asc", "desc"} {
			p := params(t, "ubuntu desktop", sort, order)
			for _, back := range []bool{false, true} {
				for _, byID := range []bool{false, true} {
					c := Cursor{Backward: back, ByID: byID, ID: 4242, Int: -7, Name: "Ubuntu 日本語 \xe2\x9c\x93"}
					got, err := DecodeCursor(p, EncodeCursor(p, c))
					if err != nil {
						t.Fatalf("%s/%s back=%v byID=%v: %v", sort, order, back, byID, err)
					}
					want := Cursor{Backward: back, ByID: byID, ID: 4242}
					if !byID {
						if p.Sort == SortName {
							want.Name = c.Name
						} else {
							want.Int = c.Int
						}
					}
					if got != want {
						t.Errorf("%s/%s back=%v byID=%v: got %+v want %+v", sort, order, back, byID, got, want)
					}
				}
			}
		}
	}
}

func TestCursorDirectionIsExplicitInEveryKind(t *testing.T) {
	p := params(t, "ubuntu", "name", "asc")
	enc := func(c Cursor) string { return EncodeCursor(p, c) }
	kinds := map[string]byte{}
	for _, c := range []Cursor{{ID: 1}, {ID: 1, Backward: true}, {ID: 1, ByID: true}, {ID: 1, ByID: true, Backward: true}} {
		raw, _ := base64.RawURLEncoding.DecodeString(enc(c))
		kinds[string(raw[1:2])] = raw[1]
	}
	if len(kinds) != 4 {
		t.Fatalf("the four kinds must be distinct bytes: %v", kinds)
	}
	// Flipping the direction byte of a valid cursor is rejected (checksummed),
	// and re-checksumming a flipped kind decodes to the *other* direction, never
	// silently to the same one.
	raw, _ := base64.RawURLEncoding.DecodeString(enc(Cursor{ID: 9, ByID: true}))
	raw[1] = kindBackwardAnchor
	if _, err := DecodeCursor(p, base64.RawURLEncoding.EncodeToString(raw)); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("flipped direction accepted without re-checksum: %v", err)
	}
	raw = raw[:len(raw)-4]
	raw = binary.BigEndian.AppendUint32(raw, crc32.ChecksumIEEE(raw))
	c, err := DecodeCursor(p, base64.RawURLEncoding.EncodeToString(raw))
	if err != nil || !c.Backward || !c.ByID {
		t.Fatalf("re-checksummed kind decodes as %+v %v", c, err)
	}
}

func TestLongNamesUseAnchorCursorsInBothDirections(t *testing.T) {
	p := params(t, "ubuntu", "name", "asc")
	long := strings.Repeat("x", maxCursorName+1)
	for _, back := range []bool{false, true} {
		s := EncodeCursor(p, Cursor{ID: 5, Name: long, Backward: back})
		if len(s) > 100 {
			t.Fatalf("anchor cursor is %d bytes", len(s))
		}
		c, err := DecodeCursor(p, s)
		if err != nil || !c.ByID || c.Backward != back || c.ID != 5 || c.Name != "" {
			t.Fatalf("back=%v: %+v %v", back, c, err)
		}
	}
	// Exactly the limit still travels as a value.
	s := EncodeCursor(p, Cursor{ID: 5, Name: strings.Repeat("x", maxCursorName)})
	if c, err := DecodeCursor(p, s); err != nil || c.ByID || len(c.Name) != maxCursorName {
		t.Fatalf("limit-length name: %+v %v", c, err)
	}
}

func TestCursorRejections(t *testing.T) {
	p := params(t, "ubuntu", "size", "desc")
	good := EncodeCursor(p, Cursor{ID: 7, Int: 100})
	if _, err := DecodeCursor(p, good); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(good)
	resum := func(b []byte) string {
		b = binary.BigEndian.AppendUint32(b[:len(b)-4:len(b)-4], crc32.ChecksumIEEE(b[:len(b)-4]))
		return base64.RawURLEncoding.EncodeToString(b)
	}
	mut := func(f func(b []byte) []byte) string { return resum(f(append([]byte(nil), raw...))) }

	bad := map[string]string{
		"empty":         "",
		"not base64":    "!!!",
		"truncated":     good[:len(good)-6],
		"too long":      strings.Repeat("A", maxCursorLen+1),
		"bad crc":       good[:len(good)-1] + map[bool]string{true: "B", false: "A"}[good[len(good)-1] == 'A'],
		"wrong version": mut(func(b []byte) []byte { b[0] = 2; return b }),
		"unknown kind":  mut(func(b []byte) []byte { b[1] = 'x'; return b }),
		"id zero":       mut(func(b []byte) []byte { binary.BigEndian.PutUint32(b[10:], 0); return b }),
		"id negative":   mut(func(b []byte) []byte { binary.BigEndian.PutUint32(b[10:], 0x80000001); return b }),
		"short key":     mut(func(b []byte) []byte { return append(b[:len(b)-5], 0, 0, 0, 0) }),
		"trailing":      mut(func(b []byte) []byte { return append(b[:len(b)-4], 1, 0, 0, 0, 0) }),
	}
	for name, s := range bad {
		if _, err := DecodeCursor(p, s); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	// A cursor of another search: different terms, sort, direction.
	for name, other := range map[string]Params{
		"other terms":     params(t, "debian", "size", "desc"),
		"other sort":      params(t, "ubuntu", "discovered", "desc"),
		"other direction": params(t, "ubuntu", "size", "asc"),
	} {
		if _, err := DecodeCursor(other, good); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	// Term case and order do not change the search identity; page size is not part of it.
	same := params(t, "UBUNTU", "size", "desc")
	same.PageSize = 100
	if _, err := DecodeCursor(same, good); err != nil {
		t.Errorf("case/page size must not invalidate a cursor: %v", err)
	}
	a, b := params(t, "alpha beta", "size", "desc"), params(t, "beta alpha", "size", "desc")
	if _, err := DecodeCursor(b, EncodeCursor(a, Cursor{ID: 1, Int: 1})); err != nil {
		t.Errorf("term order must not invalidate a cursor: %v", err)
	}
	// Name cursors: invalid UTF-8 and NUL inside the carried name are rejected.
	np := params(t, "ubuntu", "name", "asc")
	for _, n := range []string{"a\xffb", "a\x00b"} {
		if _, err := DecodeCursor(np, EncodeCursor(np, Cursor{ID: 1, Name: n})); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("name %q accepted: %v", n, err)
		}
	}
}
