package crawler

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/skobkin/magnetico-web/dht"
	"github.com/skobkin/magnetico-web/metadata"
)

// Precheck parameters.
const (
	PrecheckBuffer   = 4096        // fixed capacity of the DHT hand-off channel
	PrecheckBatch    = 1024        // results per exists request: the server maximum (1,024 hashes, ≈ 45 KiB of JSON)
	PrecheckLinger   = time.Second // how long the first result of a batch waits for company
	precheckPauseMin = time.Second
	precheckPauseMax = time.Minute
)

// Exister is the existence-lookup seam (*Client).
type Exister interface {
	Exists(ctx context.Context, hashes [][20]byte) (ExistsResult, error)
}

// LeechSink is the metadata-retrieval seam (*metadata.Sink).
type LeechSink interface {
	TrySink(res dht.Result) metadata.SinkStatus
}

// Gate reports whether new metadata may be admitted (local queue has room).
type Gate interface{ Open() bool }

// Prechecker sits between the DHT event loop and the leeches. The event loop
// only calls Offer, which never blocks: a full buffer drops the result.
// Existing hashes are skipped; missing and unknown ones are admitted
// (bounded fail-open: the local queue limit, the leech limit and the gate
// bound the intake when ingest is unavailable).
type Prechecker struct {
	Exist    Exister
	Sink     LeechSink
	Gate     Gate
	Counters *Counters
	Logf     func(format string, args ...any)

	in chan dht.Result

	// Linger overrides PrecheckLinger (tests).
	Linger time.Duration

	// Test seams.
	Now    func() time.Time
	Jitter func(max time.Duration) time.Duration

	pausedUntil time.Time
	failures    int
	lastLog     time.Time
}

// NewPrechecker builds a prechecker with the fixed-capacity input channel.
func NewPrechecker(e Exister, sink LeechSink, gate Gate, c *Counters, logf func(string, ...any)) *Prechecker {
	return &Prechecker{Exist: e, Sink: sink, Gate: gate, Counters: c, Logf: logf, in: make(chan dht.Result, PrecheckBuffer)}
}

// Offer hands a DHT result over without blocking; false means it was dropped.
func (p *Prechecker) Offer(res dht.Result) bool {
	select {
	case p.in <- res:
		return true
	default:
		p.Counters.PrecheckDropped.Add(1)
		return false
	}
}

func (p *Prechecker) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Prechecker) jitter(max time.Duration) time.Duration {
	if p.Jitter != nil {
		return p.Jitter(max)
	}
	return time.Duration(rand.Int64N(int64(max) + 1))
}

// Run processes results until ctx ends. Unprocessed buffered results are
// discarded (best-effort discovery data).
func (p *Prechecker) Run(ctx context.Context) {
	for {
		var first dht.Result
		select {
		case <-ctx.Done():
			return
		case first = <-p.in:
		}
		batch := []dht.Result{first}
		// The linger is a monotonic in-process timer started at the first result:
		// no wall clock is involved, so a clock step cannot lengthen it.
		linger := time.NewTimer(p.lingerFor())
	gather:
		for len(batch) < PrecheckBatch {
			select {
			case r := <-p.in:
				batch = append(batch, r)
			case <-linger.C:
				break gather
			case <-ctx.Done():
				linger.Stop()
				return
			}
		}
		linger.Stop()
		p.process(ctx, batch)
	}
}

func (p *Prechecker) lingerFor() time.Duration {
	if p.Linger > 0 {
		return p.Linger
	}
	return PrecheckLinger
}

// process classifies one batch and admits what is not already stored.
func (p *Prechecker) process(ctx context.Context, batch []dht.Result) {
	// Group results by hash: peers of the same hash all reach TrySink (it
	// merges them into one leech).
	byHash := make(map[[20]byte][]dht.Result, len(batch))
	order := make([][20]byte, 0, len(batch))
	for _, r := range batch {
		h := r.InfoHash()
		if _, ok := byHash[h]; !ok {
			order = append(order, h)
		}
		byHash[h] = append(byHash[h], r)
	}

	skip := map[[20]byte]bool{}
	if p.now().After(p.pausedUntil) {
		p.Counters.PrecheckRequests.Add(1)
		res, err := p.Exist.Exists(ctx, order)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.fail()
		} else {
			p.failures = 0
			p.Counters.PrecheckExisting.Add(uint64(len(res.Exists)))
			p.Counters.PrecheckUnknown.Add(uint64(len(res.Unknown)))
			for _, h := range res.Exists {
				skip[h] = true
			}
		}
	} else {
		p.Counters.PrecheckUnknown.Add(uint64(len(order)))
	}

	for _, h := range order {
		if skip[h] {
			continue
		}
		for _, r := range byHash[h] {
			if !p.Gate.Open() {
				p.Counters.LeechGate.Add(1)
				break
			}
			switch p.Sink.TrySink(r) {
			case metadata.SinkStarted:
				p.Counters.LeechStarted.Add(1)
			case metadata.SinkJoined:
				p.Counters.LeechJoined.Add(1)
			case metadata.SinkBusy:
				p.Counters.LeechBusy.Add(1)
			}
		}
	}
}

// fail records an unusable lookup: lookups pause for a jittered exponential
// window while intake continues fail-open.
func (p *Prechecker) fail() {
	p.Counters.PrecheckErrors.Add(1)
	p.failures++
	d := precheckPauseMin << min(p.failures-1, 6)
	if d > precheckPauseMax {
		d = precheckPauseMax
	}
	p.pausedUntil = p.now().Add(p.jitter(d))
	if p.Logf != nil && p.now().Sub(p.lastLog) >= time.Minute {
		p.lastLog = p.now()
		p.Logf("precheck: the ingest existence lookup failed; continuing without it (fail-open, bounded by the local queue limit) and retrying with backoff")
	}
}
