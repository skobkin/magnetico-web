package metadata

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/netpolicy"
	"github.com/skobkin/magnetico-web/metadata/btconn"
)

// listenLoopback returns a TCP listener and a channel that receives one value
// per accepted connection.
func listenLoopback(t *testing.T) (*net.TCPAddr, <-chan struct{}) {
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

// The dial guard refuses the resolved address before a connection is
// attempted: the peer never sees an accept.
func TestDialGuardRefusesBeforeConnecting(t *testing.T) {
	t.Parallel()
	addr, accepted := listenLoopback(t)
	policy := netpolicy.Default()

	_, _, _, _, err := btconn.DialContext(context.Background(), policy.ControlContext, addr,
		time.Now().Add(2*time.Second), [8]byte{}, [20]byte{1}, [20]byte{2})
	if !errors.Is(err, netpolicy.ErrRefused) {
		t.Fatalf("DialContext = %v, want ErrRefused", err)
	}
	if _, _, tcp := policy.Counts(); tcp != 1 {
		t.Fatalf("tcp refusals = %d, want 1", tcp)
	}
	select {
	case <-accepted:
		t.Fatal("the refused peer saw a connection")
	case <-time.After(200 * time.Millisecond):
	}
}

// A leech with a dial guard reports the refusal through OnError (as text: the
// leech flattens the dial error).
func TestLeechUsesDialGuard(t *testing.T) {
	t.Parallel()
	addr, accepted := listenLoopback(t)
	var gotErr error
	l := NewLeech([20]byte{1}, addr, randomID(), LeechEventHandlers{
		OnSuccess: func(Metadata) { t.Error("unexpected success") },
		OnError:   func(_ [20]byte, err error) { gotErr = err },
	})
	l.dialControl = netpolicy.Default().ControlContext
	l.DoContext(context.Background(), time.Now().Add(2*time.Second))
	if gotErr == nil || !strings.Contains(gotErr.Error(), netpolicy.ErrRefused.Error()) {
		t.Fatalf("OnError = %v, want a refusal", gotErr)
	}
	select {
	case <-accepted:
		t.Fatal("the refused peer saw a connection")
	default:
	}
}
