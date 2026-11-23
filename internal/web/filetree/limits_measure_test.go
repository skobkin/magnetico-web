package filetree

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/benchkit"
)

// TestMeasureComplexityLimits justifies DefaultLimits (explicit:
// MAGNETICO_BENCH_LARGE=1; MAGNETICO_W5_OUT2=<file> also writes the table). It
// builds the worst codec-valid hierarchies that stay inside the limits and
// records the index size, build time and allocation, then the cost of
// rejecting hierarchies just beyond them.
func TestMeasureComplexityLimits(t *testing.T) {
	if !benchkit.Enabled() {
		t.Skip("explicit measurement: set MAGNETICO_BENCH_LARGE=1")
	}
	type tc struct {
		name  string
		files func() []codec.File
	}
	uniqueDirs := func(n int) func() []codec.File { // one file per unique directory
		return func() []codec.File {
			fs := make([]codec.File, n)
			for i := range fs {
				fs[i] = codec.File{Path: fmt.Sprintf("d%07d/f", i), Size: 1}
			}
			return fs
		}
	}
	chains := func(nFiles, depth int) func() []codec.File { // each file in its own deep unique chain
		return func() []codec.File {
			fs := make([]codec.File, nFiles)
			for i := range fs {
				var b strings.Builder
				fmt.Fprintf(&b, "r%06d/", i)
				for d := 1; d < depth; d++ {
					b.WriteString("x/")
				}
				b.WriteString("f")
				fs[i] = codec.File{Path: b.String(), Size: 1}
			}
			return fs
		}
	}
	cases := []tc{
		{"250k-files-250k-dirs (max dirs)", uniqueDirs(250_000)},
		{"1k-files x 250-deep chains (250k dirs)", chains(1000, 250)},
		{"1953-files x 128-deep chains (250k dirs)", chains(1953, 128)},
		{"250k-files-in-one-256-deep-dir", func() []codec.File {
			p := strings.Repeat("d/", 256)
			fs := make([]codec.File, 250_000)
			for i := range fs {
				fs[i] = codec.File{Path: fmt.Sprintf("%sf%06d", p, i), Size: 1}
			}
			return fs
		}},
		{"REJECT 250001 dirs", uniqueDirs(250_001)},
		{"REJECT 1001-files x 250-deep (250.2k dirs)", chains(1001, 250)},
		{"REJECT 1 file 20000 deep", func() []codec.File {
			return []codec.File{{Path: strings.Repeat("a/", 20000) + "f", Size: 1}}
		}},
		{"REJECT 2000-files x 257-deep", chains(2000, 257)},
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n", benchkit.Describe())
	fmt.Fprintf(&out, "limits: MaxDirs=%d MaxDepth=%d\n", DefaultLimits.MaxDirs, DefaultLimits.MaxDepth)
	fmt.Fprintf(&out, "%-46s %9s %9s %12s %12s %10s  %s\n", "case", "files", "dirs", "index MiB", "alloc MiB", "build", "result")
	for _, c := range cases {
		files := c.files()
		probe := benchkit.StartProbe(2 * time.Millisecond)
		var a *Arena
		var err error
		al := benchkit.Measure(func() { a, err = BuildArena(context.Background(), files) })
		pk := probe.Stop()
		res, dirs, idxMiB := "ok", 0, 0.0
		if err != nil {
			res = err.Error()
		} else {
			dirs, idxMiB = a.Dirs()-1, float64(a.Bytes())/(1<<20)
		}
		fmt.Fprintf(&out, "%-46s %9d %9d %12.1f %12.1f %10s  %s (peak heap %.0f MiB)\n", c.name, len(files), dirs, idxMiB,
			float64(al.TotalAlloc)/(1<<20), al.Wall.Round(time.Millisecond), res, float64(pk.HeapAlloc)/(1<<20))
		a = nil
	}
	t.Log("\n" + out.String())
	if p := os.Getenv("MAGNETICO_W5_OUT2"); p != "" {
		if err := os.WriteFile(p, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
