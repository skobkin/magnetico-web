package crawlqueue

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

var bg = context.Background()

func h(n int) [20]byte {
	var x [20]byte
	binary.BigEndian.PutUint64(x[12:], uint64(n))
	return x
}

func payload(n int, size int) []byte {
	p := bytes.Repeat([]byte{byte(n)}, size)
	return p
}

func open(t *testing.T, mut func(*Options)) *Queue {
	t.Helper()
	o := Options{Dir: t.TempDir(), Logf: func(string, ...any) {}}
	if mut != nil {
		mut(&o)
	}
	q, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func reopen(t *testing.T, q *Queue, mut func(*Options)) *Queue {
	t.Helper()
	o := q.opts
	o.hooks = hooks{}
	if mut != nil {
		mut(&o)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q2, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q2.Close() })
	return q2
}

func TestAppendPeekAckQuarantine(t *testing.T) {
	t.Parallel()
	q := open(t, nil)

	for i := 1; i <= 5; i++ {
		ok, err := q.Append(bg, h(i), FormatJSONv1, payload(i, 100*i))
		if err != nil || !ok {
			t.Fatalf("append %d: ok=%v err=%v", i, ok, err)
		}
	}
	if s := q.Stats(); s.QueueRecords != 5 || s.QueueBytes != 1500 {
		t.Fatalf("stats after append: %+v", s)
	}

	items, err := q.Peek(bg, 10, 1<<20)
	if err != nil || len(items) != 5 {
		t.Fatalf("peek: %v len=%d", err, len(items))
	}
	for i, it := range items {
		if it.InfoHash != h(i+1) || !bytes.Equal(it.Payload, payload(i+1, 100*(i+1))) || it.Format != FormatJSONv1 {
			t.Fatalf("item %d mismatch (FIFO / payload)", i)
		}
	}

	// Peek is read-only.
	again, _ := q.Peek(bg, 10, 1<<20)
	if len(again) != 5 {
		t.Fatal("peek consumed rows")
	}

	// Record and byte limits; at least one record even if it exceeds maxBytes.
	if got, _ := q.Peek(bg, 2, 1<<20); len(got) != 2 {
		t.Errorf("maxRecords: got %d", len(got))
	}
	if got, _ := q.Peek(bg, 10, 350); len(got) != 2 { // 100+200 ≤ 350 < +300
		t.Errorf("maxBytes: got %d, want 2", len(got))
	}
	if got, _ := q.Peek(bg, 10, 1); len(got) != 1 || got[0].ID != items[0].ID {
		t.Errorf("head record must always be returned")
	}
	if got, _ := q.Peek(bg, 0, 1<<20); len(got) != 0 {
		t.Error("maxRecords=0 must return nothing")
	}

	if err := q.Ack(bg, []int64{items[0].ID, items[1].ID, 9999}); err != nil {
		t.Fatal(err)
	}
	if s := q.Stats(); s.QueueRecords != 3 || s.QueueBytes != 300+400+500 {
		t.Fatalf("stats after ack: %+v", s)
	}

	if err := q.Quarantine(bg, []int64{items[2].ID}, "invalid_record", 400); err != nil {
		t.Fatal(err)
	}
	s := q.Stats()
	if s.QueueRecords != 2 || s.QuarantineRecords != 1 || s.QuarantineBytes != 300 ||
		s.QueueBytes+s.QuarantineBytes != 1200 {
		t.Fatalf("stats after quarantine: %+v", s)
	}
	rest, _ := q.Peek(bg, 10, 1<<20)
	if len(rest) != 2 || rest[0].ID != items[3].ID {
		t.Fatalf("quarantined row still in queue: %d rows", len(rest))
	}

	// Quarantine keeps the payload, reason and status.
	var reason string
	var status sql.NullInt64
	var p []byte
	if err := q.rdb.QueryRow("SELECT reason_code, http_status, payload FROM quarantine").Scan(&reason, &status, &p); err != nil {
		t.Fatal(err)
	}
	if reason != "invalid_record" || status.Int64 != 400 || !bytes.Equal(p, payload(3, 300)) {
		t.Errorf("quarantine row: %q %v len=%d", reason, status, len(p))
	}
}

func TestDuplicateHashes(t *testing.T) {
	t.Parallel()
	q := open(t, nil)
	if ok, err := q.Append(bg, h(1), 1, payload(1, 50)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	ok, err := q.Append(bg, h(1), 1, payload(2, 70))
	if ok || err != nil {
		t.Fatalf("duplicate: ok=%v err=%v", ok, err)
	}
	if s := q.Stats(); s.QueueRecords != 1 || s.QueueBytes != 50 {
		t.Fatalf("duplicate changed accounting: %+v", s)
	}
	items, _ := q.Peek(bg, 10, 1<<20)
	if len(items) != 1 || !bytes.Equal(items[0].Payload, payload(1, 50)) {
		t.Fatal("first copy must be kept")
	}
	// Duplicates inside one group commit.
	var wg sync.WaitGroup
	var mu sync.Mutex
	appended := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := q.Append(bg, h(77), 1, payload(7, 10))
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				appended++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if appended != 1 {
		t.Fatalf("concurrent duplicates appended %d times", appended)
	}
}

func TestByteAccountingAcrossReopen(t *testing.T) {
	t.Parallel()
	q := open(t, nil)
	for i := 1; i <= 10; i++ {
		if _, err := q.Append(bg, h(i), 1, payload(i, 1000+i)); err != nil {
			t.Fatal(err)
		}
	}
	items, _ := q.Peek(bg, 3, 1<<20)
	_ = q.Ack(bg, []int64{items[0].ID})
	_ = q.Quarantine(bg, []int64{items[1].ID}, "x", 0)
	if err := q.AppendQuarantined(bg, h(99), 1, payload(9, 777), "record_too_large"); err != nil {
		t.Fatal(err)
	}
	before := q.Stats()

	q2 := reopen(t, q, nil)
	after := q2.Stats()
	if before.QueueRecords != after.QueueRecords || before.QueueBytes != after.QueueBytes ||
		before.QuarantineRecords != after.QuarantineRecords || before.QuarantineBytes != after.QuarantineBytes {
		t.Fatalf("counters changed across reopen:\n before %+v\n after  %+v", before, after)
	}
	if after.QueueRecords != 8 || after.QuarantineRecords != 2 {
		t.Fatalf("unexpected counts: %+v", after)
	}
	var sumQ, sumQu int64
	_ = q2.rdb.QueryRow("SELECT sum(payload_bytes) FROM queue").Scan(&sumQ)
	_ = q2.rdb.QueryRow("SELECT sum(payload_bytes) FROM quarantine").Scan(&sumQu)
	if sumQ != after.QueueBytes || sumQu != after.QuarantineBytes {
		t.Fatalf("in-memory bytes disagree with SUM(): %d/%d vs %+v", sumQ, sumQu, after)
	}
}

func TestQuotaCountsQueueAndQuarantine(t *testing.T) {
	t.Parallel()
	q := open(t, func(o *Options) { o.MaxBytes = 1000 })
	if _, err := q.Append(bg, h(1), 1, payload(1, 400)); err != nil {
		t.Fatal(err)
	}
	if err := q.AppendQuarantined(bg, h(2), 1, payload(2, 400), "record_too_large"); err != nil {
		t.Fatal(err)
	}
	// 800 used; 300 does not fit.
	if _, err := q.Append(bg, h(3), 1, payload(3, 300)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
	if err := q.AppendQuarantined(bg, h(4), 1, payload(4, 300), "x"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("quarantine append: err = %v, want ErrQueueFull", err)
	}
	// 200 fits exactly.
	if _, err := q.Append(bg, h(5), 1, payload(5, 200)); err != nil {
		t.Fatalf("exact fit rejected: %v", err)
	}
	if s := q.Stats(); s.FullRejections != 2 || s.QueueBytes+s.QuarantineBytes != 1000 {
		t.Fatalf("stats: %+v", s)
	}
	// Quarantining within the queue does not change the total.
	items, _ := q.Peek(bg, 1, 1<<20)
	if err := q.Quarantine(bg, []int64{items[0].ID}, "x", 400); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Append(bg, h(6), 1, payload(6, 1)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("quarantine must keep consuming quota: %v", err)
	}
	// Ack frees quota.
	items, _ = q.Peek(bg, 1, 1<<20)
	_ = q.Ack(bg, []int64{items[0].ID})
	if _, err := q.Append(bg, h(6), 1, payload(6, 100)); err != nil {
		t.Fatalf("append after ack: %v", err)
	}
}

// A group commit in which only some records fit must reject exactly those.
func TestQuotaInsideGroupCommit(t *testing.T) {
	t.Parallel()
	q := open(t, func(o *Options) { o.MaxBytes = 500; o.GroupMaxDelay = 200 * time.Millisecond })
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = q.Append(bg, h(i), 1, payload(i, 100))
		}()
	}
	wg.Wait()
	ok, full := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			ok++
		case errors.Is(e, ErrQueueFull):
			full++
		default:
			t.Fatal(e)
		}
	}
	if ok != 5 || full != 5 {
		t.Fatalf("ok=%d full=%d, want 5/5", ok, full)
	}
	if s := q.Stats(); s.QueueBytes != 500 || s.QueueRecords != 5 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestSQLiteFullMapsToQueueFull(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	q, err := Open(Options{Dir: dir, MaxPageCount: 60, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	var accepted int
	var sawFull bool
	for i := 1; i <= 200; i++ {
		_, err := q.Append(bg, h(i), 1, payload(i, 20_000))
		if errors.Is(err, ErrQueueFull) {
			sawFull = true
			break
		}
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		accepted++
	}
	if !sawFull || accepted == 0 {
		t.Fatalf("sawFull=%v accepted=%d", sawFull, accepted)
	}
	if q.Err() != nil {
		t.Fatalf("SQLITE_FULL must not be fatal: %v", q.Err())
	}
	// Acked space can be reused after the failure; the queue stays usable.
	items, _ := q.Peek(bg, 1000, 1<<30)
	if len(items) != accepted {
		t.Fatalf("committed rows = %d, accepted = %d", len(items), accepted)
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	if err := q.Ack(bg, ids); err != nil {
		t.Fatalf("ack after full: %v", err)
	}
	if _, err := q.Append(bg, h(500), 1, payload(1, 20_000)); err != nil {
		t.Fatalf("append after space was freed: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	rep, err := Check(bg, dir, true)
	if err != nil || !rep.OK() {
		t.Fatalf("database damaged after SQLITE_FULL: %v %+v", err, rep)
	}
	if rep.QueueRecords != 1 {
		t.Fatalf("records after reopen: %+v", rep)
	}
}

func TestSecondOpenIsRefused(t *testing.T) {
	t.Parallel()
	q := open(t, nil)
	if _, err := Open(Options{Dir: q.dir}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second open: err = %v, want ErrLocked", err)
	}
	if _, err := Check(bg, q.dir, false); !errors.Is(err, ErrLocked) {
		t.Fatalf("Check on a live queue: err = %v, want ErrLocked", err)
	}
	dir := q.dir
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q2, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatalf("open after close: %v", err)
	}
	_ = q2.Close()
}

func TestSchemaVersionMismatchRefused(t *testing.T) {
	t.Parallel()
	q := open(t, nil)
	dir := q.dir
	_ = q.Close()

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(Options{Dir: dir}); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("err = %v, want ErrSchemaVersion", err)
	}
	// The failed open must have released the lock.
	if _, err := Check(bg, dir, false); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("Check: err = %v, want ErrSchemaVersion (lock must be free)", err)
	}
}

func TestForeignDatabaseRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE other(x)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(Options{Dir: dir}); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("err = %v, want ErrSchemaVersion", err)
	}
}

func TestPragmas(t *testing.T) {
	t.Parallel()
	q := open(t, nil)
	get := func(p string) string {
		var v string
		if err := q.wdb.QueryRow("PRAGMA " + p).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for p, want := range map[string]string{
		"journal_mode": "wal", "synchronous": "2", "auto_vacuum": "2",
		"user_version": "1", "busy_timeout": "5000",
	} {
		if got := get(p); got != want {
			t.Errorf("PRAGMA %s = %s, want %s", p, got, want)
		}
	}
	if got := get("journal_size_limit"); got != strconv.Itoa(defaultJournalSizeLimit) {
		t.Errorf("journal_size_limit = %s", got)
	}
}

func TestCloseSemantics(t *testing.T) {
	t.Parallel()
	q := open(t, nil)
	if _, err := q.Append(bg, h(1), 1, payload(1, 10)); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := q.Append(bg, h(2), 1, payload(1, 10)); !errors.Is(err, ErrClosed) {
		t.Fatalf("append after close: %v", err)
	}
	if _, err := q.Peek(bg, 1, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("peek after close: %v", err)
	}
}

func TestConcurrentAppendsAreGroupCommitted(t *testing.T) {
	t.Parallel()
	q := open(t, nil)
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := q.Append(bg, h(i), 1, payload(i, 500)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	s := q.Stats()
	if s.QueueRecords != n || s.AppendedRecords != n {
		t.Fatalf("stats: %+v", s)
	}
	if s.AppendCommits >= n || s.MaxGroupRecords < 2 {
		t.Errorf("no group commit happened: commits=%d maxGroup=%d", s.AppendCommits, s.MaxGroupRecords)
	}
	if s.MaxGroupRecords > int64(q.opts.GroupMaxRecords) {
		t.Errorf("group of %d exceeds the limit %d", s.MaxGroupRecords, q.opts.GroupMaxRecords)
	}
}

func TestAppendHonoursContextWhileQueued(t *testing.T) {
	t.Parallel()
	q := open(t, func(o *Options) { o.GroupMaxDelay = time.Hour })
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := q.Append(ctx, h(1), 1, payload(1, 10))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Append ignored its context")
	}
}

// ---------------------------------------------------------- crash recovery

const (
	envChildDir  = "MAGNETICO_TEST_CRAWLQUEUE_DIR"
	envChildMode = "MAGNETICO_TEST_CRAWLQUEUE_MODE"
)

// TestChildProcess is re-executed by the crash tests.
func TestChildProcess(t *testing.T) {
	dir, mode := os.Getenv(envChildDir), os.Getenv(envChildMode)
	if dir == "" {
		t.Skip("child process helper")
	}
	switch mode {
	case "writer":
		q, err := Open(Options{Dir: dir})
		if err != nil {
			fmt.Println("OPENERR", err)
			os.Exit(3)
		}
		var wg sync.WaitGroup
		var next int64
		var mu sync.Mutex
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					mu.Lock()
					i := int(next)
					next++
					mu.Unlock()
					if _, err := q.Append(bg, h(i), 1, payload(i, 2000+i%5000)); err != nil {
						return
					}
					// Printed only after Append returned: the record is durable.
					fmt.Printf("ACK %d\n", i)
				}
			}()
		}
		wg.Wait()
	case "open":
		_, err := Open(Options{Dir: dir})
		if errors.Is(err, ErrLocked) {
			fmt.Println("LOCKED")
			return
		}
		fmt.Println("OPENED", err)
	}
}

func runChild(t *testing.T, dir, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildProcess$", "-test.v")
	cmd.Env = append(os.Environ(), envChildDir+"="+dir, envChildMode+"="+mode)
	return cmd
}

func TestSIGKILLKeepsEveryAcknowledgedRecord(t *testing.T) {
	t.Parallel()
	for round := 0; round < 3; round++ {
		dir := t.TempDir()
		cmd := runChild(t, dir, "writer")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		acked := map[int]bool{}
		var mu sync.Mutex
		readDone := make(chan struct{})
		killAt := 150 + round*100
		go func() {
			defer close(readDone)
			buf := make([]byte, 0, 1<<16)
			tmp := make([]byte, 4096)
			killed := false
			for {
				n, err := stdout.Read(tmp)
				buf = append(buf, tmp[:n]...)
				for {
					i := bytes.IndexByte(buf, '\n')
					if i < 0 {
						break
					}
					line := strings.TrimSpace(string(buf[:i]))
					buf = buf[i+1:]
					if rest, ok := strings.CutPrefix(line, "ACK "); ok {
						v, _ := strconv.Atoi(rest)
						mu.Lock()
						acked[v] = true
						cnt := len(acked)
						mu.Unlock()
						if cnt >= killAt && !killed {
							killed = true
							_ = cmd.Process.Signal(syscall.SIGKILL)
						}
					}
				}
				if err != nil {
					return
				}
			}
		}()
		<-readDone
		_ = cmd.Wait()

		mu.Lock()
		n := len(acked)
		mu.Unlock()
		if n < killAt {
			t.Fatalf("round %d: child exited early with %d acks", round, n)
		}

		q, err := Open(Options{Dir: dir, Logf: func(string, ...any) {}})
		if err != nil {
			t.Fatalf("round %d: reopen after SIGKILL: %v", round, err)
		}
		items, err := q.Peek(bg, 1_000_000, 1<<40)
		if err != nil {
			t.Fatal(err)
		}
		present := map[[20]byte]bool{}
		for _, it := range items {
			present[it.InfoHash] = true
		}
		mu.Lock()
		for i := range acked {
			if !present[h(i)] {
				t.Errorf("round %d: acknowledged record %d is missing after SIGKILL", round, i)
			}
		}
		mu.Unlock()
		// The rows that were committed but not yet printed may exist too.
		if len(items) < n {
			t.Errorf("round %d: %d rows < %d acked", round, len(items), n)
		}
		// Counters are rebuilt consistently.
		s := q.Stats()
		var sum int64
		for _, it := range items {
			sum += int64(len(it.Payload))
		}
		if s.QueueRecords != int64(len(items)) || s.QueueBytes != sum {
			t.Errorf("round %d: counters %+v vs rows=%d bytes=%d", round, s, len(items), sum)
		}
		_ = q.Close()
		rep, err := Check(bg, dir, true)
		if err != nil || !rep.OK() {
			t.Errorf("round %d: integrity after SIGKILL: %v %+v", round, err, rep)
		}
		t.Logf("round %d: %d acknowledged, %d rows after SIGKILL", round, n, len(items))
	}
}

func TestSecondProcessIsRefused(t *testing.T) {
	t.Parallel()
	q := open(t, nil)
	out, err := runChild(t, q.dir, "open").CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "LOCKED") {
		t.Fatalf("second process was not refused:\n%s", out)
	}
}

// --------------------------------------------- review follow-ups (ownership)

// holdWriter makes the writer block before its next operation until release
// is called.
func holdWriter(o *Options) (release func()) {
	gate := make(chan struct{})
	var once sync.Once
	o.hooks.beforeOp = func() { <-gate }
	return func() { once.Do(func() { close(gate) }) }
}

// submitCancelled runs fn with a context that is cancelled as soon as the
// operation has been handed to the writer, then waits for fn to return.
func submitCancelled(t *testing.T, fn func(ctx context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	time.Sleep(50 * time.Millisecond) // let the op reach the writer
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestAppendCopiesPayloadBeforeHandoff(t *testing.T) {
	var release func()
	q := open(t, func(o *Options) { release = holdWriter(o) })
	defer release()

	buf := bytes.Repeat([]byte{'A'}, 64)
	submitCancelled(t, func(ctx context.Context) error {
		_, err := q.Append(ctx, h(1), 1, buf)
		return err
	})
	for i := range buf { // the caller reuses its buffer
		buf[i] = 'Z'
	}
	quar := bytes.Repeat([]byte{'B'}, 64)
	submitCancelled(t, func(ctx context.Context) error {
		return q.AppendQuarantined(ctx, h(2), 1, quar, "record_too_large")
	})
	for i := range quar {
		quar[i] = 'Z'
	}
	release()

	items := waitPeek(t, q, 1)
	if !bytes.Equal(items[0].Payload, bytes.Repeat([]byte{'A'}, 64)) {
		t.Fatalf("queued payload was mutated: %q", items[0].Payload)
	}
	// Quarantined payload: close and read the table directly.
	dir := q.opts.Dir
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "queue.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got []byte
	if err := db.QueryRow("SELECT payload FROM quarantine WHERE info_hash = ?", func() []byte { x := h(2); return x[:] }()).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{'B'}, 64)) {
		t.Fatalf("quarantined payload was mutated: %q", got)
	}
}

func TestAckAndQuarantineCopyIDsBeforeHandoff(t *testing.T) {
	var release func()
	var holding atomicBool
	q := open(t, func(o *Options) {
		gate := make(chan struct{})
		var once sync.Once
		release = func() { once.Do(func() { close(gate) }) }
		o.hooks.beforeOp = func() {
			if holding.Load() {
				<-gate
			}
		}
	})
	defer release()
	for i := 1; i <= 4; i++ {
		if _, err := q.Append(bg, h(i), 1, payload(i, 10)); err != nil {
			t.Fatal(err)
		}
	}
	items, err := q.Peek(bg, 4, 1<<20)
	if err != nil || len(items) != 4 {
		t.Fatalf("peek: %v %d", err, len(items))
	}
	holding.Store(true)

	ackIDs := []int64{items[0].ID, items[1].ID}
	submitCancelled(t, func(ctx context.Context) error { return q.Ack(ctx, ackIDs) })
	ackIDs[0], ackIDs[1] = items[2].ID, items[3].ID // reuse: must not affect the op

	quarIDs := []int64{items[2].ID}
	submitCancelled(t, func(ctx context.Context) error { return q.Quarantine(ctx, quarIDs, "x", 400) })
	quarIDs[0] = items[3].ID
	release()

	// Items 0 and 1 acked; item 2 quarantined; item 3 must remain queued.
	deadline := time.Now().Add(10 * time.Second)
	for {
		rest, err := q.Peek(bg, 10, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if len(rest) == 1 && rest[0].ID == items[3].ID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remaining queue = %+v, want only id %d", rest, items[3].ID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := q.Stats(); s.QuarantineRecords != 1 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestDuplicateAtFullCapacityIsIdempotent(t *testing.T) {
	t.Parallel()
	q := open(t, func(o *Options) { o.MaxBytes = 100 })
	if ok, err := q.Append(bg, h(1), 1, payload(1, 100)); err != nil || !ok {
		t.Fatalf("fill: %v %v", ok, err)
	}
	ok, err := q.Append(bg, h(1), 1, payload(1, 100))
	if err != nil || ok {
		t.Fatalf("duplicate at full: appended=%v err=%v, want false,nil", ok, err)
	}
	if _, err := q.Append(bg, h(2), 1, payload(2, 1)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("new hash at full: %v", err)
	}
	if s := q.Stats(); s.FullRejections != 1 || s.QueueRecords != 1 || s.QueueBytes != 100 {
		t.Fatalf("stats: %+v", s)
	}
}

func waitPeek(t *testing.T, q *Queue, n int) []Item {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		items, err := q.Peek(bg, 10, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) >= n {
			return items
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue never reached %d items", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type atomicBool struct{ v atomic.Bool }

func (b *atomicBool) Load() bool   { return b.v.Load() }
func (b *atomicBool) Store(x bool) { b.v.Store(x) }
