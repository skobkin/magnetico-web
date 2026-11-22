package password

import (
	"bufio"
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

// vmRSSKiB reads the current resident set from /proc (Linux); ok=false elsewhere.
func vmRSSKiB() (kib int64, ok bool) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, found := strings.CutPrefix(sc.Text(), "VmRSS:"); found {
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				n, err := strconv.ParseInt(fields[0], 10, 64)
				return n, err == nil
			}
		}
	}
	return 0, false
}

// Two verifications of maximum-parameter hashes (m=64 MiB, t=10, p=16) must
// stay inside the budget, and a third caller must wait or get ErrBusy rather
// than allocate. The limiter capacity is 2, the production default.
func TestLimiterMemoryBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("memory test runs a full-size Argon2 computation")
	}
	const budget = 256 << 20
	h := argonHash{id: true, memory: maxArgonMemoryKiB, time: maxArgonTime, threads: maxArgonThreads,
		salt: []byte("0123456789abcdef")}
	h.key = argon2.IDKey([]byte("max-parameter-password"), h.salt, h.time, h.memory, h.threads, keyLen)
	hash := h.encode()
	if Classify(hash) != ClassLegacy {
		t.Fatal("maximum-parameter hash is not accepted")
	}

	svc := NewService(NewLimiter(2, 100*time.Millisecond), CurrentParams)
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	rssBase, rssOK := vmRSSKiB()

	var peakHeap atomic.Uint64
	var peakRSS atomic.Int64
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peakHeap.Load() {
				peakHeap.Store(ms.HeapAlloc)
			}
			if rss, ok := vmRSSKiB(); ok && rss > peakRSS.Load() {
				peakRSS.Store(rss)
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	start := time.Now()
	var wg sync.WaitGroup
	var okCount, busyCount atomic.Int32
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := svc.Verify(context.Background(), "max-parameter-password", hash)
			switch {
			case errors.Is(err, ErrBusy):
				busyCount.Add(1)
			case err != nil || !ok:
				t.Errorf("verify = %v, %v", ok, err)
			default:
				okCount.Add(1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(stop)
	sampler.Wait()

	if okCount.Load() != 2 || busyCount.Load() != 1 {
		t.Errorf("ok = %d, busy = %d; want 2 verifications and 1 ErrBusy", okCount.Load(), busyCount.Load())
	}
	growth := int64(peakHeap.Load()) - int64(base.HeapAlloc)
	t.Logf("two concurrent m=64MiB,t=10,p=16 verifications: %v total, heap growth %d MiB (budget %d MiB)",
		elapsed.Round(time.Millisecond), growth>>20, budget>>20)
	if growth > budget {
		t.Errorf("heap growth %d MiB exceeds the %d MiB budget", growth>>20, budget>>20)
	}
	if rssOK {
		rssGrowth := (peakRSS.Load() - rssBase) << 10
		t.Logf("RSS growth %d MiB", rssGrowth>>20)
		if rssGrowth > budget {
			t.Errorf("RSS growth %d MiB exceeds the %d MiB budget", rssGrowth>>20, budget>>20)
		}
	}
}
