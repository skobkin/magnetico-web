package search

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/gate"
	"github.com/skobkin/magnetico-web/persistence"
)

// Outcome errors of Service.Search. A client cancellation is returned as
// context.Canceled (nothing to render).
var (
	// ErrBusy: all search slots are taken and the queue is full or the wait
	// expired. Distinct from a timeout and from an invalid query.
	ErrBusy = errors.New("search is busy")
	// ErrTimeout: the search did not finish in the configured time (queue wait
	// plus SQL share one budget), or too little of it remained to start.
	ErrTimeout = errors.New("search timed out")
)

// Store is what the service needs from persistence.Reader.
type Store interface {
	SearchTorrents(ctx context.Context, spec persistence.SearchSpec) (persistence.SearchPage, error)
	TorrentByInfoHash(ctx context.Context, infoHash []byte) (persistence.Torrent, bool, error)
}

var _ Store = (*persistence.Reader)(nil)

// Config configures a Service.
type Config struct {
	Concurrency int           // search slots (SEARCH_CONCURRENCY)
	QueueWait   time.Duration // bounded queue wait (default 5 s)
	MaxWaiters  int           // queue length (default 4 × Concurrency)
}

// Default queue constants (plan §5.3).
const (
	DefaultQueueWait = 5 * time.Second
	waitersPerSlot   = 4
)

// Service runs searches through the gate.
type Service struct {
	store Store
	gate  *gate.Gate

	ok, busy, timeouts, canceled, invalidCursor, failed atomic.Int64
}

// New returns a Service.
func New(store Store, cfg Config) (*Service, error) {
	if store == nil || cfg.Concurrency < 1 {
		return nil, errors.New("search: a store and at least one search slot are required")
	}
	if cfg.QueueWait <= 0 {
		cfg.QueueWait = DefaultQueueWait
	}
	if cfg.MaxWaiters <= 0 {
		cfg.MaxWaiters = waitersPerSlot * cfg.Concurrency
	}
	g, err := gate.New(int64(cfg.Concurrency), cfg.MaxWaiters, cfg.QueueWait)
	if err != nil {
		return nil, err
	}
	return &Service{store: store, gate: g}, nil
}

// Result is one page of results in display order.
type Result struct {
	Items []persistence.Torrent
	// Next and Prev are opaque cursors ("" = no such link).
	Next, Prev string
	// Lookup: the query was an info-hash lookup (no paging).
	Lookup bool
}

// Stats are cumulative counters by outcome (for tests and later metrics).
type Stats struct{ OK, Busy, Timeout, Canceled, InvalidCursor, Failed int64 }

// Stats returns the outcome counters.
func (s *Service) Stats() Stats {
	return Stats{s.ok.Load(), s.busy.Load(), s.timeouts.Load(), s.canceled.Load(), s.invalidCursor.Load(), s.failed.Load()}
}

// Outcome names an error for structured logs (never includes query text).
func Outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrBusy):
		return "busy"
	case errors.Is(err, ErrTimeout):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrInvalidCursor), errors.Is(err, ErrCursorExpired):
		return "invalid_cursor"
	case errors.Is(err, ErrInvalidQuery):
		return "invalid_query"
	}
	return "error"
}

// Search runs one page. The request context's deadline is the whole budget:
// the queue wait and the SQL statement share it, and the statement is not
// started when too little remains (persistence.ErrBudgetExhausted).
func (s *Service) Search(ctx context.Context, p Params) (Result, error) {
	res, err := s.search(ctx, p)
	switch Outcome(err) {
	case "ok":
		s.ok.Add(1)
	case "busy":
		s.busy.Add(1)
	case "timeout":
		s.timeouts.Add(1)
	case "canceled":
		s.canceled.Add(1)
	case "invalid_cursor":
		s.invalidCursor.Add(1)
	default:
		s.failed.Add(1)
	}
	return res, err
}

func (s *Service) search(ctx context.Context, p Params) (Result, error) {
	// Validate the cursor before taking a slot: a bad link costs nothing.
	var cur *Cursor
	if p.Query.InfoHash == nil && p.Cursor != "" {
		c, err := DecodeCursor(p, p.Cursor)
		if err != nil {
			return Result{}, err
		}
		cur = &c
	}

	release, err := s.gate.Acquire(ctx, 1)
	if err != nil {
		return Result{}, mapError(err)
	}
	defer release()

	if p.Query.InfoHash != nil {
		t, found, err := s.store.TorrentByInfoHash(ctx, p.Query.InfoHash)
		if err != nil {
			return Result{}, mapError(err)
		}
		res := Result{Lookup: true}
		if found {
			res.Items = []persistence.Torrent{t}
		}
		return res, nil
	}

	spec := persistence.SearchSpec{
		Terms: p.Query.Terms, Sort: p.Sort.persistence(), Desc: p.Desc, Limit: p.PageSize,
	}
	if cur != nil {
		spec.Backward = cur.Backward
		spec.After = &persistence.Anchor{ID: cur.ID, Int: cur.Int, Name: cur.Name, ByID: cur.ByID}
	}
	page, err := s.store.SearchTorrents(ctx, spec)
	if err != nil {
		return Result{}, mapError(err)
	}
	if cur != nil && len(page.Items) == 0 {
		// A stale cursor (rows deleted around it): restart from the first page
		// rather than showing an empty page with no way on.
		spec.After, spec.Backward = nil, false
		cur = nil
		if page, err = s.store.SearchTorrents(ctx, spec); err != nil {
			return Result{}, mapError(err)
		}
	}
	return s.links(p, cur, page), nil
}

// links derives Next/Previous from the traversal direction (plan §3.4):
//
//	forward or first page: Next = extra row,      Previous = an input cursor existed
//	backward:              Next = input cursor,    Previous = extra row
func (s *Service) links(p Params, cur *Cursor, page persistence.SearchPage) Result {
	res := Result{Items: page.Items}
	if len(page.Items) == 0 {
		return res
	}
	var hasNext, hasPrev bool
	if cur != nil && cur.Backward {
		hasNext, hasPrev = true, page.Extra
	} else {
		hasNext, hasPrev = page.Extra, cur != nil
	}
	if hasNext {
		res.Next = EncodeCursor(p, cursorOf(p, page.Items[len(page.Items)-1], false))
	}
	if hasPrev {
		res.Prev = EncodeCursor(p, cursorOf(p, page.Items[0], true))
	}
	return res
}

func cursorOf(p Params, t persistence.Torrent, backward bool) Cursor {
	c := Cursor{Backward: backward, ID: t.ID}
	switch p.Sort {
	case SortSize:
		c.Int = t.TotalSize
	case SortName:
		c.Name = t.Name
	default:
		c.Int = t.DiscoveredOn
	}
	return c
}

// mapError classifies a lower-layer error into the service's outcomes.
func mapError(err error) error {
	switch {
	case errors.Is(err, gate.ErrBusy):
		return ErrBusy
	case errors.Is(err, persistence.ErrAnchorNotFound):
		return fmt.Errorf("%w", ErrCursorExpired)
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, persistence.ErrStatementTimeout), errors.Is(err, persistence.ErrBudgetExhausted),
		errors.Is(err, context.DeadlineExceeded):
		return ErrTimeout
	}
	return err
}
