package metadata

import (
	"net"
	"sync"
	"testing"
	"time"
)

// Registration lifecycle (Task 06 review correction 3): a cleanup belonging to
// one registration of a hash must never remove a newer registration of the
// same hash. Before the fix pop() spawned `go flush(hash)` on an empty list and
// run() also flushed on exit; the delayed goroutine could delete a newer
// registration, losing its candidate peers and letting `has` report false
// while that leech was running.

var testPeer = net.TCPAddr{IP: net.ParseIP("1.0.0.2"), Port: 6881}

func newTestInfoHashes() *infoHashes {
	return newInfoHashes(5, nil)
}

// A late pop/release from an earlier registration (any amount of time after
// its owner finished) is a no-op against the current registration.
func TestInfoHashesStaleTokenCannotTouchNewerRegistration(t *testing.T) {
	t.Parallel()
	ih := newTestInfoHashes()
	h := [20]byte{7}

	old, ok := ih.register(h)
	if !ok {
		t.Fatal("first register refused")
	}
	ih.release(h, old)

	fresh, ok := ih.register(h)
	if !ok || fresh == old {
		t.Fatalf("second register: ok=%v token=%d old=%d", ok, fresh, old)
	}
	if !ih.push(h, []net.TCPAddr{testPeer}) {
		t.Fatal("push to a live registration refused")
	}

	// The delayed cleanup of the old owner, in every form it could take.
	ih.release(h, old)
	if got := ih.pop(h, old); got != nil {
		t.Fatalf("stale pop returned %v", got)
	}

	if !ih.has(h) {
		t.Fatal("newer registration was removed by a stale release")
	}
	if got := ih.pop(h, fresh); got == nil || !got.IP.Equal(testPeer.IP) {
		t.Fatalf("newer registration lost its peer: %v", got)
	}
}

// pop on an empty list must not remove anything: removal is the owner's single
// release.
func TestInfoHashesPopDoesNotRemoveAndRegisterIsExclusive(t *testing.T) {
	t.Parallel()
	ih := newTestInfoHashes()
	h := [20]byte{8}
	tok, _ := ih.register(h)
	if _, ok := ih.register(h); ok {
		t.Fatal("a second owner could register a live hash")
	}
	if ih.pop(h, tok) != nil {
		t.Fatal("pop of empty list returned a peer")
	}
	time.Sleep(20 * time.Millisecond) // no background cleanup may fire
	if !ih.has(h) {
		t.Fatal("pop removed the entry")
	}
	ih.release(h, tok)
	if ih.has(h) {
		t.Fatal("release did not remove the entry")
	}
}

// push never creates an entry: a result for a hash whose leech just finished
// must not leave an ownerless entry that would block the hash forever.
func TestInfoHashesPushNeverCreates(t *testing.T) {
	t.Parallel()
	ih := newTestInfoHashes()
	h := [20]byte{9}
	if ih.push(h, []net.TCPAddr{testPeer}) {
		t.Fatal("push reported a registration that does not exist")
	}
	if ih.has(h) {
		t.Fatal("push created an entry")
	}
}

// Many goroutines sink the same hashes while short leeches finish and are
// replaced. At the end nothing may be left registered and no slot leaked.
// Meaningful under -race.
func TestSinkSameHashChurnLeavesNoOrphans(t *testing.T) {
	// A closed loopback port: every dial is refused immediately, so each leech
	// finishes quickly and the hash is re-registered over and over.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().(*net.TCPAddr)
	_ = l.Close()

	sink := NewSinkWithOptions(SinkOptions{Deadline: time.Second, MaxPeersPerHash: 3, MaxActive: 4, FilterNodes: loopbackNets})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				sink.TrySink(result(byte(i%3), *addr))
			}
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for sink.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := sink.Active(); n != 0 {
		t.Fatalf("Active = %d after churn, want 0", n)
	}
	sink.incomingInfoHashes.RLock()
	left := len(sink.incomingInfoHashes.infoHashes)
	sink.incomingInfoHashes.RUnlock()
	if left != 0 {
		t.Fatalf("%d orphaned registrations after all leeches finished", left)
	}
	sink.Terminate()
}
