package fileview

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/filecache"
	"github.com/skobkin/magnetico-web/internal/web/filetree"
	"github.com/skobkin/magnetico-web/internal/web/gate"
	"github.com/skobkin/magnetico-web/persistence"
)

// ---- fakes ---------------------------------------------------------------------

type fakeTorrent struct {
	t     persistence.Torrent
	idx   *filetree.Arena
	blob  []byte
	files []codec.File
}

type fake struct {
	mu     sync.Mutex
	byID   map[int32]*fakeTorrent
	limits codec.Limits
	gets   atomic.Int64
	peeks  atomic.Int64
}

func newFake() *fake { return &fake{byID: map[int32]*fakeTorrent{}, limits: codec.DefaultLimits()} }

func canonical(files []codec.File) []codec.File {
	out := slices.Clone(files)
	slices.SortStableFunc(out, func(a, b codec.File) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		switch {
		case a.Size < b.Size:
			return -1
		case a.Size > b.Size:
			return 1
		}
		return 0
	})
	return out
}

func (f *fake) add(t *testing.T, id int32, files []codec.File) persistence.Torrent {
	t.Helper()
	files = canonical(files)
	blob, _, err := codec.Encode(files, codec.ProfileCompact, f.limits)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := filetree.BuildArena(context.Background(), files)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, x := range files {
		sum += x.Size
	}
	tor := persistence.Torrent{ID: id, InfoHash: make([]byte, 20), Name: "t", TotalSize: sum, FileCount: int32(len(files))}
	f.mu.Lock()
	f.byID[id] = &fakeTorrent{t: tor, idx: idx, blob: blob, files: files}
	f.mu.Unlock()
	return tor
}

func (f *fake) Get(_ context.Context, id int32) (filecache.Result, error) {
	f.gets.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	ft := f.byID[id]
	if ft == nil {
		return filecache.Result{}, persistence.ErrTorrentNotFound
	}
	return filecache.Result{Torrent: ft.t, Index: ft.idx, Hit: true}, nil
}

func (f *fake) FileHeader(_ context.Context, id int32) ([]byte, int, error) {
	f.peeks.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	ft := f.byID[id]
	if ft == nil {
		return nil, 0, persistence.ErrIntegrityMissingBlob
	}
	return ft.blob[:min(len(ft.blob), codec.PeekHeaderLen)], len(ft.blob), nil
}

func (f *fake) Limits() codec.Limits { return f.limits }

func newService(t *testing.T, f *fake, cfg Config) *Service {
	t.Helper()
	s, err := New(f, f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func bg() context.Context { return context.Background() }

// ---- decoded documents (test-side schema) --------------------------------------

type nameJSON struct {
	Name       *string `json:"name"`
	NameBase64 string  `json:"name_base64"`
}

func (n nameJSON) bytes(t *testing.T) string {
	t.Helper()
	if n.Name != nil {
		if n.NameBase64 != "" {
			t.Fatal("both name and name_base64 set")
		}
		return *n.Name
	}
	b, err := base64.StdEncoding.DecodeString(n.NameBase64)
	if err != nil || n.NameBase64 == "" {
		t.Fatalf("name null without valid name_base64 (%v)", err)
	}
	return string(b)
}

type treeFile struct {
	nameJSON
	Size      int64 `json:"size"`
	FileIndex int   `json:"file_index"`
	Duplicate bool  `json:"duplicate"`
}

type treeDir struct {
	nameJSON
	FileCount int        `json:"file_count"`
	Size      int64      `json:"size"`
	Dirs      []*treeDir `json:"dirs"`
	Files     []treeFile `json:"files"`
}

type treeDoc struct {
	Version   int     `json:"version"`
	FileCount int     `json:"file_count"`
	Size      int64   `json:"size"`
	Root      treeDir `json:"root"`
}

func decodeTree(t *testing.T, b []byte) treeDoc {
	t.Helper()
	if !json.Valid(b) {
		t.Fatalf("tree is not valid JSON: %.200s", b)
	}
	var d treeDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// flatten rebuilds the canonical path list of a tree document.
func flatten(t *testing.T, d *treeDir, prefix string, out *[]treeFileAt) {
	t.Helper()
	for _, c := range d.Dirs {
		flatten(t, c, prefix+c.bytes(t)+"/", out)
	}
	for _, f := range d.Files {
		*out = append(*out, treeFileAt{path: prefix + f.bytes(t), f: f})
	}
}

type treeFileAt struct {
	path string
	f    treeFile
}

type pageEntry struct {
	Type string `json:"type"`
	nameJSON
	Path       string `json:"path"`
	FileCount  int    `json:"file_count"`
	Size       int64  `json:"size"`
	DirCount   int    `json:"dir_count"`
	EntryCount int    `json:"entry_count"`
	FileIndex  int    `json:"file_index"`
	Duplicate  bool   `json:"duplicate"`
}

type pageDoc struct {
	Version   int    `json:"version"`
	Path      string `json:"path"`
	Ancestors []struct {
		nameJSON
		Path string `json:"path"`
	} `json:"ancestors"`
	Dir struct {
		FileCount  int `json:"file_count"`
		DirCount   int `json:"dir_count"`
		EntryCount int `json:"entry_count"`
	}
	Offset     int         `json:"offset"`
	Limit      int         `json:"limit"`
	NextOffset *int        `json:"next_offset"`
	Entries    []pageEntry `json:"entries"`
}

func decodePage(t *testing.T, b []byte) pageDoc {
	t.Helper()
	if !json.Valid(b) {
		t.Fatalf("page is not valid JSON: %.200s", b)
	}
	var d pageDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// ---- corpus --------------------------------------------------------------------

func adversarial() []codec.File {
	return []codec.File{
		{Path: "plain/a.txt", Size: 1},
		{Path: "plain/a.txt", Size: 1}, // duplicate
		{Path: "plain/a.txt", Size: 2}, // same path, other size
		{Path: "ctl/\x00nul", Size: 3},
		{Path: "ctl/tab\there", Size: 4},
		{Path: "ctl/bell\a", Size: 5},
		{Path: "ctl/del\x7f", Size: 6},
		{Path: "q/\"quote\\back<amp>&", Size: 7},
		{Path: "uni/ sep ", Size: 8},
		{Path: "uni/real�fffd", Size: 9},
		{Path: "bidi/‮evil", Size: 10},
		{Path: "bad/\xff\xfe", Size: 11},
		{Path: "bad\xc3/(dir)/x", Size: 12},
		{Path: "bad\xc3/(dir)/x", Size: 12},
		{Path: "/lead", Size: 13},
		{Path: "a//b", Size: 14},
		{Path: "../up", Size: 15},
		{Path: "./dot", Size: 16},
		{Path: "top.bin", Size: 17},
		{Path: "ünï/čödé/файл.txt", Size: 18},
	}
}

func manyFiles(n int) []codec.File {
	fs := make([]codec.File, n)
	for i := range fs {
		fs[i] = codec.File{Path: fmt.Sprintf("s%02d/d%02d/f%05d.bin", i%9, i%13, i), Size: int64(i + 1)}
	}
	return fs
}

// ---- tests ---------------------------------------------------------------------

func TestTreeRoundTripsAdversarialNames(t *testing.T) {
	t.Parallel()
	f := newFake()
	tor := f.add(t, 1, adversarial())
	s := newService(t, f, Config{})

	body, err := s.Tree(bg(), tor)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Release()
	doc := decodeTree(t, body.Bytes())
	if doc.Version != 1 || doc.FileCount != len(adversarial()) {
		t.Fatalf("header: %+v", doc)
	}
	var got []treeFileAt
	flatten(t, &doc.Root, "", &got)

	// Every original path appears exactly as often, byte for byte, with its
	// size and canonical file_index (the duplicate identity).
	want := map[string]int{}
	for _, x := range f.byID[1].files {
		want[fmt.Sprintf("%q/%d", x.Path, x.Size)]++
	}
	seen := map[int]bool{}
	for _, g := range got {
		want[fmt.Sprintf("%q/%d", g.path, g.f.Size)]--
		if seen[g.f.FileIndex] {
			t.Fatalf("file_index %d repeated", g.f.FileIndex)
		}
		seen[g.f.FileIndex] = true
		orig := f.byID[1].files[g.f.FileIndex]
		if orig.Path != g.path || orig.Size != g.f.Size {
			t.Fatalf("file_index %d points at %q/%d, document says %q/%d", g.f.FileIndex, orig.Path, orig.Size, g.path, g.f.Size)
		}
	}
	for k, n := range want {
		if n != 0 {
			t.Fatalf("path %s count off by %d", k, n)
		}
	}
	if strings.ContainsRune(string(body.Bytes()), '�') && !strings.Contains(adversarialJoined(), "�") {
		t.Fatal("U+FFFD substituted into output")
	}
}

func adversarialJoined() string {
	var b strings.Builder
	for _, f := range adversarial() {
		b.WriteString(f.Path)
	}
	return b.String()
}

func TestDuplicatesFlaggedAndDistinct(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.add(t, 1, adversarial())
	s := newService(t, f, Config{})
	body, err := s.Dir(bg(), 1, "", Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Release()
	doc := decodePage(t, body.Bytes())
	// Locate "plain" and read its files.
	var plain string
	for _, e := range doc.Entries {
		if e.Type == "dir" && e.nameJSON.bytes(t) == "plain" {
			plain = e.Path
		}
	}
	if plain == "" {
		t.Fatal("plain dir not listed")
	}
	pb, err := s.Dir(bg(), 1, plain, Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer pb.Release()
	pd := decodePage(t, pb.Bytes())
	if len(pd.Entries) != 3 {
		t.Fatalf("want 3 distinct entries, got %d", len(pd.Entries))
	}
	dups := 0
	for _, e := range pd.Entries {
		if e.Duplicate {
			dups++
		}
	}
	if dups != 2 { // rows 2 and 3 repeat the name of the previous file row
		t.Fatalf("want 2 rows flagged duplicate, got %d", dups)
	}
}

func TestEveryDirectoryNavigableWithoutTree(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.add(t, 1, adversarial())
	s := newService(t, f, Config{})

	var visit func(path string) (files int)
	visit = func(path string) int {
		b, err := s.Dir(bg(), 1, path, Page{Limit: 7}) // small pages on purpose
		if err != nil {
			t.Fatalf("dir %q: %v", path, err)
		}
		first := decodePage(t, b.Bytes())
		b.Release()
		total, off, n := first.Dir.EntryCount, 0, 0
		for {
			pb, err := s.Dir(bg(), 1, path, Page{Offset: off, Limit: 7})
			if err != nil {
				t.Fatal(err)
			}
			d := decodePage(t, pb.Bytes())
			pb.Release()
			for _, e := range d.Entries {
				if e.Type == "dir" {
					n += visit(e.Path)
				} else {
					n++
				}
			}
			if d.NextOffset == nil {
				break
			}
			off = *d.NextOffset
		}
		_ = total
		return n
	}
	if got := visit(""); got != len(adversarial()) {
		t.Fatalf("navigation reached %d files, want %d", got, len(adversarial()))
	}
	if f.gets.Load() == 0 {
		t.Fatal("expected index access")
	}
}

func TestDirPositionalErrors(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.add(t, 1, adversarial())
	s := newService(t, f, Config{})
	for _, p := range []string{"x", "-1", "01", "1__2", "_1", "1_", "1_a", "1000000", "1 ", "0x1", "１", "999999_0"} {
		_, err := s.Dir(bg(), 1, p, Page{Limit: 10})
		if p == "999999_0" {
			if !errors.Is(err, ErrNoSuchDir) {
				t.Errorf("%q: want ErrNoSuchDir, got %v", p, err)
			}
			continue
		}
		if !errors.Is(err, ErrBadPath) {
			t.Errorf("%q: want ErrBadPath, got %v", p, err)
		}
	}
	// Components that exist as files, not directories, are out of range.
	if _, err := s.Dir(bg(), 1, "100", Page{Limit: 10}); !errors.Is(err, ErrNoSuchDir) {
		t.Errorf("out-of-range component: %v", err)
	}
	// Depth bound.
	deep := strings.TrimSuffix(strings.Repeat("0_", 257), "_")
	if _, err := s.Dir(bg(), 1, deep, Page{Limit: 10}); !errors.Is(err, ErrBadPath) {
		t.Errorf("257 components: %v", err)
	}
	if _, err := s.Dir(bg(), 99, "", Page{Limit: 10}); !errors.Is(err, persistence.ErrTorrentNotFound) {
		t.Errorf("unknown torrent: %v", err)
	}
}

func TestParsePage(t *testing.T) {
	t.Parallel()
	good := []struct {
		o, l string
		want Page
	}{
		{"", "", Page{0, DefaultLimit}}, {"0", "1", Page{0, 1}}, {"25", "1000", Page{25, 1000}}, {"999999999", "", Page{999999999, DefaultLimit}},
	}
	for _, g := range good {
		p, err := ParsePage(g.o, g.l)
		if err != nil || p != g.want {
			t.Errorf("ParsePage(%q,%q) = %+v, %v", g.o, g.l, p, err)
		}
	}
	for _, b := range [][2]string{{"-1", ""}, {"01", ""}, {"a", ""}, {"1.5", ""}, {"1000000000", ""}, {"", "0"}, {"", "1001"}, {"", "-5"}, {"", "007"}, {"", "99999"}, {"+1", ""}} {
		if _, err := ParsePage(b[0], b[1]); !errors.Is(err, ErrBadPage) {
			t.Errorf("ParsePage(%q,%q) = %v, want ErrBadPage", b[0], b[1], err)
		}
	}
}

func TestSmallTorrentUsesEitherEndpoint(t *testing.T) {
	t.Parallel()
	f := newFake()
	tor := f.add(t, 1, manyFiles(12))
	s := newService(t, f, Config{})
	d, err := s.Decide(bg(), tor)
	if err != nil || !d.FullTreeAvailable() {
		t.Fatalf("decision %+v %v", d, err)
	}
	if b, err := s.Tree(bg(), tor); err != nil {
		t.Fatal(err)
	} else {
		b.Release()
	}
	if b, err := s.Dir(bg(), 1, "", Page{Limit: 5}); err != nil {
		t.Fatal(err)
	} else {
		b.Release()
	}
}

func TestHundredFileTorrentCanPaginate(t *testing.T) {
	t.Parallel()
	f := newFake()
	tor := f.add(t, 1, manyFiles(100))
	s := newService(t, f, Config{})
	if d, _ := s.Decide(bg(), tor); !d.FullTreeAvailable() {
		t.Fatal("100 files should also be tree-eligible")
	}
	b, err := s.Dir(bg(), 1, "", Page{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	d := decodePage(t, b.Bytes())
	if len(d.Entries) != 3 || d.NextOffset == nil || *d.NextOffset != 3 || d.Dir.FileCount != 100 {
		t.Fatalf("pagination on a small torrent: %+v", d)
	}
}

func TestLargeTorrentRefusesTreeButPaginates(t *testing.T) {
	t.Parallel()
	f := newFake()
	tor := f.add(t, 1, manyFiles(400))
	s := newService(t, f, Config{MaxTreeFiles: 300})
	d, err := s.Decide(bg(), tor)
	if err != nil || d.FullTreeAvailable() || d.Reason != "file_count" {
		t.Fatalf("decision %+v %v", d, err)
	}
	gets := f.gets.Load()
	_, err = s.Tree(bg(), tor)
	var ue *UnavailableError
	if !errors.As(err, &ue) || ue.Reason != "file_count" {
		t.Fatalf("want UnavailableError(file_count), got %v", err)
	}
	if f.gets.Load() != gets {
		t.Fatal("an ineligible tree request must not touch the index (no decode)")
	}
	b, err := s.Dir(bg(), 1, "", Page{Limit: 50})
	if err != nil {
		t.Fatalf("pagination must keep working: %v", err)
	}
	b.Release()
}

func TestEstimateRefusal(t *testing.T) {
	t.Parallel()
	f := newFake()
	tor := f.add(t, 1, manyFiles(500))
	s := newService(t, f, Config{MaxTreeEstimate: 1000})
	d, err := s.Decide(bg(), tor)
	if err != nil || d.Reason != "estimate" || d.Mode != ModeDirs {
		t.Fatalf("decision %+v %v", d, err)
	}
	var ue *UnavailableError
	if _, err := s.Tree(bg(), tor); !errors.As(err, &ue) || ue.Reason != "estimate" {
		t.Fatalf("got %v", err)
	}
}

// The estimate says eligible but the exact size is over the cap: the result
// is a typed error and no body exists to send.
func TestExactCapOverridesEstimate(t *testing.T) {
	t.Parallel()
	f := newFake()
	tor := f.add(t, 1, manyFiles(800))
	probe := newService(t, f, Config{})
	pb, err := probe.Tree(bg(), tor)
	if err != nil {
		t.Fatal(err)
	}
	size := len(pb.Bytes())
	pb.Release()

	// Exactly at the cap: served. One byte below: refused.
	at := newService(t, f, Config{MaxTreeBytes: size, MaxPageBytes: 1024})
	b, err := at.Tree(bg(), tor)
	if err != nil {
		t.Fatalf("at the cap must succeed: %v", err)
	}
	if len(b.Bytes()) != size {
		t.Fatalf("size changed: %d vs %d", len(b.Bytes()), size)
	}
	b.Release()
	under := newService(t, f, Config{MaxTreeBytes: size - 1, MaxPageBytes: 1024})
	if d, _ := under.Decide(bg(), tor); !d.FullTreeAvailable() {
		t.Fatal("test premise: the estimate must say eligible")
	}
	var ue *UnavailableError
	body, err := under.Tree(bg(), tor)
	if !errors.As(err, &ue) || ue.Reason != "size" || body != nil {
		t.Fatalf("one byte over the cap: body=%v err=%v", body, err)
	}
	if under.Stats().InUse != 0 {
		t.Fatal("encode budget leaked on refusal")
	}
}

func TestDirPageByteCapTrimsAndContinues(t *testing.T) {
	t.Parallel()
	f := newFake()
	long := strings.Repeat("n", 40_000)
	var files []codec.File
	for i := 0; i < 30; i++ {
		files = append(files, codec.File{Path: fmt.Sprintf("%s%02d", long, i), Size: 1})
	}
	f.add(t, 1, files)
	s := newService(t, f, Config{MaxPageBytes: 256 << 10, MaxTreeBytes: 1 << 20})
	off, got := 0, 0
	for pages := 0; ; pages++ {
		b, err := s.Dir(bg(), 1, "", Page{Offset: off, Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Bytes()) > 256<<10 {
			t.Fatalf("page %d is %d bytes, over the cap", pages, len(b.Bytes()))
		}
		d := decodePage(t, b.Bytes())
		b.Release()
		if len(d.Entries) == 0 {
			t.Fatal("a page must always make progress")
		}
		got += len(d.Entries)
		if d.NextOffset == nil {
			break
		}
		off = *d.NextOffset
	}
	if got != 30 {
		t.Fatalf("paged %d entries, want 30", got)
	}
}

func TestDecidePeekFailureFallsBackToDirs(t *testing.T) {
	t.Parallel()
	f := newFake()
	tor := f.add(t, 1, manyFiles(50))
	delete(f.byID, 1) // peek now fails
	s := newService(t, f, Config{})
	d, err := s.Decide(bg(), tor)
	if err == nil || d.Mode != ModeDirs || d.Reason != "unknown" {
		t.Fatalf("decision %+v err %v", d, err)
	}
}

func TestSingleFileNeverPeeks(t *testing.T) {
	t.Parallel()
	f := newFake()
	tor := f.add(t, 1, []codec.File{{Path: "only.iso", Size: 5}})
	s := newService(t, f, Config{})
	before := f.peeks.Load()
	if d, _ := s.Decide(bg(), tor); !d.FullTreeAvailable() {
		t.Fatal("single file must be eligible")
	}
	if f.peeks.Load() != before {
		t.Fatal("single-file decision must not read the blob header")
	}
}

func TestIndexComplexityPassesThrough(t *testing.T) {
	t.Parallel()
	src := &complexSource{}
	s, err := New(src, src, Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Dir(bg(), 1, "", Page{Limit: 10})
	if !errors.Is(err, filetree.ErrIndexComplexity) {
		t.Fatalf("got %v", err)
	}
}

type complexSource struct{}

func (complexSource) Get(context.Context, int32) (filecache.Result, error) {
	return filecache.Result{}, &filetree.ComplexityError{Limit: filetree.LimitDepth, Allowed: 256}
}
func (complexSource) FileHeader(context.Context, int32) ([]byte, int, error) { return nil, 0, nil }
func (complexSource) Limits() codec.Limits                                   { return codec.DefaultLimits() }

func TestConcurrentTreesStayWithinEncodeBudget(t *testing.T) {
	for _, clients := range []int{8, 32} {
		t.Run(fmt.Sprint(clients), func(t *testing.T) {
			f := newFake()
			tor := f.add(t, 1, manyFiles(3000))
			s := newService(t, f, Config{EncodeSlots: 4, MaxWaiters: 64, QueueWait: 10 * time.Second})
			limit := int64(4) * int64(s.cfg.MaxTreeBytes)
			var peak, ok atomic.Int64
			var wg sync.WaitGroup
			for i := 0; i < clients; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					b, err := s.Tree(bg(), tor)
					if err != nil {
						t.Errorf("Tree: %v", err)
						return
					}
					use := s.Stats().InUse
					for {
						p := peak.Load()
						if use <= p || peak.CompareAndSwap(p, use) {
							break
						}
					}
					ok.Add(1)
					time.Sleep(5 * time.Millisecond)
					b.Release()
				}()
			}
			wg.Wait()
			if ok.Load() != int64(clients) {
				t.Fatalf("%d of %d served", ok.Load(), clients)
			}
			if peak.Load() > limit {
				t.Fatalf("peak encode budget %d > %d", peak.Load(), limit)
			}
			if s.Stats().InUse != 0 {
				t.Fatal("budget not returned")
			}
			t.Logf("%d clients: peak budget %d of %d bytes", clients, peak.Load(), limit)
		})
	}
}

func TestEncodeQueueBusyIs503Material(t *testing.T) {
	t.Parallel()
	f := newFake()
	tor := f.add(t, 1, manyFiles(50))
	s := newService(t, f, Config{EncodeSlots: 1, MaxWaiters: 1, QueueWait: 50 * time.Millisecond})
	hold, err := s.Tree(bg(), tor)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	if _, err := s.Tree(bg(), tor); !errors.Is(err, gate.ErrBusy) {
		t.Fatalf("want gate.ErrBusy, got %v", err)
	}
}

func TestTreeHonoursContext(t *testing.T) {
	t.Parallel()
	f := newFake()
	tor := f.add(t, 1, manyFiles(50))
	s := newService(t, f, Config{})
	ctx, cancel := context.WithCancel(bg())
	cancel()
	if _, err := s.Tree(ctx, tor); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// The owner-approved defaults (conservative Demo 3.1 values). A change here is
// a product decision: update docs/specs/web-v1.md with it.
func TestDefaultLimitsAreThePinnedValues(t *testing.T) {
	t.Parallel()
	if DefaultMaxTreeFiles != 4000 || DefaultMaxTreeEstimate != 1<<20 || DefaultMaxTreeBytes != 4<<20 || DefaultEncodeSlots != 8 {
		t.Fatalf("defaults changed: %d files, %d estimate, %d bytes, %d slots",
			DefaultMaxTreeFiles, DefaultMaxTreeEstimate, DefaultMaxTreeBytes, DefaultEncodeSlots)
	}
	f := newFake()
	s := newService(t, f, Config{})
	at := f.add(t, 1, manyFiles(4000))
	d, err := s.Decide(bg(), at)
	if err != nil || !d.FullTreeAvailable() {
		t.Fatalf("4,000 files must be tree mode: %+v %v", d, err)
	}
	b, err := s.Tree(bg(), at)
	if err != nil {
		t.Fatalf("a 4,000-file tree must serialize within the cap: %v", err)
	}
	if n := len(b.Bytes()); n > DefaultMaxTreeEstimate {
		t.Errorf("a 4,000-file tree is %d bytes, above the 1 MiB estimate", n)
	}
	b.Release()
	over := f.add(t, 2, manyFiles(4001))
	if d, err := s.Decide(bg(), over); err != nil || d.FullTreeAvailable() || d.Reason != "file_count" {
		t.Fatalf("4,001 files must be refused for the full tree: %+v %v", d, err)
	}
}
