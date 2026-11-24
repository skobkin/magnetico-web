package persistence

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"
)

// Keyset pagination while new torrents arrive. Offsets would shift under
// inserts (rows skipped or repeated); a keyset anchor must not. For every sort
// and direction, a forward walk and a backward walk run while rows (with ties
// on every key, in front of, behind and between the pages) are inserted after
// each page request. Invariants: no id is returned twice, every row that
// existed when the walk started is returned exactly once, and the returned rows
// follow the traversal order of the final table (a row that sorts before the
// anchor is simply not part of a forward walk, and is never smuggled into the
// middle of it).
func TestKeysetWalkWhileTorrentsAreInserted(t *testing.T) {
	ctx := context.Background()
	const page = 6
	for _, sort := range []SortKey{SortDiscovered, SortSize, SortName} {
		for _, desc := range []bool{false, true} {
			for _, byID := range []bool{false, true} {
				t.Run(fmt.Sprintf("sort%d/desc=%v/byID=%v", sort, desc, byID), func(t *testing.T) {
					e := newReaderEnv(t)
					rng := rand.New(rand.NewSource(int64(sort)*100 + map[bool]int64{false: 1, true: 2}[desc]))
					insert := func() int32 {
						name := fmt.Sprintf("Walk %c%d", 'a'+rune(rng.Intn(5)), rng.Intn(4))
						return e.seed(t, name, int64(rng.Intn(6)+1), int64(100+rng.Intn(8)), 1)
					}
					original := map[int32]bool{}
					for i := 0; i < 40; i++ {
						original[insert()] = true
					}
					base := SearchSpec{Terms: []string{"walk"}, Sort: sort, Desc: desc, Limit: page}

					check := func(dir string, walked []int32) {
						t.Helper()
						seen := map[int32]bool{}
						for _, id := range walked {
							if seen[id] {
								t.Fatalf("%s walk: id %d returned twice", dir, id)
							}
							seen[id] = true
						}
						for id := range original {
							if !seen[id] {
								t.Fatalf("%s walk: pre-existing row %d was skipped", dir, id)
							}
						}
						// Traversal order of the final table.
						pos := map[int32]int{}
						for i, x := range e.referenceOrder(t, sort, desc) {
							pos[x.ID] = i
						}
						for i := 1; i < len(walked); i++ {
							if pos[walked[i-1]] >= pos[walked[i]] {
								t.Fatalf("%s walk: rows %d and %d are out of order", dir, walked[i-1], walked[i])
							}
						}
					}

					// Forward: rows arrive between every two pages.
					var fwd []int32
					var lastPage []Torrent
					var after *Anchor
					for n := 0; ; n++ {
						spec := base
						spec.After = after
						p, err := e.r.SearchTorrents(ctx, spec)
						if err != nil {
							t.Fatal(err)
						}
						fwd = append(fwd, ids(p.Items)...)
						lastPage = p.Items
						insert()
						insert()
						if !p.Extra {
							break
						}
						if n > 60 {
							t.Fatal("the forward walk does not terminate")
						}
						after = anchorOf(sort, p.Items[len(p.Items)-1], byID)
					}
					check("forward", fwd)

					// Backward from the end of the final table.
					var back []int32
					cur := lastPage
					back = append(slices.Clone(ids(cur)), back...)
					for n := 0; ; n++ {
						spec := base
						spec.Backward = true
						spec.After = anchorOf(sort, cur[0], byID)
						p, err := e.r.SearchTorrents(ctx, spec)
						if err != nil {
							t.Fatal(err)
						}
						back = append(slices.Clone(ids(p.Items)), back...)
						insert()
						insert()
						if !p.Extra {
							break
						}
						cur = p.Items
						if n > 80 {
							t.Fatal("the backward walk does not terminate")
						}
					}
					// The backward walk starts at the last page of the first walk, so
					// rows inserted since then may exist beyond it; the invariants are
					// no duplicates and monotonic order, and no row that existed at
					// the start of THIS walk and sorts before the start is skipped.
					seen := map[int32]bool{}
					for _, id := range back {
						if seen[id] {
							t.Fatalf("backward walk: id %d returned twice", id)
						}
						seen[id] = true
					}
					pos := map[int32]int{}
					for i, x := range e.referenceOrder(t, sort, desc) {
						pos[x.ID] = i
					}
					for i := 1; i < len(back); i++ {
						if pos[back[i-1]] >= pos[back[i]] {
							t.Fatalf("backward walk: rows %d and %d are out of order", back[i-1], back[i])
						}
					}
					for id := range original {
						if pos[id] <= pos[back[len(back)-1]] && !seen[id] {
							t.Fatalf("backward walk: pre-existing row %d before the start was skipped", id)
						}
					}
				})
			}
		}
	}
}
