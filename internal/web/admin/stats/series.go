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

// Notes a bucket can carry.
const (
	// NoteApproximateCounts: at least one queue or quarantine row count of the bucket is a planner estimate.
	NoteApproximateCounts = "approximate_counts"
	// NoteReconciliationMismatch: a storage sample's categories did not add up to its total (a genuine inconsistency).
	NoteReconciliationMismatch = "reconciliation_mismatch"
	// NoteReconciliationNotEvaluated: the check could not run because a measure was unavailable (not an inconsistency).
	NoteReconciliationNotEvaluated = "reconciliation_not_evaluated"
	// NoteIncompleteBreakdown: the bucket's newest storage sample lacks a category, so no stack is drawn.
	NoteIncompleteBreakdown = "incomplete_breakdown"
)

// Line is one plotted series. A nil value is a gap (no observation); a zero is
// a measured zero.
type Line struct {
	Key, Label, Unit string
	// Group says which plot of the chart the line belongs to: lines of one chart
	// with different units or magnitudes are never drawn on one axis.
	Group  string
	Stack  bool // drawn stacked with the other Stack lines of its group
	Values []*float64
}

// Series is the answer to a Query.
type Series struct {
	Query   Query
	Buckets []time.Time // the start of every bucket of the period (missing ones included)
	Partial []bool      // bucket observed for less than its length, or still open
	// Notes lists, per bucket, what a reader must know about its values (the Note
	// constants); it is as long as Buckets and an entry is nil when there is nothing to say.
	Notes [][]string
	Lines []Line
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
	// mismatches and unevaluated count the samples of the bucket whose
	// reconciliation failed and whose reconciliation could not run.
	mismatches, unevaluated int64
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

// buildSeries turns stored rows into the lines of a chart. Every figure comes
// from what was stored with the observation: the quota utilisation was recorded
// with the quota in force at that moment, so nothing here depends on today's
// configuration.
func buildSeries(q Query, buckets []time.Time, rows map[time.Time]*bucketRow, srows map[time.Time]*storageRow, now time.Time) Series {
	s := Series{Query: q, Buckets: buckets, Partial: make([]bool, len(buckets)), Notes: make([][]string, len(buckets))}
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
	line := func(group, key, label, unit string, stack bool, f func(r *bucketRow) *float64) {
		l := Line{Key: key, Label: label, Unit: unit, Group: group, Stack: stack, Values: make([]*float64, len(buckets))}
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
		line("Records", "received", "Received", "records", false, cnt(opstats.CRecordsReceived))
		line("Records", "acknowledged", "Acknowledged", "records", false, cnt(opstats.CRecordsAcknowledged))
		line("Records", "enqueued", "Enqueued", "records", false, cnt(opstats.CRecordsEnqueued))
		line("Records", "settled", "Settled", "records", false, func(r *bucketRow) *float64 {
			return ptr(r.c[opstats.CSettledStoredNew] + r.c[opstats.CSettledStoredExisting] + r.c[opstats.CSettledQuarantined])
		})
	case ChartOutcomes:
		line("Settled records", "stored_new", "Stored (new)", "records", true, cnt(opstats.CSettledStoredNew))
		line("Settled records", "stored_existing", "Stored (already present)", "records", true, cnt(opstats.CSettledStoredExisting))
		line("Settled records", "quarantined", "Quarantined", "records", true, cnt(opstats.CSettledQuarantined))
		line("Settled records", "torrents_created", "Torrents created", "records", false, cnt(opstats.CTorrentsCreated))
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
		line("Queue depth", "queue_avg", "Queue depth (average)", "records", false, gaugeAvg(opstats.GQueueRows))
		line("Queue depth", "queue_max", "Queue depth (maximum)", "records", false, gaugeMax(opstats.GQueueRows))
		line("Queue depth", "quarantine", "Quarantine (maximum)", "records", false, gaugeMax(opstats.GQuarantineRows))
		line("Quota used", "quota", "Quota used (average)", "ratio", false, func(r *bucketRow) *float64 {
			if a := r.g[opstats.GQuotaUtilisationPPM]; a.Valid && a.N > 0 {
				v := float64(a.Sum) / float64(a.N) / 1e6
				return &v
			}
			return nil
		})
		line("Quota used", "quota_peak", "Quota used (peak)", "ratio", false, func(r *bucketRow) *float64 {
			if a := r.g[opstats.GQuotaUtilisationPPM]; a.Valid {
				v := float64(a.Max) / 1e6
				return &v
			}
			return nil
		})
		for i, b := range buckets {
			r := rows[b]
			if r == nil {
				continue
			}
			for _, g := range []opstats.Gauge{opstats.GQueueRowsApprox, opstats.GQuarantineRowsApprox} {
				if a := r.g[g]; a.Valid && a.Max > 0 {
					s.Notes[i] = append(s.Notes[i], NoteApproximateCounts)
					break
				}
			}
		}
	case ChartTraffic:
		line("Request bytes", "bytes", "Request bytes", "bytes", false, cnt(opstats.CBytesBody))
		line("Mean record size", "record_size", "Mean record size", "bytes", false, func(r *bucketRow) *float64 {
			return ratio(r.c[opstats.CBytesAccepted], r.c[opstats.CRecordsEnqueued])
		})
		line("Mean batch size", "hashes_per_request", "Mean hashes per existence request", "hashes", false, func(r *bucketRow) *float64 {
			return ratio(r.c[opstats.CExistsHashes], r.c[opstats.CExistsRequests])
		})
		line("Mean batch size", "records_per_request", "Mean records per submit request", "records", false, func(r *bucketRow) *float64 {
			return ratio(r.c[opstats.CRecordsReceived], r.c[opstats.CSubmitValidated])
		})
		line("Hit rates", "cache_hit_rate", "Existence cache hit rate", "ratio", false, func(r *bucketRow) *float64 {
			h := r.c[opstats.CCacheHitPositive] + r.c[opstats.CCacheHitNegative]
			return ratio(h, h+r.c[opstats.CCacheMiss])
		})
		line("Hit rates", "auth_hit_rate", "Authentication cache hit rate", "ratio", false, func(r *bucketRow) *float64 {
			return ratio(r.c[opstats.CAuthCacheHits], r.c[opstats.CAuthCacheHits]+r.c[opstats.CAuthCacheMisses])
		})
		line("Rejected requests", "rejected", "Rejected requests", "requests", false, func(r *bucketRow) *float64 {
			var n int64
			for c := opstats.CRejectedAuth; c <= opstats.CRejectedInternal; c++ {
				n = opstats.AddSat(n, r.c[c])
			}
			return ptr(n)
		})
		line("Mean processing time", "processing_ms", "Mean processing time", "ms", false, func(r *bucketRow) *float64 {
			return ratio(r.c[opstats.CProcessingMsSum], r.c[opstats.CProcessingMsCount])
		})
	case ChartStorage:
		sline := func(group, key, label, unit string, stack bool, f func(r *storageRow) *float64) {
			l := Line{Key: key, Label: label, Unit: unit, Group: group, Stack: stack, Values: make([]*float64, len(buckets))}
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
		// The stack is drawn only when the bucket's newest sample has all four categories:
		// a partial stack would read as a smaller database.
		complete := func(r *storageRow) bool {
			for _, m := range []opstats.Measure{opstats.MTorrentsTable, opstats.MBlobsTable, opstats.MIndexes, opstats.MOther} {
				if r.last[m] == nil {
					return false
				}
			}
			return true
		}
		stacked := func(m opstats.Measure) func(r *storageRow) *float64 {
			return func(r *storageRow) *float64 {
				if !complete(r) {
					return nil
				}
				return last(m)(r)
			}
		}
		sline("Size", "torrents_table", "Torrents table", "bytes", true, stacked(opstats.MTorrentsTable))
		sline("Size", "blobs_table", "File lists table", "bytes", true, stacked(opstats.MBlobsTable))
		sline("Size", "indexes", "Indexes", "bytes", true, stacked(opstats.MIndexes))
		sline("Size", "other", "Other", "bytes", true, stacked(opstats.MOther))
		for i, b := range buckets {
			r := srows[b]
			if r == nil || r.samples == 0 {
				continue
			}
			if !complete(r) {
				s.Notes[i] = append(s.Notes[i], NoteIncompleteBreakdown)
			}
			if r.mismatches > 0 {
				s.Notes[i] = append(s.Notes[i], NoteReconciliationMismatch)
			}
			if r.unevaluated > 0 {
				s.Notes[i] = append(s.Notes[i], NoteReconciliationNotEvaluated)
			}
		}
		sline("Size", "magneticod_total", "magneticod total", "bytes", false, last(opstats.MMagneticodTotal))
		sline("Size", "ingest_total", "Ingest queue schema", "bytes", false, last(opstats.MIngestTotal))
		sline("Size", "database_total", "Database", "bytes", false, last(opstats.MDatabaseTotal))
		sline("Bytes per torrent", "bytes_per_torrent", "Bytes per torrent (ratio of sums)", "bytes", false, func(r *storageRow) *float64 { return ratio(r.bptB, r.bptT) })
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
