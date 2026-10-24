package codec

// Boundary tests for the per-path limit on complete reconstructed paths and
// the aggregate budget charged once per file path (wire-format-v1 §5). The
// payloads below are hand-assembled where the encoder cannot produce them.

import (
	"errors"
	"testing"
)

// prefixBlob wraps a raw prefix-delta payload into an uncompressed
// prefix-delta envelope.
func prefixBlob(t testing.TB, payload []byte) []byte {
	t.Helper()
	blob, err := buildEnvelope(PrefixDelta, None, uint64(len(payload)), payload)
	if err != nil {
		t.Fatalf("buildEnvelope() error = %v", err)
	}
	return blob
}

func TestDecodePrefixFullPathLimit(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.MaxPathBytes = 8

	t.Run("single record at the boundary is accepted", func(t *testing.T) {
		t.Parallel()

		payload := bcat(bu(1), bu(0), bu(8), bs("abcdefgh"), bu(1))
		if _, err := Decode(prefixBlob(t, payload), limits); err != nil {
			t.Errorf("Decode() error = %v, want acceptance at the boundary", err)
		}
	})

	t.Run("single record one byte over is rejected", func(t *testing.T) {
		t.Parallel()

		payload := bcat(bu(1), bu(0), bu(9), bs("abcdefghi"), bu(1))
		_, err := Decode(prefixBlob(t, payload), limits)
		var lerr *LimitError
		if !errors.As(err, &lerr) || lerr.Limit != LimitPathBytes {
			t.Errorf("Decode() error = %v, want LimitError(%s)", err, LimitPathBytes)
		}
	})

	t.Run("growth past the boundary is rejected", func(t *testing.T) {
		t.Parallel()

		// shared + suffix reconstructs a 9-byte path; neither record part
		// exceeds the cap alone.
		payload := bcat(bu(2),
			bu(0), bu(4), bs("abcd"), bu(1),
			bu(4), bu(5), bs("efghi"), bu(1))
		_, err := Decode(prefixBlob(t, payload), limits)
		var lerr *LimitError
		if !errors.As(err, &lerr) || lerr.Limit != LimitPathBytes {
			t.Errorf("Decode() error = %v, want LimitError(%s)", err, LimitPathBytes)
		}
	})

	t.Run("growth to the boundary is accepted", func(t *testing.T) {
		t.Parallel()

		payload := bcat(bu(2),
			bu(0), bu(4), bs("abcd"), bu(1),
			bu(4), bu(4), bs("efgh"), bu(1))
		if _, err := Decode(prefixBlob(t, payload), limits); err != nil {
			t.Errorf("Decode() error = %v, want acceptance at the boundary", err)
		}
	})
}

func TestDecodeTrieFullPathLimit(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.MaxPathBytes = 8

	t.Run("root file at the boundary is accepted", func(t *testing.T) {
		t.Parallel()

		payload := bcat(bu(1), bu(0), bu(1), bu(8), bs("abcdefgh"), bu(1))
		if _, err := Decode(trieBlob(t, payload), limits); err != nil {
			t.Errorf("Decode() error = %v, want acceptance at the boundary", err)
		}
	})

	t.Run("dir plus basename past the boundary is rejected", func(t *testing.T) {
		t.Parallel()

		// dirPath "ab/" plus a 6-byte basename reconstructs 9 bytes; the
		// components alone (2 and 6 bytes) are far under the cap.
		payload := bcat(bu(1),
			bu(1), bu(2), bs("ab"), bu(0), bu(1),
			bu(6), bs("cdefgh"), bu(1),
			bu(0))
		_, err := Decode(trieBlob(t, payload), limits)
		var lerr *LimitError
		if !errors.As(err, &lerr) || lerr.Limit != LimitPathBytes {
			t.Errorf("Decode() error = %v, want LimitError(%s)", err, LimitPathBytes)
		}
	})

	t.Run("dir plus basename at the boundary is accepted", func(t *testing.T) {
		t.Parallel()

		payload := bcat(bu(1),
			bu(1), bu(2), bs("ab"), bu(0), bu(1),
			bu(5), bs("cdefg"), bu(1),
			bu(0))
		if _, err := Decode(trieBlob(t, payload), limits); err != nil {
			t.Errorf("Decode() error = %v, want acceptance at the boundary", err)
		}
	})

	t.Run("directory prefix past the boundary is rejected before its files", func(t *testing.T) {
		t.Parallel()

		// Node "abcd" is fine, but its subdirectory prefix "abcd/efghi/"
		// exceeds the cap; the bound fires at descent.
		payload := bcat(bu(1),
			bu(1), bu(4), bs("abcd"),
			bu(1), bu(5), bs("efghi"), bu(0), bu(0),
			bu(0),
			bu(0))
		_, err := Decode(trieBlob(t, payload), limits)
		var lerr *LimitError
		if !errors.As(err, &lerr) || lerr.Limit != LimitPathBytes {
			t.Errorf("Decode() error = %v, want LimitError(%s)", err, LimitPathBytes)
		}
	})

	t.Run("directory prefix exactly at the cap is accepted", func(t *testing.T) {
		t.Parallel()

		// The descent bound is inclusive: dirPath "abcdefg/" reconstructs
		// exactly 8 bytes and the node's own files (none) are never checked
		// against it.
		payload := bcat(bu(1),
			bu(1), bu(7), bs("abcdefg"), bu(0), bu(0),
			bu(1), bu(1), bs("x"), bu(1))
		if _, err := Decode(trieBlob(t, payload), limits); err != nil {
			t.Errorf("Decode() error = %v, want acceptance at the boundary", err)
		}
	})

	t.Run("a file under an exact-cap directory is rejected", func(t *testing.T) {
		t.Parallel()

		// dirPath "abcdefg/" is exactly at the cap, so no basename can
		// follow beneath it: the file check fires before concatenation.
		payload := bcat(bu(1),
			bu(1), bu(7), bs("abcdefg"), bu(0), bu(1),
			bu(1), bs("x"), bu(1),
			bu(0))
		_, err := Decode(trieBlob(t, payload), limits)
		var lerr *LimitError
		if !errors.As(err, &lerr) || lerr.Limit != LimitPathBytes {
			t.Errorf("Decode() error = %v, want LimitError(%s)", err, LimitPathBytes)
		}
	})
}

// A tight per-path cap round-trips through the trie: paths at and under the
// boundary survive encode and decode unchanged.
func TestTrieTightPathCapRoundTrip(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.MaxPathBytes = 16
	files := []File{
		{Path: "abcdefgh/mid/nm", Size: 1},  // 15 bytes under a 9- and 13-byte dir prefix
		{Path: "abcdefghijklmnop", Size: 2}, // exactly the cap
	}

	raw, sel, err := encodeWith(DirectoryTrie, files, limits)
	if err != nil {
		t.Fatalf("encodeWith(trie) error = %v", err)
	}
	if sel.Serializer != DirectoryTrie {
		t.Fatalf("selection = %+v, want the trie serializer", sel)
	}
	decodeRoundTrip(t, raw, files, limits)
}

func TestDecodeTrieAggregateChargedPerFilePath(t *testing.T) {
	t.Parallel()

	// Three files under one directory: the aggregate budget covers the
	// reconstructed file paths only — directory prefixes are structural and
	// add nothing of their own (spec §5 counts reconstructed full paths).
	// Under the previous directory-charging accounting this payload summed
	// 15 bytes and was rejected at the same budget.
	payload := bcat(bu(3),
		bu(1), bu(2), bs("ab"), bu(0),
		bu(3),
		bu(1), bs("c"), bu(1),
		bu(1), bs("d"), bu(1),
		bu(1), bs("e"), bu(1),
		bu(0))

	limits := DefaultLimits()
	limits.MaxAggregatePathBytes = 12 // exactly the three 4-byte file paths
	if _, err := Decode(trieBlob(t, payload), limits); err != nil {
		t.Errorf("Decode() error = %v, want acceptance at the boundary", err)
	}

	limits.MaxAggregatePathBytes = 11
	_, err := Decode(trieBlob(t, payload), limits)
	var lerr *LimitError
	if !errors.As(err, &lerr) || lerr.Limit != LimitAggregatePathBytes {
		t.Errorf("Decode() error = %v, want LimitError(%s)", err, LimitAggregatePathBytes)
	}
}
