package synth

import (
	"slices"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/codec"
)

func TestGenerateDeterministicCanonicalWithinLimits(t *testing.T) {
	lim := codec.DefaultLimits()
	for _, sp := range Corpus() {
		if sp.Files > 20_000 {
			sp.Files = 20_000 // keep the ordinary suite fast; limits scale up linearly
		}
		a, b := Generate(sp), Generate(sp)
		if !slices.Equal(a, b) {
			t.Fatalf("%s: not deterministic", sp.Name)
		}
		if len(a) < sp.Files {
			t.Fatalf("%s: %d files, want at least %d", sp.Name, len(a), sp.Files)
		}
		for i := 1; i < len(a); i++ {
			c := strings.Compare(a[i-1].Path, a[i].Path)
			if c > 0 || (c == 0 && a[i-1].Size > a[i].Size) {
				t.Fatalf("%s: not canonical at %d", sp.Name, i)
			}
		}
		st := Measure(a)
		if st.MaxPath > lim.MaxPathBytes || st.PathBytes > lim.MaxAggregatePathBytes {
			t.Fatalf("%s: outside codec limits: %+v", sp.Name, st)
		}
		if _, _, err := codec.Encode(a, codec.ProfileBalanced, lim); err != nil {
			t.Fatalf("%s: encode: %v", sp.Name, err)
		}
	}
}

func TestCorpusFullSizeStaysWithinLimits(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	lim := codec.DefaultLimits()
	for _, sp := range Corpus() {
		st := Measure(Generate(sp))
		if st.Files > lim.MaxFiles || st.MaxPath > lim.MaxPathBytes || st.PathBytes > lim.MaxAggregatePathBytes {
			t.Fatalf("%s: %+v exceeds the default limits", sp.Name, st)
		}
		t.Logf("%-16s files=%-7d pathBytes=%-9d maxPath=%-6d depth=%d", sp.Name, st.Files, st.PathBytes, st.MaxPath, st.MaxDepth)
	}
}

func TestShapesHaveTheirCharacter(t *testing.T) {
	flat := Measure(Generate(Spec{Shape: Flat, Files: 1000, Seed: 1}))
	if flat.MaxDepth != 0 {
		t.Fatalf("flat has directories: %+v", flat)
	}
	deep := Measure(Generate(Spec{Shape: Deep, Files: 20000, Seed: 1}))
	if deep.MaxDepth < 8 {
		t.Fatalf("deep too shallow: %+v", deep)
	}
	chain := Measure(Generate(Spec{Shape: DeepChain, Files: 5000, Seed: 1}))
	if chain.MaxDepth < 100 {
		t.Fatalf("deepchain depth %d", chain.MaxDepth)
	}
	long := Measure(Generate(Spec{Shape: LongPath, Files: 2000, Seed: 1}))
	if long.PathBytes/long.Files < 200 {
		t.Fatalf("longpath average path %d", long.PathBytes/long.Files)
	}
	ln := Measure(Generate(Spec{Shape: LongName, Files: 100, Seed: 1}))
	if ln.MaxPath < 20_000 {
		t.Fatalf("longname max path %d", ln.MaxPath)
	}
	adv := Generate(Spec{Shape: Adversarial, Files: 5000, Seed: 1})
	var dup, invalid, empty, lead, nul bool
	for i, f := range adv {
		if i > 0 && adv[i-1] == f {
			dup = true
		}
		invalid = invalid || !validUTF8(f.Path)
		empty = empty || strings.Contains(f.Path, "//")
		lead = lead || strings.HasPrefix(f.Path, "/")
		nul = nul || strings.Contains(f.Path, "\x00")
	}
	if !dup || !invalid || !empty || !lead || !nul {
		t.Fatalf("adversarial coverage dup=%v invalid=%v empty=%v lead=%v nul=%v", dup, invalid, empty, lead, nul)
	}
}

func validUTF8(s string) bool { return strings.ToValidUTF8(s, "") == s }

func TestTorrentsIndependentAndDeterministic(t *testing.T) {
	base := Spec{Name: "t", Shape: Typical, Files: 400, Seed: 42}
	a, b := Torrents(base, 6), Torrents(base, 6)
	if !slices.Equal(a, b) {
		t.Fatal("not deterministic")
	}
	seen := map[uint64]bool{}
	sizes := map[int]bool{}
	for _, s := range a {
		if seen[s.Seed] {
			t.Fatal("duplicate seed")
		}
		seen[s.Seed] = true
		sizes[s.Files] = true
		if s.Files < 200 || s.Files > 600 {
			t.Fatalf("file count %d outside ±50%%", s.Files)
		}
	}
	if len(sizes) < 3 {
		t.Fatalf("file counts hardly vary: %v", sizes)
	}
	if slices.Equal(Generate(a[0]), Generate(a[1])) {
		t.Fatal("torrents are identical")
	}
}
