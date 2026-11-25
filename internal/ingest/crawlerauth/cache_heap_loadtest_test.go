//go:build loadtest

package crawlerauth

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"testing"
)

// TestMeasureAuthCacheHeap: 10,000 registered crawlers authenticate in turn; the
// cache holds its bound (4,096 entries) and its heap is reported.
//
//	MAGNETICO_STATS_OUT=<dir> go test -tags loadtest -run TestMeasureAuthCacheHeap -v ./internal/ingest/crawlerauth
func TestMeasureAuthCacheHeap(t *testing.T) {
	out := os.Getenv("MAGNETICO_STATS_OUT")
	if out == "" {
		t.Skip("MAGNETICO_STATS_OUT is not set")
	}
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	l := newLookup()
	for i := 0; i < 10000; i++ {
		l.set(tok(i), rec("00000000-0000-4000-8000-"+pad12(i), "crawler-"+pad12(i)))
	}
	base := heap()
	a := newAuth(l, newClock())
	for i := 0; i < 10000; i++ {
		if _, err := a.Authenticate(context.Background(), tok(i)); err != nil {
			t.Fatal(err)
		}
	}
	used := heap() - base
	runtime.KeepAlive(a)
	res := map[string]any{"registered": 10000, "cache_entries": a.cacheLen(), "heap_bytes": used, "heap_mib": float64(used) / (1 << 20)}
	b, _ := json.Marshal(res)
	t.Logf("%s", b)
	f, _ := os.OpenFile(out+"/memory_heap.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

func pad12(i int) string {
	s := "000000000000" + itoaN(i)
	return s[len(s)-12:]
}

func itoaN(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}
