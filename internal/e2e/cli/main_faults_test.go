package cli

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
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// Credential redaction for the fault harness: the CLI arguments carry full
// database DSNs (startFaultImport/runFaultResume log them), and
// pgtest.TestDatabase inherits credentials from the cluster AdminDSN — an
// external disposable server can carry a real password. Every harness log
// line that can carry DSN material goes through redactDSN/redactArgs; the
// actual exec.Cmd arguments never do.
//
// The product already redacts its own output (redactDatabaseURL in main.go,
// pgconn's redactPW), so the harness only needs to guard its own log sites —
// but captured process output is redacted too, as defense in depth.

// faultRedactedMark matches the mask pgx uses for redacted connection
// strings, so harness logs and product logs mask alike.
const faultRedactedMark = "xxxxx"

// faultURLPasswordRe masks the userinfo password of a postgres:// URL
// wherever the URL appears — argv entries and captured process output.
var faultURLPasswordRe = regexp.MustCompile(`(postgres(?:ql)?://[^:/@?#\s]+:)[^@?#\s]+(@)`)

// faultQuotedKeywordPasswordRe masks a single-quoted libpq keyword password,
// whose value may span whitespace and carry backslash-escaped characters
// (\' for a literal quote). It must run before faultKeywordPasswordRe so the
// whole quoted value is consumed up to its closing quote — the bare rule
// would otherwise stop at the first space and leak the rest.
var faultQuotedKeywordPasswordRe = regexp.MustCompile(`(?i)\b(password=)'(?:[^'\\]|\\.)*'`)

// faultKeywordPasswordRe masks bare libpq keyword-style passwords, including
// a password= query parameter inside a URL. A leading quote is excluded so a
// value already masked by the quoted rule is left intact.
var faultKeywordPasswordRe = regexp.MustCompile(`(?i)\b(password=)[^\s&']+`)

// redactDSN replaces credential material (URL userinfo password, libpq
// keyword password — bare or single-quoted) with a fixed marker. Non-DSN
// input is returned unchanged.
func redactDSN(s string) string {
	s = faultURLPasswordRe.ReplaceAllString(s, "${1}"+faultRedactedMark+"${2}")
	s = faultQuotedKeywordPasswordRe.ReplaceAllString(s, "${1}'"+faultRedactedMark+"'")
	s = faultKeywordPasswordRe.ReplaceAllString(s, "${1}"+faultRedactedMark)
	return s
}

// faultScrubSecrets returns s with every secret replaced by a display-only
// marker, for safe inclusion in failure diagnostics: an assertion that
// reports a leaked credential must never echo that credential itself. The
// marker is deliberately distinct from faultRedactedMark so a scrubbed
// diagnostic cannot be mistaken for a successfully redacted log line.
// Callers pass longer secrets first: scrubbing a password out of a full DSN
// would leave that DSN unmatchable for the later, shorter password secret.
func faultScrubSecrets(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "<scrubbed>")
		}
	}
	return s
}

// redactArgs redacts every argument into a fresh slice; the input — the
// actual exec.Cmd arguments — is never modified.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = redactDSN(a)
	}
	return out
}

// faultLogSink, when non-nil, receives a copy of every harness log line that
// can carry DSN material, in addition to the normal t.Logf stream. Only the
// credential-redaction regression installs it, and it registers its reset
// via t.Cleanup BEFORE starting the harness so that LIFO cleanup order keeps
// the sink alive through every harness cleanup line.
var faultLogSink func(format string, args ...any)

// faultLogf emits one harness log line to the test log and to the capture
// sink when installed. Callers must pass already-redacted values.
func faultLogf(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Logf(format, args...)
	if faultLogSink != nil {
		faultLogSink(format, args...)
	}
}

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
		t.Fatalf("InitSchema() error = %v", redactDSN(err.Error()))
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
		t.Fatalf("start %v: %v", redactArgs(args), err)
	}
	faultLogf(t, "%s: started pid=%d: magnetico %v", faultTS(), cmd.Process.Pid, redactArgs(args))
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
	faultLogf(r.t, "%s: pid %d exited; output:\n%s", faultTS(), r.cmd.Process.Pid, redactDSN(out))
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
		t.Fatalf("snapshot connect: %v", redactDSN(err.Error()))
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
	faultLogf(t, "%s: resumed `magnetico %v` exited; output:\n%s", faultTS(), redactArgs(args), redactDSN(out.String()))
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
// COMMIT: the commit is forwarded, its server reply is VERIFIED (CommandComplete
// with the exact "COMMIT" tag, no ErrorResponse, terminated by ReadyForQuery),
// the authoritative checkpoint is read from the target database directly
// before the forwarding and again after the confirmation — pinning the
// durability proof to the intercepted transaction itself — and only then is
// the client connection severed without the ack. Any deviation (ErrorResponse,
// missing or malformed CommandComplete, truncated reply, failed evidence read)
// is a REJECTED injection and is fail-closed: handle returns right after the
// interception attempt, terminating the intercepted connection, and the test
// fails loudly on the recorded reason, so the scenario can never pass vacuously.
//
// It requires simple query protocol (default_query_exec_mode=simple_protocol)
// so every statement travels as one 'Q' message, and sslmode=disable so the
// startup phase is plaintext without SSL negotiation.
type commitCutProxy struct {
	t         *testing.T
	ln        net.Listener
	upstream  string // host:port of the real server
	targetDSN string // DIRECT target DSN (not through the proxy) for evidence reads

	mu                   sync.Mutex
	armed                bool
	preCheckpoint        int64
	cut                  bool
	logs                 []string
	preCommitCheckpoint  int64 // read immediately before forwarding the intercepted COMMIT
	postCommitCheckpoint int64 // read after the server confirmed it
	commitTag            string
	injectErr            string // non-empty: the injection was rejected for this reason
}

func startCommitCutProxy(t *testing.T, upstreamHostPort, targetDSN string) *commitCutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &commitCutProxy{t: t, ln: ln, upstream: upstreamHostPort, targetDSN: targetDSN}
	go p.acceptLoop(ln)
	t.Cleanup(func() { p.dumpLogs(); _ = ln.Close() })
	return p
}

func (p *commitCutProxy) addr() string { return p.ln.Addr().String() }

// arm enables the interception for the next COMMIT. The arming checkpoint is
// a lower bound for the evidence (other transactions may commit between the
// arming observation and the interception); the authoritative comparison is
// the pre/post pair read by intercept.
func (p *commitCutProxy) arm(preCheckpoint int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = true
	p.preCheckpoint = preCheckpoint
	p.appendLogLocked("armed: pre-injection checkpoint=%d; the next COMMIT is intercepted", preCheckpoint)
}

func (p *commitCutProxy) cutDone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cut
}

// injectFailed returns the recorded rejection reason, or "" while the
// interception is still pending or succeeded. Checking it in the wait loop
// makes a rejected injection fail fast with its reason instead of timing out.
func (p *commitCutProxy) injectFailed() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.injectErr
}

// commitCutEvidence is the interception's own record of the server reply and
// the authoritative checkpoint pair around THE intercepted transaction.
type commitCutEvidence struct {
	PreCommitCheckpoint  int64
	PostCommitCheckpoint int64
	CommandTag           string
}

func (p *commitCutProxy) evidence() commitCutEvidence {
	p.mu.Lock()
	defer p.mu.Unlock()
	return commitCutEvidence{
		PreCommitCheckpoint:  p.preCommitCheckpoint,
		PostCommitCheckpoint: p.postCommitCheckpoint,
		CommandTag:           p.commitTag,
	}
}

func (p *commitCutProxy) dumpLogs() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, l := range p.logs {
		p.t.Logf("proxy: %s", l)
	}
}

// logContains reports whether any recorded proxy log line contains sub. It
// is the reader half of the atomic publication contract with cutDone():
// both read p.mu, so once cutDone() is observed true the interception's
// evidence record is guaranteed to be observable.
func (p *commitCutProxy) logContains(sub string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, l := range p.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// appendLog records an evidence line. It takes p.mu itself: call it only
// WITHOUT the lock held (arm uses appendLogLocked to stay non-recursive).
func (p *commitCutProxy) appendLog(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appendLogLocked(format, args...)
}

// appendLogLocked is the lock-held variant (p.mu must be held by the caller).
func (p *commitCutProxy) appendLogLocked(format string, args ...any) {
	line := faultTS() + " " + fmt.Sprintf(format, args...)
	p.logs = append(p.logs, line)
}

// readCheckpoint reads the authoritative committed checkpoint directly from
// the target database — never through the proxy. Safe on the connection
// goroutine: it uses no testing.T state.
func (p *commitCutProxy) readCheckpoint() (int64, error) {
	prog, err := tryLoadProgress(p.targetDSN)
	if err != nil {
		return 0, err
	}
	if prog == nil {
		return 0, errors.New("no migration progress row")
	}
	return prog.LastProcessedID, nil
}

// reject records a rejected injection: the server reply or an evidence read
// did not support a lost-ack simulation. Rejection is fail-closed: handle
// returns immediately after intercept, so the intercepted connection is
// terminated, and the test fails on the recorded reason instead of passing
// vacuously.
func (p *commitCutProxy) reject(format string, args ...any) {
	p.mu.Lock()
	p.armed = false
	p.injectErr = fmt.Sprintf(format, args...)
	p.mu.Unlock()
	p.appendLog("injection REJECTED: "+format, args...)
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
	readFrames := func(r io.Reader, ch chan<- proxyFrame) {
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
			ch <- proxyFrame{hdr[0], data}
		}
	}

	clientFrames := make(chan proxyFrame, 256)
	serverFrames := make(chan proxyFrame, 256)
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
				// Verify the server reply, capture the checkpoint evidence on
				// both sides of the intercepted transaction, and only then
				// sever the client without delivering the ack. On any
				// deviation the injection is rejected; this handler returns
				// right after intercept, terminating the intercepted
				// connection, and the test fails with the recorded reason.
				p.intercept(up, f, serverFrames)
				return
			}
			if !writeProxyFrame(up, f) {
				return
			}
		case f, ok := <-serverFrames:
			if !ok {
				return
			}
			if !writeProxyFrame(client, f) {
				return
			}
		}
	}
}

// proxyFrame is one typed PostgreSQL wire frame (type byte + payload).
type proxyFrame struct {
	typ  byte
	data []byte
}

func writeProxyFrame(w io.Writer, f proxyFrame) bool {
	_, err := w.Write(appendFrame(nil, f.typ, f.data))
	return err == nil
}

// intercept performs the armed COMMIT interception. Evidence discipline: the
// authoritative checkpoint is read directly from the target database
// immediately BEFORE the COMMIT is forwarded and again AFTER the server
// confirms it — so the post-read reflects the intercepted transaction itself,
// not progress made by unrelated earlier or later commits. The reply must be
// CommandComplete with the exact "COMMIT" tag (no ErrorResponse) terminated
// by ReadyForQuery; only then is the client connection severed without the
// ack. Any deviation is recorded as a rejected injection and is fail-closed:
// handle returns right after this call, terminating the intercepted
// connection, and the test fails with the recorded reason.
func (p *commitCutProxy) intercept(up net.Conn, f proxyFrame, serverFrames <-chan proxyFrame) {
	pre, err := p.readCheckpoint()
	if err != nil {
		p.reject("checkpoint read before forwarding the COMMIT failed: %v", err)
		return
	}
	if !writeProxyFrame(up, f) {
		p.reject("forwarding the intercepted COMMIT upstream failed")
		return
	}
	var (
		tag         string
		sawComplete bool
		confirmed   bool
	)
	for sf := range serverFrames {
		switch sf.typ {
		case 'C': // CommandComplete
			sawComplete = true
			tag = strings.TrimRight(string(sf.data), "\x00")
		case 'E': // ErrorResponse
			p.reject("the server answered the intercepted COMMIT with ErrorResponse (%s)", pgErrorSummary(sf.data))
			return
		case 'Z': // ReadyForQuery
			confirmed = true
		}
		if confirmed {
			break
		}
	}
	if !confirmed {
		p.reject("the upstream connection closed before ReadyForQuery (incomplete reply)")
		return
	}
	if !sawComplete {
		p.reject("ReadyForQuery without a CommandComplete (unexpected reply sequence)")
		return
	}
	if tag != "COMMIT" {
		p.reject("CommandComplete tag %q, want exactly \"COMMIT\"", tag)
		return
	}
	post, err := p.readCheckpoint()
	if err != nil {
		p.reject("checkpoint read after the server confirmed the COMMIT failed: %v", err)
		return
	}
	p.mu.Lock()
	p.armed = false
	p.cut = true
	p.preCommitCheckpoint = pre
	p.postCommitCheckpoint = post
	p.commitTag = tag
	// Atomic publication: the evidence record joins cut=true in the SAME
	// critical section, so any reader that observes cutDone()==true must
	// also observe this line (all log readers share p.mu). Appending it
	// after the unlock would leave a window where the cut is visible but
	// the evidence behind it is not.
	p.appendLogLocked("intercepted COMMIT: CommandComplete %q + ReadyForQuery consumed (commit durable); "+
		"checkpoint %d -> %d across the intercepted transaction; client ack withheld, connection severed",
		tag, pre, post)
	p.mu.Unlock()
}

// pgErrorSummary renders the SQLSTATE and message fields of a PostgreSQL
// ErrorResponse body for the evidence log.
func pgErrorSummary(data []byte) string {
	var code, msg string
	for len(data) > 0 {
		field := data[0]
		data = data[1:]
		if field == 0 { // field terminator
			break
		}
		i := bytes.IndexByte(data, 0)
		if i < 0 {
			break
		}
		switch field {
		case 'C':
			code = string(data[:i])
		case 'M':
			msg = string(data[:i])
		}
		data = data[i+1:]
	}
	return "SQLSTATE " + code + ": " + msg
}

// proxyTargetDSN rewrites a postgres:// DSN to dial the proxy and forces the
// client-side parameters the proxy relies on. Nothing changes server-side.
func proxyTargetDSN(t *testing.T, dsn, proxyAddr string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", redactDSN(err.Error()))
	}
	u.Host = proxyAddr
	q := u.Query()
	q.Set("sslmode", "disable")
	q.Set("default_query_exec_mode", "simple_protocol")
	u.RawQuery = q.Encode()
	return u.String()
}

// F2 — lost COMMIT acknowledgement at the binary level: the intercepted
// transaction is proven by the proxy's own reply verification and independent
// database reads (CommandComplete with the exact COMMIT tag, no ErrorResponse;
// the authoritative checkpoint advances across THE intercepted transaction)
// while the client never sees the ack; the same invocation reconciles
// in-process, exits 0, and lands the exact record set without duplicate or
// skipped records.
func TestFaultLostCommitAckRecoversInProcess(t *testing.T) {
	pgtest.IntegrationCluster(t)

	src := pgtest.IntegrationCluster(t).TestDatabase(t)
	seedFaultSource(t, src, faultTorrents)
	tgt := pgtest.IntegrationCluster(t).TestDatabase(t)
	initFaultTarget(t, tgt)
	bin := buildBinary(t)

	u, err := url.Parse(tgt)
	if err != nil {
		t.Fatalf("parse dsn: %v", redactDSN(err.Error()))
	}
	proxy := startCommitCutProxy(t, u.Host, tgt)
	proxyDSN := proxyTargetDSN(t, tgt, proxy.addr())

	run := startFaultImport(t, bin, src, proxyDSN)

	pre := *pollProgress(t, tgt, faultImportingAt(faultTorrents/10), 90*time.Second)
	proxy.arm(pre.LastProcessedID)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if reason := proxy.injectFailed(); reason != "" {
			proxy.dumpLogs()
			t.Fatalf("NOT EXERCISED: injection rejected — %s", reason)
		}
		if proxy.cutDone() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !proxy.cutDone() {
		proxy.dumpLogs()
		t.Fatalf("NOT EXERCISED: the armed COMMIT interception never fired")
	}
	// Atomic publication pinned: cutDone()==true must imply the matching
	// evidence record is already observable — both are published under one
	// critical section (see intercept).
	if !proxy.logContains("intercepted COMMIT") {
		t.Fatalf("cutDone() observed true, but the interception evidence record is not published")
	}

	// Durability pinned to the intercepted transaction: the proxy read the
	// authoritative checkpoint directly from the database immediately before
	// forwarding the COMMIT and again after consuming the verified COMMIT
	// reply, so the advance between the two reads can only be the intercepted
	// transaction's own work — not unrelated commits before or after it.
	ev := proxy.evidence()
	if ev.PreCommitCheckpoint < pre.LastProcessedID {
		t.Fatalf("pre-commit checkpoint %d < arming checkpoint %d (checkpoint regression)",
			ev.PreCommitCheckpoint, pre.LastProcessedID)
	}
	if ev.PostCommitCheckpoint <= ev.PreCommitCheckpoint {
		t.Fatalf("the intercepted transaction did not advance the checkpoint (%d -> %d): "+
			"the injection hit an unrelated transaction", ev.PreCommitCheckpoint, ev.PostCommitCheckpoint)
	}
	t.Logf("%s: intercepted COMMIT verified: CommandComplete %q, checkpoint %d -> %d across the intercepted transaction",
		faultTS(), ev.CommandTag, ev.PreCommitCheckpoint, ev.PostCommitCheckpoint)

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
		t.Fatalf("parse dsn: %v", redactDSN(err.Error()))
	}
	u.Path = "/postgres"
	return u.String()
}

func faultDBName(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", redactDSN(err.Error()))
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

// TestImportWithReadOnlySourceRole proves the migrator's source discipline at
// the privilege level, complementing the historical campaign's operationally
// observed (but not enforced) SELECT-only behavior: the source role here holds
// CONNECT + USAGE + SELECT and nothing else, a write by that role is refused
// by the server itself (the restriction is enforced, not assumed), and the
// full import still completes — so any source modification attempted by the
// migrator would have failed the run exactly like the probe below.
func TestImportWithReadOnlySourceRole(t *testing.T) {
	cluster := pgtest.IntegrationCluster(t)

	src := cluster.TestDatabase(t)
	const n = 5000
	seedFaultSource(t, src, n)
	tgt := pgtest.IntegrationCluster(t).TestDatabase(t)
	initFaultTarget(t, tgt)
	bin := buildBinary(t)

	// The role is cluster-global: unique per run, removed at cleanup. Its
	// password is an independent temporary credential — never a copy of the
	// admin's — so the restricted role grants no knowledge of the harness
	// credential. Trust-auth harnesses ignore it; password-auth external
	// servers (MAGNETICO_TEST_POSTGRES_DSN) validate it, so the role connects
	// under both.
	role := "magnetico_ro_" + faultRandHex(t, 6)
	rolePW := faultRandHex(t, 16)
	admin, err := url.Parse(cluster.AdminDSN)
	if err != nil {
		t.Fatalf("parse admin dsn: %v", redactDSN(err.Error()))
	}
	execSmokeSQL(t, cluster.AdminDSN, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD %s`,
		pgtest.QuoteIdent(role), pgtest.QuoteLiteral(rolePW)))
	t.Cleanup(func() {
		// This cleanup runs BEFORE the test-database drop (LIFO), so the role
		// still holds its ACL grants — DROP OWNED revokes them (including the
		// CONNECT grant on the database itself, a shared object) in one step,
		// after which DROP ROLE always succeeds. A leftover role would pollute
		// later runs, so any cleanup failure fails the test (t.Errorf is valid
		// inside t.Cleanup).
		ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
		defer cancel()
		conn, err := pgx.Connect(ctx, src)
		if err != nil {
			t.Errorf("cleanup: connect for %s removal: %v", role, redactDSN(err.Error()))
			return
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(ctx, fmt.Sprintf(`DROP OWNED BY %s`, pgtest.QuoteIdent(role))); err != nil {
			t.Errorf("cleanup: DROP OWNED BY %s: %v", role, err)
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf(`DROP ROLE %s`, pgtest.QuoteIdent(role))); err != nil {
			t.Errorf("cleanup: DROP ROLE %s: %v", role, err)
		}
	})

	execSmokeSQL(t, src, fmt.Sprintf(`
        GRANT CONNECT ON DATABASE %s TO %[2]s;
        GRANT USAGE ON SCHEMA magneticod TO %[2]s;
        GRANT SELECT ON ALL TABLES IN SCHEMA magneticod TO %[2]s;`,
		pgtest.QuoteIdent(faultDBName(t, src)), pgtest.QuoteIdent(role)))

	// The restricted source DSN: same server and database, restricted role,
	// its own credential.
	restricted := *admin
	restricted.User = url.UserPassword(role, rolePW)
	restricted.Path = "/" + faultDBName(t, src)
	restrictedDSN := restricted.String()

	// Enforcement sanity check: a write by this role is refused by the server
	// (SQLSTATE 42501, insufficient_privilege) — SELECT-only is enforced, not
	// assumed.
	ctx, cancel := context.WithTimeout(context.Background(), pgtest.DBOpTimeout)
	defer cancel()
	conn, err := pgx.Connect(ctx, restrictedDSN)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, redactDSN(err.Error()))
	}
	_, err = conn.Exec(ctx, `INSERT INTO magneticod.torrents (id, info_hash, name, total_size, discovered_on)
        VALUES (1, decode(lpad(to_hex(1), 40, '0'), 'hex'), 'x', 1, 1)`)
	conn.Close(context.Background())
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("INSERT as the restricted role: err = %v, want SQLSTATE 42501 (privilege not held)", err)
	}
	t.Logf("%s: write as %s refused by the server (SQLSTATE 42501): SELECT-only enforced", faultTS(), role)

	// The whole import completes under the restricted role: the migrator
	// performs no source modification — any attempted write would have been
	// refused exactly like the probe above.
	if code := runFaultResume(t, bin, restrictedDSN, tgt); code != 0 {
		t.Fatalf("import under the read-only source role exit code = %d, want 0", code)
	}
	assertFaultComplete(t, tgt, n)
	if blobs := smokeScalar(t, tgt, `SELECT count(*) FROM magneticod.torrent_file_blobs`); blobs != n {
		t.Fatalf("blob rows = %d, want %d", blobs, n)
	}
	t.Logf("%s: full import of %d torrents completed against the SELECT-only source role", faultTS(), n)
}

// faultDSNWithPassword returns dsn with a synthetic password in its URL
// userinfo (preserving the username; defaulting to postgres when the cluster
// DSN carries none). Used by the failed-path regression only: paired with a
// bogus sslmode the run dies at parse time, before any server contact, so the
// synthetic credential works under trust-auth local harnesses and
// password-authenticated external servers alike.
func faultDSNWithPassword(t *testing.T, dsn, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", redactDSN(err.Error()))
	}
	user := "postgres"
	if u.User != nil && u.User.Username() != "" {
		user = u.User.Username()
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// faultDSNWithBogusSSLMode returns dsn with sslmode=bogus so the server
// driver rejects it at parse time — a deterministic failed-execution path.
func faultDSNWithBogusSSLMode(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", redactDSN(err.Error()))
	}
	q := u.Query()
	q.Set("sslmode", "bogus")
	u.RawQuery = q.Encode()
	return u.String()
}

// TestRedactDSNCredentials pins the redaction primitive itself: passwords in
// URL userinfo and libpq keyword form are masked wherever they appear, while
// everything else — including the input slice passed to redactArgs — is
// returned unchanged.
func TestRedactDSNCredentials(t *testing.T) {
	const pw = "sup3r-s3cr3t-fixtUre-pw"
	cases := []struct {
		name, in, want string
	}{
		{"url userinfo", "postgres://migrator:" + pw + "@127.0.0.1:5432/db?sslmode=disable",
			"postgres://migrator:" + faultRedactedMark + "@127.0.0.1:5432/db?sslmode=disable"},
		{"postgresql scheme", "postgresql://u:" + pw + "@h/db",
			"postgresql://u:" + faultRedactedMark + "@h/db"},
		{"embedded in env-style string", "MAGNETICO_MIGRATION_SOURCE_DSN=postgres://u:" + pw + "@h/db",
			"MAGNETICO_MIGRATION_SOURCE_DSN=postgres://u:" + faultRedactedMark + "@h/db"},
		{"keyword form", "host=127.0.0.1 user=mig password=" + pw + " dbname=db",
			"host=127.0.0.1 user=mig password=" + faultRedactedMark + " dbname=db"},
		{"quoted keyword password with spaces", "host=db password='" + pw + " extra words' user=mig dbname=d",
			"host=db password='" + faultRedactedMark + "' user=mig dbname=d"},
		{"quoted keyword password with escaped quote", "password='it\\'s " + pw + "' host=db",
			"password='" + faultRedactedMark + "' host=db"},
		{"quoted empty keyword password", "host=db password='' user=m",
			"host=db password='" + faultRedactedMark + "' user=m"},
		{"quoted and bare keyword passwords", "password='" + pw + " hidden' password=" + pw,
			"password='" + faultRedactedMark + "' password=" + faultRedactedMark},
		{"query parameter", "postgres://h/db?password=" + pw + "&sslmode=disable",
			"postgres://h/db?password=" + faultRedactedMark + "&sslmode=disable"},
		{"percent-encoded password", "postgres://u:s3cr%3At" + pw + "@h/db",
			"postgres://u:" + faultRedactedMark + "@h/db"},
		{"no password", "postgres://postgres@127.0.0.1:5432/db?sslmode=disable",
			"postgres://postgres@127.0.0.1:5432/db?sslmode=disable"},
		{"no userinfo", "postgres://127.0.0.1:5432/db?sslmode=disable",
			"postgres://127.0.0.1:5432/db?sslmode=disable"},
		{"empty password", "postgres://u:@h/db", "postgres://u:@h/db"},
		{"not a dsn", "--batch-size", "--batch-size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactDSN(tc.in)
			if got != tc.want {
				t.Fatalf("redactDSN(%q) = %q, want %q",
					faultScrubSecrets(tc.in, pw), faultScrubSecrets(got, pw), faultScrubSecrets(tc.want, pw))
			}
			if tc.in != tc.want && strings.Contains(got, pw) {
				t.Fatalf("redactDSN(%q) still contains the password: %q",
					faultScrubSecrets(tc.in, pw), faultScrubSecrets(got, pw))
			}
		})
	}

	raw := "postgres://migrator:" + pw + "@127.0.0.1:5432/db?sslmode=disable"
	args := []string{"migrate", "torrents", "--database", raw, "--batch-size", "10"}
	redacted := redactArgs(args)
	if strings.Contains(strings.Join(redacted, " "), pw) {
		t.Fatalf("redactArgs leaked the password: %q", faultScrubSecrets(strings.Join(redacted, " "), pw))
	}
	if args[3] != raw {
		t.Fatalf("redactArgs modified its input slice: args[3] = %q", args[3])
	}
	if redacted[3] != "postgres://migrator:"+faultRedactedMark+"@127.0.0.1:5432/db?sslmode=disable" {
		t.Fatalf("redacted args[3] = %q", redacted[3])
	}
}

// TestFaultHarnessLogsRedactCredentials runs the real import harness against
// credential-bearing DSNs and asserts the passwords reach the exec.Cmd
// arguments but never the captured harness log stream. The failed path uses a
// synthetic password behind a parse-time-invalid sslmode — the server is
// never contacted, so it is auth-method independent. The successful path uses
// the real target DSN, whose credentials are valid for the cluster even on
// password-authenticated external PostgreSQL (MAGNETICO_TEST_POSTGRES_DSN
// preserved), and asserts its actual password cannot appear in the logs.
func TestFaultHarnessLogsRedactCredentials(t *testing.T) {
	pgtest.IntegrationCluster(t)

	const pw = "s3cr3t-synthetic-04d-pw"
	src := pgtest.IntegrationCluster(t).TestDatabase(t)
	seedFaultSource(t, src, 5000)
	tgt := pgtest.IntegrationCluster(t).TestDatabase(t)
	initFaultTarget(t, tgt)
	bin := buildBinary(t)

	// Failed-path DSN: synthetic password, killed at parse time by the bogus
	// sslmode — no server authentication involved, works on every backend.
	badTgt := faultDSNWithBogusSSLMode(t, faultDSNWithPassword(t, tgt, pw))
	// Successful-path DSN: the target DSN itself — valid credentials for the
	// server, password-authenticated external clusters included.
	realPW := ""
	if u, err := url.Parse(tgt); err != nil {
		t.Fatalf("parse target dsn: %v", redactDSN(err.Error()))
	} else if u.User != nil {
		realPW, _ = u.User.Password()
	}
	// Failure diagnostics must never echo the credential whose leak they
	// report: every captured log or argv excerpt is scrubbed of the two
	// full DSNs and both passwords before being printed (longest first —
	// a password scrubbed out of its DSN would leave that DSN behind).
	scrub := func(s string) string { return faultScrubSecrets(s, badTgt, tgt, pw, realPW) }

	// Sink reset registered FIRST: LIFO cleanup order keeps the sink alive
	// through every harness cleanup line.
	sink := &safeBuffer{}
	t.Cleanup(func() { faultLogSink = nil })
	faultLogSink = func(format string, args ...any) { fmt.Fprintf(sink, format+"\n", args...) }

	// Failed execution path: the import dies in preflight on the unparsable
	// DSN, after startFaultImport logged the full (passworded) argument list.
	run := startFaultImport(t, bin, src, badTgt)
	code, sig, _ := run.waitExit()
	if sig != 0 || code == 0 {
		t.Fatalf("bad-DSN run: want a non-zero, non-signal exit, got code=%d sig=%v", code, sig)
	}
	// The real arguments reached exec.Cmd unmodified...
	if raw := strings.Join(run.cmd.Args, " "); !strings.Contains(raw, pw) {
		t.Fatalf("exec.Cmd arguments lost the synthetic password: %q", scrub(strings.Join(run.cmd.Args, " ")))
	}
	// ...while the captured harness log shows the redacted form only.
	logged := sink.String()
	if strings.Contains(logged, pw) {
		t.Fatalf("harness log leaked the synthetic password:\n%s", scrub(logged))
	}
	if !strings.Contains(logged, "started pid=") || !strings.Contains(logged, faultRedactedMark) {
		t.Fatalf("harness log is missing the redacted start line (capture broken or redaction not applied):\n%s", scrub(logged))
	}

	// Successful execution path: the valid credential-bearing target DSN
	// drives a complete import — under trust auth and password auth alike —
	// and its real password reaches exec.Cmd but never the harness log.
	beforeSuccess := sink.String()
	if code := runFaultResume(t, bin, src, tgt); code != 0 {
		t.Fatalf("import with the credential-bearing target DSN exit code = %d, want 0", code)
	}
	assertFaultComplete(t, tgt, 5000)
	logged = sink.String()
	if strings.Contains(logged, pw) {
		t.Fatalf("harness log leaked the synthetic password:\n%s", scrub(logged))
	}
	// Only the lines appended by the successful run: the DSN's own password
	// (and the whole credential-bearing DSN) must be absent, the redacted
	// form present — unless the cluster runs trust auth and the DSN carries
	// no password at all.
	successLog := logged[len(beforeSuccess):]
	if realPW != "" {
		if strings.Contains(successLog, realPW) || strings.Contains(successLog, tgt) {
			t.Fatalf("harness log leaked the target DSN password:\n%s", scrub(successLog))
		}
		if !strings.Contains(successLog, faultRedactedMark) {
			t.Fatalf("successful run's log line shows no redacted form — redaction not applied:\n%s", scrub(successLog))
		}
	}
	if !strings.Contains(successLog, "resumed `magnetico") {
		t.Fatalf("harness log is missing the resume log line — sink capture incomplete:\n%s", scrub(successLog))
	}
}
