package opstats

import (
	"fmt"
	"strings"
)

// SchemaVersion is the only accepted magnetico_stats.stats_version row.
const SchemaVersion = 1

type colSpec struct {
	name, typ string
	notNull   bool
	def       string // default expression ("" for none)
}

type tableSpec struct {
	name   string
	cols   []colSpec
	pk     []string
	checks []string // table constraints, "CONSTRAINT x CHECK (...)" text
	// autovacuum marks small, frequently updated tables.
	hot bool
}

func (t tableSpec) col(name string) (colSpec, bool) {
	for _, c := range t.cols {
		if c.name == name {
			return c, true
		}
	}
	return colSpec{}, false
}

func bigint(name string) colSpec { return colSpec{name, "bigint", true, "0"} }

func nullable(name, typ string) colSpec { return colSpec{name: name, typ: typ} }

// aggTables returns the columns shared by agg_hour, agg_day and agg_month.
func aggColumns(unit string) []colSpec {
	cols := []colSpec{{"bucket_start", "timestamp with time zone", true, ""}}
	for c := Counter(0); c < NumCounters; c++ {
		cols = append(cols, bigint(c.Name()))
	}
	for g := Gauge(0); g < NumGauges; g++ {
		n := g.Name()
		cols = append(cols, bigint(n+"_sum"), bigint(n+"_n"), nullable(n+"_min", "bigint"), nullable(n+"_max", "bigint"), nullable(n+"_last", "bigint"))
	}
	cols = append(cols, nullable("gauge_last_at", "timestamp with time zone"))
	if unit == "hour" {
		// One bit per UTC minute in which some instance was running and recording.
		cols = append(cols, bigint("covered_mask"))
	} else {
		cols = append(cols, colSpec{"covered_minutes", "integer", true, "0"})
	}
	return cols
}

func aggChecks(unit string) []string {
	checks := []string{
		fmt.Sprintf("CONSTRAINT agg_%s_aligned CHECK (date_trunc('%s', bucket_start, 'UTC') = bucket_start)", unit, unit),
	}
	var nonneg []string
	for c := Counter(0); c < NumCounters; c++ {
		nonneg = append(nonneg, c.Name()+" >= 0")
	}
	checks = append(checks, fmt.Sprintf("CONSTRAINT agg_%s_counters_nonneg CHECK (%s)", unit, strings.Join(nonneg, " AND ")))
	for g := Gauge(0); g < NumGauges; g++ {
		n := g.Name()
		checks = append(checks, fmt.Sprintf("CONSTRAINT agg_%s_%s_range CHECK (%s_sum >= 0 AND %s_n >= 0 AND ((%s_min IS NULL AND %s_max IS NULL) OR %s_min <= %s_max))",
			unit, n, n, n, n, n, n, n))
	}
	if unit == "hour" {
		checks = append(checks, "CONSTRAINT agg_hour_mask CHECK (covered_mask >= 0 AND covered_mask < (1::bigint << 60))")
	} else {
		max := 1440
		if unit == "month" {
			max = 31 * 1440
		}
		checks = append(checks, fmt.Sprintf("CONSTRAINT agg_%s_covered CHECK (covered_minutes >= 0 AND covered_minutes <= %d)", unit, max))
	}
	return checks
}

func storageColumns() []colSpec {
	cols := []colSpec{{"bucket_start", "timestamp with time zone", true, ""}}
	for m := Measure(0); m < NumMeasures; m++ {
		n := m.Name()
		cols = append(cols, nullable(n+"_last", "bigint"), nullable(n+"_max", "bigint"), bigint(n+"_sum"), bigint(n+"_n"))
	}
	cols = append(cols,
		bigint("bpt_bytes_sum"), bigint("bpt_torrents_sum"), bigint("samples"), bigint("reconciliation_mismatches"),
		nullable("last_sample_at", "timestamp with time zone"))
	return cols
}

func storageChecks(unit string) []string {
	checks := []string{
		fmt.Sprintf("CONSTRAINT storage_%s_aligned CHECK (date_trunc('%s', bucket_start, 'UTC') = bucket_start)", unit, unit),
	}
	var nonneg []string
	for m := Measure(0); m < NumMeasures; m++ {
		n := m.Name()
		nonneg = append(nonneg, n+"_sum >= 0", n+"_n >= 0", "("+n+"_last IS NULL OR "+n+"_last >= 0)", "("+n+"_max IS NULL OR "+n+"_max >= 0)")
	}
	nonneg = append(nonneg, "bpt_bytes_sum >= 0", "bpt_torrents_sum >= 0", "samples >= 0", "reconciliation_mismatches >= 0")
	return append(checks, fmt.Sprintf("CONSTRAINT storage_%s_nonneg CHECK (%s)", unit, strings.Join(nonneg, " AND ")))
}

// snapshotWindowMetrics are the metrics for which a heartbeat publishes
// completed-slot window counts (rates are derived from them).
var snapshotWindowMetrics = []string{"records_received", "records_enqueued", "records_acknowledged", "settled", "exists_hashes", "bytes_body"}

// WindowSeconds are the rolling windows of the heartbeat.
var WindowSeconds = []int{60, 900, 3600}

// snapshotCumulative are counters published as totals since the process started.
var snapshotCumulative = []string{"records_received", "records_enqueued", "records_acknowledged",
	"settled_stored_new", "settled_stored_existing", "settled_quarantined", "torrents_created"}

func snapshotColumns() []colSpec {
	cols := []colSpec{
		{"instance_id", "uuid", true, ""},
		{"started_at", "timestamp with time zone", true, ""},
		{"published_at", "timestamp with time zone", true, ""},
		nullable("window_end", "timestamp with time zone"),
		{"owns_consumer", "boolean", true, "false"},
		bigint("in_flight_records"),
		bigint("flush_stalled_s"),
		bigint("hours_dropped"), bigint("sequence_anomalies"), bigint("batches_dropped_unresolved"),
		bigint("resolved_committed"), bigint("gauge_batches_skipped_overlap"), bigint("crawlers_overflow"),
		{"clock_anomaly", "boolean", true, "false"},
	}
	for _, c := range snapshotCumulative {
		cols = append(cols, bigint("total_"+c))
	}
	for _, w := range WindowSeconds {
		cols = append(cols, colSpec{fmt.Sprintf("w%d_covered_s", w), "integer", true, "0"})
		for _, m := range snapshotWindowMetrics {
			cols = append(cols, bigint(fmt.Sprintf("w%d_%s", w, m)))
		}
	}
	return cols
}

func sharedStateColumns() []colSpec {
	cols := []colSpec{
		{"id", "smallint", true, "1"},
		nullable("queue_sampled_at", "timestamp with time zone"),
		bigint("gauge_last_slot"),
		bigint("quota_max_bytes"),
	}
	for g := Gauge(0); g < NumGauges; g++ {
		cols = append(cols, nullable(g.Name(), "bigint")) // NULL: unavailable, never zero
	}
	cols = append(cols,
		colSpec{"queue_counts_approx", "boolean", true, "false"},
		colSpec{"oldest_age_approx", "boolean", true, "false"},
		colSpec{"queue_interval_backoff", "boolean", true, "false"},
		nullable("storage_sampled_at", "timestamp with time zone"),
		bigint("storage_last_slot"),
		colSpec{"storage_reconciled", "boolean", true, "true"},
	)
	for m := Measure(0); m < NumMeasures; m++ {
		cols = append(cols, nullable("storage_"+m.Name(), "bigint"))
	}
	return cols
}

var activityColumns = []colSpec{
	{"crawler_id", "uuid", true, ""},
	{"first_seen_at", "timestamp with time zone", true, ""},
	{"last_seen_at", "timestamp with time zone", true, ""},
	nullable("last_ip", "inet"),
	nullable("last_submit_at", "timestamp with time zone"),
	bigint("records_received"), bigint("records_enqueued"), bigint("records_duplicate"),
	bigint("bytes_body"), bigint("exists_requests"), bigint("exists_hashes"), bigint("rejected_requests"),
}

// tables lists every table of the schema in creation order.
var tables = buildTables()

func buildTables() []tableSpec {
	ts := []tableSpec{
		{name: "stats_version", cols: []colSpec{{"version", "integer", true, ""}}, pk: []string{"version"}},
		{name: "ingest_instances", hot: true, pk: []string{"instance_id"}, cols: []colSpec{
			{"instance_id", "uuid", true, ""},
			{"started_at", "timestamp with time zone", true, ""},
			bigint("last_flush_seq"),
			nullable("last_flush_at", "timestamp with time zone"),
			nullable("heartbeat_at", "timestamp with time zone"),
		}, checks: []string{"CONSTRAINT ingest_instances_seq CHECK (last_flush_seq >= 0)"}},
		{name: "snapshot", hot: true, pk: []string{"instance_id"}, cols: snapshotColumns()},
		{name: "shared_state", hot: true, pk: []string{"id"}, cols: sharedStateColumns(),
			checks: []string{"CONSTRAINT shared_state_single CHECK (id = 1)"}},
		{name: "crawler_activity", pk: []string{"crawler_id"}, cols: activityColumns, checks: []string{
			"CONSTRAINT crawler_activity_nonneg CHECK (records_received >= 0 AND records_enqueued >= 0 AND records_duplicate >= 0 AND bytes_body >= 0 AND exists_requests >= 0 AND exists_hashes >= 0 AND rejected_requests >= 0)",
		}},
	}
	for _, unit := range []string{"hour", "day", "month"} {
		ts = append(ts, tableSpec{name: "agg_" + unit, pk: []string{"bucket_start"}, cols: aggColumns(unit), checks: aggChecks(unit)})
	}
	for _, unit := range []string{"hour", "day", "month"} {
		ts = append(ts, tableSpec{name: "storage_" + unit, pk: []string{"bucket_start"}, cols: storageColumns(), checks: storageChecks(unit)})
	}
	return ts
}

func (t tableSpec) ddl() string {
	var parts []string
	for _, c := range t.cols {
		p := c.name + " " + strings.ToUpper(c.typ)
		if c.typ == "timestamp with time zone" {
			p = c.name + " TIMESTAMPTZ"
		}
		if c.notNull {
			p += " NOT NULL"
		}
		if c.def != "" {
			p += " DEFAULT " + c.def
		}
		parts = append(parts, p)
	}
	parts = append(parts, "PRIMARY KEY ("+strings.Join(t.pk, ", ")+")")
	parts = append(parts, t.checks...)
	stmt := "CREATE TABLE " + SchemaName + "." + t.name + " (\n    " + strings.Join(parts, ",\n    ") + "\n)"
	if t.hot {
		stmt += " WITH (fillfactor = 70, autovacuum_vacuum_scale_factor = 0.05, autovacuum_vacuum_threshold = 50, autovacuum_vacuum_cost_delay = 2)"
	}
	return stmt
}

// TableNames lists the tables of the schema (for documentation checks).
func TableNames() []string {
	out := make([]string, len(tables))
	for i, t := range tables {
		out[i] = t.name
	}
	return out
}

// ColumnNames lists every column name of every table (for documentation and
// privacy checks).
func ColumnNames() []string {
	var out []string
	for _, t := range tables {
		for _, c := range t.cols {
			out = append(out, t.name+"."+c.name)
		}
	}
	return out
}
