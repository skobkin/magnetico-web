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
// stay inside the budget, and a third caller must get ErrBusy rather than
// allocate while both slots are taken. The limiter capacity is 2, the production
// default.
//
// The third caller is tested against slots the test holds itself: racing it
// against two real verifications depended on Argon2's speed (on a fast CI
// agent the verifications finished within the 100 ms wait and the third caller
// legitimately got a slot).
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
	var okCount atomic.Int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := svc.Verify(context.Background(), "max-parameter-password", hash)
			if err != nil || !ok {
				t.Errorf("verify = %v, %v", ok, err)
				return
			}
			okCount.Add(1)
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(stop)
	sampler.Wait()

	if okCount.Load() != 2 {
		t.Errorf("ok = %d; want 2 verifications", okCount.Load())
	}
	growth := int64(peakHeap.Load()) - int64(base.HeapAlloc)
	t.Logf("two concurrent m=64MiB,t=10,p=16 verifications: %v total, heap growth %d MiB (budget %d MiB)",
		elapsed.Round(time.Millisecond), growth>>20, budget>>20)
	if growth > budget {
		t.Errorf("heap growth %d MiB exceeds the %d MiB budget", growth>>20, budget>>20)
	}
	// RSS is diagnostic only: it depends on allocator state and on what
	// earlier tests left resident, so asserting on it would be flaky.
	if rssOK {
		t.Logf("RSS growth %d MiB (diagnostic, not asserted)", ((peakRSS.Load()-rssBase)<<10)>>20)
	}

	// A third caller while both slots are taken: ErrBusy after the wait, and no
	// Argon2 memory allocated for it.
	l := svc.Limiter()
	release := make(chan struct{})
	var held sync.WaitGroup
	for i := 0; i < 2; i++ {
		held.Add(1)
		go func() {
			defer held.Done()
			if err := l.Do(context.Background(), func() { <-release }); err != nil {
				t.Errorf("holding a slot: %v", err)
			}
		}()
	}
	defer func() { close(release); held.Wait() }()
	for deadline := time.Now().Add(5 * time.Second); len(l.slots) < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the test could not take both limiter slots")
		}
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	ok, err := svc.Verify(context.Background(), "max-parameter-password", hash)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrBusy) || ok {
		t.Errorf("third caller: verify = %v, %v; want ErrBusy", ok, err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc >= 1<<20 {
		t.Errorf("the refused caller allocated %d KiB; an Argon2 computation must not have started", alloc>>10)
	}
}
