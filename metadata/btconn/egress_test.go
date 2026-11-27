package btconn

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/netpolicy"
)

// watchAccepts returns a loopback listener's address and a channel that
// receives one value per accepted connection.
func watchAccepts(t *testing.T) (*net.TCPAddr, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{}, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
			accepted <- struct{}{}
		}
	}()
	return ln.Addr().(*net.TCPAddr), accepted
}

func expectNoAccept(t *testing.T, accepted <-chan struct{}) {
	t.Helper()
	select {
	case <-accepted:
		t.Fatal("the refused peer saw a connection")
	case <-time.After(200 * time.Millisecond):
	}
}

// Without an explicit control, both entry points apply the default egress
// policy: a loopback peer is refused before any connection is attempted.
func TestDefaultDialPathsRefuseNonGlobalPeer(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(2 * time.Second)

	t.Run("DialContext with nil control", func(t *testing.T) {
		t.Parallel()
		addr, accepted := watchAccepts(t)
		_, _, _, _, err := DialContext(context.Background(), nil, addr, deadline, ext1, infoHash, id1)
		if !errors.Is(err, netpolicy.ErrRefused) {
			t.Fatalf("DialContext(nil control) = %v, want ErrRefused", err)
		}
		expectNoAccept(t, accepted)
	})

	t.Run("Dial", func(t *testing.T) {
		t.Parallel()
		addr, accepted := watchAccepts(t)
		_, _, _, _, err := Dial(addr, deadline, ext1, infoHash, id1)
		if !errors.Is(err, netpolicy.ErrRefused) {
			t.Fatalf("Dial = %v, want ErrRefused", err)
		}
		expectNoAccept(t, accepted)
	})
}

// An explicitly supplied policy is used as given: a private-DHT allowlist
// that covers the peer lets the connection through.
func TestExplicitDialControlIsPreserved(t *testing.T) {
	t.Parallel()
	addr, accepted := watchAccepts(t)
	// The handshake fails (the listener closes at once); only the accept matters.
	_, _, _, _, _ = DialContext(context.Background(), loopbackControl, addr, time.Now().Add(2*time.Second), ext1, infoHash, id1)
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("an allowlisted peer was not connected to")
	}
}
