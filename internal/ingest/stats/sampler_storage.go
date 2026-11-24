package stats

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/opstats"
)

var storageLimits = samplerLimits{statement: 5 * time.Second, lock: 2 * time.Second, cycle: 20 * time.Second}

// storageSamplerLoop samples the storage sizes every StorageInterval, only
// while this process owns the queue consumer.
func (w *Writer) storageSamplerLoop(ctx context.Context) {
	first := true
	for {
		d := w.opt.StorageInterval
		if first {
			d = min(d, 15*time.Second) // a first sample soon after start
			first = false
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if w.owner == nil || !w.owner.Owns() {
			continue
		}
		s, err := sampleStorage(ctx, w.samplePool, w.now(), storageLimits)
		if err != nil {
			if ctx.Err() == nil {
				w.logChange("storage-sample", err.Error(), "statistics: sampling the storage sizes failed (%v); the previous sample stays and shows its age", err)
			}
			continue
		}
		w.logChange("storage-sample", "ok", "statistics: storage sampling works")
		w.addStorageSample(s)
	}
}

// The storage measures (docs/specs/statistics-v1.md). 1–4 are non-overlapping
// categories of schema magneticod; 5 is computed by an independent expression
// and equals their sum exactly.
const (
	sqlTorrentsTable = `SELECT pg_table_size('magneticod.torrents')`
	sqlBlobsTable    = `SELECT pg_table_size('magneticod.torrent_file_blobs')`
	// every ordinary index of every table or materialised view of the schema
	sqlIndexes = `SELECT COALESCE(sum(pg_indexes_size(c.oid)), 0)::bigint FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'magneticod' AND c.relkind IN ('r', 'm')`
	// every other table or materialised view (heap + TOAST, no indexes) and every sequence
	sqlOther = `SELECT (COALESCE(sum(pg_table_size(c.oid)) FILTER (WHERE c.relkind IN ('r', 'm') AND c.relname NOT IN ('torrents', 'torrent_file_blobs')), 0)
  + COALESCE(sum(pg_relation_size(c.oid)) FILTER (WHERE c.relkind = 'S'), 0))::bigint
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'magneticod'`
	sqlMagneticodTotal = `SELECT COALESCE(sum(pg_total_relation_size(c.oid)), 0)::bigint FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'magneticod' AND c.relkind IN ('r', 'm', 'S')`
	sqlIngestTotal = `SELECT COALESCE(sum(pg_total_relation_size(c.oid)), 0)::bigint FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'magnetico_ingest' AND c.relkind IN ('r', 'm', 'S')`
	sqlDatabaseTotal = `SELECT pg_database_size(current_database())`
	// an estimate: n_live_tup, else reltuples; unknown (NULL) when neither is known
	sqlIndexedTorrents = `SELECT CASE WHEN s.n_live_tup > 0 THEN s.n_live_tup WHEN c.reltuples > 0 THEN c.reltuples::bigint END
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace LEFT JOIN pg_stat_all_tables s ON s.relid = c.oid
WHERE n.nspname = 'magneticod' AND c.relname = 'torrents'`
)

var storageQueries = [opstats.NumMeasures]string{
	opstats.MTorrentsTable: sqlTorrentsTable, opstats.MBlobsTable: sqlBlobsTable, opstats.MIndexes: sqlIndexes,
	opstats.MOther: sqlOther, opstats.MMagneticodTotal: sqlMagneticodTotal, opstats.MIngestTotal: sqlIngestTotal,
	opstats.MDatabaseTotal: sqlDatabaseTotal, opstats.MIndexedTorrents: sqlIndexedTorrents,
}

// sampleStorage measures the eight storage quantities. Each runs in its own
// savepoint, so one failure makes only that measure unavailable. The
// reconciliation (1+2+3+4 = 5) is evaluated only when those five are all valid;
// a mismatch (concurrent DDL between the reads) is retried once.
func sampleStorage(ctx context.Context, pool *pgxpool.Pool, at time.Time, lim samplerLimits) (opstats.StorageState, error) {
	s := opstats.StorageState{At: at, Slot: at.Unix() / storageSlotSeconds, Reconciled: true}
	for attempt := 0; attempt < 2; attempt++ {
		s.Values, s.Valid = [opstats.NumMeasures]int64{}, [opstats.NumMeasures]bool{}
		err := runSampled(ctx, pool, lim, func(t *sampledTx) error {
			ok := 0
			for m := opstats.Measure(0); m < opstats.NumMeasures; m++ {
				var v *int64
				if err := t.try(storageQueries[m], nil, &v); err == nil && v != nil {
					s.Values[m], s.Valid[m] = *v, true
					ok++
				}
			}
			if ok == 0 {
				return errors.New("no storage measure could be read")
			}
			return nil
		})
		if err != nil {
			return opstats.StorageState{}, err
		}
		s.Reconciled = true
		if s.Valid[opstats.MTorrentsTable] && s.Valid[opstats.MBlobsTable] && s.Valid[opstats.MIndexes] && s.Valid[opstats.MOther] && s.Valid[opstats.MMagneticodTotal] {
			sum := s.Values[opstats.MTorrentsTable] + s.Values[opstats.MBlobsTable] + s.Values[opstats.MIndexes] + s.Values[opstats.MOther]
			s.Reconciled = sum == s.Values[opstats.MMagneticodTotal]
		}
		if s.Reconciled {
			break
		}
	}
	return s, nil
}
