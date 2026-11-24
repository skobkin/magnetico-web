package stats

import "github.com/skobkin/magnetico-web/internal/opstats"

// Window metrics: the quantities whose rolling rates the heartbeat publishes.
// The order matches the snapshot columns (opstats.snapshotWindowMetrics).
const (
	wmReceived = iota
	wmEnqueued
	wmAcknowledged
	wmSettled
	wmExistsHashes
	wmBytesBody
	numWindowMetrics
)

// WindowMetricNames are the snapshot column stems of the window metrics.
var WindowMetricNames = [numWindowMetrics]string{
	"records_received", "records_enqueued", "records_acknowledged", "settled", "exists_hashes", "bytes_body",
}

const ringSeconds = 3600

// windowMetricOf maps a counter to its window metric, or -1.
var windowMetricOf = func() [opstats.NumCounters]int8 {
	var m [opstats.NumCounters]int8
	for i := range m {
		m[i] = -1
	}
	m[opstats.CRecordsReceived] = wmReceived
	m[opstats.CRecordsEnqueued] = wmEnqueued
	m[opstats.CRecordsAcknowledged] = wmAcknowledged
	m[opstats.CSettledStoredNew] = wmSettled
	m[opstats.CSettledStoredExisting] = wmSettled
	m[opstats.CSettledQuarantined] = wmSettled
	m[opstats.CExistsHashes] = wmExistsHashes
	m[opstats.CBytesBody] = wmBytesBody
	return m
}()

// ring holds one hour of 1-second slots (172 KiB). A slot is valid only while
// its stamp equals the second it stands for, so stale data is never summed.
type ring struct {
	stamp [ringSeconds]int64
	v     [ringSeconds][numWindowMetrics]int64
}

func (r *ring) add(sec int64, metric int, n int64) {
	i := sec % ringSeconds
	if r.stamp[i] != sec {
		r.stamp[i] = sec
		r.v[i] = [numWindowMetrics]int64{}
	}
	r.v[i][metric] += n
}

// sum returns the totals of the w completed seconds ending at endSec
// (exclusive), i.e. the seconds endSec-w … endSec-1.
func (r *ring) sum(endSec int64, w int) (out [numWindowMetrics]int64) {
	for s := endSec - int64(w); s < endSec; s++ {
		if s < 0 {
			continue
		}
		i := s % ringSeconds
		if r.stamp[i] != s {
			continue
		}
		for m := range out {
			out[m] += r.v[i][m]
		}
	}
	return out
}

// WindowSnapshot is the rolling-window view a heartbeat publishes: for each of
// the windows (60 s, 15 min, 60 min) how many seconds this process really
// observed and what the window metrics summed to. Only completed seconds count.
type WindowSnapshot struct {
	EndSecond int64 // unix second at which the windows end (exclusive)
	Covered   [3]int
	Counts    [3][numWindowMetrics]int64
}
