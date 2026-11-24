package fileview

import (
	"errors"
	"strings"
	"testing"
)

// FuzzParsePath: whatever a client sends as a positional directory path, the
// parser never panics, rejects with ErrBadPath only, and accepts exactly the
// canonical form (no sign, no leading zeros, at most six digits, bounded
// depth), so FormatPath(ParsePath(s)) == s. The seed corpus runs in the normal
// test suite; `go test -fuzz=FuzzParsePath` explores further.
func FuzzParsePath(f *testing.F) {
	for _, s := range []string{
		"", "0", "2_5_6", "00", "0__1", "_0", "0_", "-1", "+1", "1e3", "0x10", " 1", "1 ", "1\x00", "٠", "１", "999999", "1000000",
		"0_0_0_0_0_0_0_0", strings.Repeat("1_", 255) + "1", strings.Repeat("1_", 256) + "1", strings.Repeat("9", 4096),
		"1_2_x", "1__", "__", "_", "1_2_3_", "%31", "1%5f2", "\xff", "0\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		comps, err := ParsePath(s, 256)
		if err != nil {
			if !errors.Is(err, ErrBadPath) {
				t.Fatalf("ParsePath(%q): unexpected error %v", s, err)
			}
			return
		}
		if len(comps) > 256 {
			t.Fatalf("ParsePath(%q): %d components, the limit is 256", s, len(comps))
		}
		for _, c := range comps {
			if c < 0 || c > 999999 {
				t.Fatalf("ParsePath(%q): component %d out of range", s, c)
			}
		}
		if got := FormatPath(comps); got != s {
			t.Fatalf("ParsePath(%q) accepted a non-canonical path (canonical form %q)", s, got)
		}
	})
}
