// Package stats is the read model of the administrator dashboard: a cached,
// coalescing reader of the magnetico_stats schema (and of the safe columns of
// the crawler registry) that distinguishes unavailable, stale, approximate and
// zero. It holds no per-user data and checks the role on every entry point.
package stats

import (
	"errors"
	"strings"
	"time"

	"github.com/skobkin/magnetico-web/internal/opstats"
)

// State is the freshness of one data source.
type State int

const (
	// StateUp: the source is fresh.
	StateUp State = iota
	// StateStale: older than the fresh window; shown with its age.
	StateStale
	// StateUnavailable: too old or never observed; no number is shown.
	StateUnavailable
	// StateUninitialised: the statistics schema does not exist.
	StateUninitialised
)

func (s State) String() string {
	return [...]string{"up", "stale", "unavailable", "uninitialised"}[s]
}

// Freshness windows (docs/specs/statistics-v1.md §8): three independent clocks.
const (
	IngestUp, IngestStale   = 30 * time.Second, 5 * time.Minute
	QueueUp, QueueStale     = 90 * time.Second, 15 * time.Minute
	StorageUp, StorageStale = 15 * time.Minute, time.Hour
	RateWindowSkew          = 30 * time.Second
	SnapshotTTL, SeriesTTL  = 10 * time.Second, 60 * time.Second
	SeriesCacheEntries      = 64
	CrawlerPageSize         = 50
	instanceHorizon         = 24 * time.Hour
	maxInstances            = 50
)

func classify(age, up, stale time.Duration) State {
	switch {
	case age < -RateWindowSkew:
		return StateUnavailable // a source far "from the future" was written by a wrong clock: its value is not trusted
	case age < 0:
		return StateUp // a small skew between hosts is not a stale reading
	case age < up:
		return StateUp
	case age < stale:
		return StateStale
	}
	return StateUnavailable
}

// ClassifyIngest, ClassifyQueue and ClassifyStorage apply the three clocks.
func ClassifyIngest(age time.Duration) State  { return classify(age, IngestUp, IngestStale) }
func ClassifyQueue(age time.Duration) State   { return classify(age, QueueUp+1, QueueStale) } // fresh ≤ 90 s
func ClassifyStorage(age time.Duration) State { return classify(age, StorageUp+1, StorageStale) }

// Measured is a value that may be unavailable (never rendered as zero) and
// approximate (an estimate, flagged).
type Measured[T any] struct {
	Value  T
	Valid  bool
	Approx bool
}

func known[T any](v T) Measured[T]     { return Measured[T]{Value: v, Valid: true} }
func estimated[T any](v T) Measured[T] { return Measured[T]{Value: v, Valid: true, Approx: true} }
func nullInt(p *int64) Measured[int64] {
	if p == nil {
		return Measured[int64]{}
	}
	return known(*p)
}

// Freshness describes how current an answer is.
type Freshness struct {
	Ingest, Queue, Storage State
	FetchedAt              time.Time
	Stale                  bool // the answer is the last known good: the refresh failed
}

// Instance is the heartbeat state of one ingest process.
type Instance struct {
	ID                       string
	StartedAt                time.Time
	Age                      time.Duration // since the last heartbeat (database clock)
	State                    State
	OwnsConsumer             bool
	InFlight                 int64
	FlushStalledSeconds      int64
	HoursDropped             int64
	SequenceAnomalies        int64
	BatchesDroppedUnresolved int64
	ClockAnomaly             bool
	TotalReceived            int64
	TotalAcknowledged        int64
	TotalSettled             int64
	windowEndAge             time.Duration
	covered                  [3]int
	counts                   [3][metricCount]int64
}

// Rolling-rate metrics, in the order of opstats.WindowMetricNames.
const metricCount = opstats.NumWindowMetrics

// MetricNames are the display keys of the rate metrics (also the keys of the
// JSON answer): the snapshot column stems without the "records_" prefix.
var MetricNames = func() (n [metricCount]string) {
	for i, s := range opstats.WindowMetricNames {
		n[i] = strings.TrimPrefix(s, "records_")
	}
	return
}()

// MetricIndex is the position of a named rate metric.
func MetricIndex(name string) int {
	for i, n := range MetricNames {
		if n == name {
			return i
		}
	}
	return -1
}

// RateWindow is one rolling window summed over the fresh, compatible instances.
type RateWindow struct {
	Seconds  int
	Unit     string // "per second", "per minute", "per hour"
	Included int
	Excluded []Excluded
	Values   [metricCount]Measured[float64]
}

// Excluded names an instance left out of a rate and why.
type Excluded struct{ InstanceID, Reason string }

// Rates are the rolling rates of the three windows (60 s, 15 min, 60 min).
type Rates struct{ Windows [3]RateWindow }

// QueueView is the shared queue and quota state (one row, never summed).
type QueueView struct {
	State        State
	Age          time.Duration
	Rows, Bytes  Measured[int64]
	QuotaUsed    Measured[int64]
	QuotaMax     Measured[int64]
	Utilisation  Measured[float64] // quota used ÷ quota max
	Quarantine   Measured[int64]
	QuarantineBy Measured[int64]
	OldestAge    Measured[time.Duration]
	Backoff      bool
}

// StorageView is the newest storage sample.
type StorageView struct {
	State        State
	Age          time.Duration
	Torrents     Measured[int64]
	Blobs        Measured[int64]
	Indexes      Measured[int64]
	Other        Measured[int64]
	Magneticod   Measured[int64]
	Ingest       Measured[int64]
	Database     Measured[int64]
	IndexedCount Measured[int64]   // always approximate
	BytesPerItem Measured[float64] // magneticod total ÷ indexed estimate, from this one sample
	// Reconciliation says whether the four categories added up to the total in
	// this sample: not evaluated (a measure was unavailable) is not a mismatch.
	Reconciliation opstats.Reconciliation
}

// Users counts accounts (optional: unavailable when the web schema cannot be read).
type Users struct{ Total, Active, Disabled, Admins Measured[int64] }

// ActiveWindow is how recent a crawler's last authenticated request, as seen by
// the statistics, must be for the crawler to count as recently active. The
// statistics persist once a minute, so the figure can lag by about that much.
const ActiveWindow = 15 * time.Minute

// CrawlerCountCap bounds the registry counts: a larger registry is reported as
// "at least" the cap, so the cost never depends on the registered count.
const CrawlerCountCap = 10000

// CrawlerCounts are the registry counts of the dashboard (optional: unavailable
// when the ingest schema cannot be read; Active also needs the statistics).
type CrawlerCounts struct {
	Registered, Enabled, Active Measured[int64]
	Capped                      bool // the registry holds more than CrawlerCountCap rows: the counts are lower bounds
}

// Overview is the dashboard's current state.
type Overview struct {
	Schema     State  // StateUp, StateUninitialised or StateUnavailable (incompatible or unreadable)
	Problem    string // what is wrong and the remedy, for a Schema other than StateUp
	Instances  []Instance
	Rates      Rates
	Queue      QueueView
	Storage    StorageView
	ThisHour   [metricCount]int64 // counts so far in the open UTC hour (partial)
	ThisHourAt time.Time
	Users      Users
	Crawlers   CrawlerCounts
}

var (
	errPanic = errors.New("statistics: a load panicked")
	// ErrBadQuery: an unknown chart or granularity, or a malformed period.
	ErrBadQuery = errors.New("statistics: invalid query")
	// ErrBusy: too many reads are pending at once; retry shortly.
	ErrBusy = errors.New("statistics: too many pending reads")
	// ErrPeriodOutOfRange: a well-formed period with no data and none expected.
	ErrPeriodOutOfRange = errors.New("statistics: period out of range")
)
