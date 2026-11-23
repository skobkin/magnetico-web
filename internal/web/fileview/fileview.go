// Package fileview is the shared file-view service behind the web UI's JSON
// endpoints and the future public API v2 (docs/specs/web-v1.md §6.4, §7). It
// owns eligibility for full-tree delivery, positional directory navigation,
// pagination and the JSON serialization, on top of filecache (Arena index,
// LRU, weighted decode gate). Authentication adapters differ; this behaviour
// does not.
//
// Two operations are offered, independent of each other:
//
//   - Tree: the whole hierarchy in one document, only for torrents that
//     qualify. Eligibility is a cheap, conservative pre-check (Decide); the
//     exact serialized size cap is the authority and is enforced before any
//     byte is handed to the caller.
//   - Dir: one bounded page of one directory, addressed by a deterministic
//     positional path, available for every torrent.
//
// Logs never carry names or paths; only ids, outcome categories and counts.
package fileview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/filecache"
	"github.com/skobkin/magnetico-web/internal/web/filetree"
	"github.com/skobkin/magnetico-web/internal/web/gate"
	"github.com/skobkin/magnetico-web/persistence"
)

// SchemaVersion is the "version" member of every document.
const SchemaVersion = 1

// Mode is the browsing mode the backend selects.
type Mode string

const (
	// ModeTree: the complete tree may be fetched in one response.
	ModeTree Mode = "tree"
	// ModeDirs: use paginated directory navigation.
	ModeDirs Mode = "dirs"
)

// Decision is the server's initial eligibility verdict.
type Decision struct {
	Mode   Mode
	Reason string // "ok", "file_count", "estimate" or "unknown"
}

// FullTreeAvailable is the API v2 full_tree_available value.
func (d Decision) FullTreeAvailable() bool { return d.Mode == ModeTree }

// UnavailableError: full-tree delivery is not available for this torrent.
// Reason is one of "file_count", "estimate" or "size" (the exact cap).
type UnavailableError struct{ Reason string }

func (e *UnavailableError) Error() string {
	return "fileview: full tree unavailable (" + e.Reason + ")"
}

// Source is the persistence subset used for the header peek.
type Source interface {
	FileHeader(ctx context.Context, id int32) (header []byte, storedLen int, err error)
	Limits() codec.Limits
}

// Files is the cache subset used for index access.
type Files interface {
	Get(ctx context.Context, id int32) (filecache.Result, error)
}

// Config holds the thresholds; zero values take the documented defaults.
type Config struct {
	// MaxTreeFiles: torrents with more files never get a full tree.
	MaxTreeFiles int
	// MaxTreeEstimate: upper limit of Estimate for the pre-check.
	MaxTreeEstimate int64
	// MaxTreeBytes: hard serialized-size cap of a tree response.
	MaxTreeBytes int
	// MaxPageBytes: hard serialized-size cap of a directory page.
	MaxPageBytes int
	// EncodeSlots bounds concurrent encoded responses; the encode gate holds
	// EncodeSlots × MaxTreeBytes bytes of buffer budget.
	EncodeSlots int
	QueueWait   time.Duration
	MaxWaiters  int
	// PeekTimeout bounds the header peek of Decide.
	PeekTimeout time.Duration
	Logger      *slog.Logger
}

// Defaults (docs/wip/07F-web-ui-report.md records the measurements).
const (
	DefaultMaxTreeFiles    = 4_000
	DefaultMaxTreeEstimate = 1 << 20
	DefaultMaxTreeBytes    = 4 << 20
	DefaultMaxPageBytes    = 1 << 20
	DefaultEncodeSlots     = 8
	DefaultQueueWait       = 2 * time.Second
	DefaultMaxWaiters      = 32
	DefaultPeekTimeout     = time.Second

	// MaxLimit is the largest page size (the API v2 cap, spec §7.2).
	MaxLimit = 1000
	// DefaultLimit is the page size used when none is given.
	DefaultLimit = 160
)

// Service implements the shared file-view behaviour.
type Service struct {
	files  Files
	src    Source
	cfg    Config
	enc    *gate.Gate
	idxLim filetree.Limits
	log    *slog.Logger
}

// New builds a Service.
func New(files Files, src Source, cfg Config) (*Service, error) {
	if cfg.MaxTreeFiles <= 0 {
		cfg.MaxTreeFiles = DefaultMaxTreeFiles
	}
	if cfg.MaxTreeEstimate <= 0 {
		cfg.MaxTreeEstimate = DefaultMaxTreeEstimate
	}
	if cfg.MaxTreeBytes <= 0 {
		cfg.MaxTreeBytes = DefaultMaxTreeBytes
	}
	if cfg.MaxPageBytes <= 0 {
		cfg.MaxPageBytes = DefaultMaxPageBytes
	}
	if cfg.EncodeSlots <= 0 {
		cfg.EncodeSlots = DefaultEncodeSlots
	}
	if cfg.QueueWait <= 0 {
		cfg.QueueWait = DefaultQueueWait
	}
	if cfg.MaxWaiters <= 0 {
		cfg.MaxWaiters = DefaultMaxWaiters
	}
	if cfg.PeekTimeout <= 0 {
		cfg.PeekTimeout = DefaultPeekTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.MaxPageBytes > cfg.MaxTreeBytes {
		return nil, errors.New("fileview: MaxPageBytes exceeds MaxTreeBytes")
	}
	g, err := gate.New(int64(cfg.EncodeSlots)*int64(cfg.MaxTreeBytes), cfg.MaxWaiters, cfg.QueueWait)
	if err != nil {
		return nil, err
	}
	return &Service{files: files, src: src, cfg: cfg, enc: g, idxLim: filetree.DefaultLimits, log: cfg.Logger}, nil
}

// Config returns the effective configuration.
func (s *Service) Config() Config { return s.cfg }

// perFileJSON is the conservative per-file JSON overhead used by Estimate.
const perFileJSON = 96

// Estimate is the cheap serialized-size prediction of a full tree. It is
// not an upper bound: prefix-delta and trie payloads can expand well beyond
// the declared payload length. The exact cap in Tree is authoritative.
func Estimate(files int, declared uint64) uint64 {
	return perFileJSON*uint64(files) + declared
}

// Decide is the eligibility pre-check. It never decodes a file list or
// builds an index: it reads the torrent's file count and, for multi-file
// torrents within the count limit, the blob's 12-byte header (one TOAST
// page). When the peek fails, the verdict is ModeDirs with Reason "unknown"
// and the error is returned only so the caller can log its category.
func (s *Service) Decide(ctx context.Context, t persistence.Torrent) (Decision, error) {
	files := int(t.FileCount)
	if files <= 1 {
		return Decision{Mode: ModeTree, Reason: "ok"}, nil
	}
	if files > s.cfg.MaxTreeFiles {
		return Decision{Mode: ModeDirs, Reason: "file_count"}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.PeekTimeout)
	defer cancel()
	hdr, stored, err := s.src.FileHeader(ctx, t.ID)
	if err != nil {
		return Decision{Mode: ModeDirs, Reason: "unknown"}, err
	}
	_, declared, err := codec.PeekDecodedSize(hdr, stored, s.src.Limits())
	if err != nil {
		return Decision{Mode: ModeDirs, Reason: "unknown"}, fmt.Errorf("%w: %w", persistence.ErrCorruptBlob, err)
	}
	if int64(Estimate(files, declared)) > s.cfg.MaxTreeEstimate {
		return Decision{Mode: ModeDirs, Reason: "estimate"}, nil
	}
	return Decision{Mode: ModeTree, Reason: "ok"}, nil
}

// Body is an encoded response held within the encode budget. Release must be
// called exactly once after the bytes were written (or abandoned); it
// returns the buffer and the budget.
type Body struct {
	b       []byte
	release func()
	once    sync.Once
}

// Bytes returns the document. It is valid until Release.
func (b *Body) Bytes() []byte { return b.b }

// Release frees the budget and recycles small buffers.
func (b *Body) Release() {
	b.once.Do(func() {
		putBuf(b.b)
		b.b = nil
		b.release()
	})
}

const poolMaxCap = 256 << 10

var bufPool = sync.Pool{New: func() any { return make([]byte, 0, 16<<10) }}

func getBuf() []byte { return bufPool.Get().([]byte)[:0] }

// putBuf drops large buffers instead of pooling them so that a few big
// responses do not pin memory indefinitely.
func putBuf(b []byte) {
	if cap(b) <= poolMaxCap {
		bufPool.Put(b[:0]) //nolint:staticcheck // slice header boxing is acceptable here
	}
}

// Tree returns the full-tree document of t, or *UnavailableError when the
// torrent is not eligible or the exact serialized size exceeds the cap.
// Nothing is returned (and so nothing can be sent) in either case.
func (s *Service) Tree(ctx context.Context, t persistence.Torrent) (*Body, error) {
	d, err := s.Decide(ctx, t)
	if err != nil {
		return nil, err
	}
	if d.Mode != ModeTree {
		return nil, &UnavailableError{Reason: d.Reason}
	}
	res, err := s.files.Get(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if res.Index.Files() > s.cfg.MaxTreeFiles {
		return nil, &UnavailableError{Reason: "file_count"}
	}
	release, err := s.enc.Acquire(ctx, int64(s.cfg.MaxTreeBytes))
	if err != nil {
		return nil, err
	}
	w := &jsonBuf{b: getBuf(), max: s.cfg.MaxTreeBytes}
	if err := encodeTree(ctx, w, res.Index); err != nil {
		putBuf(w.b)
		release()
		return nil, err
	}
	if w.over {
		putBuf(w.b)
		release()
		return nil, &UnavailableError{Reason: "size"}
	}
	return &Body{b: w.b, release: release}, nil
}

// Page is a validated page request.
type Page struct {
	Offset int
	Limit  int
}

// ParsePage validates offset and limit text. Empty strings select the
// defaults (offset 0, DefaultLimit). Values must be canonical non-negative
// decimals; limit must be 1..MaxLimit.
func ParsePage(offset, limit string) (Page, error) {
	p := Page{Limit: DefaultLimit}
	if offset != "" {
		n, ok := canonicalInt(offset, 9)
		if !ok {
			return Page{}, ErrBadPage
		}
		p.Offset = n
	}
	if limit != "" {
		n, ok := canonicalInt(limit, 4)
		if !ok || n < 1 || n > MaxLimit {
			return Page{}, ErrBadPage
		}
		p.Limit = n
	}
	return p, nil
}

func canonicalInt(s string, maxDigits int) (int, bool) {
	if s == "" || len(s) > maxDigits || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// Dir returns one page of the directory at positional path. Available for
// every torrent, whatever its size.
func (s *Service) Dir(ctx context.Context, id int32, path string, pg Page) (*Body, error) {
	comps, err := ParsePath(path, s.idxLim.MaxDepth)
	if err != nil {
		return nil, err
	}
	if pg.Limit < 1 || pg.Limit > MaxLimit || pg.Offset < 0 {
		return nil, ErrBadPage
	}
	res, err := s.files.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	idx := res.Index
	d, err := resolve(idx, comps)
	if err != nil {
		return nil, err
	}
	info := idx.Info(d)
	entries := idx.Page(d, pg.Offset, pg.Limit, make([]filetree.Entry, 0, min(pg.Limit, 256)))

	// Upper-bound the encoded size before encoding: six bytes per name byte
	// is the worst JSON escape, plus fixed overhead per entry. Trim the page
	// (never below one entry) so that the bound fits the page cap; the
	// response's next_offset tells the client where to continue.
	const entryOverhead, docOverhead = 200, 512
	ancestors := make([]ancestor, 0, len(comps))
	bound := docOverhead + len(path)
	cur := filetree.Root
	for i, c := range comps {
		cur, _ = idx.Child(cur, c)
		a := ancestor{name: idx.DirName(cur), path: FormatPath(comps[:i+1])}
		bound += 6*len(a.name) + entryOverhead + len(a.path)
		ancestors = append(ancestors, a)
	}
	keep := 0
	for _, e := range entries {
		eb := 6*len(e.Name) + entryOverhead + len(path) + 8
		if keep > 0 && bound+eb > s.cfg.MaxPageBytes {
			break
		}
		bound += eb
		keep++
	}
	entries = entries[:keep]
	if bound > s.cfg.MaxPageBytes {
		bound = s.cfg.MaxPageBytes // a single entry always fits (name ≤ 64 KiB)
	}
	release, err := s.enc.Acquire(ctx, int64(bound))
	if err != nil {
		return nil, err
	}
	w := &jsonBuf{b: getBuf(), max: s.cfg.MaxPageBytes}
	encodeDirPage(w, idx, d, path, ancestors, info, pg, entries)
	if w.over { // cannot happen: the bound covers the worst case
		putBuf(w.b)
		release()
		return nil, errors.New("fileview: directory page exceeds its cap")
	}
	return &Body{b: w.b, release: release}, nil
}

// Stats reports the encode gate occupancy.
type Stats struct{ InUse int64 }

// Stats returns the current encode budget in use.
func (s *Service) Stats() Stats { return Stats{InUse: s.enc.InUse()} }
