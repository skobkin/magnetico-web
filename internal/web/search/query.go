// Package search is the web search service: query parsing, opaque keyset
// cursors and the gated, time-bounded search call (docs/wip/07E-web-search-
// details-plan.md §3.2–3.4, §5). It holds no HTTP and no SQL.
package search

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Limits of a query (owner decision O6; the spec does not bound them).
const (
	MaxQueryBytes = 512
	MaxTerms      = 8
	// MinTermRunes is the shortest usable word, counted in Unicode code points
	// (spec §6.2.1).
	MinTermRunes = 3
)

// ErrInvalidQuery is wrapped by every query rejection. The query is never
// "repaired" (spec §6.2): invalid input is refused with a reason.
var ErrInvalidQuery = errors.New("invalid search query")

// Reasons, each matching ErrInvalidQuery.
var (
	ErrQueryEncoding = fmt.Errorf("%w: not valid UTF-8 text", ErrInvalidQuery)
	ErrQueryTooLong  = fmt.Errorf("%w: query is too long", ErrInvalidQuery)
	ErrTooManyTerms  = fmt.Errorf("%w: too many search words", ErrInvalidQuery)
)

// Query is a parsed search string.
type Query struct {
	// Terms are the usable words (at least MinTermRunes code points), in the
	// typed order, de-duplicated case-insensitively. All must match (AND).
	Terms []string
	// Ignored counts words dropped for being shorter than MinTermRunes.
	Ignored int
	// InfoHash is set when the whole query is exactly 40 hexadecimal
	// characters: an exact info-hash lookup.
	InfoHash []byte
}

// Empty reports that the query selects the newest torrents (no usable word and
// no info hash).
func (q Query) Empty() bool { return len(q.Terms) == 0 && q.InfoHash == nil }

// ParseQuery parses the user's search text. Words are separated by Unicode
// white space (strings.Fields); a query that, trimmed, is exactly 40
// hexadecimal characters is an info-hash lookup.
func ParseQuery(s string) (Query, error) {
	if len(s) > MaxQueryBytes {
		return Query{}, ErrQueryTooLong
	}
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return Query{}, ErrQueryEncoding
	}
	trimmed := strings.TrimSpace(s)
	if len(trimmed) == 40 {
		if h, err := hex.DecodeString(trimmed); err == nil {
			return Query{InfoHash: h}, nil
		}
	}
	var q Query
	seen := map[string]struct{}{}
	for _, w := range strings.Fields(s) {
		if utf8.RuneCountInString(w) < MinTermRunes {
			q.Ignored++
			continue
		}
		key := strings.ToLower(w)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		q.Terms = append(q.Terms, w)
	}
	if len(q.Terms) > MaxTerms {
		return Query{}, ErrTooManyTerms
	}
	return q, nil
}
