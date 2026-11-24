package stats

import (
	"fmt"
	"net/netip"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/opstats"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{t: t} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const (
	cidA = "11111111-1111-4111-8111-111111111111"
	cidB = "22222222-2222-4222-8222-222222222222"
)

var t0 = time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)

func newRec() (*Recorder, *fakeClock) {
	c := newClock(t0)
	return New(c.Now), c
}

// sum folds every hour of a delta.
func sum(d opstats.Delta) (t [opstats.NumCounters]int64) {
	for _, h := range d.Hours {
		for i, v := range h.Counters {
			t[i] += v
		}
	}
	return
}

func TestReceivedIsValidatedBeforeAdmission(t *testing.T) {
	r, _ := newRec()
	r.SubmitValidated(cidA, netip.MustParseAddr("192.0.2.1"), 5)
	if r.InFlight() != 5 {
		t.Fatalf("in flight = %d, want 5", r.InFlight())
	}
	d := r.Drain()
	s := sum(d)
	if s[opstats.CRecordsReceived] != 5 || s[opstats.CSubmitValidated] != 1 || s[opstats.CRecordsAcknowledged] != 0 || s[opstats.CRecordsEnqueued] != 0 {
		t.Fatalf("counters after validation only: %v", s)
	}
	if d.InFlightAtDrain != 5 {
		t.Fatalf("InFlightAtDrain = %d", d.InFlightAtDrain)
	}
}

func TestAcknowledgedEqualsEnqueuedPlusDuplicates(t *testing.T) {
	r, _ := newRec()
	r.SubmitValidated(cidA, netip.Addr{}, 10)
	r.SubmitAcked(cidA, 7, 3, 4096)
	s := sum(r.Drain())
	if s[opstats.CRecordsAcknowledged] != s[opstats.CRecordsEnqueued]+s[opstats.CAdmissionDuplicates] ||
		s[opstats.CRecordsEnqueued] != 7 || s[opstats.CAdmissionDuplicates] != 3 || s[opstats.CBytesAccepted] != 4096 {
		t.Fatalf("counters: %v", s)
	}
	if r.InFlight() != 0 {
		t.Fatalf("in flight = %d", r.InFlight())
	}
}

func TestReceivedEqualsAcknowledgedPlusRefusedPlusInFlight(t *testing.T) {
	r, _ := newRec()
	check := func(step string) {
		t.Helper()
		s := r.Totals()
		got := s[opstats.CRecordsReceived] - s[opstats.CRecordsAcknowledged] - s[opstats.CRecordsRefused]
		if got != r.InFlight() {
			t.Fatalf("%s: received-acknowledged-refused = %d, in flight = %d", step, got, r.InFlight())
		}
	}
	r.SubmitValidated(cidA, netip.Addr{}, 10)
	check("validated 10")
	r.SubmitValidated(cidB, netip.Addr{}, 4)
	check("validated 4")
	r.SubmitAcked(cidA, 6, 4, 100)
	check("acked a")
	r.SubmitRefused(cidB, 4, opstats.RejectQueueFull)
	check("refused b")
	if r.InFlight() != 0 {
		t.Fatalf("not at quiescence: %d", r.InFlight())
	}
	s := r.Totals()
	if s[opstats.CRecordsReceived] != s[opstats.CRecordsAcknowledged]+s[opstats.CRecordsRefused] {
		t.Fatalf("at quiescence received = %d, acknowledged+refused = %d", s[opstats.CRecordsReceived], s[opstats.CRecordsAcknowledged]+s[opstats.CRecordsRefused])
	}
}

func TestRejectedRequestsAndRefusedRecordsAreSeparateCounters(t *testing.T) {
	r, _ := newRec()
	r.SubmitValidated(cidA, netip.Addr{}, 5)
	r.SubmitRefused(cidA, 5, opstats.RejectQueueFull)
	r.SubmitValidated(cidA, netip.Addr{}, 3)
	r.SubmitRefused(cidA, 3, NoReject) // client went away: refused records, no rejected request
	r.RequestRejected(opstats.RejectMalformed, "")
	r.RequestRejected(opstats.RejectTooLarge, cidA)
	d := r.Drain()
	s := sum(d)
	if s[opstats.CRecordsRefused] != 8 || s[opstats.CRejectedQueueFull] != 1 || s[opstats.CRejectedMalformed] != 1 || s[opstats.CRejectedTooLarge] != 1 {
		t.Fatalf("counters: %v", s)
	}
	for c := opstats.CRejectedAuth; c <= opstats.CRejectedInternal; c++ {
		switch c {
		case opstats.CRejectedQueueFull, opstats.CRejectedMalformed, opstats.CRejectedTooLarge:
		default:
			if s[c] != 0 {
				t.Errorf("%s = %d", c.Name(), s[c])
			}
		}
	}
	// A request refused before authentication counts globally only.
	if len(d.Crawlers) != 1 || d.Crawlers[0].Rejected != 2 {
		t.Fatalf("crawler deltas: %+v", d.Crawlers)
	}
}

func TestSettlementOutcomesAreMutuallyExclusive(t *testing.T) {
	r, _ := newRec()
	r.TorrentCreated()
	r.SettledStoredNew(40 * time.Millisecond)
	r.SettledStoredExisting(20 * time.Millisecond)
	r.SettledQuarantined()
	s := sum(r.Drain())
	if s[opstats.CSettledStoredNew] != 1 || s[opstats.CSettledStoredExisting] != 1 || s[opstats.CSettledQuarantined] != 1 || s[opstats.CTorrentsCreated] != 1 {
		t.Fatalf("counters: %v", s)
	}
	if ws := r.Windows(); ws.EndSecond == 0 {
		t.Fatal("no window")
	}
}

func TestProcessingDurationExcludesRejectedRows(t *testing.T) {
	r, _ := newRec()
	r.SettledStoredNew(100 * time.Millisecond)
	r.SettledStoredExisting(300 * time.Millisecond)
	r.SettledQuarantined()
	r.ProcessAttempt()
	s := sum(r.Drain())
	if s[opstats.CProcessingMsCount] != 2 || s[opstats.CProcessingMsSum] != 400 {
		t.Fatalf("processing ms sum=%d count=%d, want 400 and 2 (quarantined rows are not timed)", s[opstats.CProcessingMsSum], s[opstats.CProcessingMsCount])
	}
	// Sub-millisecond work is rounded, not truncated away.
	r.SettledStoredNew(900 * time.Microsecond)
	if s := sum(r.Drain()); s[opstats.CProcessingMsSum] != 1 {
		t.Fatalf("0.9 ms recorded as %d ms", s[opstats.CProcessingMsSum])
	}
}

func TestExistsOutcomesAndCacheKindsAreSplit(t *testing.T) {
	r, _ := newRec()
	r.ExistsRequest(cidA)
	r.ExistsDone(cidA, netip.MustParseAddr("198.51.100.7"), ExistsOutcome{Hashes: 10, Found: 4, Missing: 5, Unknown: 1, CacheHitPositive: 3, CacheHitNegative: 2, CacheMiss: 5})
	d := r.Drain()
	s := sum(d)
	if s[opstats.CExistsRequests] != 1 || s[opstats.CExistsHashes] != 10 || s[opstats.CExistsFound] != 4 || s[opstats.CExistsMissing] != 5 ||
		s[opstats.CExistsUnknown] != 1 || s[opstats.CCacheHitPositive] != 3 || s[opstats.CCacheHitNegative] != 2 || s[opstats.CCacheMiss] != 5 {
		t.Fatalf("counters: %v", s)
	}
	if c := d.Crawlers[0]; c.ExistsRequests != 1 || c.ExistsHashes != 10 || c.LastIP != netip.MustParseAddr("198.51.100.7") || !c.LastSubmit.IsZero() {
		t.Fatalf("crawler delta: %+v", c)
	}
}

func TestAuthCounters(t *testing.T) {
	r, _ := newRec()
	r.AuthHit()
	r.AuthHit()
	r.AuthMiss()
	r.AuthDBLookup()
	r.AuthStorageFailure()
	r.AuthBusy()
	s := sum(r.Drain())
	if s[opstats.CAuthCacheHits] != 2 || s[opstats.CAuthCacheMisses] != 1 || s[opstats.CAuthDBLookups] != 1 || s[opstats.CAuthStorageFailures] != 1 || s[opstats.CAuthBusy] != 1 {
		t.Fatalf("counters: %v", s)
	}
}

func TestNilRecorderIsNoOp(t *testing.T) {
	var r *Recorder
	r.SubmitAuthenticated(cidA)
	r.RequestBytes(cidA, 5)
	r.SubmitValidated(cidA, netip.Addr{}, 1)
	r.SubmitAcked(cidA, 1, 0, 1)
	r.SubmitRefused(cidA, 1, opstats.RejectBusy)
	r.RequestRejected(opstats.RejectAuth, "")
	r.ExistsRequest(cidA)
	r.ExistsDone(cidA, netip.Addr{}, ExistsOutcome{})
	r.AuthHit()
	r.ProcessAttempt()
	r.TorrentCreated()
	r.SettledStoredNew(time.Second)
	r.SettledQuarantined()
	r.MarkRunning()
	if d := r.Drain(); !d.IsEmpty() || r.InFlight() != 0 || r.ClockAnomaly() || r.Windows().EndSecond != 0 {
		t.Fatal("a nil recorder must record nothing")
	}
}

// ---- concurrency and rotation -------------------------------------------------

func TestRecorderConcurrentUpdatesNeverLoseCounts(t *testing.T) {
	r := New(nil)
	const goroutines, per = 32, 5000
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			id := fmt.Sprintf("00000000-0000-4000-8000-%012d", g%8)
			for i := 0; i < per; i++ {
				r.SubmitValidated(id, netip.Addr{}, 2)
				r.SubmitAcked(id, 1, 1, 10)
				r.RequestBytes(id, 3)
				r.AuthHit()
			}
		}(g)
	}
	wg.Wait()
	s := sum(r.Drain())
	const n = goroutines * per
	if s[opstats.CRecordsReceived] != 2*n || s[opstats.CRecordsAcknowledged] != 2*n || s[opstats.CRecordsEnqueued] != n ||
		s[opstats.CBytesBody] != 3*n || s[opstats.CAuthCacheHits] != n || s[opstats.CBytesAccepted] != 10*n {
		t.Fatalf("lost updates: %v", s)
	}
	if r.InFlight() != 0 {
		t.Fatalf("in flight = %d", r.InFlight())
	}
}

// TestRotationWithARecorderPausedInsideTheCriticalSection: an event parked inside
// the critical section lands wholly in the drained delta, because Drain waits for it.
func TestRotationWithARecorderPausedInsideTheCriticalSection(t *testing.T) {
	r, _ := newRec()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.hook = func() {
		once.Do(func() { close(entered); <-release })
	}
	evDone := make(chan struct{})
	go func() { defer close(evDone); r.SubmitAcked(cidA, 3, 2, 50) }()
	<-entered
	drained := make(chan opstats.Delta, 1)
	go func() { drained <- r.Drain() }()
	select {
	case <-drained:
		t.Fatal("Drain completed while an event was inside the critical section")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	d := <-drained
	<-evDone
	s := sum(d)
	if s[opstats.CRecordsEnqueued] != 3 || s[opstats.CRecordsAcknowledged] != 5 || s[opstats.CAdmissionDuplicates] != 2 || s[opstats.CBytesAccepted] != 50 {
		t.Fatalf("the parked event is not whole in the drained delta: %v", s)
	}
	if next := sum(r.Drain()); next[opstats.CRecordsAcknowledged] != 0 {
		t.Fatalf("the event leaked into the next delta: %v", next)
	}
}

func TestRotationWithARecorderPausedBeforeTheLock(t *testing.T) {
	r, _ := newRec()
	r.SubmitAcked(cidA, 1, 0, 1) // before the rotation
	d1 := r.Drain()
	r.SubmitAcked(cidA, 4, 0, 1) // arrives after the swap: the next delta
	d2 := r.Drain()
	if sum(d1)[opstats.CRecordsEnqueued] != 1 || sum(d2)[opstats.CRecordsEnqueued] != 4 {
		t.Fatalf("d1=%v d2=%v", sum(d1)[opstats.CRecordsEnqueued], sum(d2)[opstats.CRecordsEnqueued])
	}
}

// TestRelatedCountersNeverSplitAcrossADrain: in every delta acknowledged =
// enqueued + duplicates and Δreceived − Δacknowledged − Δrefused = Δin_flight;
// at the end, once the goroutines returned, received = acknowledged + refused.
func TestRelatedCountersNeverSplitAcrossADrain(t *testing.T) {
	r := New(nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			id := fmt.Sprintf("00000000-0000-4000-8000-%012d", g%4)
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				r.SubmitValidated(id, netip.Addr{}, 3)
				if i%3 == 0 {
					r.SubmitRefused(id, 3, opstats.RejectQueueFull)
				} else {
					r.SubmitAcked(id, 2, 1, 7)
				}
			}
		}(g)
	}
	var prevInFlight int64
	var total [opstats.NumCounters]int64
	for i := 0; i < 10000; i++ {
		d := r.Drain()
		s := sum(d)
		if s[opstats.CRecordsAcknowledged] != s[opstats.CRecordsEnqueued]+s[opstats.CAdmissionDuplicates] {
			t.Fatalf("drain %d: acknowledged %d != enqueued %d + duplicates %d", i, s[opstats.CRecordsAcknowledged], s[opstats.CRecordsEnqueued], s[opstats.CAdmissionDuplicates])
		}
		if got, want := s[opstats.CRecordsReceived]-s[opstats.CRecordsAcknowledged]-s[opstats.CRecordsRefused], d.InFlightAtDrain-prevInFlight; got != want {
			t.Fatalf("drain %d: Δreceived−Δacknowledged−Δrefused = %d, Δin_flight = %d", i, got, want)
		}
		prevInFlight = d.InFlightAtDrain
		for c := range total {
			total[c] += s[c]
		}
		runtime.Gosched()
	}
	close(stop)
	wg.Wait()
	d := r.Drain()
	s := sum(d)
	for c := range total {
		total[c] += s[c]
	}
	if d.InFlightAtDrain != 0 || r.InFlight() != 0 {
		t.Fatalf("not quiescent: in flight %d", d.InFlightAtDrain)
	}
	if total[opstats.CRecordsReceived] != total[opstats.CRecordsAcknowledged]+total[opstats.CRecordsRefused] {
		t.Fatalf("at quiescence received %d != acknowledged %d + refused %d", total[opstats.CRecordsReceived], total[opstats.CRecordsAcknowledged], total[opstats.CRecordsRefused])
	}
}

func TestDrainedAccumulatorIsNeverWrittenAgain(t *testing.T) {
	r, _ := newRec()
	r.SubmitAcked(cidA, 1, 0, 1)
	d := r.Drain()
	before := sum(d)
	hours := len(d.Hours)
	for i := 0; i < 100; i++ {
		r.SubmitAcked(cidA, 1, 0, 1)
	}
	if sum(d) != before || len(d.Hours) != hours || d.Crawlers[0].RecordsEnqueued != 1 {
		t.Fatal("a drained delta changed after more events were recorded")
	}
}

func TestTotalsAreConservedAcrossManyDrains(t *testing.T) {
	r, c := newRec()
	var total int64
	for i := 0; i < 500; i++ {
		r.SubmitAcked(cidA, 2, 1, 1)
		c.Advance(7 * time.Minute) // crosses hours and days
		total += sum(r.Drain())[opstats.CRecordsAcknowledged]
	}
	if total != 1500 || r.Totals()[opstats.CRecordsAcknowledged] != 1500 {
		t.Fatalf("drained %d, totals %d, want 1500", total, r.Totals()[opstats.CRecordsAcknowledged])
	}
}

// ---- event-time attribution ---------------------------------------------------

func TestEventHourIsTakenInsideTheCriticalSection(t *testing.T) {
	r, c := newRec()
	c.Set(time.Date(2026, 11, 25, 10, 59, 59, 0, time.UTC))
	r.SubmitAcked(cidA, 1, 0, 1)
	c.Set(time.Date(2026, 11, 25, 11, 0, 1, 0, time.UTC))
	r.SubmitAcked(cidA, 2, 0, 1)
	d := r.Drain()
	if len(d.Hours) != 2 || d.Hours[0].Hour.Hour() != 10 || d.Hours[1].Hour.Hour() != 11 ||
		d.Hours[0].Counters[opstats.CRecordsEnqueued] != 1 || d.Hours[1].Counters[opstats.CRecordsEnqueued] != 2 {
		t.Fatalf("hours: %+v", d.Hours)
	}
}

func TestLateDrainAfterTheHourBoundaryKeepsEventsInTheirOwnHour(t *testing.T) {
	for _, boundary := range []time.Time{
		time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC), // hour
		time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC),  // day
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),   // month
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),    // year
		time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),   // leap day
		time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC),
	} {
		c := newClock(boundary.Add(-2 * time.Second))
		r := New(c.Now)
		r.SubmitAcked(cidA, 1, 0, 1) // two seconds before the boundary
		c.Set(boundary.Add(30 * time.Second))
		r.SubmitAcked(cidA, 1, 0, 1) // thirty seconds after it, with no drain in between
		d := r.Drain()               // a tick that fires late
		if len(d.Hours) != 2 || !d.Hours[1].Hour.Equal(boundary) || !d.Hours[0].Hour.Equal(boundary.Add(-time.Hour)) ||
			d.Hours[0].Counters[opstats.CRecordsEnqueued] != 1 || d.Hours[1].Counters[opstats.CRecordsEnqueued] != 1 {
			t.Fatalf("boundary %v: %+v", boundary, d.Hours)
		}
	}
}

func TestBackwardClockStepDoesNotCreateAnOlderHour(t *testing.T) {
	r, c := newRec()
	r.SubmitAcked(cidA, 1, 0, 1)
	c.Advance(-3 * time.Hour)
	r.SubmitAcked(cidA, 1, 0, 1)
	d := r.Drain()
	if len(d.Hours) != 1 || d.Hours[0].Counters[opstats.CRecordsEnqueued] != 2 || d.ClockAnomaly {
		t.Fatalf("hours: %+v anomaly=%v", d.Hours, d.ClockAnomaly)
	}
}

func TestForwardStepLeavesNoRowsForSkippedHours(t *testing.T) {
	r, c := newRec()
	r.SubmitAcked(cidA, 1, 0, 1)
	c.Advance(5 * time.Hour)
	r.SubmitAcked(cidA, 1, 0, 1)
	d := r.Drain()
	if len(d.Hours) != 2 || d.Hours[1].Hour.Sub(d.Hours[0].Hour) != 5*time.Hour {
		t.Fatalf("hours: %+v (skipped hours must simply be absent)", d.Hours)
	}
}

func TestHugeClockStepMarksAnomalyAndKeepsLatestHour(t *testing.T) {
	r, c := newRec()
	r.SubmitAcked(cidA, 1, 0, 1)
	c.Advance(30 * time.Hour)
	r.SubmitAcked(cidA, 1, 0, 1)
	d := r.Drain()
	if !d.ClockAnomaly || len(d.Hours) != 2 {
		t.Fatalf("forward step: anomaly=%v hours=%d", d.ClockAnomaly, len(d.Hours))
	}
	r2, c2 := newRec()
	r2.SubmitAcked(cidA, 1, 0, 1)
	c2.Advance(-30 * time.Hour)
	r2.SubmitAcked(cidA, 1, 0, 1)
	d2 := r2.Drain()
	if !d2.ClockAnomaly || len(d2.Hours) != 1 || d2.Hours[0].Counters[opstats.CRecordsEnqueued] != 2 {
		t.Fatalf("backward step: anomaly=%v hours=%+v (must stay in the latest hour)", d2.ClockAnomaly, d2.Hours)
	}
}

func TestMinuteCoverageBitsAreSetPerRunningMinute(t *testing.T) {
	r, c := newRec() // 10:30
	r.MarkRunning()
	c.Set(time.Date(2026, 11, 25, 10, 17, 5, 0, time.UTC)) // goes back in the hour: still hour 10 (same hour)
	r.MarkRunning()
	c.Set(time.Date(2026, 11, 25, 10, 40, 0, 0, time.UTC))
	r.SubmitAcked(cidA, 1, 0, 1) // an event covers its minute too
	d := r.Drain()
	want := int64(1<<30 | 1<<17 | 1<<40)
	if len(d.Hours) != 1 || d.Hours[0].Coverage != want {
		t.Fatalf("coverage = %b, want %b", d.Hours[0].Coverage, want)
	}
}

// ---- bounds -----------------------------------------------------------------

func TestPerFlushCrawlerMapIsBounded(t *testing.T) {
	r, _ := newRec()
	const n = opstats.MaxCrawlersPerDrain + 476
	for i := 0; i < n; i++ {
		r.SubmitValidated(fmt.Sprintf("00000000-0000-4000-8000-%012d", i), netip.Addr{}, 1)
	}
	d := r.Drain()
	if len(d.Crawlers) != opstats.MaxCrawlersPerDrain || d.CrawlersOverflow != 476 {
		t.Fatalf("%d crawler deltas, overflow %d", len(d.Crawlers), d.CrawlersOverflow)
	}
	if got := sum(d)[opstats.CRecordsReceived]; got != n {
		t.Fatalf("global received = %d, want all %d (overflow must not affect global counters)", got, n)
	}
	if d2 := r.Drain(); d2.CrawlersOverflow != 0 {
		t.Fatal("the overflow counter was not reset by the drain")
	}
}

func TestRecorderMemoryIsBounded(t *testing.T) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := New(nil)
	// 10,000 registered crawlers over ten drains (each at most 1,024), plus a full ring.
	for round := 0; round < 10; round++ {
		for i := 0; i < 1000; i++ {
			r.SubmitValidated(fmt.Sprintf("00000000-%04d-4000-8000-%012d", round, i), netip.MustParseAddr("192.0.2.1"), 1)
		}
		_ = r.Drain()
	}
	for i := 0; i < 1500; i++ { // 1,500 distinct crawlers in one window: the cap holds
		r.SubmitValidated(fmt.Sprintf("00000001-0000-4000-8000-%012d", i), netip.Addr{}, 1)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	const budget = 8 << 20
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > budget {
		t.Fatalf("recorder heap grew by %d bytes, budget %d", grown, budget)
	}
	runtime.KeepAlive(r)
}

func TestCounterSaturatesAtTwoToTheSixtyTwo(t *testing.T) {
	r, _ := newRec()
	r.RequestBytes(cidA, opstats.Saturation-5)
	r.RequestBytes(cidA, 100)
	r.RequestBytes(cidA, 100)
	d := r.Drain()
	if got := d.Hours[0].Counters[opstats.CBytesBody]; got != opstats.Saturation {
		t.Fatalf("counter = %d, want saturation %d", got, opstats.Saturation)
	}
	if d.Crawlers[0].BytesBody != opstats.Saturation {
		t.Fatalf("crawler counter = %d", d.Crawlers[0].BytesBody)
	}
}

func TestRecordingNeverBlocksTheHandlerOnWriterStalls(t *testing.T) {
	// A stalled writer is a writer that never drains: events must keep flowing
	// at full speed, memory stays bounded by the live-hour cap and the loss is counted.
	c := newClock(t0)
	r := New(c.Now)
	start := time.Now()
	for i := 0; i < 40; i++ { // 40 simulated hours without a drain
		for j := 0; j < 2000; j++ {
			r.SubmitAcked(cidA, 1, 0, 1)
		}
		c.Advance(time.Hour)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("80,000 events took %v with no drain", time.Since(start))
	}
	d := r.Drain()
	if len(d.Hours) > opstats.MaxLiveHours || d.HoursDropped < 14 {
		t.Fatalf("%d hours buffered, %d dropped; want at most %d buffered and the rest counted", len(d.Hours), d.HoursDropped, opstats.MaxLiveHours)
	}
}

func TestRecordingDoesNotAllocateOnTheHotPath(t *testing.T) {
	r := New(nil)
	ip := netip.MustParseAddr("192.0.2.1")
	r.SubmitValidated(cidA, ip, 1) // creates the crawler record
	allocs := testing.AllocsPerRun(1000, func() {
		r.SubmitAuthenticated(cidA)
		r.SubmitValidated(cidA, ip, 2)
		r.SubmitAcked(cidA, 1, 1, 100)
		r.RequestBytes(cidA, 10)
		r.AuthHit()
		r.SettledStoredNew(time.Millisecond)
	})
	if allocs != 0 {
		t.Fatalf("%v allocations per event batch, want 0", allocs)
	}
}

func BenchmarkRecorderUncontended(b *testing.B) {
	r := New(nil)
	r.SubmitValidated(cidA, netip.Addr{}, 1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.SubmitAcked(cidA, 1, 0, 100)
	}
}

func BenchmarkRecorderContended64(b *testing.B) {
	r := New(nil)
	r.SubmitValidated(cidA, netip.Addr{}, 1)
	b.ReportAllocs()
	b.SetParallelism(64)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.SubmitAcked(cidA, 1, 0, 100)
		}
	})
}

// ---- rolling windows ----------------------------------------------------------

func TestWindowsCountOnlyCompletedSeconds(t *testing.T) {
	c := newClock(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	r := New(c.Now)
	c.Advance(2 * time.Second)
	for i := 0; i < 10; i++ { // all inside second 10:00:02
		r.SubmitAcked(cidA, 1, 0, 1)
	}
	if ws := r.Windows(); ws.Counts[0][wmEnqueued] != 0 {
		t.Fatalf("the current, incomplete second was counted: %v", ws.Counts[0])
	}
	c.Advance(time.Second)
	ws := r.Windows()
	if ws.Counts[0][wmEnqueued] != 10 || ws.Counts[0][wmAcknowledged] != 10 || ws.Counts[2][wmEnqueued] != 10 {
		t.Fatalf("windows: %+v", ws.Counts)
	}
}

func TestStaleRingSlotsAreNeverSummed(t *testing.T) {
	c := newClock(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	r := New(c.Now)
	r.SubmitAcked(cidA, 5, 0, 1)
	c.Advance(time.Hour + 30*time.Second) // the slot is reused one hour later
	r.SubmitAcked(cidA, 1, 0, 1)
	c.Advance(time.Second)
	ws := r.Windows()
	if ws.Counts[2][wmEnqueued] != 1 {
		t.Fatalf("60-minute window = %d, want 1 (the old slot is stale)", ws.Counts[2][wmEnqueued])
	}
}

func TestRollingWindowCoveredIsObservedTimeNotNominal(t *testing.T) {
	c := newClock(time.Date(2026, 11, 25, 10, 0, 0, 500_000_000, time.UTC))
	r := New(c.Now)
	c.Advance(20 * time.Second)
	ws := r.Windows()
	if ws.Covered[0] != 19 || ws.Covered[1] != 19 || ws.Covered[2] != 19 {
		t.Fatalf("covered = %v, want 19 s observed for every window", ws.Covered)
	}
	c.Advance(2 * time.Hour)
	if ws := r.Windows(); ws.Covered != [3]int{60, 900, 3600} {
		t.Fatalf("covered = %v", ws.Covered)
	}
}

// ---- layering ----------------------------------------------------------------

// The crawler's DHT path must never depend on statistics.
func TestNoStatisticsImportInCrawlerDHTPath(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/skobkin/magnetico-web/internal/crawler",
		"github.com/skobkin/magnetico-web/dht", "github.com/skobkin/magnetico-web/metadata").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, p := range strings.Fields(string(out)) {
		if strings.Contains(p, "/internal/ingest/stats") || strings.Contains(p, "/internal/opstats") || strings.Contains(p, "/internal/ingest/server") {
			t.Errorf("the crawler path depends on %s", p)
		}
	}
}

var _ = atomic.Int64{}
