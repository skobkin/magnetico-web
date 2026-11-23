package ratelimit

import (
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newLimiter(t *testing.T, c *fakeClock, burst int, interval time.Duration, maxKeys int) *Limiter {
	t.Helper()
	l, err := New(Config{Burst: burst, Interval: interval, MaxKeys: maxKeys, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestNewValidates(t *testing.T) {
	for _, c := range []Config{
		{Burst: 0, Interval: time.Second, MaxKeys: 1},
		{Burst: 1, Interval: 0, MaxKeys: 1},
		{Burst: 1, Interval: time.Second, MaxKeys: 0},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("config %+v accepted", c)
		}
	}
}

func TestBurstThenRefusalWithExactRetryAfter(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c, 3, 10*time.Second, 10)
	k := UserKey("alice")
	for i := 0; i < 3; i++ {
		if r := l.Reserve(k); !r.OK {
			t.Fatalf("attempt %d refused inside the burst", i+1)
		}
	}
	r := l.Reserve(k)
	if r.OK || r.RetryAfter != 10*time.Second {
		t.Fatalf("4th: ok=%v retry=%v, want refused with 10s", r.OK, r.RetryAfter)
	}
	// A refused attempt consumes nothing: the wait does not grow.
	for i := 0; i < 5; i++ {
		l.Reserve(k)
	}
	c.Advance(4 * time.Second)
	if r := l.Reserve(k); r.OK || r.RetryAfter != 6*time.Second {
		t.Fatalf("after 4s: ok=%v retry=%v, want refused with 6s", r.OK, r.RetryAfter)
	}
	c.Advance(6 * time.Second)
	if r := l.Reserve(k); !r.OK {
		t.Fatal("not admitted after the advertised wait")
	}
}

func TestRefillIsGradualAndCapped(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c, 2, time.Minute, 10)
	k := UserKey("bob")
	l.Reserve(k)
	l.Reserve(k)
	if l.Reserve(k).OK {
		t.Fatal("burst exceeded")
	}
	c.Advance(time.Minute)
	if !l.Reserve(k).OK {
		t.Fatal("one unit should have refilled")
	}
	if l.Reserve(k).OK {
		t.Fatal("only one unit should have refilled")
	}
	c.Advance(24 * time.Hour) // idle for a long time: still only the burst
	for i := 0; i < 2; i++ {
		if !l.Reserve(k).OK {
			t.Fatal("full burst expected after a long idle period")
		}
	}
	if l.Reserve(k).OK {
		t.Fatal("refill must be capped at the burst")
	}
}

func TestCancelRefundsOnceAndIsIdempotent(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c, 1, time.Hour, 10)
	k := UserKey("carol")
	r := l.Reserve(k)
	if !r.OK || l.Reserve(k).OK {
		t.Fatal("setup")
	}
	r.Cancel()
	r.Cancel() // second cancel must not refund again
	if !l.Reserve(k).OK {
		t.Fatal("refund did not restore the unit")
	}
	if l.Reserve(k).OK {
		t.Fatal("double Cancel granted an extra unit")
	}
	refused := l.Reserve(k)
	refused.Cancel() // no-op
	if l.Reserve(k).OK {
		t.Fatal("cancelling a refused reservation must not grant anything")
	}
	var nilRes *Reservation
	nilRes.Cancel()
}

func TestReservePairRefundsFirstBucketOnRefusal(t *testing.T) {
	c := newClock()
	ip := newLimiter(t, c, 5, time.Minute, 10)
	user := newLimiter(t, c, 1, time.Hour, 10)
	ka, kb := IPKey(netip.MustParseAddr("192.0.2.1")), UserKey("dave")

	if p := ReservePair(ip, ka, user, kb); !p.OK {
		t.Fatal("first pair refused")
	}
	// The user bucket is empty now; the pair must refuse and must not charge the IP.
	for i := 0; i < 20; i++ {
		p := ReservePair(ip, ka, user, kb)
		if p.OK || p.RetryAfter != time.Hour {
			t.Fatalf("pair %d: ok=%v retry=%v", i, p.OK, p.RetryAfter)
		}
	}
	// IP bucket consumed exactly one unit: four more IP-only reservations fit.
	for i := 0; i < 4; i++ {
		if !ip.Reserve(ka).OK {
			t.Fatalf("IP bucket was charged by refused pairs (reservation %d refused)", i)
		}
	}
	if ip.Reserve(ka).OK {
		t.Fatal("IP bucket allowed more than its burst")
	}
}

func TestReservePairReportsLongestWait(t *testing.T) {
	c := newClock()
	a := newLimiter(t, c, 1, 10*time.Second, 10)
	b := newLimiter(t, c, 1, 30*time.Second, 10)
	k := UserKey("x")
	a.Reserve(k)
	b.Reserve(k)
	if p := ReservePair(a, k, b, k); p.OK || p.RetryAfter != 30*time.Second {
		t.Fatalf("ok=%v retry=%v, want refused with 30s", p.OK, p.RetryAfter)
	}
}

func TestPairPartialRefund(t *testing.T) {
	c := newClock()
	ip := newLimiter(t, c, 1, time.Hour, 10)
	user := newLimiter(t, c, 1, time.Hour, 10)
	ka, kb := IPKey(netip.MustParseAddr("192.0.2.9")), UserKey("erin")
	p := ReservePair(ip, ka, user, kb)
	if !p.OK {
		t.Fatal("pair refused")
	}
	p.B.Cancel() // success refunds the username unit only
	if !user.Reserve(kb).OK {
		t.Fatal("username unit not refunded")
	}
	if ip.Reserve(ka).OK {
		t.Fatal("IP unit must stay consumed")
	}
}

func TestKeyDerivation(t *testing.T) {
	v4 := IPKey(netip.MustParseAddr("192.0.2.7"))
	if v4 != IPKey(netip.MustParseAddr("::ffff:192.0.2.7")) {
		t.Error("IPv4-mapped IPv6 must share the IPv4 key")
	}
	if v4 == IPKey(netip.MustParseAddr("192.0.2.8")) {
		t.Error("distinct IPv4 addresses share a key")
	}
	a := IPKey(netip.MustParseAddr("2001:db8:1:2:aaaa:bbbb:cccc:dddd"))
	b := IPKey(netip.MustParseAddr("2001:db8:1:2:1111:2222:3333:4444"))
	d := IPKey(netip.MustParseAddr("2001:db8:1:3::1"))
	if a != b {
		t.Error("addresses within one /64 must share a key")
	}
	if a == d {
		t.Error("different /64 networks share a key")
	}
	if IPKey(netip.Addr{}) != IPKey(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("invalid and zoned addresses must share the unknown key")
	}
	if IPKey(netip.Addr{}) == v4 {
		t.Error("unknown key collides with a real address")
	}
	if UserKey("Alice") != UserKey("alice") {
		t.Error("username keys must be case-folded")
	}
	if UserKey("alice") == UserKey("alicf") {
		t.Error("distinct usernames share a key")
	}
	// A username can never collide with an address key (distinct kind byte).
	if UserKey("x")[0] != 'u' {
		t.Error("user key kind")
	}
}

func TestMaxKeysBoundUnderChurn(t *testing.T) {
	c := newClock()
	const max = 1000
	l := newLimiter(t, c, 2, time.Minute, max)
	for i := 0; i < 100_000; i++ {
		l.Reserve(UserKey(fmt.Sprintf("churn-%d", i)))
		if n := l.Len(); n > max {
			t.Fatalf("tracked keys %d exceed the bound %d", n, max)
		}
	}
	if l.Len() != max {
		t.Fatalf("expected the table to be full, got %d", l.Len())
	}
}

func TestEvictionPrefersRefilledBuckets(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c, 2, time.Hour, 3)
	busy, i1, i2 := UserKey("busy"), UserKey("i1"), UserKey("i2")
	l.Reserve(busy)
	l.Reserve(busy) // busy: two units taken, full again only after 2h; least recently used
	l.Reserve(i1)   // one unit: full again after 1h
	l.Reserve(i2)
	c.Advance(90 * time.Minute) // i1 and i2 are refilled, busy is not
	l.Reserve(UserKey("new"))   // table is full: one key must go

	l.mu.Lock()
	_, haveBusy := l.m[busy]
	_, haveI1 := l.m[i1]
	_, haveI2 := l.m[i2]
	l.mu.Unlock()
	if !haveBusy {
		t.Fatal("the busy bucket (least recently used but not refilled) was evicted")
	}
	if haveI1 && haveI2 {
		t.Fatal("no refilled bucket was evicted")
	}
	if l.Len() != 3 {
		t.Fatalf("len = %d, want 3", l.Len())
	}
}

func TestEvictionFallsBackToLeastRecentlyUsed(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c, 1, time.Hour, 2)
	a, b := UserKey("a"), UserKey("b")
	l.Reserve(a)
	l.Reserve(b) // both exhausted, nothing refilled
	l.Reserve(UserKey("c"))
	l.mu.Lock()
	_, haveA := l.m[a]
	_, haveB := l.m[b]
	l.mu.Unlock()
	if haveA || !haveB {
		t.Fatalf("expected the least recently used key (a) to be evicted: a=%v b=%v", haveA, haveB)
	}
}

func TestNoOverGrantUnderConcurrency(t *testing.T) {
	c := newClock() // frozen clock: exactly Burst grants are possible
	const burst = 7
	l := newLimiter(t, c, burst, time.Hour, 10)
	k := IPKey(netip.MustParseAddr("198.51.100.1"))
	var granted atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if l.Reserve(k).OK {
					granted.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if granted.Load() != burst {
		t.Fatalf("granted %d, want exactly %d", granted.Load(), burst)
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	cases := map[time.Duration]int{
		0: 1, time.Millisecond: 1, time.Second: 1, time.Second + time.Nanosecond: 2, 29*time.Second + time.Millisecond: 30,
	}
	for d, want := range cases {
		if got := RetryAfterSeconds(d); got != want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", d, got, want)
		}
	}
}

// A refund belongs to the bucket instance that was charged. If that bucket was
// evicted and the key recreated, cancelling the old reservation must not
// credit the new bucket.
func TestCancelIgnoresEvictedBucket(t *testing.T) {
	c := newClock()
	l := newLimiter(t, c, 1, time.Hour, 1) // MaxKeys = 1: every new key evicts the old one
	a, b := UserKey("a"), UserKey("b")

	original := l.Reserve(a) // 1. reserve A
	if !original.OK {
		t.Fatal("setup: first reservation refused")
	}
	if !l.Reserve(b).OK { // 2. evict A by reserving B
		t.Fatal("setup: B refused")
	}
	if !l.Reserve(a).OK { // 3. recreate A and exhaust it
		t.Fatal("setup: recreated A refused")
	}
	if l.Reserve(a).OK {
		t.Fatal("setup: recreated A should be exhausted")
	}
	original.Cancel() // 4. cancel the reservation of the evicted A
	if r := l.Reserve(a); r.OK {
		t.Fatal("a refund of an evicted bucket credited the recreated bucket") // 5.
	}
	// A refund of a live bucket still works.
	live := l.Reserve(UserKey("c")) // evicts a
	live.Cancel()
	if !l.Reserve(UserKey("c")).OK {
		t.Fatal("refund of a live bucket no longer works")
	}
}
