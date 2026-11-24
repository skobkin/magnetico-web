package stats

import (
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/opstats"
)

var storageTestLimits = samplerLimits{statement: 10 * time.Second, lock: 2 * time.Second, cycle: 30 * time.Second}

func (e *wenv) sampleStorage() opstats.StorageState {
	e.t.Helper()
	s, err := sampleStorage(bg, e.w.samplePool, e.clock.Now(), storageTestLimits)
	if err != nil {
		e.t.Fatalf("sampleStorage: %v", err)
	}
	return s
}

func (e *wenv) seedTorrents(n int) {
	e.t.Helper()
	e.exec(`INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
SELECT decode(lpad(to_hex(g), 40, '0'), 'hex'), 'torrent ' || repeat('x', 200) || g, 1000, 1700000000, 1 FROM generate_series(1, $1) g`, n)
}

func TestStorageSizesMatchPostgresFunctionsWithoutDoubleCounting(t *testing.T) {
	e := newWEnv(t, nil)
	e.seedTorrents(3000)
	// TOASTed content: values far above the 2 kB threshold, incompressible enough to be stored out of line.
	e.exec(`INSERT INTO magneticod.torrent_file_blobs (torrent_id, data)
SELECT id, convert_to((SELECT string_agg(md5(random()::text || g::text), '') FROM generate_series(1, 400) g), 'UTF8') FROM magneticod.torrents LIMIT 200`)
	e.exec(`ANALYZE`)
	s := e.sampleStorage()
	for m := opstats.Measure(0); m < opstats.NumMeasures; m++ {
		if m != opstats.MIndexedTorrents && !s.Valid[m] {
			t.Fatalf("measure %s unavailable", m.Name())
		}
	}
	v := s.Values
	if !s.Reconciled || v[opstats.MTorrentsTable]+v[opstats.MBlobsTable]+v[opstats.MIndexes]+v[opstats.MOther] != v[opstats.MMagneticodTotal] {
		t.Fatalf("categories do not sum to the total: %v (reconciled %v)", v, s.Reconciled)
	}
	// Independent computation straight from PostgreSQL's own functions.
	if want := e.n(`SELECT sum(pg_total_relation_size(c.oid)) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'magneticod' AND c.relkind IN ('r','m','S')`); v[opstats.MMagneticodTotal] != want {
		t.Fatalf("magneticod total %d, pg_total_relation_size says %d", v[opstats.MMagneticodTotal], want)
	}
	if want := e.n(`SELECT pg_table_size('magneticod.torrent_file_blobs')`); v[opstats.MBlobsTable] != want || want <= e.n(`SELECT pg_relation_size('magneticod.torrent_file_blobs')`)+100_000 {
		t.Fatalf("blobs table %d, pg_table_size %d (TOAST must be inside it)", v[opstats.MBlobsTable], want)
	}
	if v[opstats.MIngestTotal] <= 0 || v[opstats.MDatabaseTotal] < v[opstats.MMagneticodTotal] {
		t.Fatalf("ingest total %d, database total %d", v[opstats.MIngestTotal], v[opstats.MDatabaseTotal])
	}
	if want := e.n(`SELECT sum(pg_total_relation_size(c.oid)) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'magnetico_ingest' AND c.relkind IN ('r','m','S')`); v[opstats.MIngestTotal] != want {
		t.Fatalf("ingest total %d, want %d", v[opstats.MIngestTotal], want)
	}
	if !s.Valid[opstats.MIndexedTorrents] || v[opstats.MIndexedTorrents] != 3000 {
		t.Fatalf("estimate = %d (valid %v), want 3000 after ANALYZE", v[opstats.MIndexedTorrents], s.Valid[opstats.MIndexedTorrents])
	}
}

func TestTorrentEstimateIsUnknownBeforeAnyStatisticsExist(t *testing.T) {
	e := newWEnv(t, nil)
	if s := e.sampleStorage(); s.Valid[opstats.MIndexedTorrents] {
		t.Fatalf("an estimate of %d was reported for a table that was never analysed and is empty", s.Values[opstats.MIndexedTorrents])
	}
}

func TestOtherBytesExcludesIndexesAlreadyInIndexesBytes(t *testing.T) {
	e := newWEnv(t, nil)
	e.seedTorrents(500)
	e.exec(`CREATE TABLE magneticod.extra_indexed (id serial PRIMARY KEY, k text NOT NULL, v text)`)
	e.exec(`CREATE INDEX extra_indexed_k ON magneticod.extra_indexed (k)`)
	e.exec(`INSERT INTO magneticod.extra_indexed (k, v) SELECT md5(g::text), repeat('v', 50) FROM generate_series(1, 20000) g`)
	before := e.sampleStorage()
	// A table with no row growth but a much larger index: other_bytes must not move.
	e.exec(`CREATE INDEX extra_indexed_k2 ON magneticod.extra_indexed (k, v)`)
	after := e.sampleStorage()
	if after.Values[opstats.MOther] != before.Values[opstats.MOther] {
		t.Fatalf("other_bytes changed from %d to %d when only an index was added: indexes leak into it", before.Values[opstats.MOther], after.Values[opstats.MOther])
	}
	if after.Values[opstats.MIndexes] <= before.Values[opstats.MIndexes] {
		t.Fatal("indexes_bytes did not grow with a new index")
	}
	if !after.Reconciled {
		t.Fatal("not reconciled")
	}
}

func TestStorageReconcilesWithAdditionalIndexedTablesMatviewsAndSequences(t *testing.T) {
	e := newWEnv(t, nil)
	e.seedTorrents(1000)
	e.exec(`CREATE TABLE magneticod.extra_a (id serial PRIMARY KEY, payload text)`)
	e.exec(`CREATE INDEX extra_a_payload ON magneticod.extra_a (md5(payload))`)
	e.exec(`INSERT INTO magneticod.extra_a (payload) SELECT repeat(md5(g::text), 300) FROM generate_series(1, 400) g`) // TOASTed
	e.exec(`CREATE MATERIALIZED VIEW magneticod.mv AS SELECT id, md5(payload) AS h FROM magneticod.extra_a`)
	e.exec(`CREATE INDEX mv_h ON magneticod.mv (h)`)
	e.exec(`CREATE SEQUENCE magneticod.standalone_seq`)
	e.exec(`SELECT nextval('magneticod.standalone_seq')`)
	s := e.sampleStorage()
	v := s.Values
	if !s.Reconciled {
		t.Fatalf("not reconciled: %v", v)
	}
	if sum := v[opstats.MTorrentsTable] + v[opstats.MBlobsTable] + v[opstats.MIndexes] + v[opstats.MOther]; sum != v[opstats.MMagneticodTotal] {
		t.Fatalf("1+2+3+4 = %d, total = %d", sum, v[opstats.MMagneticodTotal])
	}
	// indexes_bytes = Σ pg_indexes_size over EVERY table and matview, extra ones included.
	wantIdx := e.n(`SELECT sum(pg_indexes_size(c.oid)) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'magneticod' AND c.relkind IN ('r','m')`)
	if v[opstats.MIndexes] != wantIdx {
		t.Fatalf("indexes_bytes %d, want %d", v[opstats.MIndexes], wantIdx)
	}
	extraIdx := e.n(`SELECT pg_indexes_size('magneticod.extra_a') + pg_indexes_size('magneticod.mv')`)
	if v[opstats.MIndexes] < e.n(`SELECT pg_indexes_size('magneticod.torrents')`)+extraIdx {
		t.Fatal("the indexes of the extra table and the materialised view are missing from indexes_bytes")
	}
	// other_bytes = Σ pg_table_size of the non-main tables and matviews + every sequence — no index byte.
	wantOther := e.n(`SELECT COALESCE(sum(pg_table_size(c.oid)) FILTER (WHERE c.relkind IN ('r','m') AND c.relname NOT IN ('torrents','torrent_file_blobs')), 0)
 + COALESCE(sum(pg_relation_size(c.oid)) FILTER (WHERE c.relkind = 'S'), 0)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'magneticod'`)
	if v[opstats.MOther] != wantOther {
		t.Fatalf("other_bytes %d, want %d", v[opstats.MOther], wantOther)
	}
	if v[opstats.MOther] < e.n(`SELECT pg_table_size('magneticod.extra_a') + pg_table_size('magneticod.mv')`) {
		t.Fatal("the extra table and the materialised view are missing from other_bytes")
	}
	if strings.Contains(e.w.lastErr, "reconcil") {
		t.Fatal(e.w.lastErr)
	}
}

func TestStorageMeasuresFailIndependently(t *testing.T) {
	e := newWEnv(t, nil)
	e.seedTorrents(100)
	e.exec(`ALTER TABLE magneticod.torrent_file_blobs RENAME TO blobs_renamed`) // the blobs measure can no longer be read
	s := e.sampleStorage()
	if s.Valid[opstats.MBlobsTable] {
		t.Fatal("the blobs measure was reported although its table is gone")
	}
	for _, m := range []opstats.Measure{opstats.MTorrentsTable, opstats.MIndexes, opstats.MOther, opstats.MMagneticodTotal, opstats.MIngestTotal, opstats.MDatabaseTotal} {
		if !s.Valid[m] {
			t.Errorf("measure %s was lost because another one failed", m.Name())
		}
	}
	// The reconciliation is judged only when its five inputs are all valid: no false alarm here.
	if !s.Reconciled {
		t.Fatal("a missing input made the sample look unreconciled")
	}
}
