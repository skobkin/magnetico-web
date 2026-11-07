package migration

// Conversion of legacy v0.12.0 source windows into neutral pipeline jobs.
// Read-stage failures are pre-settled here: file-count overflow and
// missing-file torrents become categorized per-record errors without
// entering the processors; orphaned file rows are excluded from every
// record and counted (the single orphan policy).

import (
	"context"

	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/pipeline"
)

// windowSource adapts original.Source's keyset windows to pipeline.Source.
// Not safe for concurrent use; pipeline.Run calls Next from its feed
// goroutine only.
type windowSource struct {
	src     *original.Source
	afterID int64
	window  int
	limits  original.WindowLimits

	// observedOrphans accumulates per-window orphan file-row counts; the
	// runner compares the total with the fingerprint at the end.
	observedOrphans int64
}

// newWindowSource starts keyset iteration strictly above afterID. The
// cursor stays int64 end to end: it is initialized to source_min_id-1, which
// underflows int32 when the minimum source ID is MinInt32.
func newWindowSource(src *original.Source, afterID int64, window int, limits original.WindowLimits) *windowSource {
	return &windowSource{src: src, afterID: afterID, window: window, limits: limits}
}

// Next reads the next keyset window and converts it to jobs in strictly
// increasing source-ID order. Windows truncated by the byte budget simply
// end early; the next call continues from the last converted ID, so no
// source row is skipped — the budget only bounds read-side buffering.
func (w *windowSource) Next(ctx context.Context) ([]pipeline.Job, bool, error) {
	win, err := w.src.Window(ctx, w.afterID, w.window, w.limits)
	if err != nil {
		return nil, false, err
	}
	if len(win.Torrents) == 0 {
		return nil, false, nil
	}
	w.afterID = int64(win.Torrents[len(win.Torrents)-1].ID)
	w.observedOrphans += win.OrphanFiles

	jobs := make([]pipeline.Job, 0, len(win.Torrents))
	for _, t := range win.Torrents {
		switch {
		case t.ID <= 0:
			// Defensive: v0.12.0 IDs are positive INTEGERs; anything else is
			// a read-stage integrity failure for this record alone.
			jobs = append(jobs, pipeline.Job{Failure: &pipeline.Failure{
				SourceID: int64(t.ID),
				InfoHash: t.InfoHash,
				Stage:    pipeline.StageRead,
				Code:     pipeline.CodeSourceIntegrity,
				Details:  map[string]any{"reason": "source ID out of the supported positive range"},
			}})
		case w.overflowed(win, t.ID):
			ov := win.Overflow[t.ID]
			code, limitName, allowed := pipeline.CodeFileCountExceeded, original.OverflowFileCount, int64(w.limits.MaxFiles)
			if ov.Reason == original.OverflowPathBytes {
				// The codec rejects over-cap aggregates itself; the read
				// stage already knows the outcome, so report it directly.
				code, limitName, allowed = pipeline.CodeCodecLimit, original.OverflowPathBytes, int64(w.limits.MaxPathBytes)
			}
			jobs = append(jobs, pipeline.Job{Failure: &pipeline.Failure{
				SourceID: int64(t.ID),
				InfoHash: t.InfoHash,
				Stage:    pipeline.StageRead,
				Code:     code,
				Details: map[string]any{
					"limit":   limitName,
					"actual":  ov.Observed,
					"allowed": allowed,
				},
			}})
		case len(win.Files[t.ID]) == 0:
			jobs = append(jobs, pipeline.Job{Failure: &pipeline.Failure{
				SourceID: int64(t.ID),
				InfoHash: t.InfoHash,
				Stage:    pipeline.StageRead,
				Code:     pipeline.CodeMissingFiles,
			}})
		default:
			rows := win.Files[t.ID]
			rec := &pipeline.Record{
				SourceID:     t.ID,
				InfoHash:     t.InfoHash,
				Name:         t.Name,
				TotalSize:    t.TotalSize,
				DiscoveredOn: t.DiscoveredOn,
				Files:        make([]pipeline.FileEntry, len(rows)),
			}
			for i, f := range rows {
				rec.Files[i] = pipeline.FileEntry{Path: f.Path, Size: f.Size, SourceFileID: f.ID}
			}
			jobs = append(jobs, pipeline.Job{Record: rec})
		}
	}
	return jobs, true, nil
}

func (w *windowSource) overflowed(win *original.Window, id int32) bool {
	_, has := win.Overflow[id]
	return has
}
