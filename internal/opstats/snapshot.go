package opstats

import (
	"fmt"
	"strings"
	"time"
)

// NumSnapshotTotals is len(SnapshotTotalCounters).
const NumSnapshotTotals = 7

// SnapshotTotalCounters are the counters a heartbeat publishes as totals since
// the process started (snapshot columns total_<counter name>).
var SnapshotTotalCounters = [NumSnapshotTotals]Counter{
	CRecordsReceived, CRecordsEnqueued, CRecordsAcknowledged,
	CSettledStoredNew, CSettledStoredExisting, CSettledQuarantined, CTorrentsCreated,
}

// NumWindowMetrics is the number of metrics published per rolling window.
const NumWindowMetrics = 14

// WindowMetric is one rolling-rate metric: the snapshot column stem and the
// counters whose events it sums. This is the one definition; the ingest ring,
// the heartbeat, the schema and the dashboard are all derived from it.
type WindowMetric struct {
	Name     string
	Counters []Counter
}

// WindowMetrics are the rolling-rate metrics, in the order of
// Snapshot.WindowCounts and of the dashboard's rate table.
var WindowMetrics = [NumWindowMetrics]WindowMetric{
	{"records_received", []Counter{CRecordsReceived}},
	{"records_enqueued", []Counter{CRecordsEnqueued}},
	{"records_acknowledged", []Counter{CRecordsAcknowledged}},
	{"settled", []Counter{CSettledStoredNew, CSettledStoredExisting, CSettledQuarantined}},
	{"torrents_created", []Counter{CTorrentsCreated}},
	{"submit_requests", []Counter{CSubmitRequests}},
	{"exists_requests", []Counter{CExistsRequests}},
	{"exists_hashes", []Counter{CExistsHashes}},
	{"bytes_body", []Counter{CBytesBody}},
	{"auth_cache_hits", []Counter{CAuthCacheHits}},
	{"auth_cache_misses", []Counter{CAuthCacheMisses}},
	{"auth_db_lookups", []Counter{CAuthDBLookups}},
	{"auth_busy", []Counter{CAuthBusy}},
	{"auth_storage_failures", []Counter{CAuthStorageFailures}},
}

// WindowMetricNames are the snapshot column stems of the window metrics, in
// the order of WindowMetrics.
var WindowMetricNames = func() (n [NumWindowMetrics]string) {
	for i, m := range WindowMetrics {
		n[i] = m.Name
	}
	return
}()

// WindowMetricIndex is the position of the named metric, or -1.
func WindowMetricIndex(name string) int {
	for i, m := range WindowMetrics {
		if m.Name == name {
			return i
		}
	}
	return -1
}

// Snapshot is the heartbeat state of one ingest instance: cumulative totals
// since start, completed-second window counts with the seconds really
// observed, and the status counters of the writer.
type Snapshot struct {
	InstanceID  string
	StartedAt   time.Time
	PublishedAt time.Time // informational: the stored value is the database clock
	WindowEnd   time.Time // zero: no window yet

	OwnsConsumer bool
	InFlight     int64

	FlushStalledSeconds      int64
	HoursDropped             int64
	SequenceAnomalies        int64
	BatchesDroppedUnresolved int64
	ResolvedCommitted        int64
	GaugeBatchesSkipped      int64
	CrawlersOverflow         int64
	ClockAnomaly             bool

	Totals        [NumSnapshotTotals]int64
	WindowCovered [3]int
	WindowCounts  [3][NumWindowMetrics]int64
}

// snapshotScalarColumns precede the totals and the windows.
var snapshotScalarColumns = []string{
	"instance_id", "started_at", "published_at", "window_end", "owns_consumer", "in_flight_records", "flush_stalled_s",
	"hours_dropped", "sequence_anomalies", "batches_dropped_unresolved", "resolved_committed",
	"gauge_batches_skipped_overlap", "crawlers_overflow", "clock_anomaly",
}

// SnapshotColumns lists the snapshot columns in the order of Args.
func SnapshotColumns() []string {
	cols := append([]string(nil), snapshotScalarColumns...)
	for _, c := range SnapshotTotalCounters {
		cols = append(cols, "total_"+c.Name())
	}
	for _, w := range WindowSeconds {
		cols = append(cols, fmt.Sprintf("w%d_covered_s", w))
		for _, m := range WindowMetricNames {
			cols = append(cols, fmt.Sprintf("w%d_%s", w, m))
		}
	}
	return cols
}

// UpsertSnapshotSQL is the in-place update of one instance's heartbeat row.
// published_at is the DATABASE clock (now()), not an argument: freshness is
// judged against the same clock by the reader, so clock skew between the ingest
// host and the web host never makes a live instance look stale.
func UpsertSnapshotSQL() string {
	cols := SnapshotColumns()
	ph := make([]string, len(cols))
	sets := make([]string, 0, len(cols))
	n := 0
	for i, c := range cols {
		if c == "published_at" {
			ph[i] = "now()"
		} else {
			n++
			ph[i] = fmt.Sprintf("$%d", n)
		}
		if c != "instance_id" && c != "started_at" {
			sets = append(sets, c+" = EXCLUDED."+c)
		}
	}
	return fmt.Sprintf("INSERT INTO %s.snapshot (%s) VALUES (%s) ON CONFLICT (instance_id) DO UPDATE SET %s",
		SchemaName, strings.Join(cols, ", "), strings.Join(ph, ", "), strings.Join(sets, ", "))
}

// Args are the statement arguments for UpsertSnapshotSQL.
func (s *Snapshot) Args() []any {
	args := []any{s.InstanceID, s.StartedAt, nullTime(s.WindowEnd), s.OwnsConsumer, s.InFlight, s.FlushStalledSeconds,
		s.HoursDropped, s.SequenceAnomalies, s.BatchesDroppedUnresolved, s.ResolvedCommitted,
		s.GaugeBatchesSkipped, s.CrawlersOverflow, s.ClockAnomaly}
	for _, t := range s.Totals {
		args = append(args, t)
	}
	for i := range WindowSeconds {
		args = append(args, int32(s.WindowCovered[i]))
		for _, v := range s.WindowCounts[i] {
			args = append(args, v)
		}
	}
	return args
}
