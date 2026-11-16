package main

// 04D fault-injection fixture suite: deterministic, observation-gated
// interruption and recovery scenarios driven against the real binary
// (04D matrix IDs F1–F6; the real-sample campaign phases are R2–R9).
//
// Every trigger polls the persisted migration state and fires only after the
// intended state is OBSERVED — never after a blind sleep — and every
// interruption records the pre-kill observation, the signal, the real exit
// status, and a post-death atomicity snapshot taken against the authoritative
// (post-death) checkpoint. Recovery always uses the ordinary CLI resume path.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

const (
	// The 50k/batch-10 scale of the proven 04A binary smoke: large enough
	// that a poll-based trigger lands mid-run, small enough for a fast
	// fixture test.
	faultTorrents  = 50000
	faultBatchSize = 10

	faultPollInterval = 250 * time.Millisecond
	faultImportBudget = 8 * time.Minute
)

func faultTS() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// seedFaultSource creates the frozen v0.12.0 source layout with n clean
// single-file torrents (ids 1..n, contiguous).
func seedFaultSource(t *testing.T, dsn string, n int) {
	t.Helper()
	execSmokeSQL(t, dsn, smokeSourceDDL)
	execSmokeSQL(t, dsn, fmt.Sprintf(`
        INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        SELECT g, decode(lpad(to_hex(g), 40, '0'), 'hex'), 't' || g, 2, 1700000000 + g
        FROM generate_series(1, %d) g;
        INSERT INTO magneticod.files (torrent_id, path, size)
        SELECT g, 'f' || g || '.bin', 2 FROM generate_series(1, %d) g;`, n, n))
}

func initFaultTarget(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	if err := persistence.InitSchema(ctx, dsn); err != nil {
		t.Fatalf("InitSchema() error = %v", err)
	}
}

// safeBuffer tolerates concurrent writes from the pipe feeder goroutine.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// runningBinary wraps a started `magnetico migrate torrents` process that is
// left running so the test can observe state and trigger the fault.
type runningBinary struct {
	t    *testing.T
	cmd  *exec.Cmd
	out  *safeBuffer
	done chan struct{}
}

// startFaultImport starts an import and returns while it runs. A watchdog
// context kills the process after faultImportBudget so a hung run fails the
// test instead of hanging the suite.
func startFaultImport(t *testing.T, bin, sourceDSN, targetDSN string, extraArgs ...string) *runningBinary {
	t.Helper()
	args := append([]string{"migrate", "torrents", "--database", targetDSN,
		"--batch-size", fmt.Sprint(faultBatchSize)}, extraArgs...)
	ctx, cancel := context.WithTimeout(context.Background(), faultImportBudget)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "MAGNETICO_MIGRATION_SOURCE_DSN="+sourceDSN)
	buf := &safeBuffer{}
	cmd.Stdout, cmd.Stderr = buf, buf
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %v: %v", args, err)
	}
	t.Logf("%s: started pid=%d: magnetico %v", faultTS(), cmd.Process.Pid, args)
	r := &runningBinary{t: t, cmd: cmd, out: buf, done: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-r.done:
		default:
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			t.Logf("cleanup: killed still-running pid %d", cmd.Process.Pid)
		}
		cancel()
	})
	return r
}

// waitExit reaps the process and returns its real exit status: exit code,
// and the signal name when death was by signal (code -1).
func (r *runningBinary) waitExit() (int, syscall.Signal, string) {
	err := r.cmd.Wait()
	close(r.done)
	out := r.out.String()
	r.t.Logf("%s: pid %d exited; output:\n%s", faultTS(), r.cmd.Process.Pid, out)
	if err == nil {
		return 0, 0, out
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		r.t.Fatalf("wait: %v", err)
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -1, ws.Signal(), out
	}
	return exitErr.ExitCode(), 0, out
}

// signal sends sig and records it with a timestamp (the interruption record).
func (r *runningBinary) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	t.Logf("%s: sending %s to pid %d", faultTS(), sig.String(), r.cmd.Process.Pid)
	if err := r.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal %s to pid %d: %v", sig.String(), r.cmd.Process.Pid, err)
	}
}

// tryLoadProgress reads the migration progress row; a target without run
// state yet (preflight has not committed) yields an error, not a failure.
func tryLoadProgress(targetDSN string) (*state.Progress, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := target.OpenPool(ctx, targetDSN)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	return state.Load(ctx, pool)
}

// pollProgress polls the target's migration progress until cond holds and
// logs every advancing observation — the phase evidence required before any
// trigger fires. Fails the test on timeout.
func pollProgress(t *testing.T, targetDSN string, cond func(*state.Progress) bool, timeout time.Duration) *state.Progress {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *state.Progress
	for time.Now().Before(deadline) {
		p, err := tryLoadProgress(targetDSN)
		if err == nil && p != nil {
			if last == nil || last.ScannedCount != p.ScannedCount || last.LastProcessedID != p.LastProcessedID {
				t.Logf("%s: progress phase=%s scanned=%d checkpoint=%d success=%d errors=%d",
					faultTS(), p.Phase, p.ScannedCount, p.LastProcessedID, p.SuccessCount, p.ErrorCount)
			}
			if cond(p) {
				return p
			}
			last = p
		}
		time.Sleep(faultPollInterval)
	}
	t.Fatalf("trigger condition not met within %s; last progress %+v (scenario NOT EXERCISED)", timeout, last)
	return nil
}

// faultSnapshot is the post-interruption database observation used for the
// atomicity assertions. Everything is read against the authoritative
// checkpoint as of THIS snapshot, never a pre-kill observation.
type faultSnapshot struct {
	Checkpoint       int64
	Scanned          int64
	Success          int64
	Errors           int64
	TorrentRows      int64
	RowsBeyond       int64
	DistinctIDs      int64
	MaxID            int64
	DistinctInfoHash int64
}

const faultSnapshotSQL = `
WITH p AS (SELECT last_processed_id, scanned_count, success_count, error_count FROM magnetico_migration.progress)
SELECT p.last_processed_id, p.scanned_count, p.success_count, p.error_count,
       (SELECT count(*) FROM magneticod.torrents),
       (SELECT count(*) FROM magneticod.torrents WHERE id > p.last_processed_id),
       (SELECT count(DISTINCT id) FROM magneticod.torrents),
       (SELECT coalesce(max(id), 0) FROM magneticod.torrents),
       (SELECT count(DISTINCT info_hash) FROM magneticod.torrents)
FROM p`

func readFaultSnapshot(t *testing.T, dsn string) faultSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("snapshot connect: %v", err)
	}
	defer conn.Close(context.Background())
	var s faultSnapshot
	err = conn.QueryRow(ctx, faultSnapshotSQL).Scan(
		&s.Checkpoint, &s.Scanned, &s.Success, &s.Errors,
		&s.TorrentRows, &s.RowsBeyond, &s.DistinctIDs, &s.MaxID, &s.DistinctInfoHash)
	if err != nil {
		t.Fatalf("snapshot query: %v", err)
	}
	return s
}

// assertPostDeathAtomicity proves the interrupted batch was all-or-nothing:
// no rows landed beyond the authoritative checkpoint, the row count matches
// success_count, and the accounting invariant holds.
func assertPostDeathAtomicity(t *testing.T, dsn string) faultSnapshot {
	t.Helper()
	s := readFaultSnapshot(t, dsn)
	t.Logf("%s: post-death snapshot: checkpoint=%d scanned=%d success=%d errors=%d rows=%d "+
		"beyond_checkpoint=%d distinct_ids=%d max_id=%d distinct_info_hash=%d",
		faultTS(), s.Checkpoint, s.Scanned, s.Success, s.Errors, s.TorrentRows,
		s.RowsBeyond, s.DistinctIDs, s.MaxID, s.DistinctInfoHash)
	if s.RowsBeyond != 0 {
		t.Fatalf("%d torrent rows exist beyond checkpoint %d: a batch partially landed", s.RowsBeyond, s.Checkpoint)
	}
	if s.TorrentRows != s.Success {
		t.Fatalf("torrent rows %d != success_count %d at checkpoint %d", s.TorrentRows, s.Success, s.Checkpoint)
	}
	if s.Scanned != s.Success+s.Errors {
		t.Fatalf("scanned %d != success %d + errors %d (accounting invariant)", s.Scanned, s.Success, s.Errors)
	}
	if s.DistinctIDs != s.TorrentRows || s.DistinctInfoHash != s.TorrentRows {
		t.Fatalf("duplicate ids (%d distinct of %d) or info hashes (%d distinct)", s.DistinctIDs, s.TorrentRows, s.DistinctInfoHash)
	}
	return s
}

// assertFaultComplete asserts the completed resume: exact row set, no
// duplicates, the import-complete phase, and clean counters.
func assertFaultComplete(t *testing.T, dsn string, n int64) {
	t.Helper()
	s := readFaultSnapshot(t, dsn)
	if s.TorrentRows != n || s.MaxID != n || s.DistinctIDs != n || s.DistinctInfoHash != n {
		t.Fatalf("final snapshot %+v: want exactly %d rows, contiguous ids 1..%d, no duplicates", s, n, n)
	}
	p := smokeProgress(t, dsn)
	if p.Phase != state.PhaseImportComplete || p.ScannedCount != n || p.SuccessCount != n || p.ErrorCount != 0 {
		t.Fatalf("final progress %+v: want import_complete with scanned=success=%d, 0 errors", p, n)
	}
}

// runFaultResume runs one ordinary (blocking, uninterrupted) resume via the
// CLI — recovery never repairs checkpoint or schema by hand.
func runFaultResume(t *testing.T, bin, sourceDSN, targetDSN string, extraArgs ...string) int {
	t.Helper()
	args := append([]string{"migrate", "torrents", "--database", targetDSN,
		"--batch-size", fmt.Sprint(faultBatchSize)}, extraArgs...)
	ctx, cancel := context.WithTimeout(context.Background(), faultImportBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "MAGNETICO_MIGRATION_SOURCE_DSN="+sourceDSN)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	t.Logf("%s: resumed `magnetico %v` exited; output:\n%s", faultTS(), args, out.String())
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("wait: %v", err)
	}
	return exitErr.ExitCode()
}

// faultImportingAt returns a trigger condition for "the import is in the
// data-importing phase and has committed past scanned".
func faultImportingAt(scanned int64) func(*state.Progress) bool {
	return func(p *state.Progress) bool {
		return p.Phase == state.PhaseImporting && p.ScannedCount >= scanned
	}
}

// F1 — SIGKILL mid-import: death by signal with no graceful path, then an
// ordinary CLI resume that completes with the exact row set.
func TestFaultImportKilledBySignalResumes(t *testing.T) {
	pgtest.IntegrationCluster(t)

	src := pgtest.IntegrationCluster(t).TestDatabase(t)
	seedFaultSource(t, src, faultTorrents)
	tgt := pgtest.IntegrationCluster(t).TestDatabase(t)
	initFaultTarget(t, tgt)
	bin := buildBinary(t)

	run := startFaultImport(t, bin, src, tgt)

	// Trigger only after committed advance is observed in the importing
	// phase (scanned_count only advances with committed batches).
	pre := *pollProgress(t, tgt, faultImportingAt(faultTorrents/10), 90*time.Second)

	run.signal(t, syscall.SIGKILL)
	code, sig, _ := run.waitExit()
	if code != -1 || sig != syscall.SIGKILL {
		t.Fatalf("exit code=%d signal=%v; want death by SIGKILL", code, sig)
	}

	// Atomicity against the authoritative post-death checkpoint (a batch
	// may legitimately have committed between the observation and death).
	post := assertPostDeathAtomicity(t, tgt)
	t.Logf("%s: checkpoint advanced between last observation (%d) and death (%d): +%d",
		faultTS(), pre.LastProcessedID, post.Checkpoint, post.Checkpoint-pre.LastProcessedID)
	if post.Checkpoint < pre.LastProcessedID {
		t.Fatalf("checkpoint regressed: observed %d, post-death %d", pre.LastProcessedID, post.Checkpoint)
	}

	if code := runFaultResume(t, bin, src, tgt); code != 0 {
		t.Fatalf("resume exit code = %d, want 0", code)
	}
	assertFaultComplete(t, tgt, faultTorrents)
}

// commitCutProxy is a test-only TCP proxy between the binary and the test
// server. Once armed it swells the acknowledgement of exactly one transaction
// COMMIT: the commit is forwarded and confirmed server-side (ReadyForQuery
// consumed) before the client connection is severed — the lost-COMMIT-ack
// ambiguity the reconciler must survive on the real wire.
//
// It requires simple query protocol (default_query_exec_mode=simple_protocol)
// so every statement travels as one 'Q' message, and sslmode=disable so the
// startup phase is plaintext without SSL negotiation.
type commitCutProxy struct {
	t        *testing.T
	ln       net.Listener
	upstream string // host:port of the real server

	mu            sync.Mutex
	armed         bool
	preCheckpoint int64
	cut           bool
	logs          []string
}

func startCommitCutProxy(t *testing.T, upstreamHostPort string) *commitCutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &commitCutProxy{t: t, ln: ln, upstream: upstreamHostPort}
	go p.acceptLoop(ln)
	t.Cleanup(func() { p.dumpLogs(); _ = ln.Close() })
	return p
}

func (p *commitCutProxy) addr() string { return p.ln.Addr().String() }

func (p *commitCutProxy) arm(preCheckpoint int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = true
	p.preCheckpoint = preCheckpoint
	p.appendLog("armed: pre-injection checkpoint=%d; the next COMMIT is intercepted", preCheckpoint)
}

func (p *commitCutProxy) cutDone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cut
}

func (p *commitCutProxy) dumpLogs() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, l := range p.logs {
		p.t.Logf("proxy: %s", l)
	}
}

func (p *commitCutProxy) appendLog(format string, args ...any) {
	line := faultTS() + " " + fmt.Sprintf(format, args...)
	p.logs = append(p.logs, line)
}

func (p *commitCutProxy) acceptLoop(ln net.Listener) {
	for {
		client, err := ln.Accept()
		if err != nil {
			return
		}
		go p.handle(client)
	}
}

func appendFrame(buf []byte, typ byte, data []byte) []byte {
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(data)+4))
	buf = append(buf, hdr[:]...)
	return append(buf, data...)
}

func (p *commitCutProxy) handle(client net.Conn) {
	defer client.Close()
	up, err := net.Dial("tcp", p.upstream)
	if err != nil {
		p.appendLog("upstream dial failed: %v", err)
		return
	}
	defer up.Close()

	// Startup phase: the first client message is the raw startup packet,
	// not a typed frame. Pass it through verbatim (sslmode=disable means no
	// SSL negotiation); handle the two 8-byte pre-messages defensively.
	first := make([]byte, 8)
	if _, err := io.ReadFull(client, first); err != nil {
		return
	}
	switch binary.BigEndian.Uint32(first[4:]) {
	case 80877102: // CancelRequest: raw passthrough, no session follows
		_, _ = up.Write(first)
		return
	case 80877103:
		p.appendLog("refusing SSLRequest; the fixture DSN must use sslmode=disable")
		return
	}
	total := int(binary.BigEndian.Uint32(first[:4]))
	if total < 8 || total > 1<<20 {
		return
	}
	rest := make([]byte, total-8)
	if _, err := io.ReadFull(client, rest); err != nil {
		return
	}
	startup := append(append([]byte{}, first...), rest...)
	if _, err := up.Write(startup); err != nil {
		return
	}

	// Typed-frame relay: every frame is forwarded as soon as it is read, in
	// both directions, with NO statement/reply correlation — extended-
	// protocol pipelines (Parse/Describe/Sync from COPY FROM STDIN's
	// statement description), the copy-data sub-protocol and simple 'Q'
	// round trips all pass through unchanged. The only correlation point is
	// the armed COMMIT interception below.
	type frame struct {
		typ  byte
		data []byte
	}
	readFrames := func(r io.Reader, ch chan<- frame) {
		defer close(ch)
		var hdr [5]byte
		for {
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				return
			}
			n := int(binary.BigEndian.Uint32(hdr[1:])) - 4
			if n < 0 || n > 1<<30 {
				return
			}
			data := make([]byte, n)
			if n > 0 {
				if _, err := io.ReadFull(r, data); err != nil {
					return
				}
			}
			ch <- frame{hdr[0], data}
		}
	}
	writeFrame := func(w io.Writer, f frame) bool {
		_, err := w.Write(appendFrame(nil, f.typ, f.data))
		return err == nil
	}

	clientFrames := make(chan frame, 256)
	serverFrames := make(chan frame, 256)
	go readFrames(client, clientFrames)
	go readFrames(up, serverFrames)

	for {
		select {
		case f, ok := <-clientFrames:
			if !ok {
				return
			}
			p.mu.Lock()
			armed := p.armed
			p.mu.Unlock()
			if f.typ == 'Q' && armed && strings.TrimRight(string(f.data), "\x00") == "commit" {
				// Forward the COMMIT, consume the server reply through
				// ReadyForQuery — the COMMIT is durable — then sever the
				// client without delivering the ack.
				if !writeFrame(up, f) {
					return
				}
				for {
					sf, ok := <-serverFrames
					if !ok {
						return
					}
					if sf.typ == 'Z' {
						break
					}
				}
				p.mu.Lock()
				p.armed = false
				p.cut = true
				pre := p.preCheckpoint
				p.mu.Unlock()
				p.appendLog("intercepted COMMIT: forwarded upstream, server ReadyForQuery consumed "+
					"(commit durable); client ack withheld, connection severed (armed at checkpoint %d)", pre)
				return
			}
			if !writeFrame(up, f) {
				return
			}
		case f, ok := <-serverFrames:
			if !ok {
				return
			}
			if !writeFrame(client, f) {
				return
			}
		}
	}
}

// proxyTargetDSN rewrites a postgres:// DSN to dial the proxy and forces the
// client-side parameters the proxy relies on. Nothing changes server-side.
func proxyTargetDSN(t *testing.T, dsn, proxyAddr string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Host = proxyAddr
	q := u.Query()
	q.Set("sslmode", "disable")
	q.Set("default_query_exec_mode", "simple_protocol")
	u.RawQuery = q.Encode()
	return u.String()
}

// F2 — lost COMMIT acknowledgement at the binary level: the intercepted
// transaction provably commits on the server (checkpoint advances past the
// arming value) while the client never sees the ack; the same invocation
// reconciles and finishes without duplicate or skipped records.
func TestFaultLostCommitAckRecoversInProcess(t *testing.T) {
	pgtest.IntegrationCluster(t)

	src := pgtest.IntegrationCluster(t).TestDatabase(t)
	seedFaultSource(t, src, faultTorrents)
	tgt := pgtest.IntegrationCluster(t).TestDatabase(t)
	initFaultTarget(t, tgt)
	bin := buildBinary(t)

	u, err := url.Parse(tgt)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	proxy := startCommitCutProxy(t, u.Host)
	proxyDSN := proxyTargetDSN(t, tgt, proxy.addr())

	run := startFaultImport(t, bin, src, proxyDSN)

	pre := *pollProgress(t, tgt, faultImportingAt(faultTorrents/10), 90*time.Second)
	proxy.arm(pre.LastProcessedID)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && !proxy.cutDone() {
		time.Sleep(50 * time.Millisecond)
	}
	if !proxy.cutDone() {
		proxy.dumpLogs()
		t.Fatalf("NOT EXERCISED: the armed COMMIT interception never fired")
	}
	// Server-side proof the intercepted transaction committed: the durable
	// checkpoint advanced past the arming value despite the lost ack.
	pollProgress(t, tgt, func(p *state.Progress) bool { return p.LastProcessedID > pre.LastProcessedID }, 10*time.Second)

	code, sig, _ := run.waitExit()
	if code != 0 {
		t.Fatalf("exit code=%d signal=%v; want 0 (in-process reconcile and completion)", code, sig)
	}
	proxy.dumpLogs()
	assertFaultComplete(t, tgt, faultTorrents)
}

// faultAdminDSN returns a DSN for the same server's postgres administrative
// database (used for pg_stat_activity work).
func faultAdminDSN(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/postgres"
	return u.String()
}

func faultDBName(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	return strings.TrimPrefix(u.Path, "/")
}

// F3 — target connection loss: all backends of the target database are
// terminated repeatedly (the lock connection dies too), the run must stop
// with a fatal exit, and a normal CLI resume must complete exactly.
func TestFaultTargetConnectionLossRefusesThenResumes(t *testing.T) {
	pgtest.IntegrationCluster(t)

	src := pgtest.IntegrationCluster(t).TestDatabase(t)
	seedFaultSource(t, src, faultTorrents)
	tgt := pgtest.IntegrationCluster(t).TestDatabase(t)
	initFaultTarget(t, tgt)
	bin := buildBinary(t)

	run := startFaultImport(t, bin, src, tgt)
	pre := *pollProgress(t, tgt, faultImportingAt(faultTorrents/10), 90*time.Second)
	t.Logf("%s: terminating all backends of %q (checkpoint=%d scanned=%d)",
		faultTS(), faultDBName(t, tgt), pre.LastProcessedID, pre.ScannedCount)

	stop := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		conn, err := pgx.Connect(ctx, faultAdminDSN(t, tgt))
		if err != nil {
			return // the assertions below fail loudly if the kill loop never ran
		}
		defer conn.Close(context.Background())
		db := faultDBName(t, tgt)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var killed int
			_ = conn.QueryRow(ctx,
				`SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
                 WHERE datname = $1 AND pid <> pg_backend_pid()`, db).Scan(&killed)
			time.Sleep(400 * time.Millisecond)
		}
	}()

	code, sig, _ := run.waitExit()
	close(stop)
	if code != 1 {
		t.Fatalf("exit code=%d signal=%v; want 1 (fatal after connection loss)", code, sig)
	}

	assertPostDeathAtomicity(t, tgt)

	if code := runFaultResume(t, bin, src, tgt); code != 0 {
		t.Fatalf("resume exit code = %d, want 0", code)
	}
	assertFaultComplete(t, tgt, faultTorrents)
}

// F4 — advisory-lock exclusion: a second import invocation on the same
// target refuses fast with the lock-busy sentinel while the first completes.
func TestFaultConcurrentImportLockExclusion(t *testing.T) {
	pgtest.IntegrationCluster(t)

	src := pgtest.IntegrationCluster(t).TestDatabase(t)
	seedFaultSource(t, src, faultTorrents)
	tgt := pgtest.IntegrationCluster(t).TestDatabase(t)
	initFaultTarget(t, tgt)
	bin := buildBinary(t)

	first := startFaultImport(t, bin, src, tgt)
	pollProgress(t, tgt, faultImportingAt(faultTorrents/10), 90*time.Second)

	second := startFaultImport(t, bin, src, tgt)
	code2, sig2, _ := second.waitExit()
	if code2 != 1 || !strings.Contains(second.out.String(), "holds the lock") {
		t.Fatalf("second invocation exit=%d signal=%v output=%s; want exit 1 with the lock-busy refusal", code2, sig2, second.out.String())
	}

	code1, sig1, _ := first.waitExit()
	if code1 != 0 {
		t.Fatalf("first invocation exit=%d signal=%v; want 0", code1, sig1)
	}
	assertFaultComplete(t, tgt, faultTorrents)
}

// F5 — mid-resume source drift: an interrupted import refuses to resume when
// the source grew, preserves the stored state, and resumes cleanly once the
// source matches the stored fingerprint again.
func TestFaultSourceDriftRefusalMidResume(t *testing.T) {
	pgtest.IntegrationCluster(t)

	src := pgtest.IntegrationCluster(t).TestDatabase(t)
	seedFaultSource(t, src, faultTorrents)
	tgt := pgtest.IntegrationCluster(t).TestDatabase(t)
	initFaultTarget(t, tgt)
	bin := buildBinary(t)

	run := startFaultImport(t, bin, src, tgt)
	pollProgress(t, tgt, faultImportingAt(faultTorrents/10), 90*time.Second)

	run.signal(t, syscall.SIGTERM) // graceful: handled → application exit 2
	code, sig, _ := run.waitExit()
	if code != 2 || sig != 0 {
		t.Fatalf("SIGTERM exit code=%d signal=%v; want graceful exit 2, not signal death", code, sig)
	}
	snap := assertPostDeathAtomicity(t, tgt)

	// Grow the source: the stored fingerprint no longer matches.
	execSmokeSQL(t, src, fmt.Sprintf(`
        INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        VALUES (%[1]d, decode(lpad(to_hex(%[1]d), 40, '0'), 'hex'), 'drift', 2, 1700999999);
        INSERT INTO magneticod.files (torrent_id, path, size) VALUES (%[1]d, 'drift.bin', 2);`,
		faultTorrents+1))

	code = runFaultResume(t, bin, src, tgt)
	if code != 1 {
		t.Fatalf("drifted resume exit code = %d, want 1", code)
	}
	// State preservation: the refusal changed nothing.
	after := readFaultSnapshot(t, tgt)
	if after.Checkpoint != snap.Checkpoint || after.Scanned != snap.Scanned || after.Success != snap.Success {
		t.Fatalf("refused resume mutated state: before %+v, after %+v", snap, after)
	}
	t.Logf("%s: drift refusal preserved state (checkpoint=%d scanned=%d)", faultTS(), after.Checkpoint, after.Scanned)

	// Restore the source: the same ordinary resume now completes.
	execSmokeSQL(t, src, fmt.Sprintf(`
        DELETE FROM magneticod.files WHERE torrent_id = %d;
        DELETE FROM magneticod.torrents WHERE id = %d;`, faultTorrents+1, faultTorrents+1))
	if code := runFaultResume(t, bin, src, tgt); code != 0 {
		t.Fatalf("resume after restoring the source exit code = %d, want 0", code)
	}
	assertFaultComplete(t, tgt, faultTorrents)
}

// faultDockerUsable reports whether docker with the locally cached
// postgres:17 image is available (the image is never pulled).
func faultDockerUsable(t *testing.T) bool {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return exec.Command("docker", "image", "inspect", "postgres:17").Run() == nil
}

func faultRandHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// faultContainerHostPort resolves the container's CURRENT published port.
// The mapping is re-resolved on every call: `docker restart` re-randomizes
// the published ephemeral port on this host.
func faultContainerHostPort(t *testing.T, name string) string {
	t.Helper()
	portOut, err := exec.Command("docker", "port", name, "5432").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	hostPort := ""
	for _, field := range strings.Fields(string(portOut)) {
		if strings.HasPrefix(field, "127.0.0.1:") {
			hostPort = field
		}
	}
	if hostPort == "" {
		t.Fatalf("no IPv4 port mapping for %s in docker port output: %q", name, string(portOut))
	}
	return hostPort
}

func faultContainerAdminDSN(t *testing.T, name string) string {
	t.Helper()
	return "postgres://postgres@" + faultContainerHostPort(t, name) + "/postgres?sslmode=disable"
}

// faultContainerDSN returns the source-database DSN on the container's
// current published port.
func faultContainerDSN(t *testing.T, name string) string {
	t.Helper()
	return "postgres://postgres@" + faultContainerHostPort(t, name) + "/magnetico_04d_f6_src?sslmode=disable"
}

// waitFaultSourceReady polls the container (re-resolving its mapped port)
// until the server accepts connections, via the administrative database.
func waitFaultSourceReady(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := pgx.Connect(ctx, faultContainerAdminDSN(t, name))
		cancel()
		if err == nil {
			_ = conn.Close(context.Background())
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("source %s never became ready within %s", name, timeout)
}

// startFaultPostgresContainer starts a dedicated disposable postgres:17
// container (removed on cleanup) and returns its name and source DSN.
func startFaultPostgresContainer(t *testing.T) (string, string) {
	t.Helper()
	name := "magnetico-04d-f6-" + faultRandHex(t, 6)
	out, err := exec.Command("docker", "run", "-d", "--name", name,
		"-e", "POSTGRES_HOST_AUTH_METHOD=trust", "-e", "POSTGRES_USER=postgres",
		"-p", "127.0.0.1::5432", "postgres:17").Output()
	if err != nil {
		t.Fatalf("docker run: %v: %s", err, out)
	}
	t.Logf("%s: started container %s", faultTS(), name)
	t.Cleanup(func() {
		if o, err := exec.Command("docker", "rm", "-f", name).CombinedOutput(); err != nil {
			t.Logf("cleanup docker rm %s: %v: %s", name, err, o)
		}
	})

	waitFaultSourceReady(t, name, 90*time.Second)
	execSmokeSQL(t, faultContainerAdminDSN(t, name), `CREATE DATABASE magnetico_04d_f6_src`)
	return name, faultContainerDSN(t, name)
}

// F6(i) — a restarted, otherwise unchanged source must not be mistaken for
// content drift: the run resumes over a changed postmaster start time, with
// the restart itself proven (changed pg_postmaster_start_time).
func TestFaultRestartedSourceIsNotDrift(t *testing.T) {
	pgtest.IntegrationCluster(t)
	if !faultDockerUsable(t) {
		t.Skip("docker with a locally cached postgres:17 image is required for the restart scenario")
	}

	name, src := startFaultPostgresContainer(t)
	seedFaultSource(t, src, faultTorrents)
	tgt := pgtest.IntegrationCluster(t).TestDatabase(t)
	initFaultTarget(t, tgt)
	bin := buildBinary(t)

	run := startFaultImport(t, bin, src, tgt)
	pollProgress(t, tgt, faultImportingAt(faultTorrents/10), 90*time.Second)

	run.signal(t, syscall.SIGTERM) // graceful: handled → application exit 2
	code, sig, _ := run.waitExit()
	if code != 2 || sig != 0 {
		t.Fatalf("SIGTERM exit code=%d signal=%v; want graceful exit 2, not signal death", code, sig)
	}
	assertPostDeathAtomicity(t, tgt)

	postmasterSQL := `SELECT floor(extract(epoch FROM pg_postmaster_start_time()))::bigint`
	pmBefore := smokeScalar(t, src, postmasterSQL)
	if o, err := exec.Command("docker", "restart", name).CombinedOutput(); err != nil {
		t.Fatalf("docker restart: %v: %s", err, o)
	}
	// The restart re-randomizes the published port; re-resolve before use.
	waitFaultSourceReady(t, name, 90*time.Second)
	src = faultContainerDSN(t, name)
	pmAfter := smokeScalar(t, src, postmasterSQL)
	if pmAfter == pmBefore {
		t.Fatalf("postmaster start time unchanged after docker restart (%d): scenario NOT EXERCISED", pmAfter)
	}
	t.Logf("%s: postmaster restarted (start time %d → %d), content unchanged", faultTS(), pmBefore, pmAfter)

	if code := runFaultResume(t, bin, src, tgt); code != 0 {
		t.Fatalf("resume after source restart exit code = %d, want 0", code)
	}
	assertFaultComplete(t, tgt, faultTorrents)
}

// F6(ii) — revision staleness survives retry mutations: a retry attempt
// bumps data_revision, the previous decode pass can never be mistaken for
// current evidence, and the next verify-decode starts a fresh run whose
// start_data_revision matches the new revision.
func TestFaultVerificationStalenessAfterRetry(t *testing.T) {
	pgtest.IntegrationCluster(t)

	// Three-torrent fixture with one invalid record (size-sum mismatch):
	// the exact seed of the 04B binary smoke.
	src := pgtest.IntegrationCluster(t).TestDatabase(t)
	execSmokeSQL(t, src, smokeSourceDDL)
	execSmokeSQL(t, src, `
        INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        VALUES (1, decode(lpad(to_hex(1), 40, '0'), 'hex'), 't1', 4, 1700000001),
               (2, decode(lpad(to_hex(2), 40, '0'), 'hex'), 't2', 2, 1700000002),
               (3, decode(lpad(to_hex(3), 40, '0'), 'hex'), 't3', 100, 1700000003);
        INSERT INTO magneticod.files (torrent_id, path, size)
        VALUES (1, 'a.bin', 2), (1, 'b.bin', 2),
               (2, 'f2.bin', 2),
               (3, 'f3.bin', 50);`)
	tgt := pgtest.IntegrationCluster(t).TestDatabase(t)
	initFaultTarget(t, tgt)
	bin := buildBinary(t)

	// Import: record 3 lands in the registry; exit 0.
	if code := runFaultResume(t, bin, src, tgt); code != 0 {
		t.Fatalf("import exit code = %d, want 0", code)
	}
	if n := smokeScalar(t, tgt, `SELECT error_count FROM magnetico_migration.progress`); n != 1 {
		t.Fatalf("error_count = %d, want 1", n)
	}

	// verify-decode passes at data revision 0 (run seq 1).
	code, out := runMigrateBinary(t, bin, "", "migrate", "verify-decode", "--database", tgt)
	if code != 0 || !strings.Contains(out, "PASS") {
		t.Fatalf("first verify-decode exit = %d, output = %s; want 0 PASS", code, out)
	}
	const decodeRuns = `SELECT count(*) FROM magnetico_migration.verification_runs WHERE kind = 'decode'`
	const decodeRev = `SELECT start_data_revision FROM magnetico_migration.verification_runs WHERE kind = 'decode' AND seq = %d`
	if n := smokeScalar(t, tgt, decodeRuns); n != 1 {
		t.Fatalf("decode run count = %d, want 1", n)
	}
	if rev := smokeScalar(t, tgt, fmt.Sprintf(decodeRev, 1)); rev != 0 {
		t.Fatalf("run 1 start_data_revision = %d, want 0", rev)
	}

	// Retry the still-invalid record: the attempt mutates registry state →
	// exit 3 (remaining) and data_revision bumps.
	code, out = runMigrateBinary(t, bin, src, "migrate", "torrents", "retry-errors", "--database", tgt)
	if code != 3 {
		t.Fatalf("still-invalid retry exit = %d, output = %s; want 3", code, out)
	}
	revision := smokeScalar(t, tgt, `SELECT data_revision FROM magnetico_migration.progress`)
	if revision != 1 {
		t.Fatalf("data_revision after retry attempt = %d, want 1", revision)
	}

	// A fresh verify-decode must not adopt the stale pass: new seq, and its
	// start_data_revision matches the current revision.
	code, out = runMigrateBinary(t, bin, "", "migrate", "verify-decode", "--database", tgt)
	if code != 0 || !strings.Contains(out, "PASS") {
		t.Fatalf("second verify-decode exit = %d, output = %s; want 0 PASS", code, out)
	}
	if n := smokeScalar(t, tgt, decodeRuns); n != 2 {
		t.Fatalf("decode run count = %d, want 2 (the stale pass must not be adopted)", n)
	}
	if rev := smokeScalar(t, tgt, fmt.Sprintf(decodeRev, 2)); rev != revision {
		t.Fatalf("run 2 start_data_revision = %d, want the current revision %d", rev, revision)
	}
	if stale := smokeScalar(t, tgt, fmt.Sprintf(decodeRev, 1)); stale == revision {
		t.Fatalf("run 1 start_data_revision %d equals the current revision: the staleness mechanism did not engage", stale)
	}
	t.Logf("%s: run 1 (revision %d) is stale against current revision %d; run 2 is fresh",
		faultTS(), smokeScalar(t, tgt, fmt.Sprintf(decodeRev, 1)), revision)
}
