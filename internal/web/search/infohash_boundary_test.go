package search

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/persistence"
)

// The exact info-hash lookup activates only when the whole trimmed query is
// exactly 40 hexadecimal characters. Every other query is an ordinary name
// search: one SearchTorrents call, no info-hash lookup, whatever it looks like.
// (The SQL side of the same property, no info-hash predicate and no extra
// statement, is TestNameSearchSQLHasNoInfoHashPredicate in the persistence
// package.)
func TestOnlyAnExactTrimmedFortyHexQueryTakesTheLookupPath(t *testing.T) {
	const h = "0123456789abcdef0123456789abcdef01234567"
	hash := []string{
		h, strings.ToUpper(h), "0123456789ABCDEF0123456789abcdef01234567",
		"  " + h, h + "  ", "\t" + h + "\n", "\u00a0" + h + "\u3000", // Unicode white space is trimmed
	}
	ordinary := []string{
		h[:39], h + "0", strings.Repeat("g", 40), strings.Repeat("0", 39) + "g",
		h + " ubuntu", "ubuntu " + h, h[:20] + " " + h[20:], // a space inside, or another word
		"０１２３４５６７８９ａｂｃｄｅｆ０１２３４５６７８９ａｂｃｄｅｆ０１２３４５６７", // full-width digits are not hex
		"0x" + h[:38], "sha1:" + h[:35], "urn:btih:" + h, "magnet:?xt=urn:btih:" + h,
		"ubuntu", "ubu", "",
	}
	for _, q := range hash {
		var asked [][]byte
		st := &fakeStore{hash: func(b []byte) (persistence.Torrent, bool, error) {
			asked = append(asked, b)
			return persistence.Torrent{}, false, nil
		}}
		p, err := ParseParams(q, "", "", "", "")
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		res, err := newSvc(t, st, 1, time.Second).Search(context.Background(), p)
		if err != nil || !res.Lookup || len(asked) != 1 || st.callCount() != 0 {
			t.Errorf("%q: lookup=%v asked=%d searches=%d err=%v; want exactly one info-hash lookup and no search", q, res.Lookup, len(asked), st.callCount(), err)
		}
	}
	for _, q := range ordinary {
		var asked int
		st := &fakeStore{hash: func([]byte) (persistence.Torrent, bool, error) {
			asked++
			return persistence.Torrent{}, false, nil
		}}
		p, err := ParseParams(q, "", "", "", "")
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		res, err := newSvc(t, st, 1, time.Second).Search(context.Background(), p)
		if err != nil || res.Lookup || asked != 0 || st.callCount() != 1 {
			t.Errorf("%q: lookup=%v asked=%d searches=%d err=%v; want one search and no info-hash lookup", q, res.Lookup, asked, st.callCount(), err)
		}
	}
}
