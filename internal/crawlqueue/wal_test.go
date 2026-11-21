package crawlqueue

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func walSize(t *testing.T, q *Queue) int64 {
	t.Helper()
	fi, err := os.Stat(filepath.Join(q.dir, dbFileName+"-wal"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func fill(t *testing.T, q *Queue, from, n, size int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		if _, err := q.Append(bg, h(i), 1, payload(i, size)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// With only short readers (Peek), maintenance resets the WAL and the
// journal_size_limit truncation brings it back under the limit.
func TestWALReclaimedWithShortReaders(t *testing.T) {
	t.Parallel()
	const limit = 1 << 20
	q := open(t, func(o *Options) { o.JournalSizeLimit = limit })

	fill(t, q, 1, 40, 200_000) // 8 MB through the WAL
	if _, err := q.Peek(bg, 5, 1<<30); err != nil {
		t.Fatal(err)
	}
	if err := q.Maintain(bg); err != nil {
		t.Fatal(err)
	}
	if w := walSize(t, q); w > limit {
		t.Errorf("WAL = %d bytes after maintenance with only short readers, want ≤ %d", w, limit)
	}
	if s := q.Stats(); s.CheckpointRuns == 0 || s.CheckpointIncomplete != 0 || s.CheckpointFailures != 0 {
		t.Errorf("stats: %+v", s)
	}
}

// A long-lived read transaction pins the WAL: it grows past the limit while
// the reader is open and is reclaimed once the reader is gone. The queue
// stays writable throughout. (journal_size_limit is not a ceiling.)
func TestWALGrowsUnderLongReaderThenIsReclaimed(t *testing.T) {
	t.Parallel()
	const limit = 1 << 20
	var warnMu sync.Mutex
	var warnings []string
	q := open(t, func(o *Options) {
		o.JournalSizeLimit = limit
		o.walWarn = 2 << 20
		o.Logf = func(f string, a ...any) {
			warnMu.Lock()
			warnings = append(warnings, fmt.Sprintf(f, a...))
			warnMu.Unlock()
		}
	})
	fill(t, q, 1, 5, 1000)
	if err := q.Maintain(bg); err != nil {
		t.Fatal(err)
	}

	// The long reader: its own connection, snapshot held open.
	rdb, err := sql.Open("sqlite", "file:"+filepath.Join(q.dir, dbFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	tx, err := rdb.BeginTx(bg, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM queue").Scan(&n); err != nil {
		t.Fatal(err)
	}

	fill(t, q, 100, 60, 200_000) // 12 MB of new frames the reader pins
	if err := q.Maintain(bg); err != nil {
		t.Fatalf("maintenance must not fail under a long reader: %v", err)
	}
	grown := walSize(t, q)
	if grown <= limit {
		t.Fatalf("WAL = %d, expected it to grow past journal_size_limit %d while a reader pins it", grown, limit)
	}
	s := q.Stats()
	if s.CheckpointIncomplete == 0 {
		t.Errorf("an incomplete checkpoint must be counted: %+v", s)
	}
	if s.WALBytes != grown {
		t.Errorf("Stats.WALBytes = %d, file = %d", s.WALBytes, grown)
	}
	warnMu.Lock()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "WAL file is") {
		t.Errorf("want one WAL size warning, got %v", warnings)
	}
	warnMu.Unlock()
	// A second pass is throttled.
	if err := q.Maintain(bg); err != nil {
		t.Fatal(err)
	}
	warnMu.Lock()
	if len(warnings) != 1 {
		t.Errorf("WAL warning not throttled: %v", warnings)
	}
	warnMu.Unlock()
	// Writes still work while the WAL cannot be reset.
	fill(t, q, 1000, 5, 1000)

	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := q.Maintain(bg); err != nil {
		t.Fatal(err)
	}
	if w := walSize(t, q); w > limit {
		t.Errorf("WAL = %d after the reader closed, want ≤ %d", w, limit)
	}
	// All data is intact.
	got, err := q.Peek(bg, 1000, 1<<40)
	if err != nil || len(got) != 5+60+5 {
		t.Fatalf("rows: %v len=%d", err, len(got))
	}
}

// A failing checkpoint is logged, counted and retried; the writer keeps
// accepting appends.
func TestCheckpointFailureIsNotFatal(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var warnings []string
	fail := true
	q := open(t, func(o *Options) {
		o.Logf = func(f string, a ...any) {
			mu.Lock()
			warnings = append(warnings, fmt.Sprintf(f, a...))
			mu.Unlock()
		}
		o.hooks.checkpointErr = func() error {
			mu.Lock()
			defer mu.Unlock()
			if fail {
				return errors.New("injected checkpoint failure")
			}
			return nil
		}
	})
	fill(t, q, 1, 3, 100)
	if err := q.Maintain(bg); err != nil {
		t.Fatalf("Maintain returned %v", err)
	}
	if err := q.Maintain(bg); err != nil {
		t.Fatal(err)
	}
	if q.Err() != nil {
		t.Fatalf("checkpoint failure must not poison the queue: %v", q.Err())
	}
	if s := q.Stats(); s.CheckpointFailures != 2 {
		t.Errorf("failures = %d, want 2", s.CheckpointFailures)
	}
	mu.Lock()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "injected checkpoint failure") {
		t.Errorf("want exactly one throttled warning, got %v", warnings)
	}
	fail = false
	mu.Unlock()

	fill(t, q, 10, 3, 100) // writer still works
	if err := q.Maintain(bg); err != nil {
		t.Fatal(err)
	}
	if s := q.Stats(); s.CheckpointFailures != 2 || s.QueueRecords != 6 {
		t.Errorf("stats after recovery: %+v", s)
	}
}

// Freed space is returned to the filesystem by incremental_vacuum.
func TestFileShrinksAfterIncrementalVacuum(t *testing.T) {
	t.Parallel()
	q := open(t, func(o *Options) { o.VacuumThreshold = 1 << 20 })
	dbPath := filepath.Join(q.dir, dbFileName)

	fill(t, q, 1, 40, 200_000) // ≈ 8 MB
	if err := q.Maintain(bg); err != nil {
		t.Fatal(err)
	}
	grown := fileSize(t, dbPath)
	if grown < 6<<20 {
		t.Fatalf("database only %d bytes after filling", grown)
	}

	items, _ := q.Peek(bg, 1000, 1<<40)
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	if err := q.Ack(bg, ids); err != nil {
		t.Fatal(err)
	}
	if fileSize(t, dbPath) != grown {
		t.Log("file already changed after ack (fine)")
	}
	for i := 0; i < 4; i++ { // vacuum runs in chunks; the second half needs a checkpoint
		if err := q.Maintain(bg); err != nil {
			t.Fatal(err)
		}
	}
	shrunk := fileSize(t, dbPath)
	if shrunk >= grown/2 {
		t.Errorf("file did not shrink: %d → %d", grown, shrunk)
	}
	if q.Stats().VacuumRuns == 0 {
		t.Error("vacuum never ran")
	}
	// Still consistent.
	q2 := reopen(t, q, nil)
	if s := q2.Stats(); s.QueueRecords != 0 || s.QueueBytes != 0 {
		t.Errorf("stats after vacuum+reopen: %+v", s)
	}
}

func TestCheckDetectsCorruption(t *testing.T) {
	t.Parallel()
	q := open(t, nil)
	fill(t, q, 1, 30, 20_000)
	dir := q.dir
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}

	rep, err := Check(bg, dir, false)
	if err != nil || !rep.OK() || rep.QueueRecords != 30 || rep.QueueBytes != 30*20_000 {
		t.Fatalf("healthy queue: %v %+v", err, rep)
	}
	if rep.CheckMode != "quick_check" {
		t.Errorf("mode = %s", rep.CheckMode)
	}
	rep, err = Check(bg, dir, true)
	if err != nil || !rep.OK() || rep.CheckMode != "integrity_check" {
		t.Fatalf("integrity_check: %v %+v", err, rep)
	}

	// Damage a page in the middle of the file.
	path := filepath.Join(dir, dbFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mid := len(data) / 2
	copy(data[mid:], bytes.Repeat([]byte{0xA5}, 4096))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err = Check(context.Background(), dir, true)
	if err == nil && rep.OK() {
		t.Fatalf("corruption not detected: %+v", rep)
	}
	// Diagnostic only: nothing was repaired or deleted.
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, data) {
		t.Error("Check modified the database file")
	}
	// A normal open does not scan (no startup integrity check) and does not
	// repair either.
	if q2, err := Open(Options{Dir: dir, Logf: func(string, ...any) {}}); err == nil {
		_ = q2.Close()
	}
}
