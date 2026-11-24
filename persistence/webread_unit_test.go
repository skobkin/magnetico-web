package persistence

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
)

func TestStatementTimeoutBudget(t *testing.T) {
	ms := time.Millisecond
	cases := []struct {
		remaining time.Duration
		want      time.Duration
		ok        bool
	}{
		{10 * time.Second, 9500 * ms, true}, // margin capped at 500 ms
		{600 * ms, 540 * ms, true},          // margin = remaining/10
		{151 * ms, 101 * ms, true},          // smallest startable budget
		{150 * ms, 0, false},                // margin 50 ms + minStatement 100 ms
		{60 * ms, 0, false},
		{1, 0, false},
		{0, 0, false},
		{-time.Second, 0, false},
	}
	for _, c := range cases {
		got, ok := StatementTimeout(c.remaining)
		if ok != c.ok || got != c.want {
			t.Errorf("StatementTimeout(%v) = %v,%v want %v,%v", c.remaining, got, ok, c.want, c.ok)
		}
	}
	// Invariant: whenever a statement may start its timeout is positive and
	// strictly below the remaining budget.
	for r := time.Duration(-5) * ms; r < 3*time.Second; r += 7 * ms {
		if d, ok := StatementTimeout(r); ok && (d <= 0 || d >= r) {
			t.Fatalf("remaining %v: timeout %v not in (0, remaining)", r, d)
		}
	}
}

// A near-exhausted deadline must not start any statement: the Reader has no
// pool, so reaching the database would panic.
func TestReaderDoesNotStartStatementWithoutBudget(t *testing.T) {
	r := NewReader(nil, codec.DefaultLimits())
	for _, rem := range []time.Duration{150 * time.Millisecond, 40 * time.Millisecond, -time.Second} {
		ctx, cancel := context.WithTimeout(context.Background(), rem)
		_, err := r.TorrentByID(ctx, 1)
		cancel()
		if !errors.Is(err, ErrBudgetExhausted) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("remaining %v: got %v", rem, err)
		}
		cctx, ccancel := context.WithTimeout(context.Background(), rem)
		_, err = r.SearchTorrents(cctx, SearchSpec{Limit: 10})
		ccancel()
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Errorf("search remaining %v: got %v", rem, err)
		}
	}
}

var forbiddenSQL = regexp.MustCompile(`\bCOUNT\s*\(|\bOFFSET\b`)

func TestBuildSearchSQLShape(t *testing.T) {
	terms := []string{"ubun'tu; DROP TABLE x", "50%_off\\"}
	placeholder := regexp.MustCompile(`\$(\d+)`)
	for _, sort := range []SortKey{SortDiscovered, SortSize, SortName} {
		for _, desc := range []bool{false, true} {
			for _, back := range []bool{false, true} {
				for _, after := range []bool{false, true} {
					for _, expanded := range []bool{false, true} {
						spec := SearchSpec{Terms: terms, Sort: sort, Desc: desc, Backward: back, Limit: 50}
						if after {
							spec.After = &Anchor{ID: 7, Int: 99, Name: "n"}
						}
						sql, args, err := buildSearchSQL(spec, expanded)
						if err != nil {
							t.Fatal(err)
						}
						up := strings.ToUpper(sql)
						if forbiddenSQL.MatchString(up) {
							t.Errorf("COUNT/OFFSET in %q", sql)
						}
						if !strings.Contains(up, "LIMIT $") {
							t.Errorf("no bound LIMIT: %q", sql)
						}
						for _, term := range terms {
							if strings.Contains(sql, term) || strings.Contains(sql, "DROP") {
								t.Errorf("user text interpolated: %q", sql)
							}
						}
						max := 0
						for _, m := range placeholder.FindAllStringSubmatch(sql, -1) {
							var n int
							_, _ = fmt.Sscan(m[1], &n) // the capture is \d+: it always parses
							max = max2(max, n)
						}
						if max != len(args) {
							t.Errorf("highest placeholder %d != %d args: %q", max, len(args), sql)
						}
						if last := args[len(args)-1]; last != int64(51) {
							t.Errorf("limit arg = %v, want 51", last)
						}
						effDesc := desc != back
						wantDir := map[bool]string{true: "DESC", false: "ASC"}[effDesc]
						col, _ := sort.column()
						if !strings.Contains(sql, fmt.Sprintf("ORDER BY %s %s, id %s", col, wantDir, wantDir)) {
							t.Errorf("ordering wrong for desc=%v back=%v: %q", desc, back, sql)
						}
						wantOp := map[bool]string{true: "<", false: ">"}[effDesc]
						if after && !strings.Contains(sql, " "+wantOp+" ") {
							t.Errorf("operator %s missing: %q", wantOp, sql)
						}
					}
				}
			}
		}
	}
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func TestBuildSearchSQLRejectsInvalidSpecs(t *testing.T) {
	good := SearchSpec{Terms: []string{"abc"}, Limit: 20}
	bad := map[string]SearchSpec{
		"limit 0":     {Limit: 0},
		"limit 101":   {Limit: 101},
		"sort":        {Limit: 10, Sort: SortKey(9)},
		"too many":    {Limit: 10, Terms: make([]string, MaxSearchTerms+1)},
		"empty term":  {Limit: 10, Terms: []string{""}},
		"invalid utf": {Limit: 10, Terms: []string{"a\xffb"}},
		"nul":         {Limit: 10, Terms: []string{"a\x00b"}},
		"byid raw":    {Limit: 10, Sort: SortName, After: &Anchor{ID: 1, ByID: true}},
		"bad name":    {Limit: 10, Sort: SortName, After: &Anchor{ID: 1, Name: "\xff"}},
	}
	if _, _, err := BuildSearchSQL(good); err != nil {
		t.Fatal(err)
	}
	for n, s := range bad {
		if _, _, err := BuildSearchSQL(s); !errors.Is(err, ErrInvalidSearch) {
			t.Errorf("%s accepted: %v", n, err)
		}
	}
}

func TestSingleFileList(t *testing.T) {
	got, err := SingleFileList(Torrent{Name: "x", TotalSize: 5, FileCount: 1})
	if err != nil || len(got) != 1 || got[0] != (File{Path: "x", Size: 5}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := SingleFileList(Torrent{FileCount: 2}); !errors.Is(err, ErrIntegrityMissingBlob) {
		t.Fatalf("multi-file without blob: %v", err)
	}
}
