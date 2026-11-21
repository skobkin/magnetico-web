package codec

import (
	"bytes"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// Determinism regression for wire-format-v1 §4: encoding a canonical input
// must yield the same bytes and the same Selection regardless of what the
// compressor or Encoder encoded before.
//
// Root cause being guarded: brotli.Writer.Reset clears the stream size hint
// but keeps the hasher object chosen for the first payload. At quality 5-9
// the hasher type depends on that size hint (a payload of at least
// brotliLargeHint selects a different hasher), so a reused writer compresses
// a payload differently depending on the size class of whatever it
// compressed first.

// brotliLargeHint is the payload size at which brotli (v1.2.6, quality <= 9)
// switches its hasher choice (size_hint >= 1<<20).
const brotliLargeHint = 1 << 20

// detFixture builds a canonical, deterministic file list. Styles differ in
// path structure so the payloads have different statistics; none is random,
// because incompressible data would be stored raw and hide the effect.
func detFixture(style, n int) []File {
	files := make([]File, 0, n)
	for i := 0; i < n; i++ {
		var p string
		switch style {
		case 0: // deep, repetitive media tree
			p = fmt.Sprintf("Media/Show %02d/Season %02d/Show.S%02dE%03d.1080p.WEB-DL.mkv", i%7, i%5, i%5, i)
		case 1: // flat, high-entropy names
			p = fmt.Sprintf("flat-%07d-%x.dat", i*7919, i*2654435761%100003)
		default: // shallow numbered tree
			p = fmt.Sprintf("a/b/c/%d/file%d.txt", i%13, i)
		}
		files = append(files, File{Path: p, Size: int64(i*37 + style)})
	}
	sorted, err := canonicalize(files, DefaultLimits())
	if err != nil {
		panic(err)
	}
	return sorted
}

type detInput struct {
	name  string
	files []File
}

// detInputs returns two large inputs of different shape (both serialize to
// at least brotliLargeHint bytes with both prefix-delta and trie), a small
// one and a tiny one.
func detInputs(t testing.TB) (large1, large2, small, tiny detInput) {
	t.Helper()
	large1 = detInput{"large-media", detFixture(0, 52000)}
	large2 = detInput{"large-flat", detFixture(1, 70000)}
	small = detInput{"small-shallow", detFixture(2, 800)}
	tiny = detInput{"tiny", []File{{Path: "movie.mkv", Size: 1}, {Path: "movie.srt", Size: 2}}}

	for _, in := range []detInput{large1, large2} {
		for _, ser := range []SerializerID{PrefixDelta, DirectoryTrie} {
			raw, err := encodeRaw(ser, in.files)
			if err != nil {
				t.Fatalf("encodeRaw(%s, %d) error = %v", in.name, ser, err)
			}
			if len(raw) < brotliLargeHint {
				t.Fatalf("%s serializer %d payload = %d bytes, want >= %d", in.name, ser, len(raw), brotliLargeHint)
			}
		}
	}
	for _, in := range []detInput{small, tiny} {
		raw, err := encodeRaw(PrefixDelta, in.files)
		if err != nil {
			t.Fatalf("encodeRaw(%s) error = %v", in.name, err)
		}
		if len(raw) >= brotliLargeHint {
			t.Fatalf("%s payload = %d bytes, want < %d", in.name, len(raw), brotliLargeHint)
		}
	}
	return large1, large2, small, tiny
}

// detMedium is a mid-sized input (payloads above the 2^18 brotli block size
// but below brotliLargeHint) that keeps q11 tests fast enough for CI.
func detMedium(t testing.TB) detInput {
	t.Helper()
	in := detInput{"medium-media", detFixture(0, 12000)}
	for _, ser := range []SerializerID{PrefixDelta, DirectoryTrie} {
		raw, err := encodeRaw(ser, in.files)
		if err != nil {
			t.Fatalf("encodeRaw(%s, %d) error = %v", in.name, ser, err)
		}
		if len(raw) <= 1<<18 || len(raw) >= brotliLargeHint {
			t.Fatalf("%s serializer %d payload = %d bytes, want in (%d, %d)", in.name, ser, len(raw), 1<<18, brotliLargeHint)
		}
	}
	return in
}

// compressCopy compresses payload and copies the result: compress returns a
// slice aliasing the state's reusable buffer, which the next call overwrites.
func compressCopy(t testing.TB, st *compressState, comp CompressorID, payload []byte) []byte {
	t.Helper()
	out, err := st.compress(comp, payload)
	if err != nil {
		t.Fatalf("compress() error = %v", err)
	}
	return bytes.Clone(out)
}

type compressStep struct {
	in  detInput
	ser SerializerID
}

func stepName(history []compressStep) string {
	var b bytes.Buffer
	for i, s := range history {
		if i > 0 {
			b.WriteString(" -> ")
		}
		fmt.Fprintf(&b, "%s/ser%d", s.in.name, s.ser)
	}
	return b.String()
}

// TestCompressStateHistoryIndependence reuses one compressState across
// payload histories and requires every result to equal a fresh state's
// output for the same payload. It exercises compress() directly, so it does
// not depend on sync.Pool handing back the same object.
func TestCompressStateHistoryIndependence(t *testing.T) {
	t.Parallel()

	large1, large2, small, tiny := detInputs(t)
	medium := detMedium(t)

	type histories struct {
		profile Profile
		short   bool // keep in -short runs
		steps   [][]compressStep
	}
	p := func(in detInput) compressStep { return compressStep{in, PrefixDelta} }
	tr := func(in detInput) compressStep { return compressStep{in, DirectoryTrie} }

	cases := []histories{
		{ProfileCompact, true, [][]compressStep{
			// small -> large and large -> small (the original defect)
			{p(small), p(large1)}, {p(large1), p(small)},
			// different large inputs, repeated mixed history
			{p(large1), p(large2), p(large1)},
			// the same for trie payloads
			{tr(small), tr(large1)}, {tr(large1), tr(tiny)},
			// prefix -> trie and trie -> prefix inside one state
			{p(large1), tr(small), p(large2), tr(tiny)},
		}},
		// The randomized sweep in determinism_reuse_test.go supplies breadth
		// across sizes and shapes; q11 is slow on multi-MiB payloads, so
		// these histories use the medium input and always use the same
		// hasher regardless of size.
		{ProfileMaximum, true, [][]compressStep{
			{p(small), p(medium), tr(tiny), tr(medium)},
		}},
	}

	for _, c := range cases {
		t.Run(c.profile.name(), func(t *testing.T) {
			t.Parallel()
			if testing.Short() && !c.short {
				t.Skip("q11 large payloads skipped in -short mode")
			}

			var mu sync.Mutex
			type refKey struct {
				name string
				ser  SerializerID
			}
			refs := map[refKey][]byte{}
			ref := func(s compressStep) []byte {
				mu.Lock()
				defer mu.Unlock()
				key := refKey{s.in.name, s.ser}
				if r, ok := refs[key]; ok {
					return r
				}
				raw, err := encodeRaw(s.ser, s.in.files)
				if err != nil {
					t.Fatalf("encodeRaw() error = %v", err)
				}
				r := compressCopy(t, newCompressState(c.profile), Brotli, raw)
				refs[key] = r
				return r
			}

			for _, history := range c.steps {
				t.Run(stepName(history), func(t *testing.T) {
					shared := newCompressState(c.profile)
					for i, s := range history {
						raw, err := encodeRaw(s.ser, s.in.files)
						if err != nil {
							t.Fatalf("encodeRaw() error = %v", err)
						}
						got := compressCopy(t, shared, Brotli, raw)
						if want := ref(s); !bytes.Equal(got, want) {
							t.Fatalf("step %d (%s/ser%d) after history: %d bytes, fresh state gives %d bytes (differs)",
								i, s.in.name, s.ser, len(got), len(want))
						}
					}
				})
			}
		})
	}
}

// TestZstdStateReuseControl is the control for the test above: Zstd reuse
// has no history dependence and must stay that way.
func TestZstdStateReuseControl(t *testing.T) {
	t.Parallel()

	large1, _, small, tiny := detInputs(t)
	shared := newCompressState(ProfileBalanced)
	for _, in := range []detInput{small, large1, tiny, large1, small} {
		raw, err := encodeRaw(PrefixDelta, in.files)
		if err != nil {
			t.Fatalf("encodeRaw() error = %v", err)
		}
		got := compressCopy(t, shared, Zstd, raw)
		want := compressCopy(t, newCompressState(ProfileBalanced), Zstd, raw)
		if !bytes.Equal(got, want) {
			t.Errorf("zstd reuse diverged for %s", in.name)
		}
	}
}

type encodedRef struct {
	blob []byte
	sel  Selection
}

// referenceEncode is the history-free reference: package-level Encode builds
// fresh compressor state for every candidate.
func referenceEncode(t testing.TB, in detInput, p Profile) encodedRef {
	t.Helper()
	blob, sel, err := Encode(in.files, p, DefaultLimits())
	if err != nil {
		t.Fatalf("Encode(%s) error = %v", in.name, err)
	}
	return encodedRef{blob, sel}
}

func checkEncoded(t testing.TB, label string, in detInput, blob []byte, sel Selection, want encodedRef) {
	t.Helper()
	if sel != want.sel {
		t.Errorf("%s: Selection = %+v, fresh encoding selects %+v", label, sel, want.sel)
	}
	if !bytes.Equal(blob, want.blob) {
		t.Errorf("%s: blob differs from fresh encoding (%d vs %d bytes)", label, len(blob), len(want.blob))
	}
	got, err := Decode(blob, DefaultLimits())
	if err != nil {
		t.Errorf("%s: Decode() error = %v", label, err)
		return
	}
	if !reflect.DeepEqual(got, in.files) {
		t.Errorf("%s: decoded content differs from canonical input", label)
	}
}

// newPinnedEncoder returns a single-worker Encoder whose pool can only ever
// yield one compressState. sync.Pool may drop idle objects at any GC, which
// would silently turn a "reused" encoder into a fresh one and make a history
// test pass vacuously; pinning makes the reuse unconditional.
func newPinnedEncoder(p Profile) *Encoder {
	enc := NewEncoder(p, DefaultLimits(), 1)
	st := newCompressState(p)
	enc.states.New = func() any { return st }
	return enc
}

// TestEncoderHistoryIndependence drives a reused single-worker Encoder (one
// pinned compressState) through different histories and compares every
// output to package-level Encode and a fresh Encoder.
func TestEncoderHistoryIndependence(t *testing.T) {
	t.Parallel()

	large1, large2, small, tiny := detInputs(t)
	medium := detMedium(t)
	// Both size classes in both directions, plus a mixed repeated history.
	largeHistories := [][]detInput{
		{small, large1}, {large1, small},
		{large1, large2, large1}, {tiny, large2, small},
	}
	// q11 is slow on multi-MiB payloads; medium inputs keep it CI-sized.
	mediumHistories := [][]detInput{
		{small, medium, tiny},
	}

	for _, p := range []Profile{ProfileBalanced, ProfileCompact, ProfileMaximum} {
		histories := largeHistories
		if p == ProfileMaximum {
			histories = mediumHistories
		}
		t.Run(p.name(), func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			refs := map[string]encodedRef{}
			ref := func(in detInput) encodedRef {
				mu.Lock()
				defer mu.Unlock()
				if r, ok := refs[in.name]; ok {
					return r
				}
				r := referenceEncode(t, in, p)
				refs[in.name] = r
				return r
			}

			for _, history := range histories {
				var names []string
				for _, in := range history {
					names = append(names, in.name)
				}
				enc := newPinnedEncoder(p)
				for i, in := range history {
					blob, sel, err := enc.Encode(in.files)
					if err != nil {
						t.Fatalf("Encode(%s) error = %v", in.name, err)
					}
					checkEncoded(t, fmt.Sprintf("%v step %d (%s)", names, i, in.name), in, blob, sel, ref(in))
				}
				enc.Close()

				// A brand-new Encoder must agree with the reference too.
				last := history[len(history)-1]
				fresh := NewEncoder(p, DefaultLimits(), 1)
				blob, sel, err := fresh.Encode(last.files)
				if err != nil {
					t.Fatalf("fresh Encode(%s) error = %v", last.name, err)
				}
				checkEncoded(t, "fresh Encoder "+last.name, last, blob, sel, ref(last))
				fresh.Close()
			}
		})
	}
}

// TestEncoderConcurrentMixedInputs encodes different inputs concurrently
// through a multi-worker Encoder in differing orders; each result must equal
// the history-free reference.
func TestEncoderConcurrentMixedInputs(t *testing.T) {
	t.Parallel()

	large1, _, small, tiny := detInputs(t)
	inputs := []detInput{large1, small, detMedium(t), tiny}
	refs := make([]encodedRef, len(inputs))
	for i, in := range inputs {
		refs[i] = referenceEncode(t, in, ProfileCompact)
	}

	enc := NewEncoder(ProfileCompact, DefaultLimits(), 2)
	defer enc.Close()

	const goroutines = 4
	var wg sync.WaitGroup
	errs := make(chan string, goroutines*len(inputs))
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < len(inputs); k++ {
				i := (g + k) % len(inputs)
				blob, sel, err := enc.Encode(inputs[i].files)
				if err != nil {
					errs <- fmt.Sprintf("Encode(%s) error = %v", inputs[i].name, err)
					return
				}
				if sel != refs[i].sel || !bytes.Equal(blob, refs[i].blob) {
					errs <- fmt.Sprintf("goroutine %d: %s diverged from fresh encoding", g, inputs[i].name)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
