package search

import (
	"strconv"
	"strings"

	"github.com/skobkin/magnetico-web/persistence"
)

// Sort is the user-visible sort selection.
type Sort int

const (
	SortDiscovered Sort = iota // default
	SortSize
	SortName // only with at least one usable term
)

func (s Sort) String() string {
	switch s {
	case SortSize:
		return "size"
	case SortName:
		return "name"
	}
	return "discovered"
}

func (s Sort) persistence() persistence.SortKey {
	switch s {
	case SortSize:
		return persistence.SortSize
	case SortName:
		return persistence.SortName
	}
	return persistence.SortDiscovered
}

// Page sizes (spec §6.3.2).
var PageSizes = [...]int{20, 50, 100}

// DefaultPageSize is used for a missing or unsupported size.
const DefaultPageSize = 50

// Params is a normalised search request: unknown values fall back to
// defaults (spec §6.3.1) and are never errors, except the query and cursor,
// which are validated (ErrInvalidQuery / ErrInvalidCursor).
type Params struct {
	Query    Query
	Sort     Sort
	Desc     bool
	PageSize int
	Cursor   string // opaque; "" = first page
}

// ParseParams normalises the raw request parameters. sort is "discovered",
// "size" or "name"; order is "asc" or "desc"; size one of PageSizes.
//
//   - sort=name without a usable term falls back to the default sort;
//   - the default direction is descending for discovered and size, ascending
//     for name; any unknown order uses it;
//   - an info-hash lookup ignores sort, order and cursor (zero or one row).
func ParseParams(q, sort, order, size, cursor string) (Params, error) {
	query, err := ParseQuery(q)
	if err != nil {
		return Params{}, err
	}
	p := Params{Query: query, PageSize: DefaultPageSize, Cursor: cursor}
	switch strings.ToLower(sort) {
	case "size":
		p.Sort = SortSize
	case "name":
		if len(query.Terms) > 0 {
			p.Sort = SortName
		}
	}
	p.Desc = p.Sort != SortName
	switch strings.ToLower(order) {
	case "asc":
		p.Desc = false
	case "desc":
		p.Desc = true
	}
	if n, err := strconv.Atoi(size); err == nil {
		for _, ok := range PageSizes {
			if n == ok {
				p.PageSize = n
			}
		}
	}
	if query.InfoHash != nil {
		p.Sort, p.Desc, p.Cursor = SortDiscovered, true, ""
	}
	return p, nil
}
