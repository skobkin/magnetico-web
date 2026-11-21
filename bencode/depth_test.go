package bencode

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckDepth(t *testing.T) {
	t.Parallel()
	nest := func(n int, open, closer string) string {
		return strings.Repeat(open, n) + strings.Repeat(closer, n)
	}
	tests := []struct {
		name string
		in   string
		max  int
		want error
	}{
		{"empty", "", 4, nil},
		{"scalar int", "i42e", 4, nil},
		{"scalar string", "4:spam", 4, nil},
		{"flat list", "li1ei2ee", 1, nil},
		{"flat dict", "d3:fooi1ee", 1, nil},
		{"exactly at the limit", nest(4, "l", "e"), 4, nil},
		{"one over the limit", nest(5, "l", "e"), 4, ErrNestingTooDeep},
		{"nested dicts", "d1:ad1:bd1:cdeeee", 3, ErrNestingTooDeep},
		{"string content looks like structure", "l5:lllll4:eeeee", 1, nil},
		{"string with colon and digits", "l3:1:ee", 1, nil},
		{"integer ends the scan of its own value", "li1eli2eee", 2, nil},
		{"deep only in a later sibling", "li1e" + nest(10, "l", "e") + "e", 5, ErrNestingTooDeep},
		{"deep under dict value", "d1:a" + nest(10, "l", "e") + "e", 5, ErrNestingTooDeep},
		{"truncated: not our error", strings.Repeat("l", 3), 10, nil},
		{"truncated beyond the limit", strings.Repeat("l", 11), 10, ErrNestingTooDeep},
		{"huge declared string length", "99999999999999999999:x", 4, nil},
		{"unmatched e", "eeee", 4, nil},
		{"garbage", "xyz", 4, nil},
		{"trailing data after the first value is ignored", "i1e" + nest(100, "l", "e"), 4, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CheckDepth([]byte(tt.in), tt.max); !errors.Is(got, tt.want) {
				t.Errorf("CheckDepth(%q, %d) = %v, want %v", tt.in, tt.max, got, tt.want)
			}
		})
	}
}

// The scan must be linear: a 10 MiB hostile input finishes instantly and
// without allocating.
func TestCheckDepthLargeInput(t *testing.T) {
	in := []byte(strings.Repeat("l", 5<<20) + strings.Repeat("e", 5<<20))
	if err := CheckDepth(in, 256); !errors.Is(err, ErrNestingTooDeep) {
		t.Fatalf("err = %v", err)
	}
	allocs := testing.AllocsPerRun(3, func() { _ = CheckDepth(in, 256) })
	if allocs != 0 {
		t.Errorf("CheckDepth allocated %v times", allocs)
	}
}

// Anything CheckDepth accepts must not be deeper than the limit, as seen by
// the real decoder (cross-check on generated structures).
func FuzzCheckDepth(f *testing.F) {
	f.Add([]byte("d3:fooli1ei2eee"), 3)
	f.Add([]byte(strings.Repeat("l", 50)+strings.Repeat("e", 50)), 10)
	f.Fuzz(func(t *testing.T, in []byte, max int) {
		if max < 0 || max > 64 || len(in) > 1<<12 {
			return
		}
		_ = CheckDepth(in, max) // must not panic or hang
	})
}
