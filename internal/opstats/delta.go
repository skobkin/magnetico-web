package opstats

import (
	"net/netip"
	"time"
)

// HourDelta is what one UTC hour accumulated since the last drain: event
// counters, gauge aggregates and the minutes in which the process was running.
type HourDelta struct {
	Hour        time.Time
	Counters    [NumCounters]int64
	Gauges      [NumGauges]GaugeAgg
	GaugeLastAt time.Time // time of the newest gauge sample (zero: none)
	Coverage    int64     // bit i: UTC minute i of the hour was observed
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
}

// IsEmpty reports whether the delta carries nothing worth persisting.
func (d *Delta) IsEmpty() bool {
	return len(d.Hours) == 0 && len(d.Crawlers) == 0 && d.CrawlersOverflow == 0
}

// MaxCrawlersPerDrain bounds the per-crawler map of one drain: memory and the
// size of one flush are independent of the number of registered crawlers.
const MaxCrawlersPerDrain = 1024

// MaxLiveHours bounds the hour buckets held by a recorder or carried by a writer.
const MaxLiveHours = 26
