package crawler

import (
	"context"
	"errors"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/crawlqueue"
	"github.com/skobkin/magnetico-web/internal/ingest/record"
	"github.com/skobkin/magnetico-web/metadata"
)

// AppendQueue is the part of crawlqueue.Queue the enqueue writer uses.
type AppendQueue interface {
	Append(ctx context.Context, infoHash [20]byte, format int, payload []byte) (bool, error)
	AppendQuarantined(ctx context.Context, infoHash [20]byte, format int, payload []byte, reason string) error
}

// EnqueueGrace bounds how long the writer keeps committing buffered results
// after shutdown begins (spec §8).
const EnqueueGrace = 5 * time.Second

// Enqueuer turns retrieved metadata into durable queue rows.
type Enqueuer struct {
	Queue    AppendQueue
	Limits   codec.Limits
	Counters *Counters
	// OnInvalid is called for records rejected at the conversion boundary.
	OnInvalid func(err error)
	Logf      func(format string, args ...any)
	// OnAppended is called after a row was committed (wakes the sender).
	OnAppended func()

	lastFull time.Time
}

// Run consumes drain until it is closed. When the grace context ends
// (shutdown), buffered results that cannot be committed any more are dropped
// and counted; nothing blocks.
func (e *Enqueuer) Run(drain <-chan metadata.Metadata, graceCtx context.Context) {
	for md := range drain {
		if graceCtx.Err() != nil {
			e.Counters.DroppedShutdown.Add(1)
			continue
		}
		e.handle(graceCtx, md)
	}
}

func (e *Enqueuer) handle(ctx context.Context, md metadata.Metadata) {
	rec, err := record.FromMetadata(&md)
	if err != nil {
		e.Counters.EnqueueInvalid.Add(1)
		if e.OnInvalid != nil {
			e.OnInvalid(err)
		}
		return
	}
	data, err := record.Encode(rec, e.Limits)
	if errors.Is(err, record.ErrRecordTooLarge) {
		// Kept intact in local quarantine: never sent, never truncated.
		if qerr := e.Queue.AppendQuarantined(ctx, rec.InfoHash, record.Version, data, "record_too_large"); qerr != nil {
			e.dropped(ctx, qerr)
			return
		}
		e.Counters.EnqueueTooLarge.Add(1)
		return
	}
	if err != nil {
		e.Counters.EnqueueInvalid.Add(1)
		if e.OnInvalid != nil {
			e.OnInvalid(err)
		}
		return
	}
	appended, err := e.Queue.Append(ctx, rec.InfoHash, record.Version, data)
	if err != nil {
		e.dropped(ctx, err)
		return
	}
	if !appended {
		e.Counters.EnqueueDuplicate.Add(1)
		return
	}
	e.Counters.EnqueueAppended.Add(1)
	if e.OnAppended != nil {
		e.OnAppended()
	}
}

// dropped counts a result that could not be queued.
func (e *Enqueuer) dropped(ctx context.Context, err error) {
	switch {
	case errors.Is(err, crawlqueue.ErrQueueFull):
		e.Counters.DroppedQueueFull.Add(1)
		if e.Logf != nil && time.Since(e.lastFull) >= time.Minute {
			e.lastFull = time.Now()
			e.Logf("enqueue: the local queue is full; new metadata is dropped until the sender frees space (counted in crawl_dropped_queue_full)")
		}
	case errors.Is(err, crawlqueue.ErrClosed), ctx.Err() != nil:
		e.Counters.DroppedShutdown.Add(1)
	default:
		e.Counters.EnqueueErrors.Add(1)
		if e.Logf != nil && time.Since(e.lastFull) >= time.Minute {
			e.lastFull = time.Now()
			e.Logf("enqueue: writing to the local queue failed: %v", err)
		}
	}
}
