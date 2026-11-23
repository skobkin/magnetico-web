package mail

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Queue sizing (docs/wip/07G plan, amendment A2). Capacity bounds queued plus
// in-flight jobs; with two workers and a 30 s send bound the worst-case wait of
// the last job is 32/2 × 30 s = 8 minutes. A worker drops any job older than
// DefaultMaxAge without sending it, so a link issued before queueing can never
// be delivered long after it was meant to be used: the shortest link lifetime
// is one hour (reset).
const (
	DefaultCapacity = 32
	DefaultWorkers  = 2
	DefaultMaxAge   = 15 * time.Minute
)

// Queue errors.
var (
	// ErrQueueFull: every slot is taken. Nothing was queued.
	ErrQueueFull = errors.New("mail: queue full")
	// ErrQueueClosed: the queue no longer accepts work (shutdown).
	ErrQueueClosed = errors.New("mail: queue closed")
)

// Mailer delivers one message.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// Job is one unit of queued work. It runs on a worker and sends through send;
// it may touch the database first (the forgot-password flow issues its link
// here, so the HTTP request does the same work for every address). A Job must
// not log addresses or links.
type Job func(ctx context.Context, send func(Message) error)

type queued struct {
	job Job
	at  time.Time
}

// Options tunes a Queue; zero values use the defaults.
type Options struct {
	Capacity int
	Workers  int
	MaxAge   time.Duration
	Now      func() time.Time
}

// Stats are the delivery counters.
type Stats struct {
	Sent, Failed, Expired, Rejected, Panicked int64
}

// Queue is a bounded, in-process, non-durable delivery queue. Nothing is
// persisted: jobs that are still queued at shutdown are counted and dropped.
type Queue struct {
	mailer  Mailer
	log     *slog.Logger
	workers int
	maxAge  time.Duration
	now     func() time.Time

	sem  chan struct{} // one token per reserved slot (queued or running)
	jobs chan queued   // capacity == cap(sem), so Submit never blocks

	mu      sync.Mutex
	closed  bool
	pending atomic.Int64 // reserved and not yet finished or released

	sent, failed, expired, rejected, panicked atomic.Int64
}

// NewQueue returns a queue over m. Start its workers with Run.
func NewQueue(m Mailer, log *slog.Logger, o Options) *Queue {
	if o.Capacity <= 0 {
		o.Capacity = DefaultCapacity
	}
	if o.Workers <= 0 {
		o.Workers = DefaultWorkers
	}
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultMaxAge
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Queue{
		mailer: m, log: log, workers: o.Workers, maxAge: o.MaxAge, now: o.Now,
		sem:  make(chan struct{}, o.Capacity),
		jobs: make(chan queued, o.Capacity),
	}
}

// Slot is a reserved place in the queue. Exactly one of Submit and Release
// must be called.
type Slot struct {
	q    *Queue
	done atomic.Bool
}

// Reserve takes a slot without blocking, so a handler learns that the queue is
// full or closed BEFORE it writes anything (for example issues a link).
func (q *Queue) Reserve() (*Slot, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		q.rejected.Add(1)
		return nil, ErrQueueClosed
	}
	select {
	case q.sem <- struct{}{}:
		q.pending.Add(1)
		return &Slot{q: q}, nil
	default:
		q.rejected.Add(1)
		return nil, ErrQueueFull
	}
}

// Submit hands the job to the workers. It never blocks.
func (s *Slot) Submit(j Job) {
	if !s.done.CompareAndSwap(false, true) {
		return
	}
	s.q.jobs <- queued{job: j, at: s.q.now()}
}

// Release gives the slot back unused.
func (s *Slot) Release() {
	if s.done.CompareAndSwap(false, true) {
		s.q.free()
	}
}

func (q *Queue) free() {
	<-q.sem
	q.pending.Add(-1)
}

// Close stops accepting new reservations. Jobs already submitted still run
// until Drain gives up or the workers' context ends.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
}

// Pending is the number of reserved slots not yet finished.
func (q *Queue) Pending() int { return int(q.pending.Load()) }

// Stats returns the counters.
func (q *Queue) Stats() Stats {
	return Stats{Sent: q.sent.Load(), Failed: q.failed.Load(), Expired: q.expired.Load(),
		Rejected: q.rejected.Load(), Panicked: q.panicked.Load()}
}

// Drain closes the queue and waits until every reserved slot is finished or ctx
// ends; it returns the number of jobs that were abandoned (0 on a clean drain).
func (q *Queue) Drain(ctx context.Context) int {
	q.Close()
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for q.pending.Load() > 0 {
		select {
		case <-ctx.Done():
			n := int(q.pending.Load())
			q.log.Warn("mail queue abandoned undelivered jobs at shutdown", "jobs", n)
			return n
		case <-t.C:
		}
	}
	return 0
}

// Run starts the workers and blocks until ctx is cancelled and they return. It
// is a server background task.
func (q *Queue) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < q.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case j := <-q.jobs:
					q.run(ctx, j)
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	wg.Wait()
}

func (q *Queue) run(ctx context.Context, j queued) {
	defer q.free()
	defer func() {
		if recover() != nil {
			q.panicked.Add(1)
			q.log.Error("mail job panicked")
		}
	}()
	if age := q.now().Sub(j.at); age > q.maxAge {
		q.expired.Add(1)
		q.log.Warn("mail job expired in the queue and was dropped")
		return
	}
	j.job(ctx, func(m Message) error {
		err := q.mailer.Send(ctx, m)
		if err != nil {
			q.failed.Add(1)
			q.log.Warn("mail delivery failed", "category", Category(err))
			return err
		}
		q.sent.Add(1)
		return nil
	})
}
