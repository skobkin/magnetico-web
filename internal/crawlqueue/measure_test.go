package crawlqueue

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMeasure reports queue throughput, group-commit sizes, commit (fsync)
// latency and the disk-to-payload ratio. It is a measurement, not a gate: it
// asserts nothing about speed and is skipped unless
// MAGNETICO_CRAWLQUEUE_MEASURE_DIR names a directory on the filesystem to be
// measured (t.TempDir() is often tmpfs, where fsync is free):
//
//	MAGNETICO_CRAWLQUEUE_MEASURE_DIR=/var/tmp go test ./internal/crawlqueue \
//	    -run TestMeasure -v -count=1
func TestMeasure(t *testing.T) {
	base := os.Getenv("MAGNETICO_CRAWLQUEUE_MEASURE_DIR")
	if base == "" {
		t.Skip("set MAGNETICO_CRAWLQUEUE_MEASURE_DIR to run the measurements")
	}

	classes := []struct {
		name  string
		size  int
		count int
	}{
		{"p50  ≈350 B", 350, 4000},
		{"mean ≈3.1 KB", 3100, 4000},
		{"p99  ≈41 KB", 41_000, 800},
		{"p99.9 ≈394 KB", 394_000, 120},
	}
	t.Logf("%-14s %5s %8s %9s %9s %10s %12s %10s", "payload", "prod", "records", "rec/s", "MiB/s",
		"avg group", "commit ms", "")
	for _, c := range classes {
		for _, producers := range []int{1, 8, 64} {
			dir, err := os.MkdirTemp(base, "crawlqueue-measure-")
			if err != nil {
				t.Fatal(err)
			}
			q, err := Open(Options{Dir: dir, MaxBytes: 8 << 30, Logf: func(string, ...any) {}})
			if err != nil {
				t.Fatal(err)
			}
			var next atomic.Int64
			var wg sync.WaitGroup
			start := time.Now()
			for p := 0; p < producers; p++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					buf := payload(p, c.size)
					for {
						i := int(next.Add(1))
						if i > c.count {
							return
						}
						if _, err := q.Append(bg, h(i), 1, buf); err != nil {
							t.Error(err)
							return
						}
					}
				}()
			}
			wg.Wait()
			el := time.Since(start)
			s := q.Stats()
			total := float64(c.count) * float64(c.size)
			t.Logf("%-14s %5d %8d %9.0f %9.1f %10.1f %12.2f",
				c.name, producers, c.count, float64(c.count)/el.Seconds(), total/el.Seconds()/(1<<20),
				float64(s.AppendedRecords)/float64(s.AppendCommits),
				float64(s.AppendCommitNanos)/float64(s.AppendCommits)/1e6)

			if producers == 64 {
				// Disk usage for this payload class, before and after a
				// checkpoint that resets the WAL.
				du := func() (db, wal int64) {
					if fi, err := os.Stat(filepath.Join(dir, dbFileName)); err == nil {
						db = fi.Size()
					}
					if fi, err := os.Stat(filepath.Join(dir, dbFileName+"-wal")); err == nil {
						wal = fi.Size()
					}
					return
				}
				db1, wal1 := du()
				if err := q.Maintain(bg); err != nil {
					t.Fatal(err)
				}
				db2, wal2 := du()
				t.Logf("    disk, %d payload bytes: before checkpoint db+wal = %d (ratio %.3f, wal %d); "+
					"after TRUNCATE db+wal = %d (ratio %.3f, per-row overhead %.0f B)",
					int64(total), db1+wal1, float64(db1+wal1)/total, wal1, db2+wal2, float64(db2+wal2)/total,
					(float64(db2+wal2)-total)/float64(c.count))
			}
			_ = q.Close()
			_ = os.RemoveAll(dir)
		}
	}

	// Peek/Ack drain rate for a mixed queue.
	dir, err := os.MkdirTemp(base, "crawlqueue-measure-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	q, err := Open(Options{Dir: dir, MaxBytes: 8 << 30, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	var wg sync.WaitGroup
	for p := 0; p < 32; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := p; i < 6000; i += 32 {
				if _, err := q.Append(bg, h(i), 1, payload(i, 3100)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	start := time.Now()
	drained := 0
	for {
		items, err := q.Peek(bg, 256, 8<<20)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		ids := make([]int64, len(items))
		for i, it := range items {
			ids[i] = it.ID
		}
		if err := q.Ack(bg, ids); err != nil {
			t.Fatal(err)
		}
		drained += len(items)
	}
	el := time.Since(start)
	t.Logf("drain (Peek 256 / Ack, 3.1 KB records): %d records in %v = %.0f rec/s", drained, el.Round(time.Millisecond),
		float64(drained)/el.Seconds())
	_ = fmt.Sprint
}
