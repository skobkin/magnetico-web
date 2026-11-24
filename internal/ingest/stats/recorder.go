// Package stats is the ingest-side collection of operational statistics: an
// in-memory Recorder updated on the request and consumer paths (no I/O, no
// allocation on the hot path), and the writer that persists it periodically
// (writer.go). Nothing here can change an admission decision, a response or
// the durability of the queue.
package stats

import (
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/skobkin/magnetico-web/internal/opstats"
)

// NoReject marks a refused admission that produced no response (the client
// went away): its records are counted as refused but no request was rejected.
const NoReject opstats.RejectClass = -1

type hourAcc struct {
	counters [opstats.NumCounters]int64
	coverage int64
}

// Recorder accumulates events. Every method is safe for concurrent use and a
// no-op on a nil receiver (statistics disabled).
//
// One mutex guards everything. A critical section updates plain fields only:
// no I/O, no blocking call, no allocation beyond a bounded map insert. The
// mutex is never held across admission I/O: a handler records the validated
// request, releases, admits, then records the outcome. Drain swaps the
// accumulators under the same mutex, so an event is wholly in the drained
// delta or wholly in the next one.
//
// The UTC hour of an event is taken inside the critical section from the
// injected clock (never from a timer), clamped so it never goes backwards: a
// delayed drain, a blocked writer or a clock step cannot move an event into
// another bucket.
type Recorder struct {
	mu      sync.Mutex
	now     func() time.Time
	started time.Time

	cur          map[time.Time]*hourAcc
	lastHour     time.Time
	anomaly      bool
	hoursDropped int64

	crawlers         map[string]*opstats.CrawlerDelta
	crawlersOverflow int64

	inFlight int64
	totals   [opstats.NumCounters]int64
	ring     ring

	// hook, set by tests only, runs inside the critical section of every event.
	hook func()
}

// New returns a Recorder reading time from clock (time.Now when nil).
func New(clock func() time.Time) *Recorder {
	if clock == nil {
		clock = time.Now
	}
	r := &Recorder{now: clock, cur: map[time.Time]*hourAcc{}, crawlers: map[string]*opstats.CrawlerDelta{}}
	r.started = clock()
	return r
}

// enter takes the lock and returns the event time and the accumulator of its
// hour. The caller must unlock r.mu.
func (r *Recorder) enter() (time.Time, *hourAcc) {
	r.mu.Lock()
	if r.hook != nil {
		r.hook()
	}
	now := r.now()
	h := opstats.HourOf(now)
	switch {
	case r.lastHour.IsZero():
	case h.Before(r.lastHour):
		if r.lastHour.Sub(h) > opstats.MaxLiveHours*time.Hour {
			r.anomaly = true
		}
		h = r.lastHour // a backward step never produces an older hour
	case h.Sub(r.lastHour) > opstats.MaxLiveHours*time.Hour:
		r.anomaly = true
	}
	r.lastHour = h
	acc := r.cur[h]
	if acc == nil {
		if len(r.cur) >= opstats.MaxLiveHours {
			r.dropOldestLocked()
		}
		acc = &hourAcc{}
		r.cur[h] = acc
	}
	acc.coverage |= opstats.MinuteBit(now)
	return now, acc
}

func (r *Recorder) dropOldestLocked() {
	var oldest time.Time
	first := true
	for h := range r.cur {
		if first || h.Before(oldest) {
			oldest, first = h, false
		}
	}
	delete(r.cur, oldest)
	r.hoursDropped++
}

func (r *Recorder) add(acc *hourAcc, now time.Time, c opstats.Counter, n int64) {
	if n <= 0 {
		return
	}
	acc.counters[c] = opstats.AddSat(acc.counters[c], n)
	r.totals[c] = opstats.AddSat(r.totals[c], n)
	if m := windowMetricOf[c]; m >= 0 {
		r.ring.add(now.Unix(), int(m), n)
	}
}

// crawler returns the per-drain record of a crawler, or nil when id is empty or
// the cap is reached (the overflow is counted; global counters are unaffected).
func (r *Recorder) crawler(id string, now time.Time) *opstats.CrawlerDelta {
	if !validUUID(id) {
		return nil // an identity that cannot be stored must never poison a batch
	}
	c, ok := r.crawlers[id]
	if !ok {
		if len(r.crawlers) >= opstats.MaxCrawlersPerDrain {
			r.crawlersOverflow++
			return nil
		}
		c = &opstats.CrawlerDelta{ID: id, FirstSeen: now, LastSeen: now}
		r.crawlers[id] = c
	}
	if now.After(c.LastSeen) {
		c.LastSeen = now
	}
	if now.Before(c.FirstSeen) {
		c.FirstSeen = now
	}
	return c
}

func setIP(c *opstats.CrawlerDelta, ip netip.Addr) {
	if c != nil && ip.IsValid() {
		c.LastIP = ip
	}
}

// ---- ingest HTTP path ---------------------------------------------------

// SubmitAuthenticated records an authenticated submit request (any outcome).
func (r *Recorder) SubmitAuthenticated(id string) {
	if r == nil {
		return
	}
	now, acc := r.enter()
	r.add(acc, now, opstats.CSubmitRequests, 1)
	r.crawler(id, now)
	r.mu.Unlock()
}

// RequestBytes records the request-body bytes actually read of an
// authenticated request (not TLS, headers or responses).
func (r *Recorder) RequestBytes(id string, n int64) {
	if r == nil {
		return
	}
	now, acc := r.enter()
	r.add(acc, now, opstats.CBytesBody, n)
	if c := r.crawler(id, now); c != nil {
		c.BytesBody = opstats.AddSat(c.BytesBody, n)
	}
	r.mu.Unlock()
}

// SubmitValidated records a request whose envelope decoded and whose every
// record validated, before admission: the records are received, and in flight
// until SubmitAcked or SubmitRefused.
func (r *Recorder) SubmitValidated(id string, ip netip.Addr, records int) {
	if r == nil {
		return
	}
	now, acc := r.enter()
	n := int64(records)
	r.add(acc, now, opstats.CSubmitValidated, 1)
	r.add(acc, now, opstats.CRecordsReceived, n)
	r.inFlight += n
	if c := r.crawler(id, now); c != nil {
		c.RecordsReceived = opstats.AddSat(c.RecordsReceived, n)
		setIP(c, ip)
	}
	r.mu.Unlock()
}

// SubmitAcked records a committed admission: acknowledged = enqueued +
// duplicates, all in one critical section.
func (r *Recorder) SubmitAcked(id string, enqueued, duplicates int, payloadBytes int64) {
	if r == nil {
		return
	}
	now, acc := r.enter()
	enq, dup := int64(enqueued), int64(duplicates)
	r.add(acc, now, opstats.CRecordsEnqueued, enq)
	r.add(acc, now, opstats.CAdmissionDuplicates, dup)
	r.add(acc, now, opstats.CRecordsAcknowledged, enq+dup)
	r.add(acc, now, opstats.CBytesAccepted, payloadBytes)
	r.inFlight -= enq + dup
	if c := r.crawler(id, now); c != nil {
		c.RecordsEnqueued = opstats.AddSat(c.RecordsEnqueued, enq)
		c.RecordsDup = opstats.AddSat(c.RecordsDup, dup)
		c.LastSubmit = now
	}
	r.mu.Unlock()
}

// SubmitRefused records an admission that ended without an acknowledgement
// (queue full, storage error, cancellation). A class other than NoReject also
// counts the rejected request.
func (r *Recorder) SubmitRefused(id string, records int, class opstats.RejectClass) {
	if r == nil {
		return
	}
	now, acc := r.enter()
	n := int64(records)
	r.add(acc, now, opstats.CRecordsRefused, n)
	r.inFlight -= n
	c := r.crawler(id, now)
	if class != NoReject {
		r.add(acc, now, class.Counter(), 1)
		if c != nil {
			c.Rejected = opstats.AddSat(c.Rejected, 1)
		}
	}
	r.mu.Unlock()
}

// RequestRejected records a request refused for a class. id is empty for a
// request that was not authenticated (global only).
func (r *Recorder) RequestRejected(class opstats.RejectClass, id string) {
	if r == nil {
		return
	}
	now, acc := r.enter()
	r.add(acc, now, class.Counter(), 1)
	if c := r.crawler(id, now); c != nil {
		c.Rejected = opstats.AddSat(c.Rejected, 1)
	}
	r.mu.Unlock()
}

// ExistsRequest records an existence request that reached its handler.
func (r *Recorder) ExistsRequest(id string) {
	if r == nil {
		return
	}
	now, acc := r.enter()
	r.add(acc, now, opstats.CExistsRequests, 1)
	if c := r.crawler(id, now); c != nil {
		c.ExistsRequests = opstats.AddSat(c.ExistsRequests, 1)
	}
	r.mu.Unlock()
}

// ExistsOutcome is the result of one answered existence request.
type ExistsOutcome struct {
	Hashes, Found, Missing, Unknown               int
	CacheHitPositive, CacheHitNegative, CacheMiss int
}

// ExistsDone records the per-hash outcomes of an existence request.
func (r *Recorder) ExistsDone(id string, ip netip.Addr, o ExistsOutcome) {
	if r == nil {
		return
	}
	now, acc := r.enter()
	r.add(acc, now, opstats.CExistsHashes, int64(o.Hashes))
	r.add(acc, now, opstats.CExistsFound, int64(o.Found))
	r.add(acc, now, opstats.CExistsMissing, int64(o.Missing))
	r.add(acc, now, opstats.CExistsUnknown, int64(o.Unknown))
	r.add(acc, now, opstats.CCacheHitPositive, int64(o.CacheHitPositive))
	r.add(acc, now, opstats.CCacheHitNegative, int64(o.CacheHitNegative))
	r.add(acc, now, opstats.CCacheMiss, int64(o.CacheMiss))
	if c := r.crawler(id, now); c != nil {
		c.ExistsHashes = opstats.AddSat(c.ExistsHashes, int64(o.Hashes))
		setIP(c, ip)
	}
	r.mu.Unlock()
}

// ---- authentication (crawlerauth.Observer) --------------------------------

func (r *Recorder) bump(c opstats.Counter) {
	if r == nil {
		return
	}
	now, acc := r.enter()
	r.add(acc, now, c, 1)
	r.mu.Unlock()
}

// AuthHit, AuthMiss, AuthDBLookup, AuthStorageFailure and AuthBusy implement
// crawlerauth.Observer.
func (r *Recorder) AuthHit()            { r.bump(opstats.CAuthCacheHits) }
func (r *Recorder) AuthMiss()           { r.bump(opstats.CAuthCacheMisses) }
func (r *Recorder) AuthDBLookup()       { r.bump(opstats.CAuthDBLookups) }
func (r *Recorder) AuthStorageFailure() { r.bump(opstats.CAuthStorageFailures) }
func (r *Recorder) AuthBusy()           { r.bump(opstats.CAuthBusy) }

// ---- queue consumer ---------------------------------------------------------

// ProcessAttempt, ProcessRetry, ProcessStorageError, TorrentCreated and
// SettledQuarantined implement the pgqueue consumer observer.
func (r *Recorder) ProcessAttempt()      { r.bump(opstats.CProcessingAttempts) }
func (r *Recorder) ProcessRetry()        { r.bump(opstats.CProcessingRetries) }
func (r *Recorder) ProcessStorageError() { r.bump(opstats.CProcessingStorageErrors) }

// TorrentCreated is counted at the creation boundary: the moment AddTorrent
// reported a new torrent, whatever happens to the queue row afterwards.
func (r *Recorder) TorrentCreated() { r.bump(opstats.CTorrentsCreated) }

// SettledQuarantined is counted after the row moved to quarantine.
func (r *Recorder) SettledQuarantined() { r.bump(opstats.CSettledQuarantined) }

// SettledStoredNew and SettledStoredExisting are counted after the queue row
// was deleted (the settlement boundary); "new" and "existing" are what the
// settling attempt's AddTorrent reported. d is the claim-to-delete time.
func (r *Recorder) SettledStoredNew(d time.Duration) { r.settled(opstats.CSettledStoredNew, d) }
func (r *Recorder) SettledStoredExisting(d time.Duration) {
	r.settled(opstats.CSettledStoredExisting, d)
}

func (r *Recorder) settled(c opstats.Counter, d time.Duration) {
	if r == nil {
		return
	}
	now, acc := r.enter()
	r.add(acc, now, c, 1)
	r.add(acc, now, opstats.CProcessingMsSum, int64((d+500*time.Microsecond)/time.Millisecond))
	r.add(acc, now, opstats.CProcessingMsCount, 1)
	r.mu.Unlock()
}

// ---- coverage, windows, drain -----------------------------------------------

// MarkRunning marks the current UTC minute as observed (the writer calls it
// every few seconds so a quiet process still has coverage).
func (r *Recorder) MarkRunning() {
	if r == nil {
		return
	}
	_, _ = r.enter()
	r.mu.Unlock()
}

// Drain moves everything accumulated so far into a Delta. A recorder that
// arrives later writes to a fresh accumulator; a drained accumulator is never
// written again.
func (r *Recorder) Drain() opstats.Delta {
	if r == nil {
		return opstats.Delta{}
	}
	r.mu.Lock()
	cur, crawlers := r.cur, r.crawlers
	r.cur = map[time.Time]*hourAcc{}
	r.crawlers = map[string]*opstats.CrawlerDelta{}
	d := opstats.Delta{InFlightAtDrain: r.inFlight, CrawlersOverflow: r.crawlersOverflow, ClockAnomaly: r.anomaly, HoursDropped: r.hoursDropped}
	r.crawlersOverflow, r.hoursDropped = 0, 0
	r.mu.Unlock()

	for h, acc := range cur {
		d.Hours = append(d.Hours, opstats.HourDelta{Hour: h, Counters: acc.counters, Coverage: acc.coverage})
	}
	sort.Slice(d.Hours, func(i, j int) bool { return d.Hours[i].Hour.Before(d.Hours[j].Hour) })
	for _, c := range crawlers {
		d.Crawlers = append(d.Crawlers, *c)
	}
	sort.Slice(d.Crawlers, func(i, j int) bool { return d.Crawlers[i].ID < d.Crawlers[j].ID })
	return d
}

// InFlight is the number of validated records whose admission has not finished.
func (r *Recorder) InFlight() int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlight
}

// Totals returns the counters accumulated since the process started.
func (r *Recorder) Totals() [opstats.NumCounters]int64 {
	if r == nil {
		return [opstats.NumCounters]int64{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.totals
}

// ClockAnomaly reports whether a clock step of more than 26 hours was seen.
func (r *Recorder) ClockAnomaly() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.anomaly
}

// Windows returns the rolling windows over the completed seconds before now.
func (r *Recorder) Windows() WindowSnapshot {
	if r == nil {
		return WindowSnapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	end := r.now().Unix()
	observed := int(end - (r.started.Unix() + 1)) // first complete second of the process
	if observed < 0 {
		observed = 0
	}
	ws := WindowSnapshot{EndSecond: end}
	for i, w := range opstats.WindowSeconds {
		ws.Covered[i] = min(w, observed)
		ws.Counts[i] = r.ring.sum(end, w)
	}
	return ws
}

// validUUID reports whether id is a canonical 36-character UUID (any version).
func validUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
			if !isHex {
				return false
			}
		}
	}
	return true
}
