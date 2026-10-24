package codec

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

// bu, bs and bcat assemble raw trie payloads for malformed-structure cases
// the encoder cannot produce.
func bu(v uint64) []byte { return appendUvarint(nil, v) }

func bs(s string) []byte { return []byte(s) }

func bcat(bs ...[]byte) []byte {
	var out []byte
	for _, b := range bs {
		out = append(out, b...)
	}
	return out
}

// trieBlob wraps a raw trie payload into an uncompressed trie envelope.
func trieBlob(t testing.TB, payload []byte) []byte {
	t.Helper()
	blob, err := buildEnvelope(DirectoryTrie, None, uint64(len(payload)), payload)
	if err != nil {
		t.Fatalf("buildEnvelope() error = %v", err)
	}
	return blob
}

func TestDecodeTrieRejectsMalformedStructures(t *testing.T) {
	t.Parallel()

	// Node layout: uvarint(subdir_count), per subdir uvarint(len) bytes
	// uvarint(subdir_count)... recursively, then uvarint(file_count) and per
	// file uvarint(len) bytes uvarint(size).
	tests := []struct {
		name      string
		limits    func(Limits) Limits
		payload   []byte
		wantErr   error
		wantLimit string // set when wantErr is a *LimitError sentinel class
	}{
		{
			name: "duplicate subdirectory names",
			payload: bcat(bu(1), // global count
				bu(2),                        // root subdirs
				bu(1), bs("a"), bu(0), bu(0), // subdir a (empty node)
				bu(1), bs("a"), bu(0), bu(0)), // subdir a again
			wantErr: ErrDuplicateDirName,
		},
		{
			name: "out-of-order subdirectory list",
			payload: bcat(bu(1),
				bu(2),
				bu(1), bs("b"), bu(0), bu(0),
				bu(1), bs("a"), bu(0), bu(0)),
			wantErr: ErrNonCanonicalOrder,
		},
		{
			name: "out-of-order local file list",
			payload: bcat(bu(2),
				bu(0), // no subdirs
				bu(2), // two files
				bu(1), bs("b"), bu(1),
				bu(1), bs("a"), bu(1)),
			wantErr: ErrNonCanonicalOrder,
		},
		{
			name: "equal basenames with decreasing sizes",
			payload: bcat(bu(2),
				bu(0),
				bu(2),
				bu(1), bs("a"), bu(2),
				bu(1), bs("a"), bu(1)),
			wantErr: ErrNonCanonicalOrder,
		},
		{
			name: "nested-node equal basenames with decreasing sizes",
			payload: bcat(bu(2), // global count
				bu(1),          // root subdirs
				bu(1), bs("d"), // subdir d
				bu(0), // d: no subdirs
				bu(2), // d: two files
				bu(1), bs("a"), bu(2),
				bu(1), bs("a"), bu(1), // decreasing: non-canonical
				bu(0)), // root files
			wantErr: ErrNonCanonicalOrder,
		},
		{
			name:    "separator in subdirectory component",
			payload: bcat(bu(1), bu(1), bu(3), bs("a/b"), bu(0), bu(0), bu(0)),
			wantErr: ErrPathComponentSeparator,
		},
		{
			name:    "separator in file component",
			payload: bcat(bu(1), bu(0), bu(1), bu(3), bs("a/b"), bu(1)),
			wantErr: ErrPathComponentSeparator,
		},
		{
			name:    "empty subdirectory component",
			payload: bcat(bu(1), bu(1), bu(0)),
			wantErr: ErrEmptyPathComponent,
		},
		{
			name:    "empty file component",
			payload: bcat(bu(1), bu(0), bu(1), bu(0)),
			wantErr: ErrEmptyPathComponent,
		},
		{
			name: "local file count exceeds the remaining global count",
			payload: bcat(bu(1),
				bu(0),
				bu(2)), // claims two files, none present
			wantErr: ErrInvalidCount,
		},
		{
			name: "global file count not exhausted",
			payload: bcat(bu(2),
				bu(0),
				bu(1), bu(1), bs("a"), bu(1)),
			wantErr: ErrInvalidCount,
		},
		{
			name:    "trailing bytes",
			payload: bcat(bu(1), bu(0), bu(1), bu(1), bs("a"), bu(1), bu(9)),
			wantErr: ErrTrailingBytes,
		},
		{
			name:    "truncated subdirectory node",
			payload: bcat(bu(1), bu(1), bu(1), bs("a")),
			wantErr: ErrTruncated,
		},
		{
			name: "file size overflows int64",
			payload: bcat(bu(1), bu(0), bu(1),
				bu(1), bs("a"), bu(uint64(math.MaxInt64)+1)),
			wantErr: ErrOverflow,
		},
		{
			name: "depth limit",
			limits: func(l Limits) Limits {
				l.MaxTrieDepth = 2
				return l
			},
			// a/b/c/d.txt nests three directories below the root.
			payload: bcat(bu(1),
				bu(1), bu(1), bs("a"),
				bu(1), bu(1), bs("b"),
				bu(1), bu(1), bs("c"),
				bu(0), bu(1), bu(1), bs("d"), bu(1), // c: file d
				bu(0),  // b files
				bu(0)), // a files
			wantLimit: LimitTrieDepth,
		},
		{
			name: "aggregate path budget",
			limits: func(l Limits) Limits {
				l.MaxAggregatePathBytes = 4
				return l
			},
			payload:   bcat(bu(1), bu(0), bu(1), bu(5), bs("abcde"), bu(1)),
			wantLimit: LimitAggregatePathBytes,
		},
		{
			// readUvarint on an empty buffer classifies as truncation.
			name:    "empty payload",
			payload: nil,
			wantErr: ErrTruncated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			limits := DefaultLimits()
			if tt.limits != nil {
				limits = tt.limits(limits)
			}
			_, err := Decode(trieBlob(t, tt.payload), limits)
			if err == nil {
				t.Fatal("Decode() succeeded, want a rejection")
			}
			if tt.wantLimit != "" {
				var lerr *LimitError
				if !errors.As(err, &lerr) || lerr.Limit != tt.wantLimit {
					t.Errorf("Decode() error = %v, want LimitError(%s)", err, tt.wantLimit)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Decode() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeTrieDuplicateBasenamesAreValid(t *testing.T) {
	t.Parallel()

	// Equal basenames with non-decreasing sizes are canonical (vector D
	// semantics apply per node); identical (basename, size) duplicates stay
	// valid too.
	tests := []struct {
		name    string
		payload []byte
		want    []File
	}{
		{
			name: "increasing sizes",
			payload: bcat(bu(2), bu(0), bu(2),
				bu(1), bs("a"), bu(1),
				bu(1), bs("a"), bu(2)),
			want: []File{{Path: "a", Size: 1}, {Path: "a", Size: 2}},
		},
		{
			name: "identical duplicates",
			payload: bcat(bu(2), bu(0), bu(2),
				bu(1), bs("a"), bu(1),
				bu(1), bs("a"), bu(1)),
			want: []File{{Path: "a", Size: 1}, {Path: "a", Size: 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Decode(trieBlob(t, tt.payload), DefaultLimits())
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Decode() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestTrieSameNameFileAndDir(t *testing.T) {
	t.Parallel()

	// "a/b" as a file and "a/b/c" as a file under directory b coexist
	// without loss. The profile comparison may pick any candidate for a
	// list this small, so the trie path is exercised through the
	// fixed-serializer entry point.
	files := []File{{Path: "a/b", Size: 1}, {Path: "a/b/c", Size: 2}}
	want, err := canonicalize(files, DefaultLimits())
	if err != nil {
		t.Fatalf("canonicalize() error = %v", err)
	}

	blob, sel, err := Encode(files, ProfileCompact, DefaultLimits())
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	got, err := Decode(blob, DefaultLimits())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Encode/Decode (selection %+v) = %#v, %v, want %#v", sel, got, err, want)
	}

	raw, sel2, err := encodeWith(DirectoryTrie, files, DefaultLimits())
	if err != nil {
		t.Fatalf("encodeWith(trie) error = %v", err)
	}
	if sel2.Serializer != DirectoryTrie {
		t.Fatalf("selection = %+v, want the trie serializer", sel2)
	}
	got, err = Decode(raw, DefaultLimits())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("trie Decode() = %#v, %v, want %#v", got, err, want)
	}
}

func TestTrieIneligiblePathsFallBack(t *testing.T) {
	t.Parallel()

	// Vector F paths: trie must be excluded, prefix-delta used, the paths
	// preserved byte-exactly.
	files := []File{{Path: "a/../c", Size: 2}, {Path: "a//b", Size: 1}}
	want, err := canonicalize(files, DefaultLimits())
	if err != nil {
		t.Fatalf("canonicalize() error = %v", err)
	}

	for _, p := range []Profile{ProfileCompact, ProfileMaximum} {
		blob, sel, err := Encode(files, p, DefaultLimits())
		if err != nil {
			t.Fatalf("Encode(%v) error = %v", p, err)
		}
		if sel.Serializer != PrefixDelta {
			t.Errorf("profile %v selection = %+v, want prefix-delta fallback", p, sel)
		}
		got, err := Decode(blob, DefaultLimits())
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("profile %v: Decode() = %#v, %v, want %#v", p, got, err, want)
		}
	}
}

func TestTrieDepthOverLimitSkipsCandidate(t *testing.T) {
	t.Parallel()

	limits := DefaultLimits()
	limits.MaxTrieDepth = 2

	// Three separators need depth 3; the trie candidate is skipped and the
	// prefix candidate represents the list.
	files := []File{{Path: "a/b/c/d.txt", Size: 1}}
	want, err := canonicalize(files, limits)
	if err != nil {
		t.Fatalf("canonicalize() error = %v", err)
	}

	blob, sel, err := Encode(files, ProfileCompact, limits)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if sel.Serializer == DirectoryTrie {
		t.Errorf("selection = %+v, want the trie candidate skipped", sel)
	}
	got, err := Decode(blob, limits)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Decode() = %#v, %v, want %#v", got, err, want)
	}
}

func TestTrieRoundTripDeterministic(t *testing.T) {
	t.Parallel()

	files := []File{
		{Path: "z/deep/nest/ed/file.bin", Size: 500},
		{Path: "a.txt", Size: 1},
		{Path: "m/!odd!.name", Size: 2},
		{Path: "m/0", Size: 3},
		{Path: "m/0/inner", Size: 4},
		{Path: "dup", Size: 9},
		{Path: "dup", Size: 9},
	}
	want, err := canonicalize(files, DefaultLimits())
	if err != nil {
		t.Fatalf("canonicalize() error = %v", err)
	}

	blob, sel, err := Encode(files, ProfileCompact, DefaultLimits())
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	got, err := Decode(blob, DefaultLimits())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Decode() = %#v, %v, want %#v (selection %+v)", got, err, want, sel)
	}

	// Re-encoding the decoded list must reproduce the exact blob, and the
	// fixed-serializer path must handle the same list deterministically.
	blob2, sel2, err := Encode(want, ProfileCompact, DefaultLimits())
	if err != nil || sel2 != sel || string(blob2) != string(blob) {
		t.Errorf("re-Encode() = %x, %+v, %v; want (%x, %+v)", blob2, sel2, err, blob, sel)
	}
	raw1, _, err := encodeWith(DirectoryTrie, files, DefaultLimits())
	if err != nil {
		t.Fatalf("encodeWith(trie) error = %v", err)
	}
	raw2, _, err := encodeWith(DirectoryTrie, want, DefaultLimits())
	if err != nil {
		t.Fatalf("encodeWith(trie) of the sorted list error = %v", err)
	}
	if string(raw1) != string(raw2) {
		t.Error("encodeWith(trie) is input-order dependent")
	}
	got, err = Decode(raw1, DefaultLimits())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("trie Decode() = %#v, %v, want %#v", got, err, want)
	}
}
