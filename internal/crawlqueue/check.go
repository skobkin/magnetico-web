package crawlqueue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Report is the result of the on-demand diagnostic.
type Report struct {
	SchemaVersion     int
	QueueRecords      int64
	QueueBytes        int64
	QuarantineRecords int64
	QuarantineBytes   int64
	FileBytes         int64
	WALBytes          int64
	// CheckMode is "quick_check" or "integrity_check".
	CheckMode string
	// CheckResult holds SQLite's output lines; a healthy database yields
	// exactly ["ok"].
	CheckResult []string
}

// OK reports whether the integrity check found no problem.
func (r Report) OK() bool { return len(r.CheckResult) == 1 && r.CheckResult[0] == "ok" }

// Check is the optional diagnostic behind `crawl check-queue`. It is never
// run at startup. It takes the directory lock (so it cannot run against a
// live crawler: ErrLocked), never creates or modifies the database, and
// repairs nothing. full selects integrity_check instead of quick_check.
func Check(ctx context.Context, dir string, full bool) (Report, error) {
	var rep Report
	path := filepath.Join(dir, dbFileName)
	fi, err := os.Stat(path)
	if err != nil {
		return rep, fmt.Errorf("crawlqueue: %w", err)
	}
	rep.FileBytes = fi.Size()

	lock, err := lockDir(filepath.Join(dir, lockFileName))
	if err != nil {
		return rep, err
	}
	defer unlockDir(lock)

	// mode=ro: the diagnostic must not modify anything. A leftover WAL is
	// still readable.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return rep, fmt.Errorf("crawlqueue: open: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&rep.SchemaVersion); err != nil {
		return rep, fmt.Errorf("crawlqueue: read schema version: %w", err)
	}
	if rep.SchemaVersion != SchemaVersion {
		return rep, fmt.Errorf("%w: user_version=%d", ErrSchemaVersion, rep.SchemaVersion)
	}
	if w, err := os.Stat(path + "-wal"); err == nil {
		rep.WALBytes = w.Size()
	}

	rep.CheckMode = "quick_check"
	if full {
		rep.CheckMode = "integrity_check"
	}
	rows, err := db.QueryContext(ctx, "PRAGMA "+rep.CheckMode)
	if err != nil {
		return rep, fmt.Errorf("crawlqueue: %s: %w", rep.CheckMode, err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return rep, err
		}
		rep.CheckResult = append(rep.CheckResult, line)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return rep, fmt.Errorf("crawlqueue: %s: %w", rep.CheckMode, err)
	}

	if !rep.OK() {
		return rep, nil // counts could trip over the damage; report as is
	}
	if err := db.QueryRowContext(ctx,
		"SELECT count(*), COALESCE(sum(payload_bytes),0) FROM queue").Scan(&rep.QueueRecords, &rep.QueueBytes); err != nil {
		return rep, fmt.Errorf("crawlqueue: count queue: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT count(*), COALESCE(sum(payload_bytes),0) FROM quarantine").Scan(&rep.QuarantineRecords, &rep.QuarantineBytes); err != nil {
		return rep, fmt.Errorf("crawlqueue: count quarantine: %w", err)
	}
	return rep, nil
}
