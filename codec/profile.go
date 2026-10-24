package codec

// Profile selects the candidate (serializer, compressor) sets an encoder
// compares by total framed bytes (wire-format-v1 §4).
type Profile int

const (
	// ProfileBalanced favors write speed: prefix-delta + Zstd at the default
	// level, with an uncompressed fallback.
	ProfileBalanced Profile = iota
	// ProfileCompact is the default profile: prefix-delta/trie + Brotli q9,
	// with an uncompressed fallback.
	ProfileCompact
	// ProfileMaximum trades write speed for size: prefix-delta/trie + Brotli
	// q11, with an uncompressed fallback.
	ProfileMaximum
)

// DefaultProfile returns ProfileCompact, the 0.8.0 default.
func DefaultProfile() Profile {
	return ProfileCompact
}

type candidate struct {
	ser     SerializerID
	comp    CompressorID
	quality int // Brotli quality; ignored for other compressors
}

// candidates returns the profile's candidate set, ordered so that iterating
// and keeping strictly better results implements the deterministic
// tie-break: prefer uncompressed, then prefix-delta, then trie, then flat.
func (p Profile) candidates() []candidate {
	switch p {
	case ProfileBalanced:
		return []candidate{
			{ser: PrefixDelta, comp: None},
			{ser: PrefixDelta, comp: Zstd},
		}
	case ProfileCompact:
		return []candidate{
			{ser: PrefixDelta, comp: None},
			{ser: PrefixDelta, comp: Brotli, quality: 9},
		}
	case ProfileMaximum:
		return []candidate{
			{ser: PrefixDelta, comp: None},
			{ser: PrefixDelta, comp: Brotli, quality: 11},
		}
	default:
		return nil
	}
}

func (p Profile) brotliQuality() int {
	for _, c := range p.candidates() {
		if c.comp == Brotli {
			return c.quality
		}
	}
	return 9
}

func (p Profile) valid() bool {
	switch p {
	case ProfileBalanced, ProfileCompact, ProfileMaximum:
		return true
	}
	return false
}

// better reports whether candidate (len1, rank1) beats (len2, rank2): smaller
// total framed length first, then the lower rank tuple. Equal tuples are
// byte-identical encodings, so keeping the first is deterministic.
func better(len1, rank1, len2, rank2 int) bool {
	if len1 != len2 {
		return len1 < len2
	}
	return rank1 < rank2
}

// rank implements the spec's stable tie-break: compressor none before
// compressed, then serializer prefix-delta, then trie, then flat.
func rank(c candidate) int {
	serRank := 0
	switch c.ser {
	case PrefixDelta:
		serRank = 0
	case DirectoryTrie:
		serRank = 1
	case Flat:
		serRank = 2
	}
	compRank := 1
	if c.comp == None {
		compRank = 0
	}
	return compRank*16 + serRank
}
