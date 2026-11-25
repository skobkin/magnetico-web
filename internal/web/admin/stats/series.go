package stats

import (
	"fmt"
	"strings"
	"time"

	"github.com/skobkin/magnetico-web/internal/opstats"
)

// Chart names (docs/specs/statistics-v1.md §9).
const (
	ChartThroughput = "throughput"
	ChartOutcomes   = "outcomes"
	ChartQueue      = "queue"
	ChartTraffic    = "traffic"
	ChartStorage    = "storage"
)

// Granularities: one day of hourly buckets, one month of daily, one year of monthly.
const (
	GranDay   = "day"
	GranMonth = "month"
	GranYear  = "year"
)

// Query selects a chart, a granularity and a period ("2026-11-25", "2026-11", "2026").
type Query struct{ Chart, Granularity, Period string }

// Line is one plotted series. A nil value is a gap (no observation); a zero is
// a measured zero.
type Line struct {
	Key, Label, Unit string
	Stack            bool // drawn stacked with the other Stack lines
	Values           []*float64
}

// Series is the answer to a Query.
type Series struct {
	Query   Query
	Buckets []time.Time // the start of every bucket of the period (missing ones included)
	Partial []bool      // bucket observed for less than its length, or still open
	Lines   []Line
	// Previous and Next are the neighbouring periods that have data ("" when none).
	Previous, Next string
}

// period resolves a query to [start, end) and the bucket starts.
func period(q Query, now time.Time) (start, end time.Time, buckets []time.Time, err error) {
	var layout string
	switch q.Granularity {
	case GranDay:
		layout = "2006-01-02"
	case GranMonth:
		layout = "2006-01"
	case GranYear:
		layout = "2006"
	default:
		return start, end, nil, ErrBadQuery
	}
	p := q.Period
	if p == "" {
		p = now.UTC().Format(layout)
	}
	if len(p) != len(layout) {
		return start, end, nil, ErrBadQuery
	}
	t, perr := time.Parse(layout, p)
	if perr != nil || t.Year() < 2000 || t.Year() > now.Year()+1 {
		return start, end, nil, ErrBadQuery
	}
	switch q.Granularity {
	case GranDay:
		start, end = t, t.AddDate(0, 0, 1)
		for b := start; b.Before(end); b = b.Add(time.Hour) {
			buckets = append(buckets, b)
		}
	case GranMonth:
		start, end = t, t.AddDate(0, 1, 0)
		for b := start; b.Before(end); b = b.AddDate(0, 0, 1) {
			buckets = append(buckets, b)
		}
	default:
		start, end = t, t.AddDate(1, 0, 0)
		for b := start; b.Before(end); b = b.AddDate(0, 1, 0) {
			buckets = append(buckets, b)
		}
	}
	return start, end, buckets, nil
}

// bucketRow is one stored aggregate bucket.
type bucketRow struct {
	start   time.Time
	c       [opstats.NumCounters]int64
	g       [opstats.NumGauges]opstats.GaugeAgg
	covered int // observed minutes
}

type storageRow struct {
	start      time.Time
	last, max  [opstats.NumMeasures]*int64
	bptB, bptT int64
	samples    int64
}

func bucketMinutes(gran string, start time.Time) int {
	switch gran {
	case GranDay:
		return 60
	case GranMonth:
		return 1440
	}
	return int(start.AddDate(0, 1, 0).Sub(start) / time.Minute)
}

// buildSeries turns stored rows into the lines of a chart. quotaMax converts
// the average quota usage into a utilisation.
func buildSeries(q Query, buckets []time.Time, rows map[time.Time]*bucketRow, srows map[time.Time]*storageRow, quotaMax Measured[int64], now time.Time) Series {
	s := Series{Query: q, Buckets: buckets, Partial: make([]bool, len(buckets))}
	for i, b := range buckets {
		if r := rows[b]; r != nil {
			s.Partial[i] = r.covered < bucketMinutes(q.Granularity, b)
		}
		var end time.Time
		switch q.Granularity {
		case GranDay:
			end = b.Add(time.Hour)
		case GranMonth:
			end = b.AddDate(0, 0, 1)
		default:
			end = b.AddDate(0, 1, 0)
		}
		if end.After(now) { // the open bucket is partial by definition
			s.Partial[i] = true
		}
	}
	line := func(key, label, unit string, stack bool, f func(r *bucketRow) *float64) {
		l := Line{Key: key, Label: label, Unit: unit, Stack: stack, Values: make([]*float64, len(buckets))}
		for i, b := range buckets {
			if r := rows[b]; r != nil {
				l.Values[i] = f(r)
			}
		}
		s.Lines = append(s.Lines, l)
	}
	cnt := func(c opstats.Counter) func(r *bucketRow) *float64 {
		return func(r *bucketRow) *float64 { return ptr(r.c[c]) }
	}
	switch q.Chart {
	case ChartThroughput:
		line("received", "Received", "records", false, cnt(opstats.CRecordsReceived))
		line("acknowledged", "Acknowledged", "records", false, cnt(opstats.CRecordsAcknowledged))
		line("enqueued", "Enqueued", "records", false, cnt(opstats.CRecordsEnqueued))
		line("settled", "Settled", "records", false, func(r *bucketRow) *float64 {
			return ptr(r.c[opstats.CSettledStoredNew] + r.c[opstats.CSettledStoredExisting] + r.c[opstats.CSettledQuarantined])
		})
	case ChartOutcomes:
		line("stored_new", "Stored (new)", "records", true, cnt(opstats.CSettledStoredNew))
		line("stored_existing", "Stored (already present)", "records", true, cnt(opstats.CSettledStoredExisting))
		line("quarantined", "Quarantined", "records", true, cnt(opstats.CSettledQuarantined))
		line("torrents_created", "Torrents created", "records", false, cnt(opstats.CTorrentsCreated))
	case ChartQueue:
		gaugeAvg := func(g opstats.Gauge) func(r *bucketRow) *float64 {
			return func(r *bucketRow) *float64 {
				if a := r.g[g]; a.Valid && a.N > 0 {
					return ratio(a.Sum, a.N)
				}
				return nil
			}
		}
		gaugeMax := func(g opstats.Gauge) func(r *bucketRow) *float64 {
			return func(r *bucketRow) *float64 {
				if a := r.g[g]; a.Valid {
					return ptr(a.Max)
				}
				return nil
			}
		}
		line("queue_avg", "Queue depth (average)", "records", false, gaugeAvg(opstats.GQueueRows))
		line("queue_max", "Queue depth (maximum)", "records", false, gaugeMax(opstats.GQueueRows))
		line("quarantine", "Quarantine (maximum)", "records", false, gaugeMax(opstats.GQuarantineRows))
		line("quota", "Quota used (average)", "ratio", false, func(r *bucketRow) *float64 {
			a := r.g[opstats.GQuotaUsedBytes]
			if !a.Valid || a.N == 0 || !quotaMax.Valid || quotaMax.Value <= 0 {
				return nil
			}
			return ratio(a.Sum, a.N*quotaMax.Value)
		})
	case ChartTraffic:
		line("bytes", "Request bytes", "bytes", false, cnt(opstats.CBytesBody))
		line("record_size", "Mean record size", "bytes", false, func(r *bucketRow) *float64 {
			return ratio(r.c[opstats.CBytesAccepted], r.c[opstats.CRecordsEnqueued])
		})
		line("hashes_per_request", "Mean hashes per existence request", "hashes", false, func(r *bucketRow) *float64 {
			return ratio(r.c[opstats.CExistsHashes], r.c[opstats.CExistsRequests])
		})
		line("records_per_request", "Mean records per submit request", "records", false, func(r *bucketRow) *float64 {
			return ratio(r.c[opstats.CRecordsReceived], r.c[opstats.CSubmitValidated])
		})
		line("cache_hit_rate", "Existence cache hit rate", "ratio", false, func(r *bucketRow) *float64 {
			h := r.c[opstats.CCacheHitPositive] + r.c[opstats.CCacheHitNegative]
			return ratio(h, h+r.c[opstats.CCacheMiss])
		})
		line("auth_hit_rate", "Authentication cache hit rate", "ratio", false, func(r *bucketRow) *float64 {
			return ratio(r.c[opstats.CAuthCacheHits], r.c[opstats.CAuthCacheHits]+r.c[opstats.CAuthCacheMisses])
		})
		line("rejected", "Rejected requests", "requests", false, func(r *bucketRow) *float64 {
			var n int64
			for c := opstats.CRejectedAuth; c <= opstats.CRejectedInternal; c++ {
				n = opstats.AddSat(n, r.c[c])
			}
			return ptr(n)
		})
		line("processing_ms", "Mean processing time", "ms", false, func(r *bucketRow) *float64 {
			return ratio(r.c[opstats.CProcessingMsSum], r.c[opstats.CProcessingMsCount])
		})
	case ChartStorage:
		sline := func(key, label, unit string, stack bool, f func(r *storageRow) *float64) {
			l := Line{Key: key, Label: label, Unit: unit, Stack: stack, Values: make([]*float64, len(buckets))}
			for i, b := range buckets {
				if r := srows[b]; r != nil && r.samples > 0 {
					l.Values[i] = f(r)
				}
			}
			s.Lines = append(s.Lines, l)
		}
		last := func(m opstats.Measure) func(r *storageRow) *float64 {
			return func(r *storageRow) *float64 {
				if v := r.last[m]; v != nil {
					return ptr(*v)
				}
				return nil
			}
		}
		sline("torrents_table", "Torrents table", "bytes", true, last(opstats.MTorrentsTable))
		sline("blobs_table", "File lists table", "bytes", true, last(opstats.MBlobsTable))
		sline("indexes", "Indexes", "bytes", true, last(opstats.MIndexes))
		sline("other", "Other", "bytes", true, last(opstats.MOther))
		sline("magneticod_total", "magneticod total", "bytes", false, last(opstats.MMagneticodTotal))
		sline("ingest_total", "Ingest queue schema", "bytes", false, last(opstats.MIngestTotal))
		sline("database_total", "Database", "bytes", false, last(opstats.MDatabaseTotal))
		sline("bytes_per_torrent", "Bytes per torrent (ratio of sums)", "bytes", false, func(r *storageRow) *float64 { return ratio(r.bptB, r.bptT) })
	}
	return s
}

// periodLabel formats a bucket for the text summary.
func periodLabel(gran string, t time.Time) string {
	switch gran {
	case GranDay:
		return t.Format("2006-01-02 15:00")
	case GranMonth:
		return t.Format("2006-01-02")
	}
	return t.Format("2006-01")
}

// Summary renders a Series as a plain-text table (the no-JavaScript view).
func (s Series) Summary() string {
	var b strings.Builder
	b.WriteString("period")
	for _, l := range s.Lines {
		b.WriteString("\t" + l.Label)
	}
	b.WriteString("\n")
	for i, t := range s.Buckets {
		b.WriteString(periodLabel(s.Query.Granularity, t))
		if s.Partial[i] {
			b.WriteString(" (partial)")
		}
		for _, l := range s.Lines {
			if v := l.Values[i]; v != nil {
				fmt.Fprintf(&b, "\t%.4g", *v)
			} else {
				b.WriteString("\t–")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
