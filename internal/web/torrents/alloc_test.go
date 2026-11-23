package torrents

import (
	"fmt"
	"testing"

	"github.com/skobkin/magnetico-web/internal/web/benchkit"
	"github.com/skobkin/magnetico-web/internal/web/synth"
)

// TestWarmNavigationAllocations measures allocations of one warm directory
// page of a 250,000-file torrent through the full handler stack including the
// real FileIdentity round trip to PostgreSQL. Explicit (MAGNETICO_BENCH_LARGE=1).
func TestWarmNavigationAllocations(t *testing.T) {
	if !benchkit.Enabled() {
		t.Skip("explicit measurement: set MAGNETICO_BENCH_LARGE=1")
	}
	files := synth.Generate(synth.Spec{Name: "t", Shape: synth.Typical, Files: 250_000, Seed: 4})
	e := newEnv(t)
	id := e.seed("alloc", 0, 1, files)
	c := e.signedIn()
	root, code := c.dir(id, "limit=1000")
	if code != 200 {
		t.Fatal(code)
	}
	var dirs []string
	for _, en := range root.Entries {
		if en.Type == "dir" {
			dirs = append(dirs, en.Path)
		}
	}
	if len(dirs) == 0 {
		t.Fatal("no directories")
	}
	const n = 1000
	get := func(p string) { c.get(fmt.Sprintf("/torrents/%d/files?dir=%s", id, p)) }
	for i := 0; i < 50; i++ {
		get(dirs[i%len(dirs)])
	}
	a := benchkit.Measure(func() {
		for i := 0; i < n; i++ {
			get(dirs[i%len(dirs)])
		}
	})
	t.Logf("%d navigations: %.0f allocs/nav, %.1f KiB/nav (alloc %+v)", n,
		float64(a.Mallocs)/n, float64(a.TotalAlloc)/n/1024, a)
}
