package stats

import (
	"context"
	"time"

	"github.com/skobkin/magnetico-web/internal/opstats"
	"github.com/skobkin/magnetico-web/internal/web/admin/access"
)

// Service is the cached, coalescing entry point of the administrator section.
// Every method takes the Principal the guard produced and refuses an invalid
// one; no result depends on who asks, so nothing per-user is ever cached.
type Service struct {
	r   *Reader
	now func() time.Time

	overview *cache[struct{}, Overview]
	series   *cache[Query, Series]
	bounds   *cache[string, [2]time.Time] // earliest and latest bucket per granularity; zero time: none
	probe    *cache[struct{}, IngestStatus]
	crawlers *cache[Cursor, CrawlerPage]
}

// Option adjusts a Service (tests).
type Option func(*Service)

// WithClock replaces the clock used for cache lifetimes and open-bucket detection.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// NewService returns a Service reading through db (the web role's pool).
func NewService(db opstats.DB, opts ...Option) *Service {
	s := &Service{r: &Reader{db: db}, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	s.overview = newCache[struct{}, Overview](SnapshotTTL, 1, s.now)
	s.series = newCache[Query, Series](SeriesTTL, SeriesCacheEntries, s.now)
	s.bounds = newCache[string, [2]time.Time](SeriesTTL, 3, s.now)
	s.probe = newCache[struct{}, IngestStatus](SeriesTTL, 1, s.now)
	s.crawlers = newCache[Cursor, CrawlerPage](SnapshotTTL, SeriesCacheEntries, s.now)
	return s
}

func (s *Service) freshness(o Overview, at time.Time, stale bool) Freshness {
	f := Freshness{FetchedAt: at, Stale: stale}
	switch o.Schema {
	case StateUninitialised:
		f.Ingest, f.Queue, f.Storage = StateUninitialised, StateUninitialised, StateUninitialised
		return f
	case StateUnavailable:
		f.Ingest, f.Queue, f.Storage = StateUnavailable, StateUnavailable, StateUnavailable
		return f
	}
	f.Ingest = StateUnavailable
	for _, in := range o.Instances { // the newest heartbeat decides
		if in.State < f.Ingest {
			f.Ingest = in.State
		}
	}
	f.Queue, f.Storage = o.Queue.State, o.Storage.State
	return f
}

// Overview returns the dashboard state (cached 10 s, last known good on failure).
func (s *Service) Overview(ctx context.Context, p access.Principal) (Overview, Freshness, error) {
	if err := p.Check(); err != nil {
		return Overview{}, Freshness{}, err
	}
	o, at, stale, err := s.overview.get(ctx, struct{}{}, s.r.overview)
	if err != nil {
		return Overview{}, Freshness{}, err
	}
	// Ages and states were computed when the database was read; the answer
	// (up to 10 s later, or much later after a failed refresh) is aged to now.
	o = o.aged(at, s.now())
	return o, s.freshness(o, at, stale), nil
}

// IngestStatus returns the capability probe of the optional ingest schema
// (cached 60 s).
func (s *Service) IngestStatus(ctx context.Context, p access.Principal) (IngestStatus, error) {
	if err := p.Check(); err != nil {
		return IngestStatus{}, err
	}
	st, _, _, err := s.probe.get(ctx, struct{}{}, s.r.probeIngest)
	return st, err
}

func validChart(c string) bool {
	switch c {
	case ChartThroughput, ChartOutcomes, ChartQueue, ChartTraffic, ChartStorage:
		return true
	}
	return false
}

func nextPeriod(gran string, t time.Time, d int) time.Time {
	switch gran {
	case GranDay:
		return t.AddDate(0, 0, d)
	case GranMonth:
		return t.AddDate(0, d, 0)
	}
	return t.AddDate(d, 0, 0)
}

func periodString(gran string, t time.Time) string {
	return map[string]string{GranDay: t.Format("2006-01-02"), GranMonth: t.Format("2006-01"), GranYear: t.Format("2006")}[gran]
}

// periodStart truncates a stored bucket start to the start of its period.
func periodStart(gran string, t time.Time) time.Time {
	switch gran {
	case GranDay:
		return opstats.DayOf(t)
	case GranMonth:
		return opstats.MonthOf(t)
	}
	return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
}

// Series returns a chart for a period (cached 60 s, at most 64 entries). A
// malformed query is ErrBadQuery; a period before the first stored data or
// after the current one is ErrPeriodOutOfRange.
func (s *Service) Series(ctx context.Context, p access.Principal, q Query) (Series, Freshness, error) {
	if err := p.Check(); err != nil {
		return Series{}, Freshness{}, err
	}
	if !validChart(q.Chart) {
		return Series{}, Freshness{}, ErrBadQuery
	}
	now := s.now().UTC()
	start, _, buckets, err := period(q, now)
	if err != nil {
		return Series{}, Freshness{}, err
	}
	q.Period = periodString(q.Granularity, start) // canonical key: "" and the current period share an entry

	span, _, _, err := s.bounds.get(ctx, q.Granularity, func(ctx context.Context) ([2]time.Time, error) {
		if state, _, err := s.r.schemaState(ctx); err != nil || state != StateUp {
			return [2]time.Time{}, err
		}
		f, l, ok, err := s.r.bounds(ctx, aggTableGran(q.Granularity))
		if err != nil || !ok {
			return [2]time.Time{}, err
		}
		return [2]time.Time{f, l}, nil
	})
	if err != nil {
		return Series{}, Freshness{}, err
	}
	current := periodStart(q.Granularity, now)
	earliest := current
	if !span[0].IsZero() {
		earliest = periodStart(q.Granularity, span[0])
	}
	if start.After(current) || start.Before(earliest) {
		return Series{}, Freshness{}, ErrPeriodOutOfRange
	}

	out, at, stale, err := s.series.get(ctx, q, func(ctx context.Context) (Series, error) {
		state, _, err := s.r.schemaState(ctx)
		if err != nil {
			return Series{}, err
		}
		if state != StateUp {
			return Series{Query: q, Buckets: buckets, Partial: make([]bool, len(buckets))}, nil
		}
		var rows map[time.Time]*bucketRow
		var srows map[time.Time]*storageRow
		end := nextPeriod(q.Granularity, start, 1)
		if q.Chart == ChartStorage {
			srows, err = s.r.storageRows(ctx, q.Granularity, start, end)
		} else {
			rows, err = s.r.aggRows(ctx, q.Granularity, start, end)
		}
		if err != nil {
			return Series{}, err
		}
		sr := buildSeries(q, buckets, rows, srows, s.r.quotaMax(ctx), s.now().UTC())
		if start.After(earliest) {
			sr.Previous = periodString(q.Granularity, nextPeriod(q.Granularity, start, -1))
		}
		if start.Before(current) {
			sr.Next = periodString(q.Granularity, nextPeriod(q.Granularity, start, 1))
		}
		return sr, nil
	})
	if err != nil {
		return Series{}, Freshness{}, err
	}
	return out, Freshness{FetchedAt: at, Stale: stale}, nil
}

// aggTableGran maps a series granularity to the aggregate granularity table key
// used by Reader.bounds (it takes the granularity name itself).
func aggTableGran(g string) string { return g }

// Crawlers returns one page of the registry (with activity when statistics are
// readable), listing crawlers that never connected too. The query is a keyset
// scan on the unique name index: its cost does not depend on the registered count.
func (s *Service) Crawlers(ctx context.Context, p access.Principal, after Cursor) (CrawlerPage, Freshness, error) {
	if err := p.Check(); err != nil {
		return CrawlerPage{}, Freshness{}, err
	}
	page, at, stale, err := s.crawlers.get(ctx, after, func(ctx context.Context) (CrawlerPage, error) {
		ing, _, _, err := s.probe.get(ctx, struct{}{}, s.r.probeIngest) // one probe (cached 60 s) for every page that needs the ingest schema
		if err != nil {
			return CrawlerPage{}, err
		}
		if ing.State != IngestOK {
			return CrawlerPage{Ingest: ing}, nil
		}
		state, _, err := s.r.schemaState(ctx)
		if err != nil {
			return CrawlerPage{}, err
		}
		pg, err := s.r.crawlers(ctx, after, state == StateUp)
		pg.Ingest = ing
		return pg, err
	})
	if err != nil {
		return CrawlerPage{}, Freshness{}, err
	}
	return page, Freshness{FetchedAt: at, Stale: stale}, nil
}
