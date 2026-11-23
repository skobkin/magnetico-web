package fileview

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/benchkit"
	"github.com/skobkin/magnetico-web/internal/web/filetree"
	"github.com/skobkin/magnetico-web/internal/web/synth"
)

// TestMeasureTreeSize is the 07F threshold measurement (explicit:
// MAGNETICO_BENCH_LARGE=1; MAGNETICO_W5_OUT=<file> also writes the table). For
// each shape it records the exact full-tree JSON size, the serialization
// time and allocations, the declared payload length of the stored blob, the
// cheap Estimate and the ratio actual/estimate, to calibrate the pre-check
// and to show where it cannot predict the exact size.
func TestMeasureTreeSize(t *testing.T) {
	if !benchkit.Enabled() {
		t.Skip("explicit measurement: set MAGNETICO_BENCH_LARGE=1")
	}
	type tc struct {
		name string
		spec synth.Spec
	}
	cases := []tc{
		{"iso-collection-40", synth.Spec{Shape: synth.Flat, Files: 40, Seed: 21, AvgName: 48}},
		{"discography-300", synth.Spec{Shape: synth.Typical, Files: 300, Seed: 22, AvgName: 36}},
		{"series-2400", synth.Spec{Shape: synth.Typical, Files: 2400, Seed: 23, AvgName: 40}},
		{"typical-4k", synth.Spec{Shape: synth.Typical, Files: 4000, Seed: 24}},
		{"typical-8k", synth.Spec{Shape: synth.Typical, Files: 8000, Seed: 25}},
		{"typical-12k", synth.Spec{Shape: synth.Typical, Files: 12000, Seed: 26}},
		{"typical-20k", synth.Spec{Shape: synth.Typical, Files: 20000, Seed: 27}},
		{"typical-50k", synth.Spec{Shape: synth.Typical, Files: 50000, Seed: 28}},
		{"typical-250k", synth.Spec{Shape: synth.Typical, Files: 250000, Seed: 4}},
		{"flat-12k", synth.Spec{Shape: synth.Flat, Files: 12000, Seed: 29}},
		{"wide-12k", synth.Spec{Shape: synth.Wide, Files: 12000, Seed: 30}},
		{"deep-12k", synth.Spec{Shape: synth.Deep, Files: 12000, Seed: 31}},
		{"deepchain-12k", synth.Spec{Shape: synth.DeepChain, Files: 12000, Seed: 32}},
		{"longpath-8k", synth.Spec{Shape: synth.LongPath, Files: 8000, Seed: 33}},
		{"longpath-12k", synth.Spec{Shape: synth.LongPath, Files: 12000, Seed: 34}},
		{"adversarial-12k", synth.Spec{Shape: synth.Adversarial, Files: 12000, Seed: 35}},
		{"longname-1500", synth.Spec{Shape: synth.LongName, Files: 1500, Seed: 10}},
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n", benchkit.Describe())
	fmt.Fprintf(&out, "%-18s %7s %8s %9s %10s %10s %7s %9s %9s %10s %7s\n",
		"case", "files", "dirs", "stored", "declared", "estimate", "ratio", "json", "json/file", "encode", "allocs")
	for _, c := range cases {
		files := synth.Generate(c.spec)
		blob, _, err := codec.Encode(files, codec.ProfileCompact, codec.DefaultLimits())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		_, declared, err := codec.PeekDecodedSize(blob[:min(len(blob), codec.PeekHeaderLen)], len(blob), codec.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		idx, err := filetree.BuildArena(context.Background(), files)
		if err != nil {
			fmt.Fprintf(&out, "%-18s %7d  index: %v\n", c.name, len(files), err)
			continue
		}
		var size int
		var best time.Duration
		var alloc benchkit.Alloc
		for i := 0; i < 3; i++ {
			w := &jsonBuf{b: make([]byte, 0, 1<<20), max: 1 << 30}
			a := benchkit.Measure(func() { _ = encodeTree(context.Background(), w, idx) })
			size = len(w.b)
			if best == 0 || a.Wall < best {
				best, alloc = a.Wall, a
			}
		}
		est := Estimate(len(files), declared)
		fmt.Fprintf(&out, "%-18s %7d %8d %9d %10d %10d %7.2f %9d %9.1f %10s %7d\n", c.name, len(files), idx.Dirs(),
			len(blob), declared, est, float64(size)/float64(est), size, float64(size)/float64(len(files)),
			best.Round(time.Microsecond), alloc.Mallocs)
	}
	t.Log("\n" + out.String())
	if p := os.Getenv("MAGNETICO_W5_OUT"); p != "" {
		if err := os.WriteFile(p, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
