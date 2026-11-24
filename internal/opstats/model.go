// Package opstats is the neutral model of ingestion statistics, shared by the
// ingest process (which writes it) and the web service (which reads it): the
// metric catalogue, UTC bucket arithmetic, the versioned PostgreSQL schema
// magnetico_stats and its verification and privileges.
//
// The schema is operational telemetry only. No column can hold a payload, an
// info hash, a token, a torrent name or a request body, and nothing here ties
// a crawler to a torrent.
package opstats

// SchemaName is the PostgreSQL schema that holds the statistics.
const SchemaName = "magnetico_stats"

// Counter identifies a monotonic event counter. Counters are stored per UTC
// hour, day and month as plain sums; every column has the name CounterName
// returns.
type Counter int

// The catalogue. A new counter is a schema change.
const (
	CSubmitRequests      Counter = iota // authenticated submit requests (any outcome)
	CSubmitValidated                    // submit requests whose envelope and every record validated
	CRecordsReceived                    // records in validated requests, before admission
	CRecordsEnqueued                    // rows inserted into the queue
	CAdmissionDuplicates                // records already pending
	CRecordsAcknowledged                // enqueued + admission_duplicates
	CRecordsRefused                     // records whose admission ended without an acknowledgement
	CBytesBody                          // request-body bytes read, both endpoints
	CBytesAccepted                      // payload bytes of inserted rows
	CExistsRequests
	CExistsHashes
	CExistsFound
	CExistsMissing
	CExistsUnknown
	CCacheHitPositive
	CCacheHitNegative
	CCacheMiss
	CAuthCacheHits
	CAuthCacheMisses
	CAuthDBLookups
	CAuthStorageFailures
	CAuthBusy
	CRejectedAuth // + RejectClass, in the order of the RejectClass constants
	CRejectedUnavailableAuth
	CRejectedMalformed
	CRejectedTooLarge
	CRejectedInvalidRecord
	CRejectedQueueFull
	CRejectedStorageUnavailable
	CRejectedBusy
	CRejectedInternal
	CProcessingAttempts
	CProcessingRetries
	CProcessingStorageErrors
	CTorrentsCreated // AddTorrent reported a new torrent (creation boundary)
	CSettledStoredNew
	CSettledStoredExisting
	CSettledQuarantined
	CProcessingMsSum   // claim-to-delete wall time of settled-stored rows
	CProcessingMsCount // their number (the weight of the mean)
	NumCounters
)

var counterNames = [NumCounters]string{
	CSubmitRequests: "submit_requests", CSubmitValidated: "submit_requests_validated",
	CRecordsReceived: "records_received", CRecordsEnqueued: "records_enqueued",
	CAdmissionDuplicates: "admission_duplicates", CRecordsAcknowledged: "records_acknowledged",
	CRecordsRefused: "records_refused", CBytesBody: "bytes_body", CBytesAccepted: "bytes_accepted",
	CExistsRequests: "exists_requests", CExistsHashes: "exists_hashes", CExistsFound: "exists_found",
	CExistsMissing: "exists_missing", CExistsUnknown: "exists_unknown",
	CCacheHitPositive: "cache_hit_positive", CCacheHitNegative: "cache_hit_negative", CCacheMiss: "cache_miss",
	CAuthCacheHits: "auth_cache_hits", CAuthCacheMisses: "auth_cache_misses", CAuthDBLookups: "auth_db_lookups",
	CAuthStorageFailures: "auth_storage_failures", CAuthBusy: "auth_busy",
	CRejectedAuth: "rejected_auth", CRejectedUnavailableAuth: "rejected_unavailable_auth",
	CRejectedMalformed: "rejected_malformed", CRejectedTooLarge: "rejected_too_large",
	CRejectedInvalidRecord: "rejected_invalid_record", CRejectedQueueFull: "rejected_queue_full",
	CRejectedStorageUnavailable: "rejected_storage_unavailable", CRejectedBusy: "rejected_busy",
	CRejectedInternal:   "rejected_internal",
	CProcessingAttempts: "processing_attempts", CProcessingRetries: "processing_retries",
	CProcessingStorageErrors: "processing_storage_errors", CTorrentsCreated: "torrents_created",
	CSettledStoredNew: "settled_stored_new", CSettledStoredExisting: "settled_stored_existing",
	CSettledQuarantined: "settled_quarantined", CProcessingMsSum: "processing_ms_sum",
	CProcessingMsCount: "processing_ms_count",
}

// Name is the column name of the counter.
func (c Counter) Name() string { return counterNames[c] }

// RejectClass is one of the nine fixed reasons an ingest request is refused. A
// new class is a schema change.
type RejectClass int

const (
	RejectAuth RejectClass = iota
	RejectUnavailableAuth
	RejectMalformed
	RejectTooLarge
	RejectInvalidRecord
	RejectQueueFull
	RejectStorageUnavailable
	RejectBusy
	RejectInternal
	NumRejectClasses
)

// Counter is the counter that counts requests rejected for this class.
func (r RejectClass) Counter() Counter { return CRejectedAuth + Counter(r) }

// Gauge identifies a sampled quantity. Per bucket a gauge is stored as
// sum, count, min, max and last.
type Gauge int

const (
	GQueueRows Gauge = iota
	GQueueBytes
	GQuotaUsedBytes
	GQuarantineRows
	GQuarantineBytes
	GOldestAgeSeconds
	NumGauges
)

var gaugeNames = [NumGauges]string{
	GQueueRows: "queue_rows", GQueueBytes: "queue_bytes", GQuotaUsedBytes: "quota_used_bytes",
	GQuarantineRows: "quarantine_rows", GQuarantineBytes: "quarantine_bytes", GOldestAgeSeconds: "oldest_age_s",
}

// Name is the column-name stem of the gauge.
func (g Gauge) Name() string { return gaugeNames[g] }

// GaugeAgg is the stored aggregate of one gauge over a bucket.
type GaugeAgg struct {
	Sum, N   int64
	Min, Max int64
	Last     int64
	Valid    bool // false: no sample yet (Min, Max and Last are meaningless)
}

// Measure identifies one storage measure (see docs/specs/statistics-v1.md).
type Measure int

const (
	MTorrentsTable Measure = iota
	MBlobsTable
	MIndexes
	MOther
	MMagneticodTotal
	MIngestTotal
	MDatabaseTotal
	MIndexedTorrents // an estimate: always approximate
	NumMeasures
)

var measureNames = [NumMeasures]string{
	MTorrentsTable: "torrents_table_bytes", MBlobsTable: "blobs_table_bytes", MIndexes: "indexes_bytes",
	MOther: "other_bytes", MMagneticodTotal: "magneticod_total_bytes", MIngestTotal: "ingest_total_bytes",
	MDatabaseTotal: "database_total_bytes", MIndexedTorrents: "indexed_torrents_estimate",
}

// Name is the column-name stem of the measure.
func (m Measure) Name() string { return measureNames[m] }

// Saturation is the value at which counters stop growing (2^62): in memory and
// in SQL, so a bucket can never overflow int64.
const Saturation int64 = 1 << 62

// AddSat adds b to a, saturating at Saturation. Operands are non-negative.
func AddSat(a, b int64) int64 {
	if b >= Saturation-a || a >= Saturation {
		return Saturation
	}
	return a + b
}
