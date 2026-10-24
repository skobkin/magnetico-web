package codec

import "testing"

// name is test-only to avoid an exported String method surface.
func (p Profile) name() string {
	switch p {
	case ProfileBalanced:
		return "balanced"
	case ProfileCompact:
		return "compact"
	case ProfileMaximum:
		return "maximum"
	}
	return "unknown"
}

func TestProfileCandidateSets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		p    Profile
		want []candidate
	}{
		{
			p: ProfileBalanced,
			want: []candidate{
				{ser: PrefixDelta, comp: None},
				{ser: PrefixDelta, comp: Zstd},
			},
		},
		{
			p: ProfileCompact,
			want: []candidate{
				{ser: PrefixDelta, comp: None},
				{ser: PrefixDelta, comp: Brotli, quality: 9},
			},
		},
		{
			p: ProfileMaximum,
			want: []candidate{
				{ser: PrefixDelta, comp: None},
				{ser: PrefixDelta, comp: Brotli, quality: 11},
			},
		},
	}

	for _, tt := range tests {
		got := tt.p.candidates()
		if len(got) != len(tt.want) {
			t.Fatalf("profile %v candidates = %+v, want %+v", tt.p, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("profile %v candidate %d = %+v, want %+v", tt.p, i, got[i], tt.want[i])
			}
		}
	}
}

func TestRankImplementsSpecTieBreak(t *testing.T) {
	t.Parallel()

	// Order per wire-format-v1 §4: uncompressed first, then prefix-delta,
	// then trie, then flat.
	tests := []struct {
		c    candidate
		rank int
	}{
		{candidate{ser: PrefixDelta, comp: None}, 0},
		{candidate{ser: PrefixDelta, comp: Zstd}, 16},
		{candidate{ser: DirectoryTrie, comp: None}, 1},
		{candidate{ser: DirectoryTrie, comp: Brotli}, 17},
		{candidate{ser: Flat, comp: None}, 2},
		{candidate{ser: Flat, comp: Zstd}, 18},
	}

	for _, tt := range tests {
		if got := rank(tt.c); got != tt.rank {
			t.Errorf("rank(%+v) = %d, want %d", tt.c, got, tt.rank)
		}
	}

	// better(): smaller length wins outright; on a tie the lower rank wins.
	if !better(10, 5, 11, 0) {
		t.Error("better(10,5,11,0) = false, want true")
	}
	if !better(10, 0, 10, 1) {
		t.Error("tie: better(10,0,10,1) = false, want true")
	}
	if better(10, 2, 10, 2) {
		t.Error("equal tuples: better() = true, want false (keep first)")
	}
}

func TestEncodeRejectsUnknownProfile(t *testing.T) {
	t.Parallel()

	if _, _, err := Encode([]File{{Path: "a", Size: 1}}, Profile(42), DefaultLimits()); err == nil {
		t.Error("Encode() with unknown profile succeeded, want an error")
	}
}
