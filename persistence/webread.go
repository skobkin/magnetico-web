package persistence

// Read-only queries of the web service (docs/wip/07E-web-search-details-plan.md
// §3): reads by id, keyset search, and the split file-list reads (cheap
// identity, header for memory admission, blob). They run over a caller-owned
// pool, never write, and execute inside a READ ONLY transaction whose
// statement_timeout never exceeds the remaining request budget.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/codec"
)

// Errors of the web read path (in addition to errors.go).
var (
	// ErrStatementTimeout: PostgreSQL cancelled the statement at its
	// statement_timeout while the request context was still live.
	ErrStatementTimeout = errors.New("statement timeout")
	// ErrBudgetExhausted: too little of the request deadline remained to start a
	// statement. It also matches context.DeadlineExceeded.
	ErrBudgetExhausted = errors.New("request budget exhausted before the statement could start")
	// ErrAnchorNotFound: the row an anchor-only cursor refers to no longer exists.
	ErrAnchorNotFound = errors.New("cursor anchor row not found")
	// ErrBlobChanged: the blob is missing or its row version differs from the
	// identity the caller validated; the caller may retry from the identity.
	// (An unchanged blob over the stored-size limit is ErrCorruptBlob, not this.)
	ErrBlobChanged = errors.New("file list blob changed")
	// ErrInvalidSearch: a SearchSpec that violates the documented bounds.
	ErrInvalidSearch = errors.New("invalid search specification")
)

// Bounds of a search.
const (
	MaxSearchLimit = 100
	MaxSearchTerms = 8
)

// Statement-timeout budget constants (plan §5.2).
const (
	minStatement      = 100 * time.Millisecond
	minMargin         = 50 * time.Millisecond
	maxMargin         = 500 * time.Millisecond
	noDeadlineTimeout = 60 * time.Second
)

// StatementTimeout derives the PostgreSQL statement_timeout from the time
// remaining before the request deadline. ok is false when too little remains to
// start a statement; otherwise the result is positive and strictly below
// remaining. margin = clamp(remaining/10, 50 ms, 500 ms).
func StatementTimeout(remaining time.Duration) (d time.Duration, ok bool) {
	margin := min(max(remaining/10, minMargin), maxMargin)
	if remaining <= margin+minStatement {
		return 0, false
	}
	return (remaining - margin).Truncate(time.Millisecond), true
}

// Reader is the read-only torrent query surface of the web service.
type Reader struct {
	pool   *pgxpool.Pool
	limits codec.Limits
	now    func() time.Time
}

// NewReader returns a Reader over a caller-owned pool (the caller closes it).
func NewReader(pool *pgxpool.Pool, limits codec.Limits) *Reader {
	return &Reader{pool: pool, limits: limits, now: time.Now}
}

// readTx runs fn in a READ ONLY transaction with a statement_timeout strictly
// below the remaining deadline. It refuses to start when the budget is gone.
func (r *Reader) readTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	// Refuse early when nothing useful remains, before touching the pool.
	if _, ok := StatementTimeout(r.remaining(ctx)); !ok {
		return fmt.Errorf("%w: %w", ErrBudgetExhausted, context.DeadlineExceeded)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return r.classify(ctx, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// Recompute after BEGIN so the time already spent (pool wait, round trip)
	// is subtracted: the timeout stays strictly below the remaining budget.
	st, ok := StatementTimeout(r.remaining(ctx))
	if !ok {
		return fmt.Errorf("%w: %w", ErrBudgetExhausted, context.DeadlineExceeded)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(st.Milliseconds(), 10)); err != nil {
		return r.classify(ctx, err)
	}
	if err := fn(tx); err != nil {
		return r.classify(ctx, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return r.classify(ctx, err)
	}
	return nil
}

func (r *Reader) remaining(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return dl.Sub(r.now())
	}
	return noDeadlineTimeout
}

// classify maps a failure: typed errors pass through; a dead request context
// wins (cancel vs deadline stay distinguishable); SQLSTATE 57014 with a live
// context is a statement timeout; everything else is ErrStorageIO.
func (r *Reader) classify(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, ErrTorrentNotFound), errors.Is(err, ErrIntegrityMissingBlob),
		errors.Is(err, ErrAnchorNotFound), errors.Is(err, ErrBlobChanged), errors.Is(err, ErrInvalidSearch),
		errors.Is(err, ErrCorruptBlob):
		return err
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "57014" {
		return ErrStatementTimeout
	}
	return fmt.Errorf("%w: %w", ErrStorageIO, err)
}

const torrentColumns = `id, info_hash, name, total_size, discovered_on, file_count`

func scanTorrent(row pgx.Row) (Torrent, error) {
	var t Torrent
	err := row.Scan(&t.ID, &t.InfoHash, &t.Name, &t.TotalSize, &t.DiscoveredOn, &t.FileCount)
	return t, err
}

// TorrentByID reads one torrents row. It never touches blobs.
func (r *Reader) TorrentByID(ctx context.Context, id int32) (Torrent, error) {
	var t Torrent
	err := r.readTx(ctx, func(tx pgx.Tx) error {
		var err error
		t, err = scanTorrent(tx.QueryRow(ctx, `SELECT `+torrentColumns+` FROM magneticod.torrents WHERE id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTorrentNotFound
		}
		return err
	})
	return t, err
}

// TorrentByInfoHash is the exact lookup for a 40-hex query: zero or one row.
func (r *Reader) TorrentByInfoHash(ctx context.Context, infoHash []byte) (Torrent, bool, error) {
	var t Torrent
	found := false
	err := r.readTx(ctx, func(tx pgx.Tx) error {
		var err error
		t, err = scanTorrent(tx.QueryRow(ctx, `SELECT `+torrentColumns+` FROM magneticod.torrents WHERE info_hash = $1`, infoHash))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return t, found, err
}

// FileIdentity identifies the stored file list of a torrent without reading it.
type FileIdentity struct {
	Torrent
	HasBlob bool
	Version string // blob row xmin, "" without a blob
}

// FileIdentity is the cheap per-request lookup (cache hit path): the torrents
// row plus the blob's presence and row version (xmin). It transfers no blob
// bytes and no header. The blob length is not part of the identity: the row
// version (xmin) already identifies the stored value, and the S1 measurement
// (docs/wip/07E-s1-measurements.txt) showed the lookup touches no TOAST block
// (EXPLAIN BUFFERS: shared hit=2) whether or not octet_length is selected, so
// this is a simplification, not a cost avoidance.
func (r *Reader) FileIdentity(ctx context.Context, id int32) (FileIdentity, error) {
	var fi FileIdentity
	err := r.readTx(ctx, func(tx pgx.Tx) error {
		var ver *string
		err := tx.QueryRow(ctx, `SELECT t.id, t.info_hash, t.name, t.total_size, t.discovered_on, t.file_count,
		        b.torrent_id IS NOT NULL, b.xmin::text
		    FROM magneticod.torrents t
		    LEFT JOIN magneticod.torrent_file_blobs b ON b.torrent_id = t.id
		    WHERE t.id = $1`, id).Scan(&fi.ID, &fi.InfoHash, &fi.Name, &fi.TotalSize, &fi.DiscoveredOn, &fi.FileCount,
			&fi.HasBlob, &ver)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTorrentNotFound
		}
		if err != nil {
			return err
		}
		if ver != nil {
			fi.Version = *ver
		}
		return nil
	})
	return fi, err
}

// FileHeader returns the first codec.PeekHeaderLen bytes of the blob and its
// stored length (cache miss path only, to compute the admission weight). S1
// measurement: one TOAST block per call for a 24 MiB external value.
func (r *Reader) FileHeader(ctx context.Context, id int32) (header []byte, storedLen int, err error) {
	err = r.readTx(ctx, func(tx pgx.Tx) error {
		var n int32
		err := tx.QueryRow(ctx, `SELECT substring(data from 1 for `+strconv.Itoa(codec.PeekHeaderLen)+`), octet_length(data)
		    FROM magneticod.torrent_file_blobs WHERE torrent_id = $1`, id).Scan(&header, &n)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrIntegrityMissingBlob
		}
		storedLen = int(n)
		return err
	})
	return header, storedLen, err
}

// FileBlob fetches the blob of torrent id if its row version is still ver.
//
//   - Missing blob or a different row version: ErrBlobChanged (retryable after
//     refreshing the identity).
//   - Same version but stored larger than the codec's stored-length limit: a
//     non-retryable ErrCorruptBlob wrapping *codec.LimitError (LimitStoredBytes).
//     The size predicate runs in SQL, so the oversized bytes are never
//     transferred.
//
// There is no retry loop here; the caller decides.
func (r *Reader) FileBlob(ctx context.Context, id int32, ver string) ([]byte, error) {
	var blob []byte
	limit := min(r.limits.MaxStoredBytes, math.MaxInt32)
	err := r.readTx(ctx, func(tx pgx.Tx) error {
		var cur string
		var n int32
		err := tx.QueryRow(ctx, `SELECT xmin::text, octet_length(data),
		        CASE WHEN xmin::text = $2 AND octet_length(data) <= $3 THEN data END
		    FROM magneticod.torrent_file_blobs WHERE torrent_id = $1`,
			id, ver, int32(limit)).Scan(&cur, &n, &blob)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBlobChanged
		}
		if err != nil {
			return err
		}
		if cur != ver {
			blob = nil
			return ErrBlobChanged
		}
		if int(n) > limit {
			blob = nil
			return fmt.Errorf("%w: %w", ErrCorruptBlob, &codec.LimitError{
				Limit: codec.LimitStoredBytes, Actual: uint64(n), Allowed: uint64(limit)})
		}
		return nil
	})
	return blob, err
}

// Limits returns the codec limits this Reader decodes under.
func (r *Reader) Limits() codec.Limits { return r.limits }

// SingleFileList is the intentional no-blob representation: a one-file torrent
// whose file is (name, total_size). A multi-file torrent without a blob is
// ErrIntegrityMissingBlob, never an empty list.
func SingleFileList(t Torrent) ([]File, error) {
	if t.FileCount == 1 {
		return []File{{Path: t.Name, Size: t.TotalSize}}, nil
	}
	return nil, ErrIntegrityMissingBlob
}

// DecodeFileList decodes a stored blob and verifies it against its torrents
// row (count, checked size sum, sum == total_size). It returns the whole list
// or ErrCorruptBlob, never a partial list. Only Torrent.FileCount and
// Torrent.TotalSize are used.
func DecodeFileList(t Torrent, blob []byte, limits codec.Limits) ([]File, error) {
	files, err := codec.Decode(blob, limits)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptBlob, err)
	}
	if len(files) != int(t.FileCount) {
		return nil, fmt.Errorf("%w: decoded %d files, torrents row says %d",
			ErrCorruptBlob, len(files), t.FileCount)
	}
	var sum int64
	for _, f := range files {
		if sum > math.MaxInt64-f.Size {
			return nil, fmt.Errorf("%w: decoded file sizes overflow", ErrCorruptBlob)
		}
		sum += f.Size
	}
	if sum != t.TotalSize {
		return nil, fmt.Errorf("%w: decoded sizes sum to %d, torrents row says %d",
			ErrCorruptBlob, sum, t.TotalSize)
	}
	return files, nil
}

// ---- keyset search ----------------------------------------------------------

// SortKey selects the sort column.
type SortKey int

const (
	SortDiscovered SortKey = iota
	SortSize
	SortName
)

// Anchor is the keyset position of a cursor: the (sort value, id) of a
// boundary row. With ByID only ID is set and the sort value is read from the
// row (names too long to carry in a cursor); a missing row is ErrAnchorNotFound.
type Anchor struct {
	ID   int32
	Int  int64  // discovered_on or total_size
	Name string // name sort
	ByID bool
}

// SearchSpec is one search page request. Terms are already normalised by the
// search service (usable words, AND, literal substrings); the SQL treats each
// as a literal, case-insensitive substring.
type SearchSpec struct {
	Terms    []string
	Sort     SortKey
	Desc     bool    // display direction
	Limit    int     // page size, 1..MaxSearchLimit
	After    *Anchor // exclusive position in traversal order; nil = first page
	Backward bool    // traverse against the display direction (After is then after the page)
}

// SearchPage is one result page in display order. Extra reports that the
// page-size+1-th row existed in the traversal direction (it is not returned).
type SearchPage struct {
	Items []Torrent
	Extra bool
}

func (s SortKey) column() (string, bool) {
	switch s {
	case SortDiscovered:
		return "discovered_on", true
	case SortSize:
		return "total_size", true
	case SortName:
		return "name", true
	}
	return "", false
}

func validSearchText(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }

// BuildSearchSQL renders the keyset query for spec with a resolved (value)
// anchor. All user data are bind parameters; the SQL text depends only on the
// term count, sort, effective direction and whether an anchor is present. It
// contains no COUNT and no OFFSET.
func BuildSearchSQL(spec SearchSpec) (string, []any, error) {
	return buildSearchSQL(spec, false)
}

// buildSearchSQL: expanded selects the OR-expanded keyset predicate instead of
// the row-value comparison (measured in the S1 EXPLAIN review).
func buildSearchSQL(spec SearchSpec, expanded bool) (string, []any, error) {
	col, ok := spec.Sort.column()
	if !ok || spec.Limit < 1 || spec.Limit > MaxSearchLimit || len(spec.Terms) > MaxSearchTerms {
		return "", nil, ErrInvalidSearch
	}
	var args []any
	var where []string
	for _, term := range spec.Terms {
		if term == "" || !validSearchText(term) {
			return "", nil, ErrInvalidSearch
		}
		args = append(args, "%"+escapeLikePattern(term)+"%")
		where = append(where, fmt.Sprintf("name ILIKE $%d", len(args)))
	}
	effDesc := spec.Desc != spec.Backward
	dir, op := "ASC", ">"
	if effDesc {
		dir, op = "DESC", "<"
	}
	if a := spec.After; a != nil {
		var key any
		if spec.Sort == SortName {
			if a.ByID || !validSearchText(a.Name) {
				return "", nil, ErrInvalidSearch
			}
			key = a.Name
		} else {
			key = a.Int
		}
		args = append(args, key, a.ID)
		k, i := len(args)-1, len(args)
		if expanded {
			where = append(where, fmt.Sprintf("(%s %s $%d OR (%s = $%d AND id %s $%d))", col, op, k, col, k, op, i))
		} else {
			where = append(where, fmt.Sprintf("(%s, id) %s ($%d, $%d)", col, op, k, i))
		}
	}
	var b strings.Builder
	b.WriteString("SELECT " + torrentColumns + " FROM magneticod.torrents")
	if len(where) > 0 {
		b.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	args = append(args, int64(spec.Limit)+1)
	fmt.Fprintf(&b, " ORDER BY %s %s, id %s LIMIT $%d", col, dir, dir, len(args))
	return b.String(), args, nil
}

// SearchTorrents runs one keyset page. Rows are returned in display order.
func (r *Reader) SearchTorrents(ctx context.Context, spec SearchSpec) (SearchPage, error) {
	return r.searchTorrents(ctx, spec, false)
}

func (r *Reader) searchTorrents(ctx context.Context, spec SearchSpec, expanded bool) (SearchPage, error) {
	var page SearchPage
	err := r.readTx(ctx, func(tx pgx.Tx) error {
		if a := spec.After; a != nil && a.ByID {
			col, ok := spec.Sort.column()
			if !ok {
				return ErrInvalidSearch
			}
			resolved := Anchor{ID: a.ID}
			var dst any = &resolved.Int
			if spec.Sort == SortName {
				dst = &resolved.Name
			}
			err := tx.QueryRow(ctx, `SELECT `+col+` FROM magneticod.torrents WHERE id = $1`, a.ID).Scan(dst)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAnchorNotFound
			}
			if err != nil {
				return err
			}
			spec.After = &resolved
		}
		sql, args, err := buildSearchSQL(spec, expanded)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var items []Torrent
		for rows.Next() {
			var t Torrent
			if err := rows.Scan(&t.ID, &t.InfoHash, &t.Name, &t.TotalSize, &t.DiscoveredOn, &t.FileCount); err != nil {
				return err
			}
			items = append(items, t)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(items) > spec.Limit {
			page.Extra = true
			items = items[:spec.Limit]
		}
		if spec.Backward {
			for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
				items[i], items[j] = items[j], items[i]
			}
		}
		page.Items = items
		return nil
	})
	return page, err
}
