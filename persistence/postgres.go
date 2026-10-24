package persistence

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/codec"
)

// connectPool opens a connection pool. Parse or connect failures are reduced
// to safe content: the SQLSTATE code and message for real PostgreSQL errors,
// the Go type otherwise. The raw error is preserved under the wrapper so
// structured inspection (errors.As on *pgconn.PgError) keeps working, while
// logging goes through controlled detail paths that never print error text.
func connectPool(ctx context.Context, rawURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStorageIO, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("%w: %w", ErrStorageIO, err)
	}
	return pool, nil
}

// storageErrorDetail reduces a database error to content that is safe to log:
// the SQLSTATE code and message for real PostgreSQL errors (they never
// contain credentials), the Go type for anything else — driver and net errors
// can embed the connection string.
func storageErrorDetail(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Sprintf("pg %s: %s", pgErr.Code, pgErr.Message)
	}
	return fmt.Sprintf("unexpected database error (%T)", err)
}

// storeEncoderWorkers is the encoder-pool size owned by a store.
func storeEncoderWorkers() int {
	n := 4
	if runtime.NumCPU() < n {
		n = runtime.NumCPU()
	}
	return n
}

// pgxStore is the PostgreSQL Store implementation. It owns one shared
// codec.Encoder (semaphore-bounded workers with reusable compressor state)
// and one pool connection per operation via pgxpool.
type pgxStore struct {
	pool    *pgxpool.Pool
	encoder *codec.Encoder
	limits  codec.Limits

	// warnMu/throttle back the one-WARN-per-interval rule for inconsistent
	// already-present metadata (spec §4.5).
	warnMu      sync.Mutex
	lastWarnSec int64
}

func newPostgresStore(pool *pgxpool.Pool, profile codec.Profile, limits codec.Limits) *pgxStore {
	return &pgxStore{
		pool:    pool,
		encoder: codec.NewEncoder(profile, limits, storeEncoderWorkers()),
		limits:  limits,
	}
}

// MakeDatabase connects to a PostgreSQL database and verifies the expected
// 0.8.0 schema state read-only before any use (spec §6): pg_trgm installed,
// exact schema version state, no legacy shapes. Any mismatch fails fast with
// a typed error and actionable text; nothing is ever created, altered or
// migrated here. Non-postgres schemes fail closed with
// ErrStorageNotImplemented.
func MakeDatabase(ctx context.Context, rawURL string) (Store, error) {
	scheme := ""
	if parsed, err := url.Parse(rawURL); err == nil {
		scheme = parsed.Scheme
	}
	switch scheme {
	case "postgres", "postgresql":
		// supported below
	default:
		return nil, fmt.Errorf("%w (requested scheme %q)", ErrStorageNotImplemented, scheme)
	}

	pool, err := connectPool(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	store := newPostgresStore(pool, codec.DefaultProfile(), codec.DefaultLimits())
	if err := store.verifyStartup(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

// verifyStartup runs the read-only startup verification: exactly the expected
// schema objects and exactly one schema_version row carrying SchemaVersion,
// plus pg_trgm installed. On a fresh database (neither schema nor extension)
// the schema-init hint is the actionable one; wherever schema objects exist,
// a missing pg_trgm is reported first — its GIN index cannot exist without
// the extension, so it is the root cause of any missing-index state. It never
// creates, alters or migrates anything.
func (s *pgxStore) verifyStartup(ctx context.Context) error {
	inv, err := readInventory(ctx, s.pool)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrStorageIO, err)
	}
	state, details := inv.classify()
	switch state {
	case stateFresh:
		return stateError(stateUnsupported, details)
	case stateLegacy:
		return stateError(state, details)
	}
	if !inv.trgm {
		return ErrMissingExtension
	}
	if state != stateOK {
		return stateError(stateUnsupported, details)
	}
	return nil
}

func (s *pgxStore) AddTorrent(ctx context.Context, meta Torrent, files []File) (bool, int32, error) {
	if err := validateIngest(meta, files, s.limits); err != nil {
		return false, 0, err
	}

	// Performance hint only (spec §4): skip the encode work for known
	// infohashes; the transaction below remains the concurrency authority.
	if exists, err := s.DoesTorrentExist(ctx, meta.InfoHash); err == nil && exists {
		id, err := s.fetchExisting(ctx, s.pool, meta, len(files))
		if err != nil {
			return false, 0, fmt.Errorf("%w: %w", ErrStorageIO, err)
		}
		return false, id, nil
	}

	blobNeeded := !noBlobEligible(meta, files)
	var blob []byte
	if blobNeeded {
		var err error
		blob, _, err = s.encoder.Encode(files)
		if err != nil {
			// Codec failures caused by invalid incoming metadata: the
			// throttled WARN + drop + metric path, never a DB failure.
			return false, 0, wrapEncodeError(err)
		}
	}

	var (
		id      int32
		created bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO magneticod.torrents
            (info_hash, name, total_size, discovered_on, file_count)
            VALUES ($1, $2, $3, $4, $5)
            ON CONFLICT (info_hash) DO NOTHING
            RETURNING id`,
			meta.InfoHash, meta.Name, meta.TotalSize, meta.DiscoveredOn, int32(len(files)),
		).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			// Already present: keep the stored row untouched; warn once per
			// interval if the new metadata disagrees (infohash hex only).
			existingID, err := s.fetchExisting(ctx, tx, meta, len(files))
			if err != nil {
				return err
			}
			id, created = existingID, false
			return nil
		}
		if err != nil {
			return err
		}
		created = true

		if blobNeeded {
			if _, err := tx.Exec(ctx, `INSERT INTO magneticod.torrent_file_blobs
                (torrent_id, data) VALUES ($1, $2)`, id, blob); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, 0, fmt.Errorf("%w: %w", ErrStorageIO, err)
	}
	return created, id, nil
}

// fetchExisting loads the stored summary of an already-present infohash and
// applies the inconsistent-metadata warning policy (one WARN per interval,
// infohash hex only). Used by both the presence-hint fast path and the
// transactional conflict path.
func (s *pgxStore) fetchExisting(ctx context.Context, q queryer, meta Torrent, fileCount int) (int32, error) {
	var (
		id    int32
		name  string
		total int64
		count int32
	)
	if err := q.QueryRow(ctx, `SELECT id, name, total_size, file_count
        FROM magneticod.torrents WHERE info_hash = $1`, meta.InfoHash,
	).Scan(&id, &name, &total, &count); err != nil {
		return 0, err
	}
	if name != meta.Name || total != meta.TotalSize || int(count) != fileCount {
		s.warnInconsistentMetadata()
	}
	return id, nil
}

// inconsistentMetadataWarnInterval spaces out the already-present-with-
// different-metadata warnings.
const inconsistentMetadataWarnInterval = time.Hour

func (s *pgxStore) warnInconsistentMetadata() {
	s.warnMu.Lock()
	defer s.warnMu.Unlock()
	now := time.Now().Unix()
	if s.lastWarnSec != 0 && now-s.lastWarnSec < int64(inconsistentMetadataWarnInterval/time.Second) {
		return
	}
	s.lastWarnSec = now
	log.Printf("[persistence] an already-present infohash was offered again with different metadata (dropped; infohash suppressed)")
}

func (s *pgxStore) GetTorrent(ctx context.Context, infoHash []byte) (Torrent, error) {
	var t Torrent
	err := s.pool.QueryRow(ctx, `SELECT id, info_hash, name, total_size, discovered_on, file_count
        FROM magneticod.torrents WHERE info_hash = $1`, infoHash,
	).Scan(&t.ID, &t.InfoHash, &t.Name, &t.TotalSize, &t.DiscoveredOn, &t.FileCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return Torrent{}, ErrTorrentNotFound
	}
	if err != nil {
		return Torrent{}, fmt.Errorf("%w: %w", ErrStorageIO, err)
	}
	return t, nil
}

func (s *pgxStore) GetFiles(ctx context.Context, infoHash []byte) ([]File, error) {
	var (
		name      string
		totalSize int64
		fileCount int32
		blob      []byte
	)
	err := s.pool.QueryRow(ctx, `SELECT t.name, t.total_size, t.file_count, b.data
        FROM magneticod.torrents t
        LEFT JOIN magneticod.torrent_file_blobs b ON b.torrent_id = t.id
        WHERE t.info_hash = $1`, infoHash,
	).Scan(&name, &totalSize, &fileCount, &blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTorrentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStorageIO, err)
	}

	if blob == nil {
		if fileCount == 1 {
			// The intentional no-blob representation.
			return []File{{Path: name, Size: totalSize}}, nil
		}
		return nil, ErrIntegrityMissingBlob
	}

	files, err := codec.Decode(blob, s.limits)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptBlob, err)
	}
	if len(files) != int(fileCount) {
		return nil, fmt.Errorf("%w: decoded %d files, torrents row says %d",
			ErrCorruptBlob, len(files), fileCount)
	}
	var sum int64
	for _, f := range files {
		if sum > math.MaxInt64-f.Size {
			return nil, fmt.Errorf("%w: decoded file sizes overflow", ErrCorruptBlob)
		}
		sum += f.Size
	}
	if sum != totalSize {
		return nil, fmt.Errorf("%w: decoded sizes sum to %d, torrents row says %d",
			ErrCorruptBlob, sum, totalSize)
	}
	return files, nil
}

func (s *pgxStore) QueryTorrents(ctx context.Context, query TorrentQuery) ([]Torrent, error) {
	if query.MaxTuples <= 0 {
		return nil, fmt.Errorf("%w: MaxTuples must be positive, got %d", ErrInvalidMetadata, query.MaxTuples)
	}
	order := ""
	switch query.OrderBy {
	case ByDiscoveredOnDesc:
		order = "discovered_on DESC, id DESC"
	case ByTotalSizeDesc:
		order = "total_size DESC, id DESC"
	default:
		return nil, fmt.Errorf("%w: unknown ordering %d", ErrInvalidMetadata, int(query.OrderBy))
	}

	// Substring search: the pattern is escaped in Go and passed as a bound
	// parameter — user input is never interpolated into SQL text. The
	// escaping makes `%`, `_` and `\` literals; the trigram GIN index
	// accelerates the ILIKE.
	sql := `SELECT id, info_hash, name, total_size, discovered_on, file_count
        FROM magneticod.torrents`
	var args []any
	if query.SearchString != "" {
		pattern := "%" + escapeLikePattern(query.SearchString) + "%"
		sql += fmt.Sprintf(" WHERE name ILIKE $%d", len(args)+1)
		args = append(args, pattern)
	}
	sql += " ORDER BY " + order
	sql += fmt.Sprintf(" LIMIT $%d", len(args)+1)
	args = append(args, int32(query.MaxTuples))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStorageIO, err)
	}
	defer rows.Close()

	var out []Torrent
	for rows.Next() {
		var t Torrent
		if err := rows.Scan(&t.ID, &t.InfoHash, &t.Name, &t.TotalSize, &t.DiscoveredOn, &t.FileCount); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrStorageIO, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStorageIO, err)
	}
	return out, nil
}

// escapeLikePattern escapes the ILIKE metacharacters so the search string
// matches literally.
func escapeLikePattern(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(s)
}

func (s *pgxStore) GetStatistics(ctx context.Context) (Statistics, error) {
	// SUM(BIGINT) is numeric in PostgreSQL; cast to bigint and scan as
	// int64. Negative values are impossible (CHECK constraints), so the
	// conversion to uint64 is lossless for realizable states.
	var torrents, files, totalSize int64
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*)::bigint,
            COALESCE(SUM(file_count), 0)::bigint,
            COALESCE(SUM(total_size), 0)::bigint
        FROM magneticod.torrents`,
	).Scan(&torrents, &files, &totalSize)
	if err != nil {
		return Statistics{}, fmt.Errorf("%w: %w", ErrStorageIO, err)
	}
	return Statistics{Torrents: uint64(torrents), Files: uint64(files), TotalSize: uint64(totalSize)}, nil
}

func (s *pgxStore) DoesTorrentExist(ctx context.Context, infoHash []byte) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM magneticod.torrents WHERE info_hash = $1)`, infoHash,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrStorageIO, err)
	}
	return exists, nil
}

func (s *pgxStore) Close() error {
	s.encoder.Close()
	s.pool.Close()
	return nil
}
