package search

import (
	"errors"
	"strings"
	"testing"
)

// FuzzDecodeCursor: any string a client supplies as a page cursor is either
// rejected with ErrInvalidCursor or decodes to a cursor that survives a second
// round trip unchanged; nothing panics, nothing oversized is accepted, and a
// cursor issued for one search is never accepted for another. The seeds (valid
// cursors of every sort, direction and kind, plus damaged ones) run in the
// normal suite.
func FuzzDecodeCursor(f *testing.F) {
	mk := func(q, sort, order string) Params {
		p, err := ParseParams(q, sort, order, "", "")
		if err != nil {
			f.Fatal(err)
		}
		return p
	}
	searches := []Params{mk("ubuntu desktop", "discovered", "desc"), mk("ubuntu", "size", "asc"), mk("ubuntu", "name", "asc"), mk("", "discovered", "asc")}
	for _, p := range searches {
		for _, back := range []bool{false, true} {
			for _, byID := range []bool{false, true} {
				c := EncodeCursor(p, Cursor{Backward: back, ByID: byID, ID: 7, Int: -3, Name: "Ubuntu 日本語"})
				f.Add(c)
				f.Add(c[:len(c)/2])
				f.Add(c + "A")
				f.Add(strings.ToUpper(c))
			}
		}
	}
	for _, s := range []string{"", "!", "====", "AAAA", strings.Repeat("A", 8192), strings.Repeat("A", 8193), "\xff\xfe", "AQ", "AQEAAAAAAAAAAAAAAAA"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, p := range searches {
			c, err := DecodeCursor(p, s)
			if err != nil {
				if !errors.Is(err, ErrInvalidCursor) {
					t.Fatalf("DecodeCursor(%q): unexpected error %v", s, err)
				}
				continue
			}
			if len(s) > maxCursorLen {
				t.Fatalf("a %d-character cursor was accepted", len(s))
			}
			if c.ID < 1 {
				t.Fatalf("DecodeCursor(%q) accepted id %d", s, c.ID)
			}
			again, err := DecodeCursor(p, EncodeCursor(p, c))
			if err != nil || again != c {
				t.Fatalf("round trip of an accepted cursor: %+v -> %+v, %v", c, again, err)
			}
			// Bound to its search: no other search of the set accepts it.
			for _, other := range searches {
				if other.Sort == p.Sort && other.Desc == p.Desc && strings.Join(other.Query.Terms, " ") == strings.Join(p.Query.Terms, " ") {
					continue
				}
				if _, err := DecodeCursor(other, s); err == nil {
					t.Fatalf("a cursor of one search was accepted by another: %q", s)
				}
			}
		}
	})
}
