//go:build loadtest

package crawlerauth

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/internal/ingest/crawlers"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

// TestMeasureCachedVersusUncachedAuthentication measures authenticated
// throughput through the real store with the 60 s cache and with every request
// looking the credential up (a fresh authenticator per call). Run:
//
//	MAGNETICO_STATS_OUT=<dir> go test -tags loadtest -run TestMeasureCachedVersusUncached -v ./internal/ingest/crawlerauth
func TestMeasureCachedVersusUncachedAuthentication(t *testing.T) {
	out := os.Getenv("MAGNETICO_STATS_OUT")
	if out == "" {
		t.Skip("MAGNETICO_STATS_OUT is not set")
	}
	dsn := pgtest.IntegrationCluster(t).TestDatabase(t)
	if err := persistence.InitSchema(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	if err := pgqueue.InitSchema(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := crawlers.New(pool)
	created, err := store.Add(context.Background(), "bench")
	if err != nil {
		t.Fatal(err)
	}
	run := func(workers int, d time.Duration, next func() *Authenticator) float64 {
		var n atomic.Int64
		var wg sync.WaitGroup
		stop := time.Now().Add(d)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a := next()
				for time.Now().Before(stop) {
					if a == nil {
						a2 := New(store, Options{})
						if _, err := a2.Authenticate(context.Background(), created.Token); err == nil {
							n.Add(1)
						}
						continue
					}
					if _, err := a.Authenticate(context.Background(), created.Token); err == nil {
						n.Add(1)
					}
				}
			}()
		}
		wg.Wait()
		return float64(n.Load()) / d.Seconds()
	}
	shared := New(store, Options{})
	res := map[string]float64{}
	for _, workers := range []int{1, 8, 64} {
		res["cached_"+itoa(workers)] = run(workers, 5*time.Second, func() *Authenticator { return shared })
		// Uncached: every request is a database lookup; the 8-slot bound applies, so concurrency beyond 8 is shed as busy.
		res["uncached_"+itoa(workers)] = run(workers, 5*time.Second, func() *Authenticator { return nil })
	}
	b, _ := json.Marshal(res)
	t.Logf("authentications per second: %s", b)
	f, _ := os.OpenFile(out+"/auth_throughput.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
	for _, w := range []string{"1", "8", "64"} {
		if res["cached_"+w] < res["uncached_"+w] {
			t.Errorf("%s workers: cached %.0f/s is slower than uncached %.0f/s", w, res["cached_"+w], res["uncached_"+w])
		}
	}
}

func itoa(n int) string {
	return map[int]string{1: "1", 8: "8", 64: "64"}[n]
}
