package persistence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skobkin/magnetico-web/codec"
)

type readerEnv struct {
	pool *pgxpool.Pool
	r    *Reader
	dsn  string
	n    int
}

func newReaderEnv(t *testing.T) *readerEnv {
	t.Helper()
	dsn := integrationCluster(t).TestDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := InitSchema(ctx, dsn); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &readerEnv{pool: pool, r: NewReader(pool, codec.DefaultLimits()), dsn: dsn}
}

func (e *readerEnv) seed(t *testing.T, name string, total, discovered int64, fileCount int32) int32 {
	t.Helper()
	e.n++
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", name, e.n)))
	var id int32
	err := e.pool.QueryRow(context.Background(), `INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
	    VALUES ($1,$2,$3,$4,$5) RETURNING id`, h[:20], name, total, discovered, fileCount).Scan(&id)
	if err != nil {
		t.Fatalf("seed %q: %v", name, err)
	}
	return id
}

// seedWithFiles inserts a torrent plus a blob encoded from files directly in
// SQL (bypassing AddTorrent's UTF-8 validation, which legacy or forged data
// need not satisfy).
func (e *readerEnv) seedWithFiles(t *testing.T, name string, files []File) (int32, []byte) {
	t.Helper()
	var total int64
	for _, f := range files {
		total += f.Size
	}
	id := e.seed(t, name, total, 1700000000, int32(len(files)))
	blob, _, err := codec.Encode(files, codec.ProfileCompact, codec.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO magneticod.torrent_file_blobs (torrent_id, data) VALUES ($1,$2)`, id, blob); err != nil {
		t.Fatal(err)
	}
	return id, blob
}

func TestReaderByIDAndInfoHash(t *testing.T) {
	e := newReaderEnv(t)
	ctx := context.Background()
	id := e.seed(t, "alpha", 10, 100, 1)
	got, err := e.r.TorrentByID(ctx, id)
	if err != nil || got.Name != "alpha" || got.TotalSize != 10 || got.ID != id || len(got.InfoHash) != 20 {
		t.Fatalf("TorrentByID = %+v, %v", got, err)
	}
	if _, err := e.r.TorrentByID(ctx, id+1000); !errors.Is(err, ErrTorrentNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	byHash, ok, err := e.r.TorrentByInfoHash(ctx, got.InfoHash)
	if err != nil || !ok || byHash.ID != id {
		t.Fatalf("by hash = %+v %v %v", byHash, ok, err)
	}
	if _, ok, err := e.r.TorrentByInfoHash(ctx, bytes.Repeat([]byte{9}, 20)); ok || err != nil {
		t.Fatalf("miss = %v %v", ok, err)
	}
}

func TestReaderFileIdentityHeaderBlobAndDecode(t *testing.T) {
	e := newReaderEnv(t)
	ctx := context.Background()

	single := e.seed(t, "one.mkv", 77, 5, 1)
	fi, err := e.r.FileIdentity(ctx, single)
	if err != nil || fi.HasBlob || fi.Version != "" {
		t.Fatalf("no-blob identity = %+v %v", fi, err)
	}
	if _, _, err := e.r.FileHeader(ctx, single); !errors.Is(err, ErrIntegrityMissingBlob) {
		t.Fatalf("header without blob: %v", err)
	}
	if fl, err := SingleFileList(fi.Torrent); err != nil || fl[0] != (File{Path: "one.mkv", Size: 77}) {
		t.Fatalf("single list %v %v", fl, err)
	}
	if _, err := e.r.FileIdentity(ctx, 999999); !errors.Is(err, ErrTorrentNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	missing := e.seed(t, "multi-no-blob", 10, 5, 3)
	if fi, _ := e.r.FileIdentity(ctx, missing); fi.HasBlob {
		t.Fatal("unexpected blob")
	} else if _, err := SingleFileList(fi.Torrent); !errors.Is(err, ErrIntegrityMissingBlob) {
		t.Fatalf("multi-file without blob must be a typed integrity error: %v", err)
	}

	// Paths with invalid UTF-8, duplicates and an empty component survive.
	files := []File{{Path: "a//b", Size: 1}, {Path: "d/\xff\xfe", Size: 2}, {Path: "d/x", Size: 3}, {Path: "d/x", Size: 3}, {Path: "d/x", Size: 4}}
	id, blob := e.seedWithFiles(t, "odd", files)
	fi, err = e.r.FileIdentity(ctx, id)
	if err != nil || !fi.HasBlob || fi.Version == "" {
		t.Fatalf("identity = %+v %v", fi, err)
	}
	hdr, storedLen, err := e.r.FileHeader(ctx, id)
	if err != nil || storedLen != len(blob) || !bytes.Equal(hdr, blob[:min(len(blob), codec.PeekHeaderLen)]) {
		t.Fatalf("header mismatch: %v (stored %d, want %d)", err, storedLen, len(blob))
	}
	if _, d, err := codec.PeekDecodedSize(hdr, storedLen, e.r.Limits()); err != nil || d == 0 {
		t.Fatalf("peek %d %v", d, err)
	}
	got, err := e.r.FileBlob(ctx, id, fi.Version)
	if err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("blob: %v", err)
	}
	list, err := DecodeFileList(fi.Torrent, got, e.r.Limits())
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(files)
	slices.SortFunc(want, func(a, b File) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return int(a.Size - b.Size)
	})
	if !slices.Equal(list, want) {
		t.Fatalf("decoded %v want %v", list, want)
	}
	if _, err := e.r.FileBlob(ctx, id, "0"); !errors.Is(err, ErrBlobChanged) {
		t.Fatalf("stale version: %v", err)
	}

	// Count and sum mismatches and corruption are ErrCorruptBlob, never partial.
	bad := fi.Torrent
	bad.FileCount++
	if _, err := DecodeFileList(bad, got, e.r.Limits()); !errors.Is(err, ErrCorruptBlob) {
		t.Fatalf("count mismatch: %v", err)
	}
	bad = fi.Torrent
	bad.TotalSize++
	if l, err := DecodeFileList(bad, got, e.r.Limits()); !errors.Is(err, ErrCorruptBlob) || l != nil {
		t.Fatalf("sum mismatch: %v", err)
	}
	if l, err := DecodeFileList(fi.Torrent, got[:len(got)-3], e.r.Limits()); !errors.Is(err, ErrCorruptBlob) || l != nil {
		t.Fatalf("truncated: %v", err)
	}
}

func TestSearchEscapingCaseAndAND(t *testing.T) {
	e := newReaderEnv(t)
	ctx := context.Background()
	names := []string{"Ubuntu 24.04 desktop", "ubuntu_server", "Ubuntuxserver", "100% legit", "100 percent", `back\slash`, "backXslash"}
	ids := map[string]int32{}
	for i, n := range names {
		ids[n] = e.seed(t, n, int64(i+1), int64(i+1), 1)
	}
	search := func(terms ...string) []string {
		p, err := e.r.SearchTorrents(ctx, SearchSpec{Terms: terms, Limit: 50, Desc: true})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range p.Items {
			out = append(out, it.Name)
		}
		slices.Sort(out)
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("got %q want %q", got, want)
		}
	}
	eq(search("UBUNTU", "Desktop"), "Ubuntu 24.04 desktop") // AND, case-insensitive
	eq(search("tu_s"), "ubuntu_server")                     // `_` is literal, not a wildcard
	eq(search("100%"), "100% legit")                        // `%` is literal
	eq(search(`k\s`), `back\slash`)                         // backslash is literal
	eq(search("ubuntu"), "Ubuntu 24.04 desktop", "ubuntu_server", "Ubuntuxserver")
	eq(search("nomatch-zzz"))
}

// referenceOrder returns all ids in the traversal order of (sort, desc),
// computed by one unbounded query.
func (e *readerEnv) referenceOrder(t *testing.T, sort SortKey, desc bool) []Torrent {
	t.Helper()
	col, _ := sort.column()
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	rows, err := e.pool.Query(context.Background(), `SELECT `+torrentColumns+` FROM magneticod.torrents ORDER BY `+col+` `+dir+`, id `+dir)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []Torrent
	for rows.Next() {
		var x Torrent
		if err := rows.Scan(&x.ID, &x.InfoHash, &x.Name, &x.TotalSize, &x.DiscoveredOn, &x.FileCount); err != nil {
			t.Fatal(err)
		}
		out = append(out, x)
	}
	return out
}

func anchorOf(sort SortKey, x Torrent, byID bool) *Anchor {
	if byID {
		return &Anchor{ID: x.ID, ByID: true}
	}
	switch sort {
	case SortSize:
		return &Anchor{ID: x.ID, Int: x.TotalSize}
	case SortName:
		return &Anchor{ID: x.ID, Name: x.Name}
	}
	return &Anchor{ID: x.ID, Int: x.DiscoveredOn}
}

func ids(ts []Torrent) []int32 {
	out := make([]int32, len(ts))
	for i, x := range ts {
		out[i] = x.ID
	}
	return out
}

func TestSearchKeysetWalksAllSortsAndDirections(t *testing.T) {
	e := newReaderEnv(t)
	ctx := context.Background()
	// Many ties on every sort key, plus a long (> 4 KiB) name.
	for i := 0; i < 43; i++ {
		name := fmt.Sprintf("Item %c%d", 'a'+rune(i%4), i%3)
		if i == 20 {
			name = "Item long " + strings.Repeat("x", 6000)
		}
		e.seed(t, name, int64(i%5+1), int64(100+i%6), 1)
	}
	const page = 7
	for _, sort := range []SortKey{SortDiscovered, SortSize, SortName} {
		for _, desc := range []bool{false, true} {
			for _, byID := range []bool{false, true} {
				for _, expanded := range []bool{false, true} {
					t.Run(fmt.Sprintf("sort%d/desc=%v/byID=%v/expanded=%v", sort, desc, byID, expanded), func(t *testing.T) {
						ref := e.referenceOrder(t, sort, desc)
						base := SearchSpec{Terms: []string{"item"}, Sort: sort, Desc: desc, Limit: page}

						// Forward walk.
						var walked, lastPage []Torrent
						var after *Anchor
						for n := 0; ; n++ {
							spec := base
							spec.After = after
							p, err := e.r.searchTorrents(ctx, spec, expanded)
							if err != nil {
								t.Fatal(err)
							}
							walked = append(walked, p.Items...)
							lastPage = p.Items
							if !p.Extra {
								break
							}
							if n > 20 {
								t.Fatal("walk does not terminate")
							}
							after = anchorOf(sort, p.Items[len(p.Items)-1], byID)
						}
						if !slices.Equal(ids(walked), ids(ref)) {
							t.Fatalf("forward walk differs from reference")
						}

						// Backward walk from the end, pages in display order.
						var back []Torrent
						cur := lastPage
						back = append(back, cur...)
						for n := 0; ; n++ {
							spec := base
							spec.Backward = true
							spec.After = anchorOf(sort, cur[0], byID)
							p, err := e.r.searchTorrents(ctx, spec, expanded)
							if err != nil {
								t.Fatal(err)
							}
							back = append(slices.Clone(p.Items), back...)
							if !p.Extra {
								break
							}
							cur = p.Items
							if n > 20 {
								t.Fatal("backward walk does not terminate")
							}
						}
						if !slices.Equal(ids(back), ids(ref)) {
							t.Fatalf("backward walk differs from reference")
						}

						// Return trip: page 1 → next → page 2 → previous == page 1 → next == page 2.
						p1, _ := e.r.searchTorrents(ctx, base, expanded)
						if !p1.Extra {
							t.Fatal("page 1 must have more")
						}
						s2 := base
						s2.After = anchorOf(sort, p1.Items[len(p1.Items)-1], byID)
						p2, _ := e.r.searchTorrents(ctx, s2, expanded)
						sb := base
						sb.Backward = true
						sb.After = anchorOf(sort, p2.Items[0], byID)
						pb, err := e.r.searchTorrents(ctx, sb, expanded)
						if err != nil {
							t.Fatal(err)
						}
						if !slices.Equal(ids(pb.Items), ids(p1.Items)) || pb.Extra {
							t.Fatalf("previous from page 2 = %v extra=%v, want page 1 %v without extra", ids(pb.Items), pb.Extra, ids(p1.Items))
						}
						s2b := base
						s2b.After = anchorOf(sort, pb.Items[len(pb.Items)-1], byID)
						p2b, _ := e.r.searchTorrents(ctx, s2b, expanded)
						if !slices.Equal(ids(p2b.Items), ids(p2.Items)) {
							t.Fatalf("next after the return trip differs from page 2")
						}
					})
				}
			}
		}
	}
}

func TestSearchAnchorDeleted(t *testing.T) {
	e := newReaderEnv(t)
	ctx := context.Background()
	long := e.seed(t, "z "+strings.Repeat("y", 6000), 1, 1, 1)
	for i := 0; i < 5; i++ {
		e.seed(t, fmt.Sprintf("z %d", i), 1, 1, 1)
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM magneticod.torrents WHERE id = $1`, long); err != nil {
		t.Fatal(err)
	}
	for _, back := range []bool{false, true} {
		for _, sort := range []SortKey{SortDiscovered, SortName} {
			_, err := e.r.SearchTorrents(ctx, SearchSpec{Sort: sort, Limit: 5, Backward: back, After: &Anchor{ID: long, ByID: true}})
			if !errors.Is(err, ErrAnchorNotFound) {
				t.Errorf("back=%v sort=%v: %v", back, sort, err)
			}
		}
	}
}

// Slow-statement behaviour without data volume: a second connection holds an
// ACCESS EXCLUSIVE lock so the real search statement blocks.
func lockTorrents(t *testing.T, dsn string) (release func()) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `LOCK TABLE magneticod.torrents IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var once bool
	release = func() {
		if once {
			return
		}
		once = true
		_, _ = c.Exec(ctx, `ROLLBACK`)
		_ = c.Close(ctx)
	}
	t.Cleanup(release)
	return release
}

func waitingSearchBackends(t *testing.T, dsn string) int {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var n int
	err = c.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
	    WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE 'SELECT id, info_hash, name%'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSearchStatementTimeoutFiresBeforeTheContext(t *testing.T) {
	e := newReaderEnv(t)
	e.seed(t, "x", 1, 1, 1)
	release := lockTorrents(t, e.dsn)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := e.r.SearchTorrents(ctx, SearchSpec{Limit: 10})
	took := time.Since(start)
	if !errors.Is(err, ErrStatementTimeout) {
		t.Fatalf("got %v, want ErrStatementTimeout (server-side timeout before the context deadline)", err)
	}
	if took > 2900*time.Millisecond {
		t.Fatalf("statement timeout fired late: %v", took)
	}
	if ctx.Err() != nil {
		t.Fatalf("context already expired: the server-side timeout should win")
	}
	release()
	// The pooled connection is reusable after a server-side cancel.
	if _, err := e.r.SearchTorrents(context.Background(), SearchSpec{Limit: 10}); err != nil {
		t.Fatalf("pool did not recover: %v", err)
	}
}

func TestSearchCancellationEndsTheBackendAndPoolRecovers(t *testing.T) {
	e := newReaderEnv(t)
	e.seed(t, "x", 1, 1, 1)
	release := lockTorrents(t, e.dsn)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	errc := make(chan error, 1)
	go func() {
		_, err := e.r.SearchTorrents(ctx, SearchSpec{Limit: 10})
		errc <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for waitingSearchBackends(t, e.dsn) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("search never blocked on the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	err := <-errc
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled (not a timeout)", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("cancel returned late: %v", time.Since(start))
	}
	// pgx sends a cancel request: the blocked backend must end.
	for waitingSearchBackends(t, e.dsn) != 0 {
		if time.Since(start) > 10*time.Second {
			t.Fatal("cancelled query is still waiting on the server")
		}
		time.Sleep(50 * time.Millisecond)
	}
	release()
	if _, err := e.r.TorrentByID(context.Background(), 1); err != nil {
		t.Fatalf("pool did not recover: %v", err)
	}
}

// An unchanged blob over the stored-size limit is a non-retryable limit error
// (ErrCorruptBlob + *codec.LimitError), not ErrBlobChanged; a changed or
// missing blob stays ErrBlobChanged; the oversized bytes are never returned.
func TestReaderFileBlobOversizedIsNotChanged(t *testing.T) {
	e := newReaderEnv(t)
	ctx := context.Background()
	files := []File{{Path: "a/b", Size: 1}, {Path: "a/c", Size: 2}, {Path: "d", Size: 3}}
	id, blob := e.seedWithFiles(t, "big", files)
	fi, err := e.r.FileIdentity(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	small := codec.DefaultLimits()
	small.MaxStoredBytes = len(blob) - 1
	rs := NewReader(e.pool, small)
	got, err := rs.FileBlob(ctx, id, fi.Version)
	var le *codec.LimitError
	if !errors.Is(err, ErrCorruptBlob) || errors.Is(err, ErrBlobChanged) || !errors.As(err, &le) ||
		le.Limit != codec.LimitStoredBytes || le.Actual != uint64(len(blob)) || le.Allowed != uint64(len(blob)-1) {
		t.Fatalf("oversized unchanged blob: %v (%+v)", err, le)
	}
	if got != nil {
		t.Fatalf("oversized bytes were returned: %d", len(got))
	}
	// Exactly at the limit is fine.
	small.MaxStoredBytes = len(blob)
	if got, err := NewReader(e.pool, small).FileBlob(ctx, id, fi.Version); err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("at the limit: %v", err)
	}
	// Stale version, even when oversized, is retryable: the caller refreshes
	// the identity and then meets the limit through FileHeader/Peek.
	small.MaxStoredBytes = len(blob) - 1
	if _, err := NewReader(e.pool, small).FileBlob(ctx, id, "0"); !errors.Is(err, ErrBlobChanged) {
		t.Fatalf("stale version: %v", err)
	}
	// Missing blob row: retryable ErrBlobChanged.
	other := e.seed(t, "noblob", 1, 1, 1)
	if _, err := e.r.FileBlob(ctx, other, "1"); !errors.Is(err, ErrBlobChanged) {
		t.Fatalf("missing blob: %v", err)
	}
	// Unchanged normal blob still works through the standard limits.
	if got, err := e.r.FileBlob(ctx, id, fi.Version); err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("normal: %v", err)
	}
}
