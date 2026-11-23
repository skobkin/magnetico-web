package benchkit

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestHistSummaryExactPercentiles(t *testing.T) {
	var h Hist
	for i := 100; i >= 1; i-- { // unsorted input
		h.Add(time.Duration(i) * time.Millisecond)
	}
	s := h.Summary()
	if s.N != 100 || s.Min != time.Millisecond || s.Max != 100*time.Millisecond ||
		s.P50 != 50*time.Millisecond || s.P90 != 90*time.Millisecond || s.P95 != 95*time.Millisecond || s.P99 != 99*time.Millisecond {
		t.Fatalf("%+v", s)
	}
	if s.Mean != 50500*time.Microsecond {
		t.Fatalf("mean %v", s.Mean)
	}
	if (&Hist{}).Summary() != (Summary{}) {
		t.Fatal("empty hist")
	}
	var one Hist
	one.Add(time.Second)
	if s := one.Summary(); s.P99 != time.Second || s.P50 != time.Second {
		t.Fatalf("%+v", s)
	}
	var a, b Hist
	a.Add(time.Second)
	b.Add(2 * time.Second)
	a.Merge(&b)
	if a.Len() != 2 {
		t.Fatal("merge")
	}
}

func TestRunCountsEveryJobOnceAndClassifiesErrors(t *testing.T) {
	var calls atomic.Int64
	seen := make([]atomic.Int32, 200)
	boom := errors.New("busy")
	res := Run(context.Background(), 8, 200, func(w, j int) error {
		calls.Add(1)
		seen[j].Add(1)
		if j%10 == 0 {
			return boom
		}
		return nil
	})
	if calls.Load() != 200 || res.OK != 180 || res.Errors["busy"] != 20 || res.Latency.N != 200 {
		t.Fatalf("calls=%d %+v", calls.Load(), res)
	}
	for i := range seen {
		if seen[i].Load() != 1 {
			t.Fatalf("job %d ran %d times", i, seen[i].Load())
		}
	}
}

func TestRunStopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int64
	res := Run(ctx, 4, 1_000_000, func(w, j int) error {
		if n.Add(1) == 50 {
			cancel()
		}
		return nil
	})
	if res.Latency.N >= 1_000_000 {
		t.Fatalf("did not stop: %d", res.Latency.N)
	}
}

var sink []byte

func TestMeasureSeesAllocations(t *testing.T) {
	a := Measure(func() {
		for i := 0; i < 100; i++ {
			sink = make([]byte, 64<<10)
		}
	})
	if a.TotalAlloc < 100*(64<<10) || a.Mallocs < 100 || a.Wall <= 0 {
		t.Fatalf("%+v", a)
	}
}

func TestProbeSeesAPeak(t *testing.T) {
	p := StartProbe(2 * time.Millisecond)
	big := make([]byte, 64<<20)
	for i := 0; i < len(big); i += 4096 {
		big[i] = 1
	}
	time.Sleep(20 * time.Millisecond)
	pk := p.Stop()
	_ = big[0]
	if pk.HeapAlloc < 64<<20 || pk.Samples < 2 {
		t.Fatalf("%+v", pk)
	}
	if st := ReadStatus(); st.RSS == 0 && st.HWM == 0 {
		t.Log("no /proc/self/status on this platform")
	}
}

func TestDescribe(t *testing.T) {
	m := Describe()
	if m.NumCPU < 1 || m.GoVersion == "" || m.GOMAXPROCS < 1 || m.String() == "" {
		t.Fatalf("%+v", m)
	}
}
