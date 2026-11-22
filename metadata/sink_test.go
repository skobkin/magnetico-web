package metadata

import (
	"net"
	"testing"
	"time"
)

func TestSink_NewSink(t *testing.T) {
	t.Parallel()

	sink := NewSink(time.Second, 10, []net.IPNet{})
	if sink == nil ||
		len(sink.PeerID) != 20 ||
		sink.deadline != time.Second ||
		sink.drain == nil ||
		sink.incomingInfoHashes == nil ||
		cap(sink.sem) != DefaultMaxActiveLeeches ||
		sink.ctx == nil {
		t.Error("One or more fields of Sink were not initialized correctly")
	}
}

type TestResult struct {
	infoHash  [20]byte
	peerAddrs []net.TCPAddr
}

func (tr *TestResult) InfoHash() [20]byte {
	return tr.infoHash
}

func (tr *TestResult) PeerAddrs() []net.TCPAddr {
	return tr.peerAddrs
}

func TestSink_Sink(t *testing.T) {
	t.Parallel()

	sink := NewSink(time.Minute, 2, []net.IPNet{})
	testResult := &TestResult{
		infoHash:  [20]byte{255},
		peerAddrs: []net.TCPAddr{{IP: net.ParseIP("1.0.0.1"), Port: 443}},
	}
	sink.Sink(testResult)
	if len(sink.incomingInfoHashes.infoHashes) != 0 {
		t.Error("infoHashes should be empty after sinking a result with a single peer")
	}
}

func TestSink_Terminate(t *testing.T) {
	t.Parallel()

	sink := NewSink(time.Minute, 1, []net.IPNet{})
	sink.Terminate()

	if !sink.closed {
		t.Error("closed field of Sink has not been set to true")
	}
	sink.Terminate() // idempotent
	if _, open := <-sink.Drain(); open {
		t.Error("Drain must be closed after Terminate")
	}
}

func TestSink_DrainAfterTerminateIsClosedNotPanic(t *testing.T) {
	t.Parallel()

	sink := NewSink(time.Minute, 1, []net.IPNet{})
	sink.Terminate()
	if _, open := <-sink.Drain(); open {
		t.Error("Drain channel must be closed")
	}
	if st := sink.TrySink(&TestResult{infoHash: [20]byte{1}, peerAddrs: []net.TCPAddr{{IP: net.ParseIP("1.0.0.1"), Port: 4000}}}); st != SinkClosed {
		t.Errorf("TrySink after Terminate = %v, want SinkClosed", st)
	}
}

func TestSink_DeliverDropsWhenTerminatingWithFullDrain(t *testing.T) {
	t.Parallel()

	sink := NewSink(time.Minute, 1, []net.IPNet{})
	for i := 0; i < drainCapacity; i++ {
		sink.drain <- Metadata{}
	}
	done := make(chan struct{})
	go func() { sink.deliver(Metadata{}); close(done) }()
	select {
	case <-done:
		t.Fatal("deliver must block while the drain is full and the sink is live")
	case <-time.After(50 * time.Millisecond):
	}
	sink.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("deliver did not return after cancellation")
	}
	if sink.DroppedOnShutdown() != 1 {
		t.Fatalf("dropped = %d, want 1", sink.DroppedOnShutdown())
	}
}
