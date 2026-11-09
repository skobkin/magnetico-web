package forensics

// Record/blob expansion (--include-records, spec §4): complete source and
// target records for every selected source ID, beyond codec retention caps.
// The decode verdict and the raw-blob export are decided INDEPENDENTLY
// (owner review finding 5): a stored blob over codec.MaxStoredBytes is
// typed "blob_over_limit" and never fed to codec.Decode, yet its exact
// BYTEA still exports under --include-blobs when it fits the forensic
// per-blob cap. Only a blob beyond --max-blob-bytes (or another cap) fails
// the whole export — never skip, never truncate.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/migration/legacy/original"
	"github.com/skobkin/magnetico-web/internal/migration/pgidentity"
	"github.com/skobkin/magnetico-web/internal/migration/state"
	"github.com/skobkin/magnetico-web/internal/migration/target"
	"github.com/skobkin/magnetico-web/persistence"
)

// ErrRecordsTargetUnavailable: --include-records against a target whose
// magneticod production schema is missing (a hard refusal, not per-record
// findings — the run predates the storage schema).
var ErrRecordsTargetUnavailable = errors.New("target production schema is unavailable for record export")

// record expansion constants.
const (
	selectionBatch = 64
	blobChunkBytes = 1 << 20
)

// sanitizeDetailText strips credential-shaped fragments from an error
// string before it is exported (the migration package's sanitizeDetail
// semantics; parity-tested).
var (
	passwordKVText = regexp.MustCompile(`(?i)(password|passfile|sslpassword)\s*[=:]\s*\S+`)
	userinfoText   = regexp.MustCompile(`(?i)(postgres(?:ql)?|uri)://[^/\s:@]+:[^/\s@]+@`)
	maxDetailBytes = 500
)

func sanitizeDetailText(s string) string {
	s = passwordKVText.ReplaceAllString(s, "$1=[redacted]")
	s = userinfoText.ReplaceAllString(s, "$1://[redacted]@")
	if len(s) > maxDetailBytes {
		s = s[:maxDetailBytes]
	}
	return strings.ToValidUTF8(s, "�")
}

func unixTimeRFC3339(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339Nano)
}

// targetRow is one target-side snapshot row (metadata + blob probe).
type targetRow struct {
	ID           int64
	InfoHash     []byte
	Name         string
	TotalSize    int64
	DiscoveredOn int64
	FileCount    int64
	BlobState    string // "none" | "null_data" | "stored_bytes"
	StoredBytes  int64  // 0 unless BlobState == "stored_bytes"
}

// openRecordSource opens the source for --include-records: the exact
// v0.12.0 probe, read-only session, identity distinctness (fail-closed),
// and the target production-schema availability probe. Returned identity
// feeds the header/manifest; the caller owns Close.
func openRecordSource(ctx context.Context, f *Forensics, sourceDSN string) (*original.Source, pgidentity.Identity, error) {
	src, err := original.Open(ctx, sourceDSN)
	if err != nil {
		return nil, pgidentity.Identity{}, fmt.Errorf("%w: %s", ErrSourceUnavailable, sanitizeDetailText(err.Error()))
	}
	srcID, err := src.Identity(ctx)
	if err != nil {
		src.Close()
		return nil, pgidentity.Identity{}, fmt.Errorf("%w: reading source identity: %s", ErrSourceUnavailable, sanitizeDetailText(err.Error()))
	}
	same, ok := pgidentity.SameDatabase(srcID, f.Identity)
	if !ok {
		src.Close()
		return nil, pgidentity.Identity{}, fmt.Errorf("%w: source identity unreadable", ErrSourceUnavailable)
	}
	if same {
		src.Close()
		return nil, pgidentity.Identity{}, target.ErrSameDatabase
	}
	// Target availability probe (spec: fail explicitly, not per-record).
	var torrentsRegclass *string
	if err := f.Pool.QueryRow(ctx,
		`SELECT to_regclass('magneticod.torrents')::text`).Scan(&torrentsRegclass); err != nil {
		src.Close()
		return nil, pgidentity.Identity{}, err
	}
	if torrentsRegclass == nil {
		src.Close()
		return nil, pgidentity.Identity{}, ErrRecordsTargetUnavailable
	}
	return src, srcID, nil
}

// expandRecords runs the record/blob pass. The caller has already validated
// the flags and opened the source; every error is terminal (fail-open
// applies to record CONTENT, never to infrastructure). progress, when
// non-nil, receives a cumulative snapshot after every expanded batch.
func expandRecords(ctx context.Context, f *Forensics, src *original.Source, sink *jsonlSink, opts ExportOptions, progress func(ExportProgress)) error {
	records := int64(0)
	after := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ids, err := state.SelectedRecordIDs(ctx, f.Pool, f.Progress.RunID, after, selectionBatch)
		if err != nil {
			return fmt.Errorf("selecting records: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}
		// Budget check AFTER the empty-page check (review round 1, finding 2):
		// selected IDs beyond an exhausted budget are a hard cap error — never
		// a zero-length slice (the old code sliced to empty and then indexed
		// take[len-1], panicking) and never a silent truncation. An exact-fit
		// selection (records == cap, no further rows) still returns nil above.
		if records >= opts.Limits.MaxRecords {
			return capError("max-records", records, opts.Limits.MaxRecords)
		}
		take := ids
		if records+int64(len(take)) > opts.Limits.MaxRecords {
			take = take[:opts.Limits.MaxRecords-records]
		}
		if err := expandBatch(ctx, f, src, sink, opts, take); err != nil {
			return err
		}
		records += int64(len(take))
		after = take[len(take)-1]
		if progress != nil {
			progress(ExportProgress{Phase: "records", Events: sink.events, Records: records})
		}
		if len(take) < len(ids) {
			// The cap stopped this page mid-batch: a hard error (never a
			// silent stop).
			return capError("max-records", records, opts.Limits.MaxRecords)
		}
	}
}

// sourceReadError marks a failure of the SOURCE read path inside the
// snapshot callback, as opposed to a sink/export failure (a cap or disk
// error must surface verbatim, never misclassified as source unavailability).
type sourceReadError struct{ err error }

func (e *sourceReadError) Error() string { return e.err.Error() }
func (e *sourceReadError) Unwrap() error { return e.err }

// targetReadError marks a failure of the TARGET read path (the snapshot
// transaction's metadata scan or a per-record blob fetch), as opposed to a
// source read (sourceReadError) or a sink/cap failure — both of which keep
// their own classification.
type targetReadError struct{ err error }

func (e *targetReadError) Error() string { return e.err.Error() }
func (e *targetReadError) Unwrap() error { return e.err }

// expandBatch expands one selection batch. ONE target REPEATABLE READ READ
// ONLY transaction supplies every target value of the batch from one
// consistent view (review round 1, finding 1: pgx.BeginFunc's default
// isolation is READ COMMITTED — a concurrent target commit between the
// batch's statements would otherwise leak into the snapshot). Blob bytes
// are NOT prefetched into a map: they are fetched INSIDE that transaction,
// one record at a time, immediately before the record's block is emitted
// (peak retention is a single blob plus one chunk, never
// selectionBatch × MaxBlobBytes). A blob over --max-blob-bytes under
// --include-blobs fails the export during the metadata scan — BEFORE any
// blob byte is fetched. The source snapshot spans the same window so each
// record's full block is emitted in the contract order (source_record →
// source_file* → target_record → decode_finding → target_file* → blob).
func expandBatch(ctx context.Context, f *Forensics, src *original.Source, sink *jsonlSink, opts ExportOptions, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}

	targets := map[int64]*targetRow{}
	codecMaxStored := int64(codec.DefaultLimits().MaxStoredBytes)
	err := pgx.BeginFunc(ctx, f.Pool, func(tx pgx.Tx) error {
		// Transaction characteristics must be set before the first query.
		if _, err := tx.Exec(ctx,
			`SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY`); err != nil {
			return &targetReadError{err}
		}
		rows, err := tx.Query(ctx, `SELECT t.id, t.info_hash, t.name, t.total_size, t.discovered_on,
            t.file_count, b.torrent_id, octet_length(b.data)
        FROM magneticod.torrents t
        LEFT JOIN magneticod.torrent_file_blobs b ON b.torrent_id = t.id
        WHERE t.id = ANY($1::bigint[])
        ORDER BY t.id`, ids)
		if err != nil {
			return &targetReadError{err}
		}
		defer rows.Close()
		for rows.Next() {
			var r targetRow
			var blobID *int64
			var stored *int64
			if err := rows.Scan(&r.ID, &r.InfoHash, &r.Name, &r.TotalSize, &r.DiscoveredOn,
				&r.FileCount, &blobID, &stored); err != nil {
				return &targetReadError{err}
			}
			switch {
			case blobID == nil:
				r.BlobState = "none"
			case stored == nil:
				r.BlobState = "null_data"
			default:
				r.BlobState = "stored_bytes"
				r.StoredBytes = *stored
			}
			targets[r.ID] = &r
		}
		if err := rows.Err(); err != nil {
			return &targetReadError{err}
		}

		// Pre-fetch rejection (review round 1, finding 1): an over-cap blob
		// under --include-blobs fails the export here — the raw-export
		// decision is made from the octet_length probe before a single byte
		// is fetched. Decode still never sees it (over the forensic cap is
		// necessarily over the codec cap too: the cap defaults to 2×).
		for _, id := range ids {
			r := targets[id]
			if r == nil || r.BlobState != "stored_bytes" {
				continue
			}
			if opts.IncludeBlobs && r.StoredBytes > opts.Limits.MaxBlobBytes {
				return capError("max-blob-bytes", r.StoredBytes, opts.Limits.MaxBlobBytes)
			}
		}

		// Source snapshot + per-ID emission inside the open target
		// transaction: the target values keep their batch snapshot while each
		// blob is fetched individually, exactly when its record needs it.
		// needBytes: decode (within the codec cap) or raw export
		// (--include-blobs; over-cap blobs were rejected above, so within
		// --include-blobs means within the forensic cap — including blobs
		// far above the codec limit, review finding 5 semantics kept).
		return src.WithSnapshot(ctx, func(ctx context.Context, snap *original.Snapshot) error {
			srows, err := snap.FetchTorrentRows(ctx, ids)
			if err != nil {
				return &sourceReadError{err}
			}
			records := make(map[int64]*original.TorrentRow, len(srows))
			for i := range srows {
				records[int64(srows[i].ID)] = &srows[i]
			}
			for _, id := range ids {
				tgt := targets[id]
				var blob []byte
				if tgt != nil && tgt.BlobState == "stored_bytes" &&
					(tgt.StoredBytes <= codecMaxStored || opts.IncludeBlobs) {
					if err := tx.QueryRow(ctx,
						`SELECT data FROM magneticod.torrent_file_blobs WHERE torrent_id = $1`, id).
						Scan(&blob); err != nil {
						return &targetReadError{err}
					}
				}
				if err := emitRecord(ctx, f, sink, opts, snap, id, records[id], tgt, blob); err != nil {
					return err
				}
			}
			return nil
		})
	})
	if err != nil {
		if ctx.Err() != nil {
			return err // cancellation is an interrupt, not source unavailability
		}
		var sre *sourceReadError
		if errors.As(err, &sre) {
			return fmt.Errorf("%w: %s", ErrSourceUnavailable, sanitizeDetailText(sre.err.Error()))
		}
		var tre *targetReadError
		if errors.As(err, &tre) {
			return fmt.Errorf("target snapshot: %w", tre.err)
		}
		return err // sink/cap failures surface verbatim
	}
	return nil
}

// emitRecord emits one record's full block: source_record, source_file*
// (streamed inside the shared snapshot — no codec caps, duplicates and
// source file IDs preserved, O(1) memory), target_record, decode verdict,
// target_file*, and the raw blob chunks when included.
func emitRecord(ctx context.Context, f *Forensics, sink *jsonlSink, opts ExportOptions,
	snap *original.Snapshot, id int64, src *original.TorrentRow, tgt *targetRow, blob []byte,
) error {
	runID := f.Progress.RunID

	// source_record: absent only because the query succeeded and proved it.
	srcEv := sourceRecordEvent{
		envelope: newEnvelope(TypeSourceRecord, runID),
		SourceID: id,
		Status:   "absent",
	}
	if src != nil {
		srcEv.Status = "present"
		srcEv.InfoHash = hexBytes(src.InfoHash)
		srcEv.Name = src.Name
		srcEv.TotalSize = src.TotalSize
		srcEv.DiscoveredOn = src.DiscoveredOn
		srcEv.DiscoveredOnTime = unixTimeRFC3339(src.DiscoveredOn)
	}
	if err := sink.emit(TypeSourceRecord, srcEv); err != nil {
		return err
	}

	// source_file*: a read failure is a sourceReadError (classified by the
	// caller); a sink failure passes through verbatim.
	var sinkErr error
	streamErr := snap.StreamFileRows(ctx, []int64{id}, func(fr original.FileRow) error {
		err := sink.emit(TypeSourceFile, sourceFileEvent{
			envelope: newEnvelope(TypeSourceFile, runID),
			SourceID: id,
			FileID:   fr.ID,
			Path:     fr.Path,
			Size:     fr.Size,
		})
		if err != nil {
			sinkErr = err
		}
		return err
	})
	if sinkErr != nil {
		return sinkErr
	}
	if streamErr != nil {
		return &sourceReadError{streamErr}
	}

	// target_record: blob classification keeps absence distinct from NULL
	// (spec: "blob absence is distinct from a NULL-valued blob row").
	tgtEv := targetRecordEvent{
		envelope: newEnvelope(TypeTargetRecord, runID),
		SourceID: id,
		Status:   "absent",
		Blob:     "none",
	}
	if tgt != nil {
		tgtEv.Status = "present"
		tgtEv.InfoHash = hexBytes(tgt.InfoHash)
		tgtEv.Name = tgt.Name
		tgtEv.TotalSize = tgt.TotalSize
		tgtEv.DiscoveredOn = tgt.DiscoveredOn
		tgtEv.DiscoveredOnTime = unixTimeRFC3339(tgt.DiscoveredOn)
		tgtEv.FileCount = tgt.FileCount
		tgtEv.Blob = tgt.BlobState
		tgtEv.StoredBytes = tgt.StoredBytes
	}
	if err := sink.emit(TypeTargetRecord, tgtEv); err != nil {
		return err
	}

	if tgt == nil {
		return nil
	}

	// Decode verdict (independent of raw export; finding 5).
	files, finding := decodeTargetRecord(tgt, blob)
	if finding != nil {
		if err := sink.emit(TypeDecodeFinding, decodeFindingEvent{
			envelope: newEnvelope(TypeDecodeFinding, runID),
			SourceID: id,
			Status:   finding.Status,
			Code:     finding.Code,
			Details:  finding.Details,
		}); err != nil {
			return err
		}
	}
	for i, file := range files {
		if err := sink.emit(TypeTargetFile, targetFileEvent{
			envelope: newEnvelope(TypeTargetFile, runID),
			SourceID: id,
			Index:    i,
			Path:     file.Path,
			Size:     file.Size,
			Assumed:  finding.Code == "assumed_single",
		}); err != nil {
			return err
		}
	}

	// Raw export: the exact stored BYTEA as ordered Base64 chunks, whenever
	// it fits the forensic cap — INCLUDING blobs far above codec limits
	// (finding 5). Lossless rule: concat(base64-decode of chunks by
	// ascending chunk_index) == the stored bytes; the target_blob event
	// binds the SHA-256 so the round-trip is checkable.
	if !opts.IncludeBlobs || tgt.BlobState != "stored_bytes" {
		return nil
	}
	if tgt.StoredBytes > opts.Limits.MaxBlobBytes {
		// Unreachable through expandBatch: the over-cap rejection fires during
		// the metadata scan, before any blob fetch. Kept as defense in depth.
		return fmt.Errorf("%w: blob for source %d is %d bytes, over --max-blob-bytes %d (raise the flag or drop --include-blobs)",
			ErrExportCapExceeded, id, tgt.StoredBytes, opts.Limits.MaxBlobBytes)
	}
	if blob == nil {
		return fmt.Errorf("internal: blob for source %d was not fetched inside the target snapshot", id)
	}
	sum := sha256.Sum256(blob)
	if err := sink.emit(TypeTargetBlob, targetBlobEvent{
		envelope:    newEnvelope(TypeTargetBlob, runID),
		SourceID:    id,
		StoredBytes: tgt.StoredBytes,
		SHA256:      hex.EncodeToString(sum[:]),
	}); err != nil {
		return err
	}
	rest := blob
	for idx := 0; len(rest) > 0; idx++ {
		n := len(rest)
		if n > blobChunkBytes {
			n = blobChunkBytes
		}
		chunk := rest[:n]
		rest = rest[n:]
		if err := sink.emit(TypeTargetBlobChunk, targetBlobChunkEvent{
			envelope:   newEnvelope(TypeTargetBlobChunk, runID),
			SourceID:   id,
			ChunkIndex: idx,
			DataBase64: base64.StdEncoding.EncodeToString(chunk),
			ChunkBytes: n,
		}); err != nil {
			return err
		}
	}
	return nil
}

// decodeFinding is the typed decode verdict for one record.
type decodeFinding struct {
	Status  string // decoded | failed | not_attempted
	Code    string
	Details map[string]any
}

// decodeTargetRecord decides decodability WITHOUT conflating it with raw
// export (finding 5). A blob over the codec stored limit is never fed to
// codec.Decode; a within-limit blob decodes (or fails typed); the no-blob
// single-file shape reconstructs its assumed entry through production
// validation.
func decodeTargetRecord(tgt *targetRow, blob []byte) ([]codec.File, *decodeFinding) {
	limits := codec.DefaultLimits()
	switch {
	case tgt.BlobState == "none" && tgt.FileCount != 1:
		return nil, &decodeFinding{Status: "failed", Code: "missing_blob", Details: map[string]any{
			"reason": "no blob stored", "file_count": tgt.FileCount,
		}}
	case tgt.BlobState == "none" && tgt.FileCount == 1:
		meta := persistence.Torrent{
			InfoHash: tgt.InfoHash, Name: tgt.Name,
			TotalSize: tgt.TotalSize, DiscoveredOn: tgt.DiscoveredOn,
		}
		files := []persistence.File{{Path: tgt.Name, Size: tgt.TotalSize}}
		if err := persistence.ValidateIngest(meta, files, limits); err != nil {
			return nil, &decodeFinding{Status: "failed", Code: "invalid_metadata", Details: map[string]any{
				"reason": sanitizeDetailText(err.Error()), "assumed_single": true,
			}}
		}
		return []codec.File{{Path: tgt.Name, Size: tgt.TotalSize}},
			&decodeFinding{Status: "not_attempted", Code: "assumed_single", Details: map[string]any{
				"reason": "intentional no-blob single file reconstructed as an assumption",
			}}
	case tgt.BlobState == "null_data":
		return nil, &decodeFinding{Status: "failed", Code: "blob_null_data", Details: map[string]any{
			"reason": "blob row holds NULL data",
		}}
	case tgt.StoredBytes > int64(limits.MaxStoredBytes):
		return nil, &decodeFinding{Status: "not_attempted", Code: "blob_over_limit", Details: map[string]any{
			"limit": "stored_bytes", "actual": tgt.StoredBytes, "allowed": int64(limits.MaxStoredBytes),
		}}
	case blob == nil:
		// Unreachable by construction: the snapshot fetches every
		// within-codec-limit blob. A loud guard, not a silent skip.
		return nil, &decodeFinding{Status: "not_attempted", Code: "blob_unfetched", Details: map[string]any{
			"reason": "internal: within-limit blob was not fetched",
		}}
	default:
		files, err := codec.Decode(blob, limits)
		if err != nil {
			code, details := DecodeFinding(err)
			return nil, &decodeFinding{Status: "failed", Code: code, Details: details}
		}
		return files, &decodeFinding{Status: "decoded", Code: "decoded", Details: map[string]any{
			"file_count": len(files),
		}}
	}
}

// DecodeFinding maps a codec.Decode error to a stable, bounded issue code
// from the codec taxonomy — the forensic twin of the migration package's
// decodeErrorIssueCode (parity-tested). Details stay secret-free via the
// shared sanitizer.
func DecodeFinding(err error) (code string, details map[string]any) {
	details = map[string]any{"reason": sanitizeDetailText(err.Error())}
	var le *codec.LimitError
	switch {
	case errors.As(err, &le):
		details["limit"] = le.Limit
		details["actual"] = le.Actual
		details["allowed"] = le.Allowed
		return "codec_limit", details
	case errors.Is(err, codec.ErrUnsupportedVersion):
		return "wire_version", details
	case errors.Is(err, codec.ErrUnsupportedSerializer),
		errors.Is(err, codec.ErrUnsupportedCompressor):
		return "wire_ids", details
	case errors.Is(err, codec.ErrTruncated), errors.Is(err, codec.ErrTrailingBytes):
		return "framing", details
	case errors.Is(err, codec.ErrNonCanonicalOrder):
		return "noncanonical_order", details
	case errors.Is(err, codec.ErrCorruptCompressed):
		return "compressed_stream", details
	case errors.Is(err, codec.ErrOverflow), errors.Is(err, codec.ErrNegativeSize),
		errors.Is(err, codec.ErrInvalidCount):
		return "numeric", details
	case errors.Is(err, codec.ErrNonMaximalPrefix),
		errors.Is(err, codec.ErrPrefixOutOfRange),
		errors.Is(err, codec.ErrDuplicateDirName),
		errors.Is(err, codec.ErrEmptyPathComponent),
		errors.Is(err, codec.ErrPathComponentSeparator):
		return "structure", details
	default:
		return "decode_error", details
	}
}
