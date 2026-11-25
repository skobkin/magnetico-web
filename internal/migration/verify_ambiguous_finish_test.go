package migration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/migration/state"
)

// finishProxyMode says what the proxy does with the COMMIT that follows the verdict update
// (the bind of 'completed_pass' / 'completed_fail') of a verification run.
type finishProxyMode int

const (
	// ackHeld forwards the COMMIT, so the verdict is durable, and swallows the server's reply:
	// the client sees its cancellation (issue #52) after the commit and before the
	// acknowledgement.
	ackHeld finishProxyMode = iota
	// commitHeld does not forward the COMMIT at all: the server holds the finishing
	// transaction open (row lock taken, verdict not durable) until the test decides.
	commitHeld
)

type commitDecision int

const (
	forwardCommit commitDecision = iota // the held COMMIT reaches the server
	dropCommit                          // the connection is closed without it: the server rolls back
)

// finishProxy is a TCP forwarder for PostgreSQL traffic. Every goroutine and connection it
// owns is tracked and released by its cleanup, which waits for them: a goroutine that could
// not end would hang the test instead of leaking silently.
type finishProxy struct {
	ln       net.Listener
	upstream string
	mode     finishProxyMode

	held     chan struct{}       // closed when the finishing COMMIT was intercepted
	decision chan commitDecision // commitHeld: what to do with it (buffered, read once)
	done     chan struct{}       // closed by the cleanup
	once     sync.Once

	wg    sync.WaitGroup
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func startFinishProxy(t *testing.T, dsn string, mode finishProxyMode) (*finishProxy, string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &finishProxy{ln: ln, upstream: u.Host, mode: mode,
		held: make(chan struct{}), decision: make(chan commitDecision, 1), done: make(chan struct{}),
		conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				p.handle(c)
			}()
		}
	}()
	t.Cleanup(func() {
		close(p.done)
		_ = ln.Close()
		p.mu.Lock()
		for c := range p.conns {
			_ = c.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	u.Host = ln.Addr().String()
	return p, u.String()
}

func (p *finishProxy) track(c net.Conn) {
	p.mu.Lock()
	p.conns[c] = struct{}{}
	p.mu.Unlock()
}

// decide tells a proxy in commitHeld mode what to do with the held COMMIT.
func (p *finishProxy) decide(d commitDecision) { p.decision <- d }

func (p *finishProxy) handle(client net.Conn) {
	server, err := net.Dial("tcp", p.upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	p.track(client)
	p.track(server)
	defer client.Close()
	defer server.Close()

	swallow := make(chan struct{}) // closed: replies of the server are read and discarded
	p.wg.Add(1)
	go func() { // server → client
		defer p.wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := server.Read(buf)
			if n > 0 {
				select {
				case <-swallow:
				default:
					if _, werr := client.Write(buf[:n]); werr != nil {
						return
					}
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
			chunk := append([]byte(nil), buf[:n]...)
			if bytes.Contains(chunk, []byte("completed_pass")) || bytes.Contains(chunk, []byte("completed_fail")) {
				finishing = true
			}
			if finishing && n > 5 && chunk[0] == 'Q' && bytes.EqualFold(bytes.TrimRight(chunk[5:], "\x00"), []byte("commit")) {
				close(swallow)
				switch p.mode {
				case ackHeld:
					if _, werr := server.Write(chunk); werr != nil {
						return
					}
					p.once.Do(func() { close(p.held) })
				case commitHeld:
					p.once.Do(func() { close(p.held) })
					select {
					case d := <-p.decision:
						if d == dropCommit {
							return
						}
						if _, werr := server.Write(chunk); werr != nil {
							return
						}
					case <-p.done:
						return
					}
				}
				_, _ = io.Copy(io.Discard, client) // until the client (or the cleanup) closes it
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

// finishScenario is one verification kind under test.
type finishScenario struct {
	name string
	kind string
	// setup returns the (direct) target DSN and a function running one pass against a DSN.
	setup func(t *testing.T) (tgt string, run func(ctx context.Context, dsn string) (*VerificationResult, error))
}

func finishScenarios() []finishScenario {
	seed := func(t *testing.T) (string, string) {
		src, tgt := decodeFixture(t)
		seedSourceTorrent(t, src, 1, "t1.bin", 10, 1700000001)
		seedSourceFile(t, src, 1, "t1.bin", 10)
		importAll(t, src, tgt)
		return src, tgt
	}
	return []finishScenario{
		{name: "decode", kind: state.VerificationKindDecode, setup: func(t *testing.T) (string, func(context.Context, string) (*VerificationResult, error)) {
			_, tgt := seed(t)
			return tgt, func(ctx context.Context, dsn string) (*VerificationResult, error) {
				return verifyDecoded(t, ctx, dsn, nil)
			}
		}},
		{name: "content", kind: state.VerificationKindContent, setup: func(t *testing.T) (string, func(context.Context, string) (*VerificationResult, error)) {
			src, tgt := seed(t)
			return tgt, func(ctx context.Context, dsn string) (*VerificationResult, error) {
				return verifyContent(t, ctx, src, dsn, nil)
			}
		}},
	}
}

type passOutcome struct {
	res *VerificationResult
	err error
}

// startPass runs one pass in the background and returns its eventual outcome.
func startPass(run func(context.Context, string) (*VerificationResult, error), ctx context.Context, dsn string) <-chan passOutcome {
	out := make(chan passOutcome, 1)
	go func() {
		res, err := run(ctx, dsn)
		out <- passOutcome{res, err}
	}()
	return out
}

func waitOutcome(t *testing.T, out <-chan passOutcome) passOutcome {
	t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(2 * time.Minute):
		t.Fatal("the pass did not return")
		return passOutcome{}
	}
}

func awaitIntercepted(t *testing.T, p *finishProxy) {
	t.Helper()
	select {
	case <-p.held:
	case <-time.After(time.Minute):
		t.Fatal("the finishing COMMIT was never intercepted: the scenario is vacuous")
	}
}

func latestRun(t *testing.T, tgt, kind string) *state.VerificationRun {
	t.Helper()
	run, err := state.LatestVerification(context.Background(), poolForRunner(t, tgt), kind)
	if err != nil || run == nil {
		t.Fatalf("latest %s run = %+v, %v", kind, run, err)
	}
	return run
}

// awaitState waits until the latest run of the kind is in the given state, read through a
// direct connection (not the proxy): the database, not the clock, says when it is so.
func awaitState(t *testing.T, tgt, kind, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		run, err := state.LatestVerification(context.Background(), poolForRunner(t, tgt), kind)
		if err == nil && run != nil && string(run.State) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the latest %s run never reached state %q (last: %+v, %v)", kind, want, run, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitReconcileBlocked waits until the reconciliation read of a finished run is blocked
// behind the unresolved finishing transaction (a lock wait on the run row, reported by
// PostgreSQL itself).
func awaitReconcileBlocked(t *testing.T, tgt string) {
	t.Helper()
	pool := poolForRunner(t, tgt)
	deadline := time.Now().Add(time.Minute)
	for {
		var n int
		err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
            WHERE datname = current_database() AND wait_event_type = 'Lock'
              AND query ILIKE '%verification_runs%' AND query ILIKE '%for share%'`).Scan(&n)
		if err == nil && n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reconciliation never blocked on the unresolved finishing transaction (%v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Issue #52 (a product defect), case 1: the cancellation lands after the server committed the
// verdict and before the client saw the acknowledgement. The pass used to report ErrInterrupted
// and then fail to mark the already completed run interrupted. The verdict is whatever is durable.
// The cancellation is sent only once the database itself shows the durable verdict.
func TestVerifyFinishAlreadyCommittedIsTheDurableVerdict(t *testing.T) {
	t.Parallel()
	for _, sc := range finishScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			tgt, run := sc.setup(t)
			proxy, proxied := startFinishProxy(t, tgt, ackHeld)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := startPass(run, ctx, proxied)
			awaitIntercepted(t, proxy)
			awaitState(t, tgt, sc.kind, string(state.VerificationCompletedPass))
			cancel()
			o := waitOutcome(t, out)
			if o.err != nil {
				t.Fatalf("error = %v, want nil: the verdict (completed_pass) was durable before the cancellation was seen", o.err)
			}
			if o.res == nil || !o.res.Pass {
				t.Fatalf("result = %+v, want the durable pass", o.res)
			}
			if r := latestRun(t, tgt, sc.kind); r.State != state.VerificationCompletedPass {
				t.Fatalf("durable run = %+v", r)
			}
		})
	}
}

// Case 2: the finishing transaction is still unresolved when the cancellation arrives (the
// COMMIT has not reached the server; the transaction holds the run row). The reconciliation must
// wait for it instead of reading around it: when it then commits, the verdict is the durable one.
func TestVerifyFinishUnresolvedThenCommittedIsTheDurableVerdict(t *testing.T) {
	t.Parallel()
	for _, sc := range finishScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			tgt, run := sc.setup(t)
			proxy, proxied := startFinishProxy(t, tgt, commitHeld)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := startPass(run, ctx, proxied)
			awaitIntercepted(t, proxy)
			if r := latestRun(t, tgt, sc.kind); r.State != state.VerificationRunning {
				t.Fatalf("the finishing transaction is unresolved, yet the durable run is %+v", r)
			}
			cancel()
			awaitReconcileBlocked(t, tgt)
			proxy.decide(forwardCommit)
			o := waitOutcome(t, out)
			if o.err != nil {
				t.Fatalf("error = %v, want nil: the unresolved transaction committed the pass", o.err)
			}
			if o.res == nil || !o.res.Pass {
				t.Fatalf("result = %+v, want the durable pass", o.res)
			}
			if r := latestRun(t, tgt, sc.kind); r.State != state.VerificationCompletedPass {
				t.Fatalf("durable run = %+v", r)
			}
		})
	}
}

// Case 3: the unresolved finishing transaction is rolled back (the connection is lost before
// the COMMIT arrives). Nothing is durable: the pass is a resumable interruption, and the next
// invocation resumes it to a pass.
func TestVerifyFinishUnresolvedThenRolledBackIsAResumableInterruption(t *testing.T) {
	t.Parallel()
	for _, sc := range finishScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			tgt, run := sc.setup(t)
			proxy, proxied := startFinishProxy(t, tgt, commitHeld)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := startPass(run, ctx, proxied)
			awaitIntercepted(t, proxy)
			cancel()
			awaitReconcileBlocked(t, tgt)
			proxy.decide(dropCommit)
			o := waitOutcome(t, out)
			if !errors.Is(o.err, ErrInterrupted) || o.res != nil {
				t.Fatalf("outcome = %+v, %v; want ErrInterrupted and no result", o.res, o.err)
			}
			if strings.Contains(o.err.Error(), "also failed") {
				t.Fatalf("the interruption could not be recorded: %v", o.err)
			}
			if r := latestRun(t, tgt, sc.kind); r.State != state.VerificationInterrupted {
				t.Fatalf("durable run = %+v, want interrupted (resumable)", r)
			}
			res, err := run(context.Background(), tgt)
			if err != nil || res == nil || !res.Pass || !res.Resumed {
				t.Fatalf("the next pass = %+v, %v; want a resumed pass", res, err)
			}
		})
	}
}

// Case 4: the outcome stays unresolved for the whole bound of the reconciliation (a network
// that neither delivers nor drops). It must be reported as UNKNOWN: neither as a pass (nothing is
// durable) nor as a resumable interruption (the transaction may still commit). It is the one
// test that sets the bound, so it is not parallel.
func TestVerifyFinishOutcomeNeverResolvedIsReportedUnknown(t *testing.T) {
	old := finishReconcileTimeout
	finishReconcileTimeout = 500 * time.Millisecond
	t.Cleanup(func() { finishReconcileTimeout = old })
	for _, sc := range finishScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			tgt, run := sc.setup(t)
			proxy, proxied := startFinishProxy(t, tgt, commitHeld)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := startPass(run, ctx, proxied)
			awaitIntercepted(t, proxy)
			cancel()
			o := waitOutcome(t, out)
			if o.res != nil || o.err == nil {
				t.Fatalf("outcome = %+v, %v; want an error and no verdict", o.res, o.err)
			}
			if errors.Is(o.err, ErrInterrupted) {
				t.Fatalf("an unresolved finishing transaction was reported as a resumable interruption: %v", o.err)
			}
			if !errors.Is(o.err, errFinishOutcomeUnknown) {
				t.Fatalf("error = %v, want the unknown-outcome error", o.err)
			}
			var fatal *FatalError
			if !errors.As(o.err, &fatal) {
				t.Fatalf("error = %T, want a FatalError (exit 1)", o.err)
			}
			if r := latestRun(t, tgt, sc.kind); r.State != state.VerificationRunning {
				t.Fatalf("durable run = %+v: nothing may be recorded about an unknown outcome", r)
			}
			proxy.decide(dropCommit) // let the server end the transaction before the cleanup
		})
	}
}

// Case 5: the run lock is lost while the verdict is durable but unacknowledged. The lock loss is
// the safety failure: the durable row may belong to another process that adopted the run, so the
// pass must not turn it into its own success, and it is never an operator interruption.
func TestVerifyFinishLockLostDuringTheAmbiguousCommitIsNotASuccess(t *testing.T) {
	t.Parallel()
	for _, sc := range finishScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			tgt, run := sc.setup(t)
			proxy, proxied := startFinishProxy(t, tgt, ackHeld)
			out := startPass(run, context.Background(), proxied) // never cancelled by the test
			awaitIntercepted(t, proxy)
			awaitState(t, tgt, sc.kind, string(state.VerificationCompletedPass))
			killRunLockHolder(t, tgt)
			o := waitOutcome(t, out)
			if o.res != nil || o.err == nil {
				t.Fatalf("outcome = %+v, %v; a lost run lock must not yield a verdict", o.res, o.err)
			}
			if errors.Is(o.err, ErrInterrupted) {
				t.Fatalf("a lock loss was reported as an operator interruption: %v", o.err)
			}
			if !strings.Contains(o.err.Error(), "run lock connection lost") {
				t.Fatalf("error = %v, want the lock-loss cause", o.err)
			}
		})
	}
}

// killRunLockHolder terminates the backend that holds the migrator's advisory lock and waits
// until the lock is really gone (the key is state.migrationLockAdvisoryKey, unexported, split
// into the (classid, objid) pair PostgreSQL stores).
func killRunLockHolder(t *testing.T, tgt string) {
	t.Helper()
	const key = int64(4048864026886105453)
	pool := poolForRunner(t, tgt)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i := 0; i < 200; i++ {
		var pid int32
		err := pool.QueryRow(ctx, `SELECT pid FROM pg_locks WHERE locktype = 'advisory'
            AND classid = $1 AND objid = $2
            AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
			int32(uint32(key>>32)), int32(uint32(key&0xFFFFFFFF))).Scan(&pid)
		if err != nil {
			return // the lock is gone
		}
		if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
			t.Fatalf("pg_terminate_backend(%d): %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the run lock survived 200 termination rounds")
}
