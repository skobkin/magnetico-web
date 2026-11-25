package migration

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/migration/state"
)

// holdFinishAckProxy forwards PostgreSQL traffic unchanged except for one thing:
// on a connection that has just sent the verdict update of a verification run
// (the bind of 'completed_pass' / 'completed_fail'), the COMMIT that follows is
// forwarded, so the verdict becomes durable, and the server's reply to it is
// HELD. `held` is closed at that moment. The client therefore sees the
// cancellation (issue #52) after the commit and before its acknowledgement:
// the exact window in which the production code used to report an interruption
// for a run whose verdict is durable.
type holdFinishAckProxy struct {
	ln       net.Listener
	upstream string
	held     chan struct{}
	once     sync.Once
}

func startHoldFinishAckProxy(t *testing.T, dsn string) (*holdFinishAckProxy, string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &holdFinishAckProxy{ln: ln, upstream: u.Host, held: make(chan struct{})}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.handle(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	u.Host = ln.Addr().String()
	return p, u.String()
}

func (p *holdFinishAckProxy) handle(client net.Conn) {
	server, err := net.Dial("tcp", p.upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	defer client.Close()
	defer server.Close()
	var hold sync.WaitGroup // closed to release the held server→client direction never
	hold.Add(1)
	holding := make(chan struct{})
	go func() { // server → client, paused once the finishing COMMIT was forwarded
		buf := make([]byte, 32<<10)
		for {
			n, err := server.Read(buf)
			if n > 0 {
				select {
				case <-holding:
					<-time.After(time.Hour) // the acknowledgement is never delivered
				default:
				}
				if _, werr := client.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	finishing := false
	buf := make([]byte, 32<<10)
	for {
		n, err := client.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if bytes.Contains(chunk, []byte("completed_pass")) || bytes.Contains(chunk, []byte("completed_fail")) {
				finishing = true
			}
			if finishing && n > 5 && chunk[0] == 'Q' && bytes.EqualFold(bytes.TrimRight(chunk[5:], "\x00"), []byte("commit")) {
				// forward the COMMIT, give the server time to execute it, then hold its reply
				close(holding)
				if _, werr := server.Write(chunk); werr != nil {
					return
				}
				p.once.Do(func() { close(p.held) })
				io.Copy(io.Discard, client) //nolint:errcheck // keep draining until the client gives up
				return
			}
			if _, werr := server.Write(chunk); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// TestVerifyDecodeVerdictCommittedBeforeTheAcknowledgementIsNotAnInterruption
// (issue #52, a product defect): when the cancellation lands after the server
// committed the verdict but before the client saw the acknowledgement, the pass
// used to report ErrInterrupted and then fail to mark the already completed run
// interrupted ("migration run state is corrupt or inconsistent"). The commit is
// ambiguous from the client's side; the verdict is whatever is durable.
func TestVerifyDecodeVerdictCommittedBeforeTheAcknowledgementIsNotAnInterruption(t *testing.T) {
	t.Parallel()
	src, tgt := decodeFixture(t)
	seedSourceTorrent(t, src, 1, "t1.bin", 10, 1700000001)
	seedSourceFile(t, src, 1, "t1.bin", 10)
	importAll(t, src, tgt)

	proxy, proxied := startHoldFinishAckProxy(t, tgt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-proxy.held:
			time.Sleep(200 * time.Millisecond) // the server has executed the COMMIT
			cancel()
		case <-time.After(60 * time.Second):
		}
	}()
	res, err := verifyDecoded(t, ctx, proxied, nil)
	select {
	case <-proxy.held:
	default:
		t.Fatal("the finishing COMMIT was never intercepted: the scenario is vacuous")
	}
	if err != nil {
		t.Fatalf("error = %v, want nil: the verdict (completed_pass) was durable before the cancellation was seen", err)
	}
	if res == nil || !res.Pass {
		t.Fatalf("result = %+v, want the durable pass", res)
	}
	run, lerr := state.LatestVerification(context.Background(), poolForRunner(t, tgt), state.VerificationKindDecode)
	if lerr != nil || run == nil || run.State != state.VerificationCompletedPass {
		t.Fatalf("durable run = %+v, %v", run, lerr)
	}
}

// The same window in verify-content, which shares the finish path (review finding: only
// verify-decode had a regression test).
func TestVerifyContentVerdictCommittedBeforeTheAcknowledgementIsNotAnInterruption(t *testing.T) {
	t.Parallel()
	src, tgt := decodeFixture(t)
	seedSourceTorrent(t, src, 1, "t1.bin", 10, 1700000001)
	seedSourceFile(t, src, 1, "t1.bin", 10)
	importAll(t, src, tgt)

	proxy, proxied := startHoldFinishAckProxy(t, tgt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-proxy.held:
			time.Sleep(200 * time.Millisecond)
			cancel()
		case <-time.After(60 * time.Second):
		}
	}()
	res, err := verifyContent(t, ctx, src, proxied, nil)
	select {
	case <-proxy.held:
	default:
		t.Fatal("the finishing COMMIT was never intercepted: the scenario is vacuous")
	}
	if err != nil {
		t.Fatalf("error = %v, want nil: the verdict was durable before the cancellation was seen", err)
	}
	if res == nil || !res.Pass {
		t.Fatalf("result = %+v, want the durable pass", res)
	}
	run, lerr := state.LatestVerification(context.Background(), poolForRunner(t, tgt), state.VerificationKindContent)
	if lerr != nil || run == nil || run.State != state.VerificationCompletedPass {
		t.Fatalf("durable run = %+v, %v", run, lerr)
	}
}
