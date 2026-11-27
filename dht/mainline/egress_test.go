package mainline

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/netpolicy"
)

// Learned non-globally-reachable nodes never enter the routing table, also in
// ranges Go's IsPrivate does not cover, and each refusal is counted.
func TestRoutingTableRefusesNonGlobalNodes(t *testing.T) {
	t.Parallel()
	policy := netpolicy.Default()
	rt := newRoutingTable(10, policy)
	rt.addNodes([]net.UDPAddr{
		{IP: net.ParseIP("100.64.0.1"), Port: 6881},       // CGNAT
		{IP: net.ParseIP("198.18.0.1"), Port: 6881},       // benchmarking
		{IP: net.ParseIP("240.0.0.1"), Port: 6881},        // reserved
		{IP: net.ParseIP("2002:c0a8:101::1"), Port: 6881}, // 6to4 of 192.168.1.1
		{IP: net.ParseIP("8.8.8.8"), Port: 6881},
	})
	if got := rt.getNodes(); len(got) != 1 || !got[0].IP.Equal(net.ParseIP("8.8.8.8")) {
		t.Fatalf("routing table = %v, want only 8.8.8.8", got)
	}
	if learned, _, _ := policy.Counts(); learned != 4 {
		t.Fatalf("learned refusals = %d, want 4", learned)
	}
}

// A refused destination never reaches the socket, whichever path produced the
// datagram, and is not reported as a write error.
func TestTransportRefusesNonGlobalDestination(t *testing.T) {
	t.Parallel()
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	policy := netpolicy.Default()
	tr := NewTransport("127.0.0.1:0", func(*Message, *net.UDPAddr) {}, 10, policy)
	tr.Start()
	defer tr.Terminate()

	dst := listener.LocalAddr().(*net.UDPAddr)
	for _, msg := range []*Message{NewPingResponse([]byte("aa"), randomNodeID()), NewFindNodeQuery(randomNodeID(), randomNodeID())} {
		if err := tr.WriteMessages(msg, dst); !errors.Is(err, netpolicy.ErrRefused) {
			t.Fatalf("WriteMessages(%s) = %v, want ErrRefused", msg.Q, err)
		}
	}
	if _, udp, _ := policy.Counts(); udp != 2 {
		t.Fatalf("udp refusals = %d, want 2", udp)
	}

	_ = listener.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 1500)
	if n, _, err := listener.ReadFromUDP(buf); err == nil {
		t.Fatalf("a refused datagram reached the destination (%d bytes)", n)
	}
}

// A bootstrap host that resolves to a refused address is skipped and reported
// once, not on every bootstrap round.
func TestBootstrapSkipsRefusedAddress(t *testing.T) {
	t.Parallel()
	is := NewIndexingService("127.0.0.1:0", 10, IndexingServiceEventHandlers{}, []string{"127.0.0.1:6881"}, nil)
	is.bootstrap()
	is.bootstrap()
	n := 0
	is.refusedBootstrap.Range(func(any, any) bool { n++; return true })
	if n != 1 {
		t.Fatalf("refused bootstrap addresses recorded = %d, want 1", n)
	}
}
