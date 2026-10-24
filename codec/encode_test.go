package codec

// Regression tests for candidate-skip semantics in encodeSelected
// (wire-format-v1 §4/§5): a candidate whose payload or framed blob exceeds
// a limit is skipped while the remaining candidates are evaluated; the
// typed limit error is returned only when no valid candidate remains.

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// oversizedDirFiles builds the shared fixture: 20000 files with 64-hex-
// character names under one 20000-byte directory.
func oversizedDirFiles() []File {
	dir := strings.Repeat("d", 20000)
	files := make([]File, 0, 20000)
	for i := 0; i < 20000; i++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("name-%d", i)))
		files = append(files, File{Path: fmt.Sprintf("%s/%x", dir, h), Size: int64(i * 13)})
	}
	return files
}

// oversizeFixtureRaw encodes the fixture's canonical form with both
// serializers — the aggregate budget is raised, the scenarios are not about
// that limit — and fails if the trie-beats-prefix assumption breaks.
func oversizeFixtureRaw(t *testing.T, files []File) (prefixRaw, trieRaw []byte) {
	t.Helper()
	limits := DefaultLimits()
	limits.MaxAggregatePathBytes = 1 << 40
	sorted, err := canonicalize(files, limits)
	if err != nil {
		t.Fatalf("canonicalize() error = %v", err)
	}
	prefixRaw, err = encodeRaw(PrefixDelta, sorted)
	if err != nil {
		t.Fatalf("encodeRaw(PrefixDelta) error = %v", err)
	}
	trieRaw, err = encodeRaw(DirectoryTrie, sorted)
	if err != nil {
		t.Fatalf("encodeRaw(DirectoryTrie) error = %v", err)
	}
	if len(trieRaw) >= len(prefixRaw) {
		t.Fatalf("fixture assumption broken: trie raw %d >= prefix raw %d", len(trieRaw), len(prefixRaw))
	}
	return prefixRaw, trieRaw
}

// mustCanonical returns the canonical form of files or fails the test.
func mustCanonical(t *testing.T, files []File, limits Limits) []File {
	t.Helper()
	sorted, err := canonicalize(files, limits)
	if err != nil {
		t.Fatalf("canonicalize() error = %v", err)
	}
	return sorted
}

func TestEncodeSelectionSkipsOverLimitCandidates(t *testing.T) {
	t.Parallel()

	t.Run("uncompressed prefix-delta over stored limit, compressed fits", func(t *testing.T) {
		t.Parallel()

		files := sampleFiles(100)
		base, _, err := encodeWith(PrefixDelta, files, DefaultLimits())
		if err != nil {
			t.Fatalf("encodeWith(PrefixDelta) error = %v", err)
		}
		limits := DefaultLimits()
		// The uncompressed prefix candidate no longer fits; every compressor
		// variant of another serializer must still be evaluated.
		limits.MaxStoredBytes = len(base) - 1

		blob, sel, err := Encode(files, ProfileCompact, limits)
		if err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
		if sel == (Selection{Serializer: PrefixDelta, Compressor: None}) {
			t.Errorf("Encode() selected the candidate above the stored-bytes limit: %+v", sel)
		}
		decodeRoundTrip(t, blob, files, limits)
	})

	t.Run("prefix-delta payload over uncompressed limit, trie fits", func(t *testing.T) {
		t.Parallel()

		// A >16 KiB shared directory prefix makes every prefix record pay a
		// 3-byte shared varint while the trie pays a per-node count once,
		// and long high-entropy basenames deny the prefix records any
		// suffix shortening. With enough files the trie payload is
		// decisively smaller, so the cap below the prefix payload still
		// admits it — killing both prefix candidates (they share one raw
		// length) while the trie candidates remain evaluable.
		files := oversizedDirFiles()
		prefixRaw, trieRaw := oversizeFixtureRaw(t, files)

		limits := DefaultLimits()
		// The fixture's aggregate path bytes exceed the default budget; the
		// scenario is about the uncompressed-payload cap.
		limits.MaxAggregatePathBytes = 1 << 40
		limits.MaxUncompressedBytes = (len(prefixRaw) + len(trieRaw)) / 2

		blob, sel, err := Encode(files, ProfileCompact, limits)
		if err != nil {
			t.Fatalf("Encode() error = %v", err)
		}
		if sel.Serializer != DirectoryTrie {
			t.Errorf("Encode() selection = %+v, want a DirectoryTrie candidate", sel)
		}
		decodeRoundTrip(t, blob, files, limits)
	})

	t.Run("first recorded failure wins across limit kinds", func(t *testing.T) {
		t.Parallel()

		// All four compact candidates fail, on two different limits: the
		// leading prefix+none candidate trips the uncompressed-payload cap
		// (both prefix candidates share one raw length), while the trie
		// payloads fit but every trie blob — ending with the winning
		// trie+brotli candidate — is over the stored cap. The returned
		// error must be the FIRST recorded failure, not the later
		// stored-bytes kind.
		files := oversizedDirFiles()
		prefixRaw, trieRaw := oversizeFixtureRaw(t, files)

		st := newCompressState(ProfileCompact)
		trieComp, err := st.compress(Brotli, trieRaw)
		if err != nil {
			t.Fatalf("compress(Brotli) error = %v", err)
		}
		trieBrotliBlob, err := buildEnvelope(DirectoryTrie, Brotli, uint64(len(trieRaw)), trieComp)
		if err != nil {
			t.Fatalf("buildEnvelope() error = %v", err)
		}
		trieNoneBlob, err := buildEnvelope(DirectoryTrie, None, uint64(len(trieRaw)), trieRaw)
		if err != nil {
			t.Fatalf("buildEnvelope() error = %v", err)
		}
		if len(trieBrotliBlob) >= len(trieNoneBlob) {
			t.Fatalf("fixture assumption broken: compressed trie blob %d >= uncompressed %d",
				len(trieBrotliBlob), len(trieNoneBlob))
		}

		limits := DefaultLimits()
		limits.MaxAggregatePathBytes = 1 << 40
		limits.MaxUncompressedBytes = len(prefixRaw) - 1 // kills both prefix candidates
		limits.MaxStoredBytes = len(trieBrotliBlob) - 1  // kills both trie candidates

		_, _, err = encodeSelected(nil, ProfileCompact, mustCanonical(t, files, limits), limits)
		var le *LimitError
		if !errors.As(err, &le) {
			t.Fatalf("encodeSelected() error = %v, want a LimitError", err)
		}
		if le.Limit != LimitUncompressedBytes {
			t.Errorf("LimitError.Limit = %s, want %s: the first recorded failure must win",
				le.Limit, LimitUncompressedBytes)
		}
	})

	t.Run("oversized later candidate does not invalidate an earlier valid one", func(t *testing.T) {
		t.Parallel()

		// Unique high-entropy directories and names: prefix-delta has no
		// repetitions to exploit, and the trie candidates grow past the
		// stored-bytes cap set at the valid uncompressed prefix blob.
		files := make([]File, 0, 200)
		for i := 0; i < 200; i++ {
			h := sha256.Sum256([]byte(fmt.Sprintf("entry-%d", i)))
			files = append(files, File{
				Path: fmt.Sprintf("%x/%x", h[:16], h),
				Size: int64(h[0]) + int64(i),
			})
		}
		base, _, err := encodeWith(PrefixDelta, files, DefaultLimits())
		if err != nil {
			t.Fatalf("encodeWith(PrefixDelta) error = %v", err)
		}
		trieBlob, _, err := encodeWith(DirectoryTrie, files, DefaultLimits())
		if err != nil {
			t.Fatalf("encodeWith(DirectoryTrie) error = %v", err)
		}
		limits := DefaultLimits()
		limits.MaxStoredBytes = len(base) // the uncompressed prefix candidate fits exactly
		if len(trieBlob) <= limits.MaxStoredBytes {
			t.Fatalf("fixture assumption broken: trie blob %d <= cap %d", len(trieBlob), limits.MaxStoredBytes)
		}

		blob, _, err := Encode(files, ProfileCompact, limits)
		if err != nil {
			t.Fatalf("Encode() error = %v; want the earlier valid candidate's blob", err)
		}
		decodeRoundTrip(t, blob, files, limits)
	})

	t.Run("limit error only when no candidate remains", func(t *testing.T) {
		t.Parallel()

		files := sampleFiles(50)

		limits := DefaultLimits()
		limits.MaxStoredBytes = 1
		_, _, err := Encode(files, ProfileCompact, limits)
		var le *LimitError
		if !errors.As(err, &le) || le.Limit != LimitStoredBytes {
			t.Errorf("Encode() error = %v, want a stored_bytes LimitError", err)
		}

		limits = DefaultLimits()
		limits.MaxUncompressedBytes = 1
		_, _, err = Encode(files, ProfileCompact, limits)
		if !errors.As(err, &le) || le.Limit != LimitUncompressedBytes {
			t.Errorf("Encode() error = %v, want an uncompressed_bytes LimitError", err)
		}
	})
}

// decodeRoundTrip asserts that blob decodes, under the same limits, to the
// canonical form of files.
func decodeRoundTrip(t *testing.T, blob []byte, files []File, limits Limits) {
	t.Helper()
	got, err := Decode(blob, limits)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	want, err := canonicalize(files, limits)
	if err != nil {
		t.Fatalf("canonicalize() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Decode() = %#v, want %#v", got, want)
	}
}
