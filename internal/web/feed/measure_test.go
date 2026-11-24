package feed

import (
	"sort"
	"testing"
	"time"
)

// Measurements for docs/wip/07G (synthetic rows, one local PostgreSQL). They
// assert only loose sanity bounds; the numbers go into the report via -v.
func TestMeasureFeedGeneration(t *testing.T) {
	e := newEnv(t)
	e.exec(`INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
	    SELECT decode(md5(i::text) || substr(md5((i+1)::text),1,8),'hex'), 'Synthetic torrent number ' || i || ' with a moderately long descriptive name', i * 1000, 1700000000 + i, 3 FROM generate_series(1, 2000) i`)
	tok := e.rssToken()
	path := V2Path + "?feed_token=" + tok.Plaintext

	start := time.Now()
	r := e.do(path, nil)
	cold := time.Since(start)
	if r.Status != 200 {
		t.Fatalf("status %d", r.Status)
	}
	var warm []time.Duration
	for i := 0; i < 200; i++ {
		s := time.Now()
		e.do(path, nil)
		warm = append(warm, time.Since(s))
	}
	sort.Slice(warm, func(i, j int) bool { return warm[i] < warm[j] })
	t.Logf("feed page of 1000 items: %d bytes; cold build (read+render+auth) %v; cached p50 %v p95 %v",
		len(r.Body), cold, warm[len(warm)/2], warm[len(warm)*95/100])

	// Rendering alone, without the database.
	rows, err := e.rows.inner.LatestTorrents(tctx(t), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	urls := e.trackers.URLs()
	var sum time.Duration
	const n = 50
	for i := 0; i < n; i++ {
		s := time.Now()
		if _, err := render(rows, urls, e.srvPublicBase(), V2Path, false, time.Now()); err != nil {
			t.Fatal(err)
		}
		sum += time.Since(s)
	}
	t.Logf("render of 1000 items with 2 trackers: %v average", sum/n)
	if cold > 5*time.Second {
		t.Fatalf("cold build took %v", cold)
	}
}

func BenchmarkRenderThousandItems(b *testing.B) {
	rows := syntheticRows(1000)
	urls := []string{"udp://tracker.opentrackr.org:1337/announce", "http://nyaa.tracker.wf:7777/announce"}
	base := mustURL("https://magnet.example.org")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := render(rows, urls, base, V2Path, false, time.Unix(1_800_000_000, 0)); err != nil {
			b.Fatal(err)
		}
	}
}
