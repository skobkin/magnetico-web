//go:build loadtest

package stats

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

// TestMeasureQueueSamplingCost times the exact queue-sampling cycle at 10 k,
// 100 k and 1 M queued rows (plan 09, Task 6 measurement step). Run:
//
//	go test -tags loadtest -run TestMeasureQueueSamplingCost -v ./internal/ingest/stats
func TestMeasureQueueSamplingCost(t *testing.T) {
	for _, payload := range []int{1, 1500} { // 1 byte (narrow rows) and 1,500 bytes (the widest inline rows: no TOAST)
		t.Run(fmt.Sprintf("payload=%d", payload), func(t *testing.T) { measureQueue(t, payload) })
	}
}

func measureQueue(t *testing.T, payload int) {
	e := newWEnv(t, nil)
	have := 0
	for _, n := range []int{10_000, 100_000, 1_000_000} {
		e.exec(fmt.Sprintf(`INSERT INTO magnetico_ingest.queue (info_hash, format, payload, payload_bytes)
SELECT decode(lpad(to_hex(g + %d), 40, '0'), 'hex'), 1, decode(repeat('ab', %d), 'hex'), %d FROM generate_series(1, %d) g`, have, payload, payload, n-have))
		have = n
		e.exec(`VACUUM ANALYZE magnetico_ingest.queue`)
		var d []time.Duration
		for i := 0; i < 40; i++ {
			start := time.Now()
			q := e.sampleQueue(samplerLimits{statement: 30 * time.Second, lock: time.Second, cycle: 60 * time.Second})
			d = append(d, time.Since(start))
			if q.CountsApprox || q.OldestApprox {
				t.Fatalf("the exact path was not taken at %d rows", n)
			}
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		fmt.Printf("MEASURE payload=%d rows=%d cycles=%d p50=%v p95=%v max=%v\n", payload, n, len(d), d[len(d)/2], d[len(d)*95/100], d[len(d)-1])
	}
}
