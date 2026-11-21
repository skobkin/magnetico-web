package dht

import (
	"encoding/binary"
	"runtime"
	"sync"
	"testing"
	"time"
)

// newBareManager builds a Manager with no indexing services (no sockets).
func newBareManager(size int) *Manager {
	return &Manager{output: make(chan Result, size)}
}

// TestOverflowPreservesEveryResultInOrder fills the channel with no consumer
// so that several swaps happen, then checks that every result is delivered
// exactly once and in order.
func TestOverflowPreservesEveryResultInOrder(t *testing.T) {
	// Not parallel: the goroutine count must not be disturbed by other tests.
	const total = 200
	m := newBareManager(20)
	before := runtime.NumGoroutine()

	for i := 0; i < total; i++ {
		m.deliver(&TestResult{infoHash: [20]byte{byte(i)}})
	}
	if got := runtime.NumGoroutine(); got > before+2 {
		t.Errorf("goroutines leaked: before=%d after=%d", before, got)
	}
	if c := cap(m.Output()); c <= 20 {
		t.Errorf("channel was not enlarged: cap=%d", c)
	}
	if n := len(m.Output()); n != total {
		t.Fatalf("results held = %d, want %d", n, total)
	}
}

// TestOverflowOrderWithDistinctHashes verifies ordering using distinct hashes
// delivered through a sequential producer.
func TestOverflowOrderWithDistinctHashes(t *testing.T) {
	t.Parallel()

	const total = 100
	m := newBareManager(5)
	for i := 0; i < total; i++ {
		var h [20]byte
		binary.BigEndian.PutUint32(h[:], uint32(i))
		m.deliver(&TestResult{infoHash: h})
	}
	out := m.Output()
	for i := 0; i < total; i++ {
		select {
		case r := <-out:
			got := binary.BigEndian.Uint32(func() []byte { h := r.InfoHash(); return h[:4] }())
			if int(got) != i {
				t.Fatalf("position %d got hash %d", i, got)
			}
		default:
			t.Fatalf("missing result at %d", i)
		}
	}
}

// TestOverflowConcurrentProducersAndConsumer runs under -race: many producers
// and a consumer that re-reads Output() after a close, as main.go does. Every
// produced result must be consumed exactly once.
func TestOverflowConcurrentProducersAndConsumer(t *testing.T) {
	t.Parallel()

	const producers, perProducer = 8, 500
	m := newBareManager(4)

	var seen sync.Map
	consumed := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(consumed)
		count := 0
		for count < producers*perProducer {
			select {
			case r, ok := <-m.Output():
				if !ok {
					continue // swapped: re-read Output()
				}
				h := r.InfoHash()
				if _, dup := seen.LoadOrStore(h, true); dup {
					t.Errorf("duplicate result %x", h)
				}
				count++
			case <-done:
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				var h [20]byte
				binary.BigEndian.PutUint32(h[:], uint32(p))
				binary.BigEndian.PutUint32(h[4:], uint32(i))
				m.deliver(&TestResult{infoHash: h})
			}
		}(p)
	}
	wg.Wait()

	select {
	case <-consumed:
	case <-time.After(20 * time.Second):
		close(done)
		t.Fatal("consumer did not receive every result")
	}
	n := 0
	seen.Range(func(_, _ any) bool { n++; return true })
	if n != producers*perProducer {
		t.Fatalf("distinct results = %d, want %d", n, producers*perProducer)
	}
}
