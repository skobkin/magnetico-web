package password

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

var fastParams = Params{Memory: 64, Time: 1, Threads: 1}

func fastService() *Service { return NewService(NewLimiter(2, time.Second), fastParams) }

func TestHashVerifyRoundTrip(t *testing.T) {
	svc := fastService()
	ctx := context.Background()
	hash, err := svc.Hash(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("unexpected format %q", hash)
	}
	if got := svc.params.Classify(hash); got != ClassCurrent || svc.NeedsRehash(hash) {
		t.Fatalf("fresh hash class = %v", got)
	}
	if ok, err := svc.Verify(ctx, "correct horse battery", hash); !ok || err != nil {
		t.Fatalf("verify = %v, %v", ok, err)
	}
	if ok, err := svc.Verify(ctx, "correct horse batterY", hash); ok || err != nil {
		t.Fatalf("wrong password = %v, %v", ok, err)
	}
	// Two hashes of one password differ (random salt).
	other, _ := svc.Hash(ctx, "correct horse battery")
	if other == hash {
		t.Fatal("salt is not random")
	}
}

func TestCurrentHashClassifiedWithProductionParams(t *testing.T) {
	// Built by hand to avoid a 64 MiB hash in a unit test.
	h := argonHash{id: true, memory: 65536, time: 3, threads: 4, salt: make([]byte, 16), key: make([]byte, 32)}
	if got := Classify(h.encode()); got != ClassCurrent {
		t.Fatalf("class = %v", got)
	}
	h.time = 4
	if got := Classify(h.encode()); got != ClassLegacy {
		t.Fatalf("t=4 class = %v, want legacy", got)
	}
	h.id = false
	h.time = 3
	if got := Classify(h.encode()); got != ClassLegacy {
		t.Fatalf("argon2i class = %v, want legacy", got)
	}
}

func TestLengthRules(t *testing.T) {
	svc := fastService()
	ctx := context.Background()
	if _, err := svc.Hash(ctx, strings.Repeat("a", 7)); !errors.Is(err, ErrTooShort) {
		t.Errorf("7 bytes: %v", err)
	}
	if _, err := svc.Hash(ctx, strings.Repeat("a", 8)); err != nil {
		t.Errorf("8 bytes: %v", err)
	}
	if _, err := svc.Hash(ctx, strings.Repeat("a", 1024)); err != nil {
		t.Errorf("1024 bytes: %v", err)
	}
	if _, err := svc.Hash(ctx, strings.Repeat("a", 1025)); !errors.Is(err, ErrTooLong) {
		t.Errorf("1025 bytes: %v", err)
	}
	hash, _ := svc.Hash(ctx, "some password")
	// 4096 is accepted by Verify; 4097 is refused before any hashing.
	if _, err := svc.Verify(ctx, strings.Repeat("a", 4096), hash); err != nil {
		t.Errorf("4096 bytes: %v", err)
	}
	busyBefore := svc.Limiter().BusyCount()
	if _, err := svc.Verify(ctx, strings.Repeat("a", 4097), hash); !errors.Is(err, ErrTooLong) {
		t.Errorf("4097 bytes: %v", err)
	}
	if svc.Limiter().BusyCount() != busyBefore {
		t.Error("an oversize password reached the limiter")
	}
}

func TestParameterBoundsAndUnsupportedFormats(t *testing.T) {
	salt, key := "c29tZXNhbHRzYWx0", "AAAAAAAAAAAAAAAAAAAAAA" // 12-byte salt, 16-byte key
	mk := func(alg, params string) string { return "$" + alg + "$v=19$" + params + "$" + salt + "$" + key }
	cases := []struct {
		name string
		hash string
		want Class
	}{
		{"max accepted", mk("argon2id", "m=65536,t=10,p=16"), ClassLegacy},
		{"memory above bound", mk("argon2id", "m=65537,t=3,p=4"), ClassUnsupported},
		{"time above bound", mk("argon2id", "m=65536,t=11,p=4"), ClassUnsupported},
		{"threads above bound", mk("argon2id", "m=65536,t=3,p=17"), ClassUnsupported},
		{"zero time", mk("argon2id", "m=65536,t=0,p=4"), ClassUnsupported},
		{"zero threads", mk("argon2id", "m=65536,t=3,p=0"), ClassUnsupported},
		{"memory below 8p", mk("argon2id", "m=31,t=3,p=4"), ClassUnsupported},
		{"huge memory", mk("argon2id", "m=4294967295,t=3,p=4"), ClassUnsupported},
		{"overflowing memory", mk("argon2id", "m=99999999999,t=3,p=4"), ClassUnsupported},
		{"leading zero", mk("argon2id", "m=065536,t=3,p=4"), ClassUnsupported},
		{"signed", mk("argon2id", "m=+65536,t=3,p=4"), ClassUnsupported},
		{"reordered params", mk("argon2id", "t=3,m=65536,p=4"), ClassUnsupported},
		{"extra param", mk("argon2id", "m=65536,t=3,p=4,x=1"), ClassUnsupported},
		{"argon2d", mk("argon2d", "m=65536,t=3,p=4"), ClassUnsupported},
		{"version 16", "$argon2id$v=16$m=65536,t=3,p=4$" + salt + "$" + key, ClassUnsupported},
		{"no version", "$argon2id$m=65536,t=3,p=4$" + salt + "$" + key, ClassUnsupported},
		{"padded base64", "$argon2id$v=19$m=65536,t=3,p=4$" + salt + "=$" + key, ClassUnsupported},
		{"short salt", "$argon2id$v=19$m=65536,t=3,p=4$AAAA$" + key, ClassUnsupported},
		{"short key", "$argon2id$v=19$m=65536,t=3,p=4$" + salt + "$AAAA", ClassUnsupported},
		{"bcrypt 2a", "$2a$10$" + strings.Repeat("a", 53), ClassUnsupported},
		{"bcrypt 2b", "$2b$10$" + strings.Repeat("a", 53), ClassUnsupported},
		{"bcrypt 2y", "$2y$10$" + strings.Repeat("a", 53), ClassLegacy},
		{"bcrypt cost 3", "$2y$03$" + strings.Repeat("a", 53), ClassUnsupported},
		{"bcrypt cost 17", "$2y$17$" + strings.Repeat("a", 53), ClassUnsupported},
		{"bcrypt cost 16", "$2y$16$" + strings.Repeat("a", 53), ClassLegacy},
		{"bcrypt bad length", "$2y$10$" + strings.Repeat("a", 52), ClassUnsupported},
		{"bcrypt bad alphabet", "$2y$10$" + strings.Repeat("!", 53), ClassUnsupported},
		{"md5 crypt", "$1$abcdefgh$abcdefghijklmnopqrstuv", ClassUnsupported},
		{"sha512 crypt", "$6$salt$abcdefghijklmnopqrstuv", ClassUnsupported},
		{"plain text", "hunter2", ClassUnsupported},
		{"empty", "", ClassNone},
	}
	svc := fastService()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.hash); got != c.want {
				t.Fatalf("Classify = %v, want %v", got, c.want)
			}
			if c.want == ClassUnsupported || c.want == ClassNone {
				ok, err := svc.Verify(context.Background(), "anything", c.hash)
				if ok || !errors.Is(err, ErrUnsupportedHash) {
					t.Fatalf("Verify = %v, %v; want ErrUnsupportedHash", ok, err)
				}
			}
		})
	}
}

func TestBcryptNULInputNeverMatches(t *testing.T) {
	_, cases := loadFixtures(t)
	for _, c := range cases {
		if c.ID != "bcrypt_2y_cost10/len8" {
			continue
		}
		pw := b64(t, c.PasswordB64)
		ok, err := fastService().Verify(context.Background(), pw+"\x00junk", c.Hash)
		if ok || err != nil {
			t.Fatalf("Verify with NUL = %v, %v", ok, err)
		}
		return
	}
	t.Fatal("fixture not found")
}

func TestLimiterCapsConcurrencyAndCountsBusy(t *testing.T) {
	const capacity = 2
	l := NewLimiter(capacity, 50*time.Millisecond)
	var running, peak atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	var busy atomic.Int32
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := l.Do(context.Background(), func() {
				n := running.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				<-release
				running.Add(-1)
			})
			if errors.Is(err, ErrBusy) {
				busy.Add(1)
			} else if err != nil {
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	// Let the waiters time out while the two slots stay held.
	time.Sleep(300 * time.Millisecond)
	close(release)
	wg.Wait()
	if p := peak.Load(); p > capacity {
		t.Fatalf("observed concurrency %d above the cap %d", p, capacity)
	}
	if busy.Load() != 30 || l.BusyCount() != 30 {
		t.Fatalf("busy callers = %d, counter = %d; want 30", busy.Load(), l.BusyCount())
	}
}

func TestLimiterHonoursContext(t *testing.T) {
	l := NewLimiter(1, time.Minute)
	hold := make(chan struct{})
	go func() { _ = l.Do(context.Background(), func() { <-hold }) }()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := l.Do(ctx, func() { t.Error("ran without a slot") }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	close(hold)
	if l.BusyCount() != 0 {
		t.Fatal("a cancelled wait counted as busy")
	}
}

func TestDefaultCapacity(t *testing.T) {
	if c := DefaultCapacity(); c < 1 || c > 2 {
		t.Fatalf("capacity %d outside 1..2", c)
	}
}

// Hash, Verify and DummyVerify all go through the limiter.
func TestEverythingPassesThroughTheLimiter(t *testing.T) {
	l := NewLimiter(1, 30*time.Millisecond)
	svc := NewService(l, fastParams)
	ctx := context.Background()
	hash, err := svc.Hash(ctx, "a password")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.DummyVerify(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	started := make(chan struct{})
	go func() { _ = l.Do(ctx, func() { close(started); <-hold }) }()
	<-started
	defer close(hold)
	if _, err := svc.Hash(ctx, "another password"); !errors.Is(err, ErrBusy) {
		t.Errorf("Hash: %v", err)
	}
	if _, err := svc.Verify(ctx, "a password", hash); !errors.Is(err, ErrBusy) {
		t.Errorf("Verify: %v", err)
	}
	if err := svc.DummyVerify(ctx, "x"); !errors.Is(err, ErrBusy) {
		t.Errorf("DummyVerify: %v", err)
	}
	if l.BusyCount() != 3 {
		t.Errorf("busy = %d, want 3", l.BusyCount())
	}
}

func TestDummyVerifyRequiresPreparationAndRunsOneVerification(t *testing.T) {
	svc := fastService()
	ctx := context.Background()
	if err := svc.DummyVerify(ctx, "x"); !errors.Is(err, ErrNotPrepared) {
		t.Fatalf("before Prepare: %v", err)
	}
	if err := svc.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	first := *svc.dummy.Load()
	if svc.params.Classify(first) != ClassCurrent {
		t.Fatalf("dummy hash %q is not at current parameters", first)
	}
	// DummyVerify performs a single verification (no initialization hash).
	if err := svc.DummyVerify(ctx, strings.Repeat("z", 5000)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Prepare(ctx); err != nil || *svc.dummy.Load() != first {
		t.Fatal("a second Prepare recomputed the reference")
	}
}

func TestConcurrentPrepareGeneratesOnce(t *testing.T) {
	svc := fastService()
	var wg sync.WaitGroup
	seen := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.Prepare(context.Background()); err != nil {
				t.Error(err)
				return
			}
			seen <- *svc.dummy.Load()
		}()
	}
	wg.Wait()
	close(seen)
	var first string
	for h := range seen {
		if first == "" {
			first = h
		} else if h != first {
			t.Fatal("concurrent callers produced different reference hashes")
		}
	}
	// Exactly one hash computation went through the limiter: with capacity 1
	// held by us, a further Prepare is a no-op and does not queue.
	hold := make(chan struct{})
	started := make(chan struct{})
	l := svc.limiter
	go func() { _ = l.Do(context.Background(), func() { close(started); <-hold }) }()
	<-started
	defer close(hold)
	if err := svc.Prepare(context.Background()); err != nil {
		t.Fatalf("prepared service re-entered the limiter: %v", err)
	}
}

func TestPrepareFailureLeavesServiceRetryable(t *testing.T) {
	l := NewLimiter(1, 20*time.Millisecond)
	svc := NewService(l, fastParams)
	hold := make(chan struct{})
	started := make(chan struct{})
	go func() { _ = l.Do(context.Background(), func() { close(started); <-hold }) }()
	<-started
	if err := svc.Prepare(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("Prepare under load: %v", err)
	}
	if svc.dummy.Load() != nil {
		t.Fatal("failed Prepare left a reference behind")
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Prepare(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Prepare: %v", err)
	}
	close(hold)
	if err := svc.Prepare(context.Background()); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if svc.dummy.Load() == nil {
		t.Fatal("not prepared after the retry")
	}
}

func TestPrepareWaitHonoursContext(t *testing.T) {
	svc := fastService()
	svc.prepSlot <- struct{}{} // a Prepare is "in progress"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := svc.Prepare(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestCancelledContextNeverRunsTheFunction(t *testing.T) {
	l := NewLimiter(2, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Do(ctx, func() { t.Error("fn ran under a cancelled context") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	svc := fastService()
	if _, err := svc.Hash(ctx, "a long enough password"); !errors.Is(err, context.Canceled) {
		t.Errorf("Hash: %v", err)
	}
	hash, _ := svc.Hash(context.Background(), "a long enough password")
	if _, err := svc.Verify(ctx, "a long enough password", hash); !errors.Is(err, context.Canceled) {
		t.Errorf("Verify: %v", err)
	}
	// Cancelled while waiting for a slot, then the slot frees: still no run.
	l1 := NewLimiter(1, time.Minute)
	hold := make(chan struct{})
	started := make(chan struct{})
	go func() { _ = l1.Do(context.Background(), func() { close(started); <-hold }) }()
	<-started
	ctx2, cancel2 := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l1.Do(ctx2, func() { t.Error("fn ran after cancellation") }) }()
	time.Sleep(30 * time.Millisecond)
	cancel2()
	close(hold)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter: %v", err)
	}
}

func TestOverlongPHCStringsAreRejectedBeforeDecoding(t *testing.T) {
	salt, key := "c29tZXNhbHRzYWx0", "AAAAAAAAAAAAAAAAAAAAAA"
	valid := "$argon2id$v=19$m=65536,t=3,p=4$" + salt + "$" + key
	// The longest legal string still parses (64-byte salt and key).
	b64 := func(n int) string { return strings.Repeat("A", (n*4+2)/3) } // unpadded length for n bytes
	longest := "$argon2id$v=19$m=65536,t=10,p=16$" + b64(64) + "$" + b64(64)
	if len(longest) > maxPHCLen {
		t.Fatalf("maxPHCLen %d is below the longest valid string (%d)", maxPHCLen, len(longest))
	}
	if _, ok := parseArgon(longest); !ok {
		t.Fatal("the longest valid string was rejected")
	}
	bad := map[string]string{
		"oversized salt field": "$argon2id$v=19$m=65536,t=3,p=4$" + strings.Repeat("A", 100000) + "$" + key,
		"oversized key field":  "$argon2id$v=19$m=65536,t=3,p=4$" + salt + "$" + strings.Repeat("A", 100000),
		"just over the limit":  valid + strings.Repeat("A", maxPHCLen),
		"megabytes of dollars": strings.Repeat("$", 4<<20),
		"megabyte hash":        "$argon2id$v=19$m=65536,t=3,p=4$" + salt + "$" + strings.Repeat("A", 1<<20),
		"megabyte bcrypt":      "$2y$10$" + strings.Repeat("a", 1<<20),
	}
	svc := fastService()
	for name, h := range bad {
		if got := Classify(h); got != ClassUnsupported {
			t.Errorf("%s: Classify = %v", name, got)
		}
		if _, err := svc.Verify(context.Background(), "pw", h); !errors.Is(err, ErrUnsupportedHash) {
			t.Errorf("%s: Verify err = %v", name, err)
		}
	}
	// No allocation amplification: rejecting a 4 MiB string allocates (almost) nothing.
	big := strings.Repeat("$", 4<<20)
	if allocs := testing.AllocsPerRun(5, func() { parseArgon(big) }); allocs != 0 {
		t.Errorf("parseArgon allocated %v times for an overlong string", allocs)
	}
}

func TestNewServiceRejectsParametersItCouldNotVerify(t *testing.T) {
	for _, p := range []Params{{Memory: 65537, Time: 3, Threads: 4}, {Memory: 65536, Time: 11, Threads: 4},
		{Memory: 65536, Time: 3, Threads: 17}, {Memory: 8, Time: 3, Threads: 4}, {Memory: 65536, Time: 0, Threads: 4}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%+v accepted", p)
				}
			}()
			NewService(NewLimiter(1, time.Second), p)
		}()
	}
}

func TestDefaultServiceUsesSpecParameters(t *testing.T) {
	if CurrentParams != (Params{Memory: 65536, Time: 3, Threads: 4}) {
		t.Fatalf("CurrentParams = %+v", CurrentParams)
	}
	if Default.Limiter().Capacity() != DefaultCapacity() {
		t.Fatal("default limiter capacity")
	}
}

var _ = argon2.Version

func TestRehashAcceptsAnyLoginLengthAndProducesCurrentHash(t *testing.T) {
	svc := fastService()
	ctx := context.Background()
	for _, n := range []int{1, 7, 8, 1025, MaxLoginLength} {
		pw := strings.Repeat("p", n)
		h, err := svc.Rehash(ctx, pw)
		if err != nil {
			t.Fatalf("len %d: %v", n, err)
		}
		if svc.params.Classify(h) != ClassCurrent {
			t.Errorf("len %d: rehash is not current-class", n)
		}
		if ok, err := svc.Verify(ctx, pw, h); err != nil || !ok {
			t.Errorf("len %d: rehash does not verify: %v %v", n, ok, err)
		}
		if ok, _ := svc.Verify(ctx, pw+"x", h); ok && n < MaxLoginLength {
			t.Errorf("len %d: rehash verifies a different password", n)
		}
	}
	if _, err := svc.Rehash(ctx, ""); !errors.Is(err, ErrTooShort) {
		t.Errorf("empty: %v", err)
	}
	if _, err := svc.Rehash(ctx, strings.Repeat("p", MaxLoginLength+1)); !errors.Is(err, ErrTooLong) {
		t.Errorf("over limit: %v", err)
	}
	// Hash still enforces the new-password policy; Rehash deliberately does not.
	if _, err := svc.Hash(ctx, "short"); !errors.Is(err, ErrTooShort) {
		t.Errorf("Hash policy changed: %v", err)
	}
}

func TestRehashGoesThroughTheLimiter(t *testing.T) {
	l := NewLimiter(1, 50*time.Millisecond)
	svc := NewService(l, fastParams)
	// Hold the only slot; Rehash must be refused with ErrBusy, not bypass it.
	release := make(chan struct{})
	held := make(chan struct{})
	go func() { _ = l.Do(context.Background(), func() { close(held); <-release }) }()
	<-held
	_, err := svc.Rehash(context.Background(), "some password")
	close(release)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Rehash with a saturated limiter: %v, want ErrBusy", err)
	}
}
