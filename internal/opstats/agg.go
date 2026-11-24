package opstats

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- merging --------------------------------------------------------------

// merge folds src into g. newer says src's samples are not older than g's, so
// its Last wins.
func (g *GaugeAgg) merge(src GaugeAgg, newer bool) {
	if !src.Valid {
		return
	}
	if !g.Valid {
		*g = src
		return
	}
	g.Sum, g.N = AddSat(g.Sum, src.Sum), AddSat(g.N, src.N)
	g.Min, g.Max = min(g.Min, src.Min), max(g.Max, src.Max)
	if newer {
		g.Last = src.Last
	}
}

func (s *StorageAgg) merge(src *StorageAgg) {
	if src.Samples == 0 {
		return
	}
	newer := !src.LastAt.Before(s.LastAt)
	for m := Measure(0); m < NumMeasures; m++ {
		if src.LastValid[m] && (newer || !s.LastValid[m]) {
			s.Last[m], s.LastValid[m] = src.Last[m], true
		}
		if src.MaxValid[m] {
			if !s.MaxValid[m] || src.Max[m] > s.Max[m] {
				s.Max[m], s.MaxValid[m] = src.Max[m], true
			}
		}
		s.Sum[m], s.N[m] = AddSat(s.Sum[m], src.Sum[m]), AddSat(s.N[m], src.N[m])
	}
	s.BPTBytes, s.BPTTorrents = AddSat(s.BPTBytes, src.BPTBytes), AddSat(s.BPTTorrents, src.BPTTorrents)
	s.Samples, s.Mismatches = AddSat(s.Samples, src.Samples), AddSat(s.Mismatches, src.Mismatches)
	if newer {
		s.LastAt = src.LastAt
	}
}

// Merge folds src into h (the same hour).
func (h *HourDelta) Merge(src *HourDelta) {
	for c := range h.Counters {
		h.Counters[c] = AddSat(h.Counters[c], src.Counters[c])
	}
	newer := !src.GaugeLastAt.Before(h.GaugeLastAt)
	for g := range h.Gauges {
		h.Gauges[g].merge(src.Gauges[g], newer)
	}
	if newer && !src.GaugeLastAt.IsZero() {
		h.GaugeLastAt = src.GaugeLastAt
	}
	h.Coverage |= src.Coverage
	h.Storage.merge(&src.Storage)
}

// Merge folds the activity of a later drain into c, timestamp-aware: the
// first/last seen times take the minimum/maximum, the address follows the
// newest sighting, the submit time follows the newest submit independently of
// last_seen, and the counters add.
func (c *CrawlerDelta) Merge(src *CrawlerDelta) {
	if src.FirstSeen.Before(c.FirstSeen) {
		c.FirstSeen = src.FirstSeen
	}
	if src.LastSeen.After(c.LastSeen) {
		c.LastSeen = src.LastSeen
		if src.LastIP.IsValid() {
			c.LastIP = src.LastIP
		}
	} else if !c.LastIP.IsValid() {
		c.LastIP = src.LastIP
	}
	if src.LastSubmit.After(c.LastSubmit) {
		c.LastSubmit = src.LastSubmit
	}
	c.RecordsReceived = AddSat(c.RecordsReceived, src.RecordsReceived)
	c.RecordsEnqueued = AddSat(c.RecordsEnqueued, src.RecordsEnqueued)
	c.RecordsDup = AddSat(c.RecordsDup, src.RecordsDup)
	c.BytesBody = AddSat(c.BytesBody, src.BytesBody)
	c.ExistsRequests = AddSat(c.ExistsRequests, src.ExistsRequests)
	c.ExistsHashes = AddSat(c.ExistsHashes, src.ExistsHashes)
	c.Rejected = AddSat(c.Rejected, src.Rejected)
}

// hour returns the delta of an hour, creating it in order.
func (d *Delta) hour(t time.Time) *HourDelta {
	i := sort.Search(len(d.Hours), func(i int) bool { return !d.Hours[i].Hour.Before(t) })
	if i < len(d.Hours) && d.Hours[i].Hour.Equal(t) {
		return &d.Hours[i]
	}
	d.Hours = append(d.Hours, HourDelta{})
	copy(d.Hours[i+1:], d.Hours[i:])
	d.Hours[i] = HourDelta{Hour: t}
	return &d.Hours[i]
}

// Merge folds src (a later drain) into dst. The result is bounded: at most
// MaxLiveHours hours (the oldest are dropped and counted) and at most
// MaxCrawlersPerDrain crawlers (the rest are counted as overflow).
func Merge(dst, src *Delta) {
	for i := range src.Hours {
		dst.hour(src.Hours[i].Hour).Merge(&src.Hours[i])
	}
	for len(dst.Hours) > MaxLiveHours {
		dst.Hours = dst.Hours[1:]
		dst.HoursDropped++
	}
	byID := make(map[string]int, len(dst.Crawlers))
	for i, c := range dst.Crawlers {
		byID[c.ID] = i
	}
	for i := range src.Crawlers {
		sc := src.Crawlers[i]
		if j, ok := byID[sc.ID]; ok {
			dst.Crawlers[j].Merge(&sc)
			continue
		}
		if len(dst.Crawlers) >= MaxCrawlersPerDrain {
			dst.CrawlersOverflow++
			continue
		}
		byID[sc.ID] = len(dst.Crawlers)
		dst.Crawlers = append(dst.Crawlers, sc)
	}
	sort.Slice(dst.Crawlers, func(i, j int) bool { return dst.Crawlers[i].ID < dst.Crawlers[j].ID })
	dst.CrawlersOverflow = AddSat(dst.CrawlersOverflow, src.CrawlersOverflow)
	dst.HoursDropped = AddSat(dst.HoursDropped, src.HoursDropped)
	dst.InFlightAtDrain = src.InFlightAtDrain
	dst.ClockAnomaly = dst.ClockAnomaly || src.ClockAnomaly
	mergeSlots(&dst.QueueFirstSlot, &dst.QueueLastSlot, src.QueueFirstSlot, src.QueueLastSlot)
	mergeSlots(&dst.StorageFirstSlot, &dst.StorageLastSlot, src.StorageFirstSlot, src.StorageLastSlot)
	if src.LatestQueue != nil && (dst.LatestQueue == nil || !src.LatestQueue.At.Before(dst.LatestQueue.At)) {
		q := *src.LatestQueue
		dst.LatestQueue = &q
	}
	if src.LatestStorage != nil && (dst.LatestStorage == nil || !src.LatestStorage.At.Before(dst.LatestStorage.At)) {
		s := *src.LatestStorage
		dst.LatestStorage = &s
	}
}

func mergeSlots(first, last *int64, sf, sl int64) {
	if sf == 0 && sl == 0 {
		return
	}
	if *first == 0 || sf < *first {
		*first = sf
	}
	if sl > *last {
		*last = sl
	}
}

// AddQueueSample folds one queue sample into the gauge aggregates of its UTC
// hour and records its slot and the latest state.
func (d *Delta) AddQueueSample(q QueueState) {
	h := d.hour(HourOf(q.At))
	for g := Gauge(0); g < NumGauges; g++ {
		if !q.Valid[g] {
			continue
		}
		v := q.Values[g]
		one := GaugeAgg{Sum: max(v, 0), N: 1, Min: v, Max: v, Last: v, Valid: true}
		h.Gauges[g].merge(one, !q.At.Before(h.GaugeLastAt))
	}
	if q.At.After(h.GaugeLastAt) {
		h.GaugeLastAt = q.At
	}
	mergeSlots(&d.QueueFirstSlot, &d.QueueLastSlot, q.Slot, q.Slot)
	if d.LatestQueue == nil || !q.At.Before(d.LatestQueue.At) {
		c := q
		d.LatestQueue = &c
	}
}

// AddStorageSample folds one storage sample into the aggregates of its UTC hour.
func (d *Delta) AddStorageSample(s StorageState) {
	h := d.hour(HourOf(s.At))
	one := StorageAgg{Samples: 1, LastAt: s.At}
	for m := Measure(0); m < NumMeasures; m++ {
		if !s.Valid[m] {
			continue
		}
		v := max(s.Values[m], 0)
		one.Last[m], one.LastValid[m], one.Max[m], one.MaxValid[m], one.Sum[m], one.N[m] = v, true, v, true, v, 1
	}
	if s.Valid[MMagneticodTotal] && s.Valid[MIndexedTorrents] && s.Values[MIndexedTorrents] > 0 {
		one.BPTBytes, one.BPTTorrents = max(s.Values[MMagneticodTotal], 0), s.Values[MIndexedTorrents]
	}
	if !s.Reconciled {
		one.Mismatches = 1
	}
	h.Storage.merge(&one)
	mergeSlots(&d.StorageFirstSlot, &d.StorageLastSlot, s.Slot, s.Slot)
	if d.LatestStorage == nil || !s.At.Before(d.LatestStorage.At) {
		c := s
		d.LatestStorage = &c
	}
}

// ---- SQL ----------------------------------------------------------------------

const satLimit = "4611686018427387904" // 2^62

// sat is the SQL for a saturating add of EXCLUDED.col to t.col (operands are
// non-negative and at most 2^62, so nothing overflows).
func sat(t, col string) string {
	return fmt.Sprintf("CASE WHEN %[1]s.%[2]s >= %[3]s - EXCLUDED.%[2]s THEN %[3]s ELSE %[1]s.%[2]s + EXCLUDED.%[2]s END", t, col, satLimit)
}

func aggUpsertSQL(unit string) string {
	t := "agg_" + unit
	cols := []string{"bucket_start"}
	sets := []string{}
	for c := Counter(0); c < NumCounters; c++ {
		cols = append(cols, c.Name())
		sets = append(sets, c.Name()+" = "+sat(t, c.Name()))
	}
	for g := Gauge(0); g < NumGauges; g++ {
		n := g.Name()
		cols = append(cols, n+"_sum", n+"_n", n+"_min", n+"_max", n+"_last")
		sets = append(sets,
			n+"_sum = "+sat(t, n+"_sum"), n+"_n = "+sat(t, n+"_n"),
			fmt.Sprintf("%[1]s_min = LEAST(%[2]s.%[1]s_min, EXCLUDED.%[1]s_min)", n, t),
			fmt.Sprintf("%[1]s_max = GREATEST(%[2]s.%[1]s_max, EXCLUDED.%[1]s_max)", n, t),
			fmt.Sprintf("%[1]s_last = CASE WHEN EXCLUDED.%[1]s_last IS NOT NULL AND (%[2]s.gauge_last_at IS NULL OR EXCLUDED.gauge_last_at >= %[2]s.gauge_last_at) THEN EXCLUDED.%[1]s_last ELSE %[2]s.%[1]s_last END", n, t))
	}
	cols = append(cols, "gauge_last_at")
	sets = append(sets, "gauge_last_at = GREATEST("+t+".gauge_last_at, EXCLUDED.gauge_last_at)")
	if unit == "hour" {
		cols = append(cols, "covered_mask")
		sets = append(sets, "covered_mask = "+t+".covered_mask | EXCLUDED.covered_mask")
	} else {
		cols = append(cols, "covered_minutes")
		limit := 1440
		if unit == "month" {
			limit = 31 * 1440
		}
		sets = append(sets, fmt.Sprintf("covered_minutes = LEAST(%[1]s.covered_minutes + EXCLUDED.covered_minutes, %[2]d)", t, limit))
	}
	ph := make([]string, len(cols))
	for i := range ph {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	return fmt.Sprintf("INSERT INTO %s.%s (%s) VALUES (%s) ON CONFLICT (bucket_start) DO UPDATE SET %s",
		SchemaName, t, strings.Join(cols, ", "), strings.Join(ph, ", "), strings.Join(sets, ", "))
}

func nullInt(valid bool, v int64) any {
	if !valid {
		return nil
	}
	return v
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func aggArgs(bucket time.Time, h *HourDelta, coverage int64) []any {
	args := []any{bucket}
	for c := Counter(0); c < NumCounters; c++ {
		args = append(args, h.Counters[c])
	}
	for g := Gauge(0); g < NumGauges; g++ {
		a := h.Gauges[g]
		args = append(args, a.Sum, a.N, nullInt(a.Valid, a.Min), nullInt(a.Valid, a.Max), nullInt(a.Valid, a.Last))
	}
	return append(args, nullTime(h.GaugeLastAt), coverage)
}

func storageUpsertSQL(unit string) string {
	t := "storage_" + unit
	cols := []string{"bucket_start"}
	var sets []string
	for m := Measure(0); m < NumMeasures; m++ {
		n := m.Name()
		cols = append(cols, n+"_last", n+"_max", n+"_sum", n+"_n")
		sets = append(sets,
			fmt.Sprintf("%[1]s_last = CASE WHEN EXCLUDED.%[1]s_last IS NOT NULL AND (%[2]s.last_sample_at IS NULL OR EXCLUDED.last_sample_at >= %[2]s.last_sample_at) THEN EXCLUDED.%[1]s_last ELSE %[2]s.%[1]s_last END", n, t),
			fmt.Sprintf("%[1]s_max = GREATEST(%[2]s.%[1]s_max, EXCLUDED.%[1]s_max)", n, t),
			n+"_sum = "+sat(t, n+"_sum"), n+"_n = "+sat(t, n+"_n"))
	}
	for _, c := range []string{"bpt_bytes_sum", "bpt_torrents_sum", "samples", "reconciliation_mismatches"} {
		cols = append(cols, c)
		sets = append(sets, c+" = "+sat(t, c))
	}
	cols = append(cols, "last_sample_at")
	sets = append(sets, "last_sample_at = GREATEST("+t+".last_sample_at, EXCLUDED.last_sample_at)")
	ph := make([]string, len(cols))
	for i := range ph {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	return fmt.Sprintf("INSERT INTO %s.%s (%s) VALUES (%s) ON CONFLICT (bucket_start) DO UPDATE SET %s",
		SchemaName, t, strings.Join(cols, ", "), strings.Join(ph, ", "), strings.Join(sets, ", "))
}

func storageArgs(bucket time.Time, s *StorageAgg) []any {
	args := []any{bucket}
	for m := Measure(0); m < NumMeasures; m++ {
		args = append(args, nullInt(s.LastValid[m], s.Last[m]), nullInt(s.MaxValid[m], s.Max[m]), s.Sum[m], s.N[m])
	}
	return append(args, s.BPTBytes, s.BPTTorrents, s.Samples, s.Mismatches, nullTime(s.LastAt))
}

var (
	aggSQL     = map[string]string{"hour": aggUpsertSQL("hour"), "day": aggUpsertSQL("day"), "month": aggUpsertSQL("month")}
	storageSQL = map[string]string{"hour": storageUpsertSQL("hour"), "day": storageUpsertSQL("day"), "month": storageUpsertSQL("month")}
)

const activityUpsertSQL = `
INSERT INTO magnetico_stats.crawler_activity AS t
    (crawler_id, first_seen_at, last_seen_at, last_ip, last_submit_at, records_received, records_enqueued,
     records_duplicate, bytes_body, exists_requests, exists_hashes, rejected_requests)
SELECT * FROM unnest($1::uuid[], $2::timestamptz[], $3::timestamptz[], $4::inet[], $5::timestamptz[],
    $6::bigint[], $7::bigint[], $8::bigint[], $9::bigint[], $10::bigint[], $11::bigint[], $12::bigint[])
ORDER BY 1
ON CONFLICT (crawler_id) DO UPDATE SET
    first_seen_at = LEAST(t.first_seen_at, EXCLUDED.first_seen_at),
    last_seen_at = GREATEST(t.last_seen_at, EXCLUDED.last_seen_at),
    last_ip = CASE WHEN EXCLUDED.last_seen_at > t.last_seen_at THEN COALESCE(EXCLUDED.last_ip, t.last_ip) ELSE t.last_ip END,
    last_submit_at = GREATEST(t.last_submit_at, EXCLUDED.last_submit_at),
    records_received = ` + "CASE WHEN t.records_received >= 4611686018427387904 - EXCLUDED.records_received THEN 4611686018427387904 ELSE t.records_received + EXCLUDED.records_received END" + `,
    records_enqueued = ` + "CASE WHEN t.records_enqueued >= 4611686018427387904 - EXCLUDED.records_enqueued THEN 4611686018427387904 ELSE t.records_enqueued + EXCLUDED.records_enqueued END" + `,
    records_duplicate = ` + "CASE WHEN t.records_duplicate >= 4611686018427387904 - EXCLUDED.records_duplicate THEN 4611686018427387904 ELSE t.records_duplicate + EXCLUDED.records_duplicate END" + `,
    bytes_body = ` + "CASE WHEN t.bytes_body >= 4611686018427387904 - EXCLUDED.bytes_body THEN 4611686018427387904 ELSE t.bytes_body + EXCLUDED.bytes_body END" + `,
    exists_requests = ` + "CASE WHEN t.exists_requests >= 4611686018427387904 - EXCLUDED.exists_requests THEN 4611686018427387904 ELSE t.exists_requests + EXCLUDED.exists_requests END" + `,
    exists_hashes = ` + "CASE WHEN t.exists_hashes >= 4611686018427387904 - EXCLUDED.exists_hashes THEN 4611686018427387904 ELSE t.exists_hashes + EXCLUDED.exists_hashes END" + `,
    rejected_requests = ` + "CASE WHEN t.rejected_requests >= 4611686018427387904 - EXCLUDED.rejected_requests THEN 4611686018427387904 ELSE t.rejected_requests + EXCLUDED.rejected_requests END"

// ApplyResult says which shared-gauge parts were applied.
type ApplyResult struct {
	QueueApplied, StorageApplied bool
	// QueueSkipped / StorageSkipped: a shared-gauge part overlapped a slot that
	// an earlier batch (possibly another instance) had already applied.
	QueueSkipped, StorageSkipped bool
}

// ensureRow makes sure a row exists for the bucket (a concurrent creator's
// uncommitted insert is waited for) and returns after the caller can lock it.
func hourMaskForUpdate(ctx context.Context, tx pgx.Tx, bucket time.Time) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO magnetico_stats.agg_hour (bucket_start) VALUES ($1) ON CONFLICT DO NOTHING`, bucket); err != nil {
		return 0, err
	}
	var mask int64
	if err := tx.QueryRow(ctx, `SELECT covered_mask FROM magnetico_stats.agg_hour WHERE bucket_start = $1 FOR UPDATE`, bucket).Scan(&mask); err != nil {
		return 0, err
	}
	return mask, nil
}

// stripShared returns a copy of the hours without the gauge or storage parts.
func stripShared(hours []HourDelta, gauges, storage bool) []HourDelta {
	out := make([]HourDelta, len(hours))
	copy(out, hours)
	for i := range out {
		if gauges {
			out[i].Gauges = [NumGauges]GaugeAgg{}
			out[i].GaugeLastAt = time.Time{}
		}
		if storage {
			out[i].Storage = StorageAgg{}
		}
	}
	return out
}

// Apply writes a delta inside tx. Row locks are taken in one global order so
// concurrent writers cannot deadlock: shared_state, then every hour row
// (ascending), then every day row, then every month row, then the crawler
// rows (ascending by id).
//
// The shared-gauge parts (queue and storage samples, and shared_state) are
// applied only if their first slot is greater than the marker of the last
// applied slot, and the same statement advances the marker: a batch that
// overlaps an already applied period (a handover between instances) has that
// part skipped, so two instances can never contribute the same period twice.
// Event counters are applied regardless.
//
// Coverage is a union: the hour row ORs the minute bits; a day or month row
// counts only the bits that were new in the hour row.
func Apply(ctx context.Context, tx pgx.Tx, d *Delta) (ApplyResult, error) {
	var res ApplyResult
	hasQueueGauges := d.QueueLastSlot != 0
	hasStorage := d.StorageLastSlot != 0

	if d.LatestQueue != nil {
		q := d.LatestQueue
		args := []any{d.QueueFirstSlot, d.QueueLastSlot, q.At, q.QuotaMax, q.CountsApprox, q.OldestApprox, q.Backoff}
		sets := []string{"gauge_last_slot = $2", "queue_sampled_at = $3", "quota_max_bytes = $4", "queue_counts_approx = $5", "oldest_age_approx = $6", "queue_interval_backoff = $7"}
		for g := Gauge(0); g < NumGauges; g++ {
			args = append(args, nullInt(q.Valid[g], q.Values[g]))
			sets = append(sets, fmt.Sprintf("%s = $%d", g.Name(), len(args)))
		}
		tag, err := tx.Exec(ctx, `UPDATE magnetico_stats.shared_state SET `+strings.Join(sets, ", ")+
			` WHERE id = 1 AND gauge_last_slot < $1`, args...)
		if err != nil {
			return res, err
		}
		res.QueueApplied = tag.RowsAffected() == 1
		res.QueueSkipped = !res.QueueApplied
	}
	if d.LatestStorage != nil {
		s := d.LatestStorage
		args := []any{d.StorageFirstSlot, d.StorageLastSlot, s.At, s.Reconciled}
		sets := []string{"storage_last_slot = $2", "storage_sampled_at = $3", "storage_reconciled = $4"}
		for m := Measure(0); m < NumMeasures; m++ {
			args = append(args, nullInt(s.Valid[m], s.Values[m]))
			sets = append(sets, fmt.Sprintf("storage_%s = $%d", m.Name(), len(args)))
		}
		tag, err := tx.Exec(ctx, `UPDATE magnetico_stats.shared_state SET `+strings.Join(sets, ", ")+
			` WHERE id = 1 AND storage_last_slot < $1`, args...)
		if err != nil {
			return res, err
		}
		res.StorageApplied = tag.RowsAffected() == 1
		res.StorageSkipped = !res.StorageApplied
	}
	hours := d.Hours
	if res.QueueSkipped || res.StorageSkipped {
		hours = stripShared(hours, res.QueueSkipped && hasQueueGauges, res.StorageSkipped && hasStorage)
	}

	// Phase 1: hours, ascending.
	type rollup struct {
		delta HourDelta
		bits  int64
	}
	days, months := map[time.Time]*rollup{}, map[time.Time]*rollup{}
	var dayKeys, monthKeys []time.Time
	for i := range hours {
		h := &hours[i]
		old, err := hourMaskForUpdate(ctx, tx, h.Hour)
		if err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, aggSQL["hour"], aggArgs(h.Hour, h, h.Coverage)...); err != nil {
			return res, err
		}
		newBits := int64(BitCount(h.Coverage &^ old))
		for _, bucket := range []struct {
			key time.Time
			m   map[time.Time]*rollup
			ks  *[]time.Time
		}{{DayOf(h.Hour), days, &dayKeys}, {MonthOf(h.Hour), months, &monthKeys}} {
			r := bucket.m[bucket.key]
			if r == nil {
				r = &rollup{delta: HourDelta{Hour: bucket.key}}
				bucket.m[bucket.key] = r
				*bucket.ks = append(*bucket.ks, bucket.key)
			}
			cov := r.delta.Coverage // unused for days and months: they count new bits instead
			r.delta.Merge(h)
			r.delta.Coverage = cov
			r.bits += newBits
		}
	}
	// Phase 2 and 3: days, then months, each ascending.
	for _, unit := range []struct {
		name string
		m    map[time.Time]*rollup
		ks   []time.Time
	}{{"day", days, dayKeys}, {"month", months, monthKeys}} {
		sort.Slice(unit.ks, func(i, j int) bool { return unit.ks[i].Before(unit.ks[j]) })
		for _, k := range unit.ks {
			r := unit.m[k]
			if _, err := tx.Exec(ctx, aggSQL[unit.name], aggArgs(k, &r.delta, r.bits)...); err != nil {
				return res, err
			}
		}
	}
	// Storage tables, in the same hour → day → month order.
	for _, unit := range []struct {
		name string
		key  func(time.Time) time.Time
	}{{"hour", func(t time.Time) time.Time { return t }}, {"day", DayOf}, {"month", MonthOf}} {
		agg := map[time.Time]*StorageAgg{}
		var keys []time.Time
		for i := range hours {
			if hours[i].Storage.IsZero() {
				continue
			}
			k := unit.key(hours[i].Hour)
			a := agg[k]
			if a == nil {
				a = &StorageAgg{}
				agg[k] = a
				keys = append(keys, k)
			}
			a.merge(&hours[i].Storage)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })
		for _, k := range keys {
			if _, err := tx.Exec(ctx, storageSQL[unit.name], storageArgs(k, agg[k])...); err != nil {
				return res, err
			}
		}
	}

	// Crawler activity, ascending by id.
	if len(d.Crawlers) > 0 {
		n := len(d.Crawlers)
		ids, first, last, ips, submit := make([]string, n), make([]time.Time, n), make([]time.Time, n), make([]any, n), make([]any, n)
		cols := make([][]int64, 7)
		for i := range cols {
			cols[i] = make([]int64, n)
		}
		for i, c := range d.Crawlers {
			ids[i], first[i], last[i] = c.ID, c.FirstSeen, c.LastSeen
			if c.LastIP.IsValid() {
				ips[i] = c.LastIP.String()
			}
			submit[i] = nullTime(c.LastSubmit)
			cols[0][i], cols[1][i], cols[2][i], cols[3][i], cols[4][i], cols[5][i], cols[6][i] =
				c.RecordsReceived, c.RecordsEnqueued, c.RecordsDup, c.BytesBody, c.ExistsRequests, c.ExistsHashes, c.Rejected
		}
		if _, err := tx.Exec(ctx, activityUpsertSQL, ids, first, last, ips, submit,
			cols[0], cols[1], cols[2], cols[3], cols[4], cols[5], cols[6]); err != nil {
			return res, err
		}
	}
	return res, nil
}
