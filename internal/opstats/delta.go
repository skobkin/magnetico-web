package opstats

import (
	"net/netip"
	"time"
)

// StorageAgg aggregates the storage samples of one bucket. Measures that were
// unavailable in a sample contribute nothing to it.
type StorageAgg struct {
	Last, Max, Sum, N   [NumMeasures]int64
	LastValid, MaxValid [NumMeasures]bool
	// BPTBytes and BPTTorrents accumulate the numerator and denominator of
	// bytes-per-torrent from samples where both were known and the estimate was
	// positive: the period value is the ratio of sums, never a mean of ratios.
	BPTBytes, BPTTorrents int64
	Samples               int64
	Mismatches            int64 // samples whose categories did not reconcile with the total
	LastAt                time.Time
}

// IsZero reports whether no sample contributed.
func (s *StorageAgg) IsZero() bool { return s.Samples == 0 }

// QueueState is the newest sampled state of the queue and quota: the single
// shared row. A value that is not Valid is unavailable, never zero.
type QueueState struct {
	At       time.Time
	Slot     int64
	Values   [NumGauges]int64
	Valid    [NumGauges]bool
	QuotaMax int64
	// CountsApprox: row counts are planner estimates; OldestApprox: the oldest
	// age came from the lowest id; Backoff: the sampling interval was widened.
	CountsApprox, OldestApprox, Backoff bool
}

// StorageState is the newest sampled storage state.
type StorageState struct {
	At         time.Time
	Slot       int64
	Values     [NumMeasures]int64
	Valid      [NumMeasures]bool
	Reconciled bool
}

// HourDelta is what one UTC hour accumulated since the last drain: event
// counters, gauge aggregates and the minutes in which the process was running.
type HourDelta struct {
	Hour        time.Time
	Counters    [NumCounters]int64
	Gauges      [NumGauges]GaugeAgg
	GaugeLastAt time.Time // time of the newest gauge sample (zero: none)
	Coverage    int64     // bit i: UTC minute i of the hour was observed
	Storage     StorageAgg
}

// CrawlerDelta is the activity of one crawler since the last drain. It carries
// counts and timestamps only: never a torrent, a hash or a name.
type CrawlerDelta struct {
	ID              string
	FirstSeen       time.Time
	LastSeen        time.Time
	LastIP          netip.Addr // zero when unknown
	LastSubmit      time.Time  // zero when the delta contains no submit
	RecordsReceived int64
	RecordsEnqueued int64
	RecordsDup      int64
	BytesBody       int64
	ExistsRequests  int64
	ExistsHashes    int64
	Rejected        int64
}

// Delta is the content of one drain of the recorder.
type Delta struct {
	Hours            []HourDelta // ascending
	Crawlers         []CrawlerDelta
	CrawlersOverflow int64 // activity of crawlers beyond the per-drain cap (global counters are unaffected)
	// InFlightAtDrain is the number of validated records whose admission had
	// not finished when the accumulator was swapped.
	InFlightAtDrain int64
	ClockAnomaly    bool
	// HoursDropped counts hour buckets discarded because more than MaxLiveHours
	// were live (the writer stalled for over a day).
	HoursDropped int64

	// The shared-gauge part: samples taken by the instance that owns the queue
	// consumer. The slot ranges guard against two instances contributing the
	// same period (see Apply). Latest* hold the newest sample for shared_state.
	QueueFirstSlot, QueueLastSlot     int64
	StorageFirstSlot, StorageLastSlot int64
	LatestQueue                       *QueueState
	LatestStorage                     *StorageState
}

// IsEmpty reports whether the delta carries nothing worth persisting.
func (d *Delta) IsEmpty() bool {
	return len(d.Hours) == 0 && len(d.Crawlers) == 0 && d.CrawlersOverflow == 0 && d.HoursDropped == 0 &&
		d.LatestQueue == nil && d.LatestStorage == nil
}

// MaxCrawlersPerDrain bounds the per-crawler map of one drain: memory and the
// size of one flush are independent of the number of registered crawlers.
const MaxCrawlersPerDrain = 1024

// MaxLiveHours bounds the hour buckets held by a recorder or carried by a writer.
const MaxLiveHours = 26
