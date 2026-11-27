package metadata

import (
	"net"
	"net/netip"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/netpolicy"
)

var loopbackPolicy = func() *netpolicy.Policy {
	return netpolicy.New(netpolicy.Options{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
}()

// stallPeer accepts TCP connections and never answers, like a peer that
// stops talking mid-handshake. It records live connections.
type stallPeer struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func newStallPeer(t *testing.T) *stallPeer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &stallPeer{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns = append(p.conns, c)
			p.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		p.mu.Lock()
		for _, c := range p.conns {
			c.Close()
		}
		p.mu.Unlock()
	})
	return p
}

func (p *stallPeer) addr() net.TCPAddr { return *p.ln.Addr().(*net.TCPAddr) }

func (p *stallPeer) accepted() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

func result(n byte, peers ...net.TCPAddr) *TestResult {
	return &TestResult{infoHash: [20]byte{n}, peerAddrs: peers}
}

func TestTrySinkBoundsActiveLeechesAndJoinsDuplicates(t *testing.T) {
	peer := newStallPeer(t)
	sink := NewSinkWithOptions(SinkOptions{Deadline: time.Minute, MaxPeersPerHash: 5, MaxActive: 3, Policy: loopbackPolicy})
	defer sink.Terminate()

	for i := 1; i <= 3; i++ {
		if st := sink.TrySink(result(byte(i), peer.addr())); st != SinkStarted {
			t.Fatalf("hash %d: %v, want SinkStarted", i, st)
		}
	}
	if st := sink.TrySink(result(4, peer.addr())); st != SinkBusy {
		t.Fatalf("fourth hash: %v, want SinkBusy at the active limit", st)
	}
	if st := sink.TrySink(result(2, peer.addr())); st != SinkJoined {
		t.Fatalf("running hash again: %v, want SinkJoined (no second leech)", st)
	}
	if sink.Active() != 3 {
		t.Fatalf("Active = %d, want 3", sink.Active())
	}
}

func TestTrySinkAppliesAddressPolicyToFirstPeer(t *testing.T) {
	peer := newStallPeer(t)
	// Default (open) mode: loopback/private peers are NOT allowed, including
	// as the first peer (upstream 1a0ed4b1).
	sink := NewSinkWithOptions(SinkOptions{Deadline: time.Minute, MaxPeersPerHash: 5})
	defer sink.Terminate()
	private := net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 6881}
	if st := sink.TrySink(result(1, private, peer.addr())); st != SinkIgnored {
		t.Fatalf("only disallowed peers: %v, want SinkIgnored", st)
	}
	if peer.accepted() != 0 {
		t.Fatal("connected to a disallowed address")
	}
	// A disallowed first peer is skipped in favour of the first allowed one.
	filtered := NewSinkWithOptions(SinkOptions{Deadline: time.Minute, MaxPeersPerHash: 5, Policy: loopbackPolicy})
	defer filtered.Terminate()
	if st := filtered.TrySink(result(1, net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 6881}, peer.addr())); st != SinkStarted {
		t.Fatalf("filter mode: %v, want SinkStarted", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for peer.accepted() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if peer.accepted() == 0 {
		t.Fatal("the allowed peer was never contacted")
	}
}

// Terminate must interrupt leeches blocked in the handshake, return promptly,
// close the drain channel only afterwards (no send on a closed channel) and
// leak no goroutines.
func TestTerminateCancelsBlockedLeechesWithoutLeaks(t *testing.T) {
	peer := newStallPeer(t)
	before := runtime.NumGoroutine()

	sink := NewSinkWithOptions(SinkOptions{Deadline: time.Hour, MaxPeersPerHash: 5, MaxActive: 8, Policy: loopbackPolicy})
	for i := 1; i <= 8; i++ {
		if st := sink.TrySink(result(byte(i), peer.addr())); st != SinkStarted {
			t.Fatalf("hash %d: %v", i, st)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for peer.accepted() < 8 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	done := make(chan struct{})
	go func() { sink.Terminate(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Terminate did not return: leeches were not cancelled")
	}
	if _, open := <-sink.Drain(); open {
		t.Fatal("Drain must be closed after every leech returned")
	}
	if sink.Active() != 0 {
		t.Fatalf("Active = %d after Terminate", sink.Active())
	}
	// Goroutines settle back (allow slack for the test peer's own goroutines).
	for i := 0; i < 50; i++ {
		if runtime.NumGoroutine() <= before+12 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines: %d before, %d after Terminate", before, runtime.NumGoroutine())
}
