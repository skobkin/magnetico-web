package crawler

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/skobkin/magnetico-web/internal/crawlqueue"
)

// Sender limits and timings (spec §5.4).
const (
	SendMaxRecords = 256 // hard protocol limit, not a batching target
	SendMaxBytes   = 8 << 20
	// SendLinger is how long the oldest queued record waits for company before
	// a partial batch is sent. A batch that cannot grow (the record cap, the next
	// record would not fit, an oversized head) is sent at once.
	SendLinger        = time.Second
	UnrecognisedPause = 60 * time.Second
	UnauthorizedPause = 60 * time.Second
	BackoffMin        = time.Second
	BackoffMax        = 60 * time.Second
	// GuardLimit is the number of consecutive recognised-record quarantines
	// without an intervening 202 after which quarantining stops.
	GuardLimit    = 8
	diagInterval  = time.Minute
	idlePollEvery = time.Second
)

// SendQueue is the part of crawlqueue.Queue the sender uses.
type SendQueue interface {
	PlanBatch(ctx context.Context, maxRecords int, maxBytes int64) (crawlqueue.Plan, error)
	Fetch(ctx context.Context, p crawlqueue.Plan) ([]crawlqueue.Item, error)
	Ack(ctx context.Context, ids []int64) error
	Quarantine(ctx context.Context, ids []int64, reason string, httpStatus int) error
}

// Submitter is the HTTP client seam.
type Submitter interface {
	Submit(ctx context.Context, payloads [][]byte) SubmitResult
}

// Sender delivers the oldest queued records to ingest, deterministically
// (spec §5.4): 202 acknowledges, rejected multi-record requests are bisected,
// recognised record-specific rejections quarantine the single record, and
// everything else keeps the rows and backs off.
type Sender struct {
	Queue    SendQueue
	Client   Submitter
	Counters *Counters
	Logf     func(format string, args ...any)

	// Linger overrides SendLinger (tests).
	Linger time.Duration

	// Test seams.
	Sleep  func(ctx context.Context, d time.Duration) bool
	Jitter func(max time.Duration) time.Duration
	Now    func() time.Time // wall clock, for the age of the oldest record

	wake chan struct{}

	mu          sync.Mutex
	lastDiag    map[string]time.Time
	consecutive int // consecutive recognised-record quarantines without a 202
}

// NewSender fills the defaults.
func NewSender(q SendQueue, c Submitter, counters *Counters, logf func(string, ...any)) *Sender {
	return &Sender{Queue: q, Client: c, Counters: counters, Logf: logf, wake: make(chan struct{}, 1),
		lastDiag: map[string]time.Time{}}
}

// Wake nudges an idle sender after an append.
func (s *Sender) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Sender) sleep(ctx context.Context, d time.Duration) bool {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (s *Sender) jitter(max time.Duration) time.Duration {
	if s.Jitter != nil {
		return s.Jitter(max)
	}
	return time.Duration(rand.Int64N(int64(max) + 1))
}

// throttledLog emits at most one line per key per minute.
func (s *Sender) throttledLog(key, format string, args ...any) {
	s.mu.Lock()
	now := time.Now()
	if t, ok := s.lastDiag[key]; ok && now.Sub(t) < diagInterval {
		s.mu.Unlock()
		return
	}
	s.lastDiag[key] = now
	s.mu.Unlock()
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

type outcome int

const (
	outcomeProgress outcome = iota // something was settled; continue at once
	outcomeBackoff                 // retryable failure: jittered exponential backoff
	outcomePause60                 // unrecognised rejection / guard: 60 s
	outcomeAuth                    // 401/403: long backoff
	outcomeStop                    // queue failure or context ended
)

// Run delivers until ctx ends. The grace period bounds how long the request
// in flight (and the Ack/Quarantine that settles it) may continue after ctx
// is cancelled; records are never lost either way (the queue keeps them until
// acknowledged, and redelivery is idempotent).
func (s *Sender) Run(ctx context.Context, grace time.Duration) {
	reqCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
		case <-finished:
			return
		}
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-t.C:
			cancel()
		case <-finished:
		}
	}()

	failures := 0
	var linger lingerState
	for {
		if ctx.Err() != nil {
			// Shutdown cuts a pending linger short: one last dispatch within the
			// existing grace period; the rows stay queued if it fails.
			s.finalDispatch(reqCtx)
			return
		}
		plan, err := s.Queue.PlanBatch(reqCtx, SendMaxRecords, SendMaxBytes)
		if err != nil {
			if ctx.Err() == nil {
				s.throttledLog("peek", "send: cannot read the local queue: %v", err)
				if !s.sleep(ctx, time.Second) {
					return
				}
			}
			continue
		}
		if plan.Count == 0 {
			linger.reset()
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-time.After(idlePollEvery):
			}
			continue
		}
		// A batch that can still grow waits for its linger, counted from the
		// oldest record; a wake-up (more records) re-evaluates it, so a batch
		// that just filled up goes at once.
		dispatch, wait := linger.decide(plan, s.lingerFor(), s.wallNow(), time.Now())
		if !dispatch {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-s.wake:
			case <-ctx.Done():
			}
			t.Stop()
			continue
		}
		items, err := s.Queue.Fetch(reqCtx, plan)
		if err != nil {
			if ctx.Err() == nil {
				s.throttledLog("peek", "send: cannot read the local queue: %v", err)
				if !s.sleep(ctx, time.Second) {
					return
				}
			}
			continue
		}

		switch s.deliver(reqCtx, items) {
		case outcomeProgress:
			failures = 0
		case outcomeBackoff:
			failures++
			d := BackoffMin << min(failures-1, 6)
			if d > BackoffMax {
				d = BackoffMax
			}
			if !s.sleep(ctx, s.jitter(d)) {
				return
			}
		case outcomePause60:
			if !s.sleep(ctx, UnrecognisedPause) {
				return
			}
		case outcomeAuth:
			if !s.sleep(ctx, UnauthorizedPause) {
				return
			}
		case outcomeStop:
			return
		}
	}
}

// finalDispatch sends at most one batch of what is still queued, ignoring the
// outcome: a failure leaves the rows queued for the next start.
func (s *Sender) finalDispatch(reqCtx context.Context) {
	plan, err := s.Queue.PlanBatch(reqCtx, SendMaxRecords, SendMaxBytes)
	if err != nil || plan.Count == 0 {
		return
	}
	if items, err := s.Queue.Fetch(reqCtx, plan); err == nil {
		s.deliver(reqCtx, items)
	}
}

func (s *Sender) lingerFor() time.Duration {
	if s.Linger > 0 {
		return s.Linger
	}
	return SendLinger
}

func (s *Sender) wallNow() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// lingerState decides when a batch that can still grow is sent. The linger of
// the oldest queued record is armed once per head record on the monotonic
// clock, so no wall-clock adjustment (EnqueuedAt is persisted wall time) can
// make it wait longer than the linger.
type lingerState struct {
	set      bool
	headID   int64
	deadline time.Time // monotonic
}

func (l *lingerState) reset() { l.set = false }

// decide reports whether to dispatch the planned batch now; otherwise how long
// to wait before looking again. wall is the wall clock (for the age of the
// head), mono a time with a monotonic reading (for the deadline).
func (l *lingerState) decide(p crawlqueue.Plan, linger time.Duration, wall, mono time.Time) (dispatch bool, wait time.Duration) {
	if p.Count == 0 {
		l.reset()
		return false, 0
	}
	if p.Stop != crawlqueue.StopNone {
		return true, 0 // full, or the next record cannot join, or an oversized singleton
	}
	if !l.set || l.headID != p.HeadID {
		age := min(max(wall.Sub(p.HeadEnqueuedAt), 0), linger) // a clock step cannot make it longer than the linger
		l.headID, l.set, l.deadline = p.HeadID, true, mono.Add(linger-age)
	}
	if rem := l.deadline.Sub(mono); rem > 0 {
		return false, rem
	}
	return true, 0
}

func payloads(items []crawlqueue.Item) [][]byte {
	out := make([][]byte, len(items))
	for i := range items {
		out[i] = items[i].Payload
	}
	return out
}

func ids(items []crawlqueue.Item) []int64 {
	out := make([]int64, len(items))
	for i := range items {
		out[i] = items[i].ID
	}
	return out
}

// deliver sends items as one request and, when a multi-record request is
// rejected, bisects it. It returns how the caller should proceed.
func (s *Sender) deliver(ctx context.Context, items []crawlqueue.Item) outcome {
	s.Counters.SendBatches.Add(1)
	res := s.Client.Submit(ctx, payloads(items))

	switch res.Kind {
	case SubmitAccepted:
		if err := s.Queue.Ack(ctx, ids(items)); err != nil {
			s.throttledLog("ack", "send: acknowledging %d delivered records failed (they will be resent, which is idempotent): %v", len(items), err)
			return outcomeStop
		}
		s.mu.Lock()
		s.consecutive = 0
		s.mu.Unlock()
		s.Counters.SendAcked.Add(uint64(len(items)))
		return outcomeProgress

	case SubmitUnauthorized:
		s.Counters.SendUnauthorized.Add(1)
		s.throttledLog("auth", "send: ingest answered HTTP %d: check the ingest token on both sides; records stay queued", res.Status)
		return outcomeAuth

	case SubmitRetryable:
		s.Counters.SendRetryable.Add(1)
		if ctx.Err() == nil {
			s.throttledLog("retryable", "send: ingest unavailable (status %d); records stay queued, retrying with backoff", res.Status)
		}
		return outcomeBackoff

	case SubmitAmbiguous:
		s.Counters.SendAmbiguous.Add(1)
		s.throttledLog("ambiguous", "send: unexpected HTTP status %d; not acknowledged, records stay queued", res.Status)
		return outcomeBackoff

	case SubmitRecordRejected:
		return s.quarantineSingle(ctx, items[0], res)

	default: // SubmitRejected
		if len(items) > 1 {
			return s.bisect(ctx, items)
		}
		return s.unrecognised(res)
	}
}

// bisect resubmits each half as its own request. A retryable result anywhere
// aborts the bisect; already settled halves stay settled.
func (s *Sender) bisect(ctx context.Context, items []crawlqueue.Item) outcome {
	s.Counters.SendBisections.Add(1)
	mid := len(items) / 2
	for _, half := range [][]crawlqueue.Item{items[:mid], items[mid:]} {
		switch o := s.deliver(ctx, half); o {
		case outcomeProgress:
		default:
			return o
		}
	}
	return outcomeProgress
}

func (s *Sender) quarantineSingle(ctx context.Context, it crawlqueue.Item, res SubmitResult) outcome {
	s.mu.Lock()
	tripped := s.consecutive >= GuardLimit
	s.mu.Unlock()
	if tripped {
		s.Counters.SendGuardTrips.Add(1)
		s.throttledLog("guard", "send: %d consecutive records were rejected as invalid without a single acceptance; "+
			"quarantining is suspended and the records are kept: the server is likely wrong (version or limit mismatch) — check the ingest deployment", GuardLimit)
		return outcomePause60
	}
	if err := s.Queue.Quarantine(ctx, []int64{it.ID}, res.Code, res.Status); err != nil {
		s.throttledLog("quarantine", "send: moving a rejected record to quarantine failed: %v", err)
		return outcomeStop
	}
	s.mu.Lock()
	s.consecutive++
	s.mu.Unlock()
	s.Counters.SendQuarantined.Add(1)
	s.throttledLog("quarantined", "send: ingest rejected a record as %s (HTTP %d); it was moved to the local quarantine", res.Code, res.Status)
	return outcomeProgress
}

// unrecognised handles a single-record 400/413 that is not a recognised
// record-specific error: the record is kept and an actionable diagnostic is
// emitted (at most once a minute).
func (s *Sender) unrecognised(res SubmitResult) outcome {
	s.Counters.SendUnrecognised.Add(1)
	s.throttledLog("unrecognised", "send: a rejection that does not identify the record was received "+
		"(HTTP %d, content-type %q, ingest envelope parsed: %t, code %q, records in request: %d, body: %q); "+
		"the record is kept. The server or a proxy in front of it rejected a request the crawler believes is valid: "+
		"check the ingest version, client_max_body_size-style proxy limits and the TLS terminator",
		res.Status, res.ContentType, res.EnvelopeOK, res.Code, res.Records, res.BodySample)
	return outcomePause60
}
