package crawler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/dht"
	"github.com/skobkin/magnetico-web/internal/crawlqueue"
	"github.com/skobkin/magnetico-web/metadata"
)

// Shutdown grace periods (spec §8).
const (
	SenderGrace = 30 * time.Second
	// QueueCloseGrace bounds the final wait for the SQLite writer.
	QueueCloseGrace = 10 * time.Second
	// SummaryInterval is the period of the counter summary log line.
	SummaryInterval = 5 * time.Minute
)

// ErrShutdownIncomplete is returned by Stop when the queue writer could not
// be stopped within its grace period. Committed records are safe; the caller
// must exit the process (releasing the queue lock) and SQLite recovers at the
// next start.
var ErrShutdownIncomplete = errors.New("crawler: the local queue writer did not stop in time; exit the process and let SQLite recover on the next start")

// QueueStore is the queue surface the pipeline needs; *crawlqueue.Queue
// satisfies it.
type QueueStore interface {
	SendQueue
	AppendQueue
	Stats() crawlqueue.Stats
	CloseWithin(grace time.Duration) error
	Failed() <-chan struct{}
	Err() error
}

// queueGate opens while the local queue has room: queue + quarantine payload
// bytes must stay below the limit minus a reserve for results already in
// flight (so a full queue stops new leeches instead of failing late).
type queueGate struct {
	q interface{ Stats() crawlqueue.Stats }
}

func (g queueGate) Open() bool {
	s := g.q.Stats()
	if s.MaxBytes <= 0 {
		return true
	}
	reserve := s.MaxBytes / 10
	if reserve > 32<<20 {
		reserve = 32 << 20
	}
	return s.QueueBytes+s.QuarantineBytes+reserve <= s.MaxBytes
}

// Config configures a Pipeline.
type Config struct {
	Queue     QueueStore
	Client    *Client
	Sink      *metadata.Sink
	Limits    codec.Limits
	Logf      func(format string, args ...any)
	OnInvalid func(err error)

	// Overrides for tests.
	SenderGrace     time.Duration
	QueueCloseGrace time.Duration
	EnqueueGrace    time.Duration
}

// Pipeline owns the goroutines between the DHT event loop and ingest.
type Pipeline struct {
	cfg      Config
	Counters Counters

	pre    *Prechecker
	enq    *Enqueuer
	sender *Sender

	preCancel, sendCancel context.CancelFunc
	enqGraceCtx           context.Context
	enqGraceCancel        context.CancelFunc
	wg                    sync.WaitGroup
	enqDone, sendDone     chan struct{}
	stopOnce              sync.Once
	stopErr               error
}

// New wires the pipeline; call Start to run it.
func New(cfg Config) *Pipeline {
	if cfg.SenderGrace <= 0 {
		cfg.SenderGrace = SenderGrace
	}
	if cfg.QueueCloseGrace <= 0 {
		cfg.QueueCloseGrace = QueueCloseGrace
	}
	if cfg.EnqueueGrace <= 0 {
		cfg.EnqueueGrace = EnqueueGrace
	}
	p := &Pipeline{cfg: cfg, enqDone: make(chan struct{}), sendDone: make(chan struct{})}
	p.pre = NewPrechecker(cfg.Client, cfg.Sink, queueGate{cfg.Queue}, &p.Counters, cfg.Logf)
	p.sender = NewSender(cfg.Queue, cfg.Client, &p.Counters, cfg.Logf)
	p.enq = &Enqueuer{
		Queue: cfg.Queue, Limits: cfg.Limits, Counters: &p.Counters, OnInvalid: cfg.OnInvalid, Logf: cfg.Logf,
		OnAppended: p.sender.Wake,
	}
	return p
}

// Offer is called by the DHT event loop; it never blocks.
func (p *Pipeline) Offer(res dht.Result) bool { return p.pre.Offer(res) }

// Start launches the precheck, enqueue and sender goroutines.
func (p *Pipeline) Start() {
	var preCtx, sendCtx context.Context
	preCtx, p.preCancel = context.WithCancel(context.Background())
	sendCtx, p.sendCancel = context.WithCancel(context.Background())
	p.enqGraceCtx, p.enqGraceCancel = context.WithCancel(context.Background())

	p.wg.Add(3)
	go func() { defer p.wg.Done(); p.pre.Run(preCtx) }()
	go func() {
		defer p.wg.Done()
		defer close(p.enqDone)
		p.enq.Run(p.cfg.Sink.Drain(), p.enqGraceCtx)
	}()
	go func() {
		defer p.wg.Done()
		defer close(p.sendDone)
		p.sender.Run(sendCtx, p.cfg.SenderGrace)
	}()
}

// Stop shuts the pipeline down in the documented order (the DHT must already
// be stopped by the caller): stop the precheck → terminate the sink (leeches
// are cancelled; the drain closes after they return) → let the enqueue writer
// commit what is left within its grace → let the sender finish its current
// request within its grace → close the queue with a bounded wait. It returns
// ErrShutdownIncomplete when the SQLite writer could not be stopped; the
// queue handles are then deliberately left open (see crawlqueue.CloseWithin).
func (p *Pipeline) Stop() error {
	p.stopOnce.Do(func() {
		p.preCancel()

		// The enqueue writer keeps reading the drain while leeches finish;
		// after its grace it drops (and counts) the rest.
		graceTimer := time.AfterFunc(p.cfg.EnqueueGrace, p.enqGraceCancel)
		p.cfg.Sink.Terminate()
		<-p.enqDone
		graceTimer.Stop()
		p.enqGraceCancel()
		p.Counters.DroppedShutdown.Add(p.cfg.Sink.DroppedOnShutdown())

		p.sendCancel()
		<-p.sendDone
		p.wg.Wait()

		if err := p.cfg.Queue.CloseWithin(p.cfg.QueueCloseGrace); err != nil {
			if errors.Is(err, crawlqueue.ErrCloseTimeout) {
				p.stopErr = ErrShutdownIncomplete
			} else {
				p.stopErr = err
			}
		}
	})
	return p.stopErr
}

// Summary renders the counters and queue state for the periodic log line.
func (p *Pipeline) Summary() string {
	c, s := &p.Counters, p.cfg.Queue.Stats()
	return fmt.Sprintf(
		"crawl summary: precheck requests=%d existing=%d unknown=%d errors=%d dropped=%d | leech started=%d joined=%d busy=%d gate=%d active=%d | "+
			"enqueue appended=%d duplicate=%d invalid=%d too_large=%d crawl_dropped_queue_full=%d crawl_dropped_shutdown=%d errors=%d | "+
			"send batches=%d acked=%d quarantined=%d retryable=%d ambiguous=%d unauthorized=%d crawl_send_unrecognised_rejections=%d guard_trips=%d bisections=%d | "+
			"queue records=%d bytes=%d quarantine records=%d bytes=%d max_bytes=%d",
		c.PrecheckRequests.Load(), c.PrecheckExisting.Load(), c.PrecheckUnknown.Load(), c.PrecheckErrors.Load(), c.PrecheckDropped.Load(),
		c.LeechStarted.Load(), c.LeechJoined.Load(), c.LeechBusy.Load(), c.LeechGate.Load(), p.cfg.Sink.Active(),
		c.EnqueueAppended.Load(), c.EnqueueDuplicate.Load(), c.EnqueueInvalid.Load(), c.EnqueueTooLarge.Load(),
		c.DroppedQueueFull.Load(), c.DroppedShutdown.Load(), c.EnqueueErrors.Load(),
		c.SendBatches.Load(), c.SendAcked.Load(), c.SendQuarantined.Load(), c.SendRetryable.Load(), c.SendAmbiguous.Load(),
		c.SendUnauthorized.Load(), c.SendUnrecognised.Load(), c.SendGuardTrips.Load(), c.SendBisections.Load(),
		s.QueueRecords, s.QueueBytes, s.QuarantineRecords, s.QuarantineBytes, s.MaxBytes)
}
