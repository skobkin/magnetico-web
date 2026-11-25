package stats

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/opstats"
)

// Reader runs the queries. It has no state; caching lives in Service.
type Reader struct{ db opstats.DB }

func secs(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

// schemaState maps Verify to a State: StateUp, StateUninitialised (missing) or
// StateUnavailable (structurally incompatible). Any other error is returned.
func (r *Reader) schemaState(ctx context.Context) (State, error) {
	err := opstats.Verify(ctx, r.db)
	switch {
	case err == nil:
		return StateUp, nil
	case errors.Is(err, opstats.ErrSchemaMissing):
		return StateUninitialised, nil
	case errors.Is(err, opstats.ErrSchemaInvalid):
		return StateUnavailable, nil
	}
	return 0, err
}

func windowCols() []string {
	var cols []string
	for _, w := range opstats.WindowSeconds {
		cols = append(cols, fmt.Sprintf("w%d_covered_s", w))
		for _, m := range opstats.WindowMetricNames {
			cols = append(cols, fmt.Sprintf("w%d_%s", w, m))
		}
	}
	return cols
}

func (r *Reader) instances(ctx context.Context) ([]Instance, error) {
	cols := `instance_id::text, started_at, EXTRACT(EPOCH FROM now() - published_at), COALESCE(EXTRACT(EPOCH FROM now() - window_end), 1e9),
owns_consumer, in_flight_records, flush_stalled_s, hours_dropped, sequence_anomalies, batches_dropped_unresolved, clock_anomaly,
total_records_received, total_records_acknowledged, total_settled_stored_new + total_settled_stored_existing + total_settled_quarantined, ` +
		strings.Join(windowCols(), ", ")
	rows, err := r.db.Query(ctx, `SELECT `+cols+` FROM magnetico_stats.snapshot
WHERE published_at > now() - make_interval(secs => $1) ORDER BY published_at DESC LIMIT $2`, instanceHorizon.Seconds(), maxInstances)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		var in Instance
		var age, wage float64
		dest := []any{&in.ID, &in.StartedAt, &age, &wage, &in.OwnsConsumer, &in.InFlight, &in.FlushStalledSeconds,
			&in.HoursDropped, &in.SequenceAnomalies, &in.BatchesDroppedUnresolved, &in.ClockAnomaly,
			&in.TotalReceived, &in.TotalAcknowledged, &in.TotalSettled}
		covs := make([]int32, 3)
		for w := 0; w < 3; w++ {
			dest = append(dest, &covs[w])
			for m := 0; m < metricCount; m++ {
				dest = append(dest, &in.counts[w][m])
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		for w := range covs {
			in.covered[w] = int(covs[w])
		}
		in.Age, in.windowEndAge = secs(age), secs(wage)
		in.State = ClassifyIngest(in.Age)
		out = append(out, in)
	}
	return out, rows.Err()
}

func (r *Reader) shared(ctx context.Context) (QueueView, StorageView, error) {
	var (
		qAge, sAge                      *float64
		rows, qbytes, used, qmax, qrows *int64
		qby, oldest                     *int64
		approxCounts, approxOldest, bo  bool
		reconciled                      bool
		st                              [opstats.NumMeasures]*int64
	)
	dest := []any{&qAge, &rows, &qbytes, &used, &qrows, &qby, &oldest, &qmax, &approxCounts, &approxOldest, &bo, &sAge, &reconciled}
	cols := []string{"EXTRACT(EPOCH FROM now() - queue_sampled_at)", "queue_rows", "queue_bytes", "quota_used_bytes", "quarantine_rows", "quarantine_bytes", "oldest_age_s",
		"CASE WHEN quota_max_bytes > 0 THEN quota_max_bytes END", "queue_counts_approx", "oldest_age_approx", "queue_interval_backoff",
		"EXTRACT(EPOCH FROM now() - storage_sampled_at)", "storage_reconciled"}
	for m := opstats.Measure(0); m < opstats.NumMeasures; m++ {
		cols = append(cols, "storage_"+m.Name())
		dest = append(dest, &st[m])
	}
	if err := r.db.QueryRow(ctx, `SELECT `+strings.Join(cols, ", ")+` FROM magnetico_stats.shared_state WHERE id = 1`).Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return QueueView{State: StateUnavailable}, StorageView{State: StateUnavailable}, nil
		}
		return QueueView{}, StorageView{}, err
	}
	q := QueueView{State: StateUnavailable, Backoff: bo}
	if qAge != nil {
		q.Age = secs(*qAge)
		q.State = ClassifyQueue(q.Age)
	}
	if q.State != StateUnavailable { // an expired sample is not shown as a number
		q.Rows, q.Bytes, q.QuotaUsed = nullInt(rows), nullInt(qbytes), nullInt(used)
		if approxCounts && q.Rows.Valid {
			q.Rows.Approx = true
		}
		q.Quarantine, q.QuarantineBy, q.QuotaMax = nullInt(qrows), nullInt(qby), nullInt(qmax)
		if oldest != nil {
			q.OldestAge = Measured[time.Duration]{Value: time.Duration(*oldest) * time.Second, Valid: true, Approx: approxOldest}
		}
		q.Utilisation = utilisation(q.QuotaUsed, q.QuotaMax)
	}
	s := StorageView{State: StateUnavailable, Reconciled: reconciled}
	if sAge != nil {
		s.Age = secs(*sAge)
		s.State = ClassifyStorage(s.Age)
	}
	if s.State != StateUnavailable {
		s.Torrents, s.Blobs, s.Indexes, s.Other = nullInt(st[opstats.MTorrentsTable]), nullInt(st[opstats.MBlobsTable]), nullInt(st[opstats.MIndexes]), nullInt(st[opstats.MOther])
		s.Magneticod, s.Ingest, s.Database = nullInt(st[opstats.MMagneticodTotal]), nullInt(st[opstats.MIngestTotal]), nullInt(st[opstats.MDatabaseTotal])
		if st[opstats.MIndexedTorrents] != nil {
			s.IndexedCount = estimated(*st[opstats.MIndexedTorrents])
		}
		// Bytes per torrent comes from ONE sample: the total and the estimate of the same row.
		if s.Magneticod.Valid && s.IndexedCount.Valid && s.IndexedCount.Value > 0 {
			s.BytesPerItem = estimated(float64(s.Magneticod.Value) / float64(s.IndexedCount.Value))
		}
	}
	return q, s, nil
}

func (r *Reader) thisHour(ctx context.Context) (t time.Time, v [metricCount]int64, err error) {
	var sel = `bucket_start, records_received, records_enqueued, records_acknowledged,
settled_stored_new + settled_stored_existing + settled_quarantined, exists_hashes, bytes_body`
	err = r.db.QueryRow(ctx, `SELECT `+sel+` FROM magnetico_stats.agg_hour WHERE bucket_start = date_trunc('hour', now(), 'UTC')`).
		Scan(&t, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5])
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, v, nil
	}
	return
}

func (r *Reader) users(ctx context.Context) Users {
	var total, active, admins int64
	if err := r.db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE active), count(*) FILTER (WHERE role = 'admin') FROM magnetico_web.users`).
		Scan(&total, &active, &admins); err != nil {
		return Users{} // optional: unavailable, never zero
	}
	return Users{Total: known(total), Active: known(active), Admins: known(admins)}
}

// overview assembles the dashboard state from one set of queries.
func (r *Reader) overview(ctx context.Context) (Overview, error) {
	state, err := r.schemaState(ctx)
	if err != nil {
		return Overview{}, err
	}
	o := Overview{Schema: state, Users: r.users(ctx)}
	if state != StateUp {
		return o, nil
	}
	if o.Instances, err = r.instances(ctx); err != nil {
		return Overview{}, err
	}
	o.Rates = computeRates(o.Instances)
	if o.Queue, o.Storage, err = r.shared(ctx); err != nil {
		return Overview{}, err
	}
	if o.ThisHourAt, o.ThisHour, err = r.thisHour(ctx); err != nil {
		return Overview{}, err
	}
	return o, nil
}

// ---- series ----------------------------------------------------------------

func aggTable(gran string) string {
	switch gran {
	case GranDay:
		return "agg_hour"
	case GranMonth:
		return "agg_day"
	}
	return "agg_month"
}

func storageTable(gran string) string {
	return "storage_" + map[string]string{GranDay: "hour", GranMonth: "day", GranYear: "month"}[gran]
}

func (r *Reader) aggRows(ctx context.Context, gran string, start, end time.Time) (map[time.Time]*bucketRow, error) {
	cols := []string{"bucket_start"}
	for c := opstats.Counter(0); c < opstats.NumCounters; c++ {
		cols = append(cols, c.Name())
	}
	for g := opstats.Gauge(0); g < opstats.NumGauges; g++ {
		n := g.Name()
		cols = append(cols, n+"_sum", n+"_n", n+"_min", n+"_max", n+"_last")
	}
	if gran == GranDay {
		cols = append(cols, "bit_count(covered_mask::bit(64))")
	} else {
		cols = append(cols, "covered_minutes")
	}
	rows, err := r.db.Query(ctx, `SELECT `+strings.Join(cols, ", ")+` FROM magnetico_stats.`+aggTable(gran)+
		` WHERE bucket_start >= $1 AND bucket_start < $2 ORDER BY bucket_start`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[time.Time]*bucketRow{}
	for rows.Next() {
		br := &bucketRow{}
		dest := []any{&br.start}
		for c := range br.c {
			dest = append(dest, &br.c[c])
		}
		type gv struct{ min, max, last *int64 }
		gvs := make([]gv, opstats.NumGauges)
		for g := range br.g {
			dest = append(dest, &br.g[g].Sum, &br.g[g].N, &gvs[g].min, &gvs[g].max, &gvs[g].last)
		}
		var cov int64
		dest = append(dest, &cov)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		for g := range gvs {
			if gvs[g].min != nil && gvs[g].max != nil {
				br.g[g].Min, br.g[g].Max, br.g[g].Valid = *gvs[g].min, *gvs[g].max, true
				if gvs[g].last != nil {
					br.g[g].Last = *gvs[g].last
				}
			}
		}
		br.covered = int(cov)
		br.start = br.start.UTC()
		out[br.start] = br
	}
	return out, rows.Err()
}

func (r *Reader) storageRows(ctx context.Context, gran string, start, end time.Time) (map[time.Time]*storageRow, error) {
	cols := []string{"bucket_start", "bpt_bytes_sum", "bpt_torrents_sum", "samples"}
	for m := opstats.Measure(0); m < opstats.NumMeasures; m++ {
		cols = append(cols, m.Name()+"_last", m.Name()+"_max")
	}
	rows, err := r.db.Query(ctx, `SELECT `+strings.Join(cols, ", ")+` FROM magnetico_stats.`+storageTable(gran)+
		` WHERE bucket_start >= $1 AND bucket_start < $2 ORDER BY bucket_start`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[time.Time]*storageRow{}
	for rows.Next() {
		sr := &storageRow{}
		dest := []any{&sr.start, &sr.bptB, &sr.bptT, &sr.samples}
		for m := range sr.last {
			dest = append(dest, &sr.last[m], &sr.max[m])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		sr.start = sr.start.UTC()
		out[sr.start] = sr
	}
	return out, rows.Err()
}

// bounds returns the first and last stored bucket of a granularity.
func (r *Reader) bounds(ctx context.Context, gran string) (first, last time.Time, ok bool, err error) {
	var f, l *time.Time
	if err = r.db.QueryRow(ctx, `SELECT min(bucket_start), max(bucket_start) FROM magnetico_stats.`+aggTable(gran)).Scan(&f, &l); err != nil {
		return
	}
	if f == nil || l == nil {
		return time.Time{}, time.Time{}, false, nil
	}
	return f.UTC(), l.UTC(), true, nil
}

func (r *Reader) quotaMax(ctx context.Context) Measured[int64] {
	var v *int64
	if err := r.db.QueryRow(ctx, `SELECT CASE WHEN quota_max_bytes > 0 THEN quota_max_bytes END FROM magnetico_stats.shared_state WHERE id = 1`).Scan(&v); err != nil {
		return Measured[int64]{}
	}
	return nullInt(v)
}
