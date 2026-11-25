package stats

import (
	"context"
	"time"

	"github.com/skobkin/magnetico-web/internal/opstats"
)

// heartbeatLoop publishes this instance's snapshot every HeartbeatInterval on
// its own connection. It shares no connection, goroutine or lock with the flush
// (the recorder mutex is a sub-microsecond section), so a stalled flush cannot
// delay it; it reports how long the flush has been stalled instead.
func (w *Writer) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(w.opt.HeartbeatInterval)
	defer t.Stop()
	// The first snapshot is published as soon as the writer is ready (polled
	// every second until then), so a freshly started process is visible at once
	// instead of after a whole interval.
	first := true
	for {
		if first {
			select {
			case <-ctx.Done():
				return
			case <-time.After(startPoll(w.Status().Ready)):
			}
		} else {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
		w.rec.MarkRunning() // a quiet process still has coverage
		if !w.Status().Ready {
			continue
		}
		first = false
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		snap := w.snapshotNow()
		_, err := w.hbPool.Exec(hctx, opstats.UpsertSnapshotSQL(), snap.Args()...)
		cancel()
		if err != nil && ctx.Err() == nil {
			w.logChange("heartbeat", errClass(err), "statistics: publishing the heartbeat failed (%s); retrying", safeErr(err))
		} else if err == nil {
			w.logChange("heartbeat", "ok", "statistics: heartbeat publishing works")
		}
	}
}

// startPoll is how long the first heartbeat waits before looking at the writer
// again: nothing once it is ready, a second while it is still preparing.
func startPoll(ready bool) time.Duration {
	if ready {
		return 0
	}
	return time.Second
}

// snapshotNow assembles the heartbeat row from the recorder and the writer.
func (w *Writer) snapshotNow() opstats.Snapshot {
	now := w.now()
	ws := w.rec.Windows()
	tot := w.rec.Totals()
	s := opstats.Snapshot{
		InstanceID: w.instanceID, StartedAt: w.started, PublishedAt: now,
		OwnsConsumer: w.owner != nil && w.owner.Owns(),
		InFlight:     w.rec.InFlight(),
		ClockAnomaly: w.rec.ClockAnomaly(),
	}
	if ws.EndSecond > 0 {
		s.WindowEnd = time.Unix(ws.EndSecond, 0).UTC()
	}
	for i, c := range opstats.SnapshotTotalCounters {
		s.Totals[i] = tot[c]
	}
	s.WindowCovered = ws.Covered
	for i := range ws.Counts {
		for m := 0; m < numWindowMetrics; m++ {
			s.WindowCounts[i][m] = ws.Counts[i][m]
		}
	}
	w.mu.Lock()
	c := w.counters
	stalled := int64(0)
	if w.status.sealed || w.lastErr != "" {
		stalled = int64(now.Sub(w.lastOK) / time.Second)
		if w.lastOK.IsZero() {
			stalled = int64(now.Sub(w.started) / time.Second)
		}
	}
	w.mu.Unlock()
	s.FlushStalledSeconds = max(stalled, 0)
	s.HoursDropped, s.SequenceAnomalies, s.BatchesDroppedUnresolved = c.HoursDropped, c.SequenceAnomalies, c.BatchesDroppedUnresolved
	s.ResolvedCommitted, s.GaugeBatchesSkipped, s.CrawlersOverflow = c.ResolvedCommitted, c.GaugeBatchesSkipped, c.CrawlersOverflow
	s.ClockAnomaly = s.ClockAnomaly || c.ClockAnomaly
	return s
}
