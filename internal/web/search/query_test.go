package search

import (
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestParseQueryWordsAndMinimumLength(t *testing.T) {
	cases := []struct {
		in      string
		terms   []string
		ignored int
	}{
		{"", nil, 0},
		{"   \t ", nil, 0},
		{"ubuntu 24 desktop", []string{"ubuntu", "desktop"}, 1},
		{"ab", nil, 1},                                  // 2 code points
		{"abc", []string{"abc"}, 0},                     // exactly 3
		{"日本", nil, 1},                                  // 2 code points (6 bytes): counted in code points
		{"日本語", []string{"日本語"}, 0},                     // 3 code points (9 bytes)
		{"é́", nil, 1},                                  // 2 code points though one grapheme
		{"Ubuntu UBUNTU ubuntu", []string{"Ubuntu"}, 0}, // case-insensitive de-duplication
		{"a bcd", []string{"bcd"}, 1},                   // NBSP separates (strings.Fields)
		{"foo\tbar\nbaz", []string{"foo", "bar", "baz"}, 0},
	}
	for _, c := range cases {
		q, err := ParseQuery(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if !slices.Equal(q.Terms, c.terms) || q.Ignored != c.ignored || q.InfoHash != nil {
			t.Errorf("%q: terms %q ignored %d hash %v; want %q %d", c.in, q.Terms, q.Ignored, q.InfoHash, c.terms, c.ignored)
		}
		if c.terms == nil && !q.Empty() {
			t.Errorf("%q: should be an empty (newest) query", c.in)
		}
	}
}

func TestParseQueryInfoHash(t *testing.T) {
	h := "0123456789abcdef0123456789ABCDEF01234567"
	q, err := ParseQuery("  " + h + "\n")
	if err != nil || hex.EncodeToString(q.InfoHash) != strings.ToLower(h) || len(q.Terms) != 0 {
		t.Fatalf("hash query: %+v %v", q, err)
	}
	for _, in := range []string{
		h[:39],                  // too short: an ordinary term
		h + "0",                 // 41
		strings.Repeat("g", 40), // 40 but not hex
		h + " ubuntu",           // hash plus another word: ordinary terms
	} {
		q, err := ParseQuery(in)
		if err != nil || q.InfoHash != nil {
			t.Errorf("%q: unexpected hash lookup %+v %v", in, q, err)
		}
	}
}

func TestParseQueryRejections(t *testing.T) {
	for name, in := range map[string]string{
		"invalid utf-8": "ab\xffcd",
		"nul":           "abc\x00def",
		"too long":      strings.Repeat("a", MaxQueryBytes+1),
		"too many":      "aaa bbb ccc ddd eee fff ggg hhh iii",
	} {
		if _, err := ParseQuery(in); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseQuery(strings.Repeat("a", MaxQueryBytes)); err != nil {
		t.Errorf("exactly the limit must be accepted: %v", err)
	}
	if _, err := ParseQuery("aaa bbb ccc ddd eee fff ggg hhh"); err != nil {
		t.Errorf("exactly %d terms must be accepted: %v", MaxTerms, err)
	}
	// Duplicates do not count towards the limit.
	if _, err := ParseQuery(strings.Repeat("aaa ", 20)); err != nil {
		t.Errorf("duplicates: %v", err)
	}
}

func TestParseParamsFallbacks(t *testing.T) {
	p, err := ParseParams("ubuntu", "", "", "", "")
	if err != nil || p.Sort != SortDiscovered || !p.Desc || p.PageSize != 50 {
		t.Fatalf("defaults: %+v %v", p, err)
	}
	for in, want := range map[string]struct {
		s Sort
		d bool
	}{
		"size": {SortSize, true}, "SIZE": {SortSize, true}, "name": {SortName, false}, "bogus": {SortDiscovered, true},
	} {
		p, _ := ParseParams("ubuntu", in, "", "", "")
		if p.Sort != want.s || p.Desc != want.d {
			t.Errorf("sort=%q: %v desc=%v, want %v %v", in, p.Sort, p.Desc, want.s, want.d)
		}
	}
	// name needs a usable term.
	if p, _ := ParseParams("", "name", "", "", ""); p.Sort != SortDiscovered || !p.Desc {
		t.Errorf("name without terms: %+v", p)
	}
	if p, _ := ParseParams("ab", "name", "", "", ""); p.Sort != SortDiscovered {
		t.Errorf("name with only ignored words: %+v", p)
	}
	// both directions are supported; unknown order uses the default.
	if p, _ := ParseParams("ubuntu", "size", "asc", "", ""); p.Desc {
		t.Error("order=asc ignored")
	}
	if p, _ := ParseParams("ubuntu", "name", "desc", "", ""); !p.Desc {
		t.Error("order=desc ignored")
	}
	if p, _ := ParseParams("ubuntu", "name", "sideways", "", ""); p.Desc {
		t.Error("unknown order must use the default")
	}
	for in, want := range map[string]int{"20": 20, "50": 50, "100": 100, "30": 50, "": 50, "x": 50, "-1": 50, "1000": 50} {
		if p, _ := ParseParams("", "", "", in, ""); p.PageSize != want {
			t.Errorf("size=%q → %d, want %d", in, p.PageSize, want)
		}
	}
	// An info-hash lookup ignores sort, order and cursor.
	p, _ = ParseParams(strings.Repeat("ab", 20), "size", "asc", "100", "somecursor")
	if p.Query.InfoHash == nil || p.Sort != SortDiscovered || p.Cursor != "" {
		t.Errorf("hash params: %+v", p)
	}
	if _, err := ParseParams("a\xff", "", "", "", ""); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("invalid query accepted: %v", err)
	}
}
