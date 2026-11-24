package cli

// The W9 large synthetic dataset "w9-large-v1" (task 07J, stage S5). Nothing
// here runs in the ordinary suite: it needs MAGNETICO_BENCH_LARGE=1,
// MAGNETICO_BENCH_CONFIRM_DISPOSABLE_DB (the database name, an explicit statement that
// it is disposable), MAGNETICO_BENCH_DSN (the owner DSN of a disposable database whose torrent schema
// already exists) and MAGNETICO_BENCH_DATADIR (the directory holding that
// database's files, for the free-space guard). Synthetic data only.
//
//	population                    count (scale 1)   written how
//	single-file, no blob          3,480,000         SQL generate_series, Zipf-skewed names
//	small multi-file (10-50)         12,000         codec blob via the production encoder
//	medium multi-file (51-500)        6,000         "
//	large multi-file (501-4,000)      1,900         "
//	huge multi-file (75k-225k)           24         "
//
// MAGNETICO_BENCH_SCALE (default 1) scales every count; the pilot runs at 0.01.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/web/benchkit"
	"github.com/skobkin/magnetico-web/internal/web/synth"
)

const harnessMinFreeBytes = 40 << 30

func harnessFreeBytes(t *testing.T, dir string) uint64 {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	return st.Bavail * uint64(st.Bsize)
}

func TestSeedLargeDataset(t *testing.T) {
	if !benchkit.Enabled() {
		t.Skip("set MAGNETICO_BENCH_LARGE=1 (and MAGNETICO_BENCH_DSN, MAGNETICO_BENCH_DATADIR) to seed the benchmark dataset")
	}
	dsn, dir := os.Getenv("MAGNETICO_BENCH_DSN"), os.Getenv("MAGNETICO_BENCH_DATADIR")
	if dsn == "" || dir == "" {
		t.Fatal("MAGNETICO_BENCH_DSN and MAGNETICO_BENCH_DATADIR are required")
	}
	// The seeder writes millions of rows: only into the confirmed, local database.
	if err := harnessCheckTarget(dsn, os.Getenv(harnessConfirmEnv)); err != nil {
		t.Fatalf("refusing to seed MAGNETICO_BENCH_DSN: %v", err)
	}
	scale := 1.0
	if v := os.Getenv("MAGNETICO_BENCH_SCALE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > 1 {
			t.Fatalf("MAGNETICO_BENCH_SCALE=%q", v)
		}
		scale = f
	}
	guard := func(phase string) {
		t.Helper()
		if free := harnessFreeBytes(t, dir); free < harnessMinFreeBytes {
			t.Fatalf("%s: only %d GiB free under %s, the guard is %d GiB", phase, free>>30, dir, harnessMinFreeBytes>>30)
		}
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var existing int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM magneticod.torrents`).Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing != 0 {
		t.Fatalf("the target already holds %d torrents: the seeder only fills an empty database", existing)
	}
	// The marker goes on before the first row: a partially seeded database is
	// still identifiable, and the load harness refuses any database without it.
	if err := harnessMarkDatabase(ctx, conn); err != nil {
		t.Fatalf("marking the benchmark database: %v", err)
	}
	size := func() (db, tbl, idx, toast int64) {
		_ = conn.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&db)
		_ = conn.QueryRow(ctx, `SELECT pg_table_size('magneticod.torrents'), pg_indexes_size('magneticod.torrents'),
		    COALESCE((SELECT pg_total_relation_size('magneticod.torrent_file_blobs')),0)`).Scan(&tbl, &idx, &toast)
		return
	}
	start := time.Now()
	t.Logf("free before: %d GiB, scale %.4g", harnessFreeBytes(t, dir)>>30, scale)

	// --- Phase 1: single-file rows without blobs, in chunks.
	singles := int(3_480_000 * scale)
	const chunk = 200_000
	for lo := 1; lo <= singles; lo += chunk {
		guard("single-file rows")
		hi := min(lo+chunk-1, singles)
		if _, err := conn.Exec(ctx, singleFileSQL, lo, hi); err != nil {
			t.Fatalf("rows %d-%d: %v", lo, hi, err)
		}
	}
	t.Logf("phase 1: %d single-file rows in %s", singles, time.Since(start).Round(time.Second))

	// --- Phase 2: multi-file torrents with real blobs.
	p2 := time.Now()
	rng := rand.New(rand.NewSource(20261124))
	common := []string{"1080p", "720p", "x264", "x265", "bluray", "webrip", "complete", "season", "ubuntu", "linux"}
	var multi, files int
	add := func(class string, n int, minF, maxF int, hugeBase bool) {
		var specs []synth.Spec
		if hugeBase {
			specs = synth.Torrents(synth.Spec{Name: "huge", Shape: synth.Typical, Files: 150_000, Seed: 99}, n)
		} else {
			for i := 0; i < n; i++ {
				specs = append(specs, synth.Spec{Name: fmt.Sprintf("%s#%d", class, i), Shape: synth.Typical, Files: minF + rng.Intn(maxF-minF+1), Seed: uint64(len(class))*1_000_003 + uint64(i) + 1})
			}
		}
		for i, s := range specs {
			if i%200 == 0 {
				guard(class)
			}
			fl := synth.Generate(s)
			name := fmt.Sprintf("%s %s multi %s %06d", common[rng.Intn(len(common))], common[rng.Intn(len(common))], class, i)
			if _, err := seedTorrent(ctx, conn, name, fl); err != nil {
				t.Fatalf("%s %d: %v", class, i, err)
			}
			multi++
			files += len(fl)
		}
		t.Logf("  %-6s %6d torrents (cumulative %d torrents, %d files), %s", class, n, multi, files, time.Since(p2).Round(time.Second))
	}
	add("small", int(12_000*scale), 10, 50, false)
	add("medium", int(6_000*scale), 51, 500, false)
	add("large", int(1_900*scale), 501, 4_000, false)
	add("huge", max(1, int(24*scale)), 0, 0, true)

	// --- Statistics for the planner and the visibility map.
	if _, err := conn.Exec(ctx, `VACUUM (ANALYZE) magneticod.torrents`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `VACUUM (ANALYZE) magneticod.torrent_file_blobs`); err != nil {
		t.Fatal(err)
	}
	var total, withBlob int
	_ = conn.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE file_count > 1) FROM magneticod.torrents`).Scan(&total, &withBlob)
	db, tbl, idx, blobs := size()
	t.Logf("RESULT scale=%.4g torrents=%d multi_file=%d files_in_blobs=%d wall=%s pg_database_size=%dMiB torrents_table=%dMiB torrents_indexes=%dMiB blobs_total=%dMiB free_after=%dGiB",
		scale, total, withBlob, files, time.Since(start).Round(time.Second), db>>20, tbl>>20, idx>>20, blobs>>20, harnessFreeBytes(t, dir)>>30)
}

// singleFileSQL inserts rows [$1,$2] of single-file torrents without blobs.
// Names combine a Zipf-skewed common word, a mid-frequency word, a long-tail
// word (about 17 rows each) and, for about 3% of rows, a Unicode suffix;
// discovered_on is quantised to a minute (ties); sizes are heavy-tailed.
const singleFileSQL = `
WITH g AS (
  SELECT i,
    ('x'||substr(md5(i::text),1,8))::bit(32)::bigint/4294967296.0  AS u1,
    ('x'||substr(md5(i::text),9,8))::bit(32)::bigint/4294967296.0  AS u2,
    ('x'||substr(md5(i::text),17,8))::bit(32)::bigint/4294967296.0 AS u3,
    ('x'||substr(md5(i::text),25,8))::bit(32)::bigint/4294967296.0 AS u4
  FROM generate_series($1::bigint,$2::bigint) AS i)
INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
SELECT substr(decode(md5(i::text)||md5((i+7)::text),'hex'),1,20),
  (ARRAY['1080p','720p','2160p','x264','x265','hevc','aac','bluray','webrip','hdtv','web-dl','remux','multi','complete','season','episode','collection','ubuntu','debian','fedora','linux','iso','amd64','portable','repack','proper','extended','remastered','dual','eng'])[1+floor(power(u1,3)*30)::int]
  ||' '||'m'||floor(power(u2,2)*400)::int
  ||' '||'w'||floor(u3*200000)::int
  ||CASE WHEN u4 < 0.015 THEN ' 日本語' WHEN u4 < 0.03 THEN ' Кириллица' ELSE '' END,
  floor(power(u4,4)*2e12)::bigint+1,
  1500000000+(((i*7919)%90000000)/60)*60,
  1
FROM g`
