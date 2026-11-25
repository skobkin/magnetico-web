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
				w.logChange("storage-sample", errClass(err), "statistics: sampling the storage sizes failed (%s); the previous sample stays and shows its age", safeErr(err))
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

// sampleStorage measures the eight storage quantities (see sampleStorageQueries).
func sampleStorage(ctx context.Context, pool *pgxpool.Pool, at time.Time, lim samplerLimits) (opstats.StorageState, error) {
	return sampleStorageQueries(ctx, pool, at, lim, storageQueries)
}

// sampleStorageQueries measures the eight storage quantities with the given
// queries. Each runs in its own savepoint, so one failure makes only that
// measure unavailable. The reconciliation (1+2+3+4 = 5) is evaluated only when
// those five are all valid: otherwise it is "not evaluated", which is neither a
// success nor a mismatch. A mismatch (concurrent DDL between the reads) is
// retried once, but the whole sample, retry included, shares ONE deadline
// (lim.cycle): a first read that did not reconcile is kept, flagged, when the
// retry does not finish in time, and a half-finished retry is never used.
func sampleStorageQueries(ctx context.Context, pool *pgxpool.Pool, at time.Time, lim samplerLimits, queries [opstats.NumMeasures]string) (opstats.StorageState, error) {
	total, cancel := context.WithTimeout(ctx, lim.cycle)
	defer cancel()
	var best opstats.StorageState
	have := false
	for attempt := 0; attempt < 2; attempt++ {
		s, err := storageAttempt(total, pool, at, lim, queries)
		if attempt > 0 && (err != nil || total.Err() != nil) {
			break // the retry did not complete inside the deadline: keep the first read
		}
		if err != nil {
			return opstats.StorageState{}, err
		}
		best, have = s, true
		if s.Reconciliation != opstats.ReconMismatch {
			break
		}
	}
	if !have {
		return opstats.StorageState{}, errors.New("storage sample failed")
	}
	return best, nil
}

func storageAttempt(ctx context.Context, pool *pgxpool.Pool, at time.Time, lim samplerLimits, queries [opstats.NumMeasures]string) (opstats.StorageState, error) {
	s := opstats.StorageState{At: at, Slot: at.Unix() / storageSlotSeconds}
	err := runSampled(ctx, pool, lim, func(t *sampledTx) error {
		ok := 0
		for m := opstats.Measure(0); m < opstats.NumMeasures; m++ {
			var v *int64
			if err := t.try(queries[m], nil, &v); err == nil && v != nil {
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
	if s.Valid[opstats.MTorrentsTable] && s.Valid[opstats.MBlobsTable] && s.Valid[opstats.MIndexes] && s.Valid[opstats.MOther] && s.Valid[opstats.MMagneticodTotal] {
		sum := s.Values[opstats.MTorrentsTable] + s.Values[opstats.MBlobsTable] + s.Values[opstats.MIndexes] + s.Values[opstats.MOther]
		s.Reconciliation = opstats.ReconOK
		if sum != s.Values[opstats.MMagneticodTotal] {
			s.Reconciliation = opstats.ReconMismatch
		}
	}
	return s, nil
}
