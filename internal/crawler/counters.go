package crawler

import "sync/atomic"

// Counters are the delivery-pipeline counters (logged as a periodic summary).
type Counters struct {
	PrecheckRequests atomic.Uint64
	PrecheckExisting atomic.Uint64 // hashes skipped because ingest has them
	PrecheckUnknown  atomic.Uint64 // hashes admitted fail-open
	PrecheckErrors   atomic.Uint64
	PrecheckDropped  atomic.Uint64 // DHT results dropped: precheck buffer full

	LeechStarted atomic.Uint64
	LeechJoined  atomic.Uint64
	LeechBusy    atomic.Uint64 // dropped: active-leech limit
	LeechGate    atomic.Uint64 // dropped: local queue full (admission gate closed)

	EnqueueAppended  atomic.Uint64
	EnqueueDuplicate atomic.Uint64
	EnqueueInvalid   atomic.Uint64
	EnqueueTooLarge  atomic.Uint64 // quarantined at enqueue (record_too_large)
	DroppedQueueFull atomic.Uint64 // crawl_dropped_queue_full
	DroppedShutdown  atomic.Uint64 // crawl_dropped_shutdown
	EnqueueErrors    atomic.Uint64

	SendBatches      atomic.Uint64
	SendAcked        atomic.Uint64 // records acknowledged
	SendQuarantined  atomic.Uint64
	SendRetryable    atomic.Uint64
	SendAmbiguous    atomic.Uint64
	SendUnauthorized atomic.Uint64
	SendUnrecognised atomic.Uint64 // crawl_send_unrecognised_rejections
	SendGuardTrips   atomic.Uint64
	SendBisections   atomic.Uint64
}
