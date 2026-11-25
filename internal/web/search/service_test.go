package search

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/persistence"
)

type fakeStore struct {
	mu    sync.Mutex
	calls []persistence.SearchSpec
	pages []persistence.SearchPage // returned in order, last one repeats
	err   error
	block chan struct{} // when set, SearchTorrents waits for close or ctx
	hash  func([]byte) (persistence.Torrent, bool, error)
}

func (f *fakeStore) SearchTorrents(ctx context.Context, spec persistence.SearchSpec) (persistence.SearchPage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, spec)
	n := len(f.calls)
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return persistence.SearchPage{}, ctx.Err()
		}
	}
	if f.err != nil {
		return persistence.SearchPage{}, f.err
	}
	if len(f.pages) == 0 {
		return persistence.SearchPage{}, nil
	}
	return f.pages[min(n-1, len(f.pages)-1)], nil
}

func (f *fakeStore) TorrentByInfoHash(_ context.Context, h []byte) (persistence.Torrent, bool, error) {
	return f.hash(h)
}

func (f *fakeStore) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func rows(ids ...int32) []persistence.Torrent {
	out := make([]persistence.Torrent, len(ids))
	for i, id := range ids {
		out[i] = persistence.Torrent{ID: id, DiscoveredOn: int64(100 + id), TotalSize: int64(id), Name: "n"}
	}
	return out
}

func newSvc(t *testing.T, st Store, conc int, wait time.Duration) *Service {
	t.Helper()
	s, err := New(st, Config{Concurrency: conc, QueueWait: wait})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLinksFollowTheTraversalDirection(t *testing.T) {
	p, _ := ParseParams("ubuntu", "", "", "", "")
	p.PageSize = 3
	curF := EncodeCursor(p, Cursor{ID: 50, Int: 150})
	curB := EncodeCursor(p, Cursor{ID: 50, Int: 150, Backward: true})

	cases := []struct {
		name             string
		cursor           string
		page             persistence.SearchPage
		wantNext, wantPr bool
	}{
		{"first page, more", "", persistence.SearchPage{Items: rows(9, 8, 7), Extra: true}, true, false},
		{"first page, only page", "", persistence.SearchPage{Items: rows(9, 8)}, false, false},
		{"forward, more", curF, persistence.SearchPage{Items: rows(6, 5, 4), Extra: true}, true, true},
		{"forward, last page", curF, persistence.SearchPage{Items: rows(3, 2)}, false, true},
		{"backward, more before", curB, persistence.SearchPage{Items: rows(9, 8, 7), Extra: true}, true, true},
		{"backward, reached the head", curB, persistence.SearchPage{Items: rows(9, 8, 7)}, true, false},
	}
	for _, tc := range cases {
		st := &fakeStore{pages: []persistence.SearchPage{tc.page}}
		s := newSvc(t, st, 2, time.Second)
		pp := p
		pp.Cursor = tc.cursor
		res, err := s.Search(context.Background(), pp)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if (res.Next != "") != tc.wantNext || (res.Prev != "") != tc.wantPr {
			t.Errorf("%s: next=%v prev=%v, want %v %v", tc.name, res.Next != "", res.Prev != "", tc.wantNext, tc.wantPr)
		}
		// Next is a forward cursor of the LAST row; Previous a backward cursor of the FIRST row.
		if res.Next != "" {
			if cur, err := DecodeCursor(p, res.Next); err != nil || cur.Backward || cur.ID != res.Items[len(res.Items)-1].ID {
				t.Errorf("%s: next cursor %+v %v", tc.name, cur, err)
			}
		}
		if res.Prev != "" {
			if cur, err := DecodeCursor(p, res.Prev); err != nil || !cur.Backward || cur.ID != res.Items[0].ID {
				t.Errorf("%s: prev cursor %+v %v", tc.name, cur, err)
			}
		}
	}
}

func TestBackwardCursorMapsToReversedTraversal(t *testing.T) {
	p, _ := ParseParams("ubuntu", "size", "asc", "", "")
	p.Cursor = EncodeCursor(p, Cursor{ID: 12, Int: 77, Backward: true})
	st := &fakeStore{pages: []persistence.SearchPage{{Items: rows(1)}}}
	if _, err := newSvc(t, st, 1, time.Second).Search(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got := st.calls[0]
	if !got.Backward || got.Desc || got.Sort != persistence.SortSize || got.After == nil || got.After.ID != 12 || got.After.Int != 77 {
		t.Fatalf("spec = %+v after=%+v", got, got.After)
	}
}

func TestStaleCursorRestartsFromTheFirstPage(t *testing.T) {
	p, _ := ParseParams("ubuntu", "", "", "", "")
	p.Cursor = EncodeCursor(p, Cursor{ID: 5, Int: 1})
	st := &fakeStore{pages: []persistence.SearchPage{{}, {Items: rows(9, 8), Extra: true}}}
	res, err := newSvc(t, st, 1, time.Second).Search(context.Background(), p)
	if err != nil || len(res.Items) != 2 || res.Prev != "" || res.Next == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if st.calls[1].After != nil || st.calls[1].Backward {
		t.Fatalf("the retry must be a plain first page: %+v", st.calls[1])
	}
}

func TestInvalidCursorCostsNoSlotAndNoQuery(t *testing.T) {
	p, _ := ParseParams("ubuntu", "", "", "", "garbage")
	st := &fakeStore{}
	s := newSvc(t, st, 1, time.Second)
	if _, err := s.Search(context.Background(), p); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("got %v", err)
	}
	if st.callCount() != 0 || s.gate.InUse() != 0 || s.Stats().InvalidCursor != 1 {
		t.Fatalf("calls %d in use %d stats %+v", st.callCount(), s.gate.InUse(), s.Stats())
	}
}

func TestInfoHashLookup(t *testing.T) {
	var asked []byte
	st := &fakeStore{hash: func(h []byte) (persistence.Torrent, bool, error) {
		asked = h
		return persistence.Torrent{ID: 3}, true, nil
	}}
	p, _ := ParseParams("0123456789abcdef0123456789abcdef01234567", "size", "asc", "", "ignored")
	res, err := newSvc(t, st, 1, time.Second).Search(context.Background(), p)
	if err != nil || !res.Lookup || len(res.Items) != 1 || res.Next != "" || res.Prev != "" || len(asked) != 20 || st.callCount() != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	st.hash = func([]byte) (persistence.Torrent, bool, error) { return persistence.Torrent{}, false, nil }
	if res, err := newSvc(t, st, 1, time.Second).Search(context.Background(), p); err != nil || !res.Lookup || len(res.Items) != 0 {
		t.Fatalf("miss: %+v %v", res, err)
	}
}

func TestErrorClassification(t *testing.T) {
	p, _ := ParseParams("ubuntu", "", "", "", "")
	cases := []struct {
		name string
		in   error
		want error
	}{
		{"statement timeout", persistence.ErrStatementTimeout, ErrTimeout},
		{"budget exhausted", errors.Join(persistence.ErrBudgetExhausted, context.DeadlineExceeded), ErrTimeout},
		{"deadline", context.DeadlineExceeded, ErrTimeout},
		{"canceled", context.Canceled, context.Canceled},
		{"anchor gone", persistence.ErrAnchorNotFound, ErrCursorExpired},
		{"storage", persistence.ErrStorageIO, persistence.ErrStorageIO},
	}
	for _, c := range cases {
		s := newSvc(t, &fakeStore{err: c.in}, 1, time.Second)
		_, err := s.Search(context.Background(), p)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
		// A timeout is never reported as an invalid query/cursor, nor the reverse.
		if c.want == ErrTimeout && (errors.Is(err, ErrInvalidQuery) || errors.Is(err, ErrBusy)) {
			t.Errorf("%s: timeout conflated with another outcome", c.name)
		}
		if s.gate.InUse() != 0 {
			t.Errorf("%s: permit leaked", c.name)
		}
	}
}

func TestSaturationReturnsBusyAndRecovers(t *testing.T) {
	st := &fakeStore{pages: []persistence.SearchPage{{Items: rows(1)}}, block: make(chan struct{})}
	s := newSvc(t, st, 2, 80*time.Millisecond)
	p, _ := ParseParams("ubuntu", "", "", "", "")

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.Search(context.Background(), p) }()
	}
	for s.gate.InUse() != 2 {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	_, err := s.Search(context.Background(), p)
	if !errors.Is(err, ErrBusy) || errors.Is(err, ErrTimeout) {
		t.Fatalf("third search: %v, want ErrBusy", err)
	}
	if d := time.Since(start); d < 60*time.Millisecond || d > 2*time.Second {
		t.Fatalf("busy after %v, want about the queue wait", d)
	}
	close(st.block)
	wg.Wait()
	if _, err := s.Search(context.Background(), p); err != nil {
		t.Fatalf("after the slots freed: %v", err)
	}
	if st := s.Stats(); st.Busy != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestClientCancellationReleasesThePermit(t *testing.T) {
	st := &fakeStore{pages: []persistence.SearchPage{{Items: rows(1)}}, block: make(chan struct{})}
	s := newSvc(t, st, 1, time.Second)
	p, _ := ParseParams("ubuntu", "", "", "", "")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := s.Search(ctx, p); errc <- err }()
	for s.gate.InUse() != 1 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	err := <-errc
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) || Outcome(err) != "canceled" {
		t.Fatalf("got %v", err)
	}
	if s.gate.InUse() != 0 || s.Stats().Canceled != 1 || s.Stats().Timeout != 0 {
		t.Fatalf("in use %d stats %+v", s.gate.InUse(), s.Stats())
	}
	// Cancelled while still queued behind a busy slot.
	st2 := &fakeStore{block: make(chan struct{})}
	s2 := newSvc(t, st2, 1, 5*time.Second)
	go func() { _, _ = s2.Search(context.Background(), p) }()
	for s2.gate.InUse() != 1 {
		time.Sleep(time.Millisecond)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	e2 := make(chan error, 1)
	go func() { _, err := s2.Search(ctx2, p); e2 <- err }()
	for s2.gate.Waiting() != 1 {
		time.Sleep(time.Millisecond)
	}
	cancel2()
	if err := <-e2; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancel: %v", err)
	}
	close(st2.block)
}

// movingDeadline is a context whose deadline is set by the test: it never fires
// by itself (its Done channel is the parent's), so what the Reader sees when the
// statement would start does not depend on any timer or on scheduling.
type movingDeadline struct {
	context.Context
	mu sync.Mutex
	dl time.Time
}

func (m *movingDeadline) Deadline() (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dl, !m.dl.IsZero()
}

func (m *movingDeadline) set(t time.Time) { m.mu.Lock(); m.dl = t; m.mu.Unlock() }

// Near-exhausted deadlines after waiting for a slot: the real Reader (without
// a pool, so any statement would panic) must refuse to start the statement and
// the service must report a timeout, release the permit and stay usable.
//
// Deterministic by construction (issue #53): the request is queued behind a held
// slot; only once it is observably queued is its deadline set to `left` from now
// and the slot released. Nothing waits on a timer, so a late scheduler can only
// leave the Reader with less budget than `left`, which is refused all the same.
func TestNearExhaustedDeadlineAfterGateWaitStartsNoStatement(t *testing.T) {
	r := persistence.NewReader(nil, codec.DefaultLimits())
	s := newSvc(t, r, 1, 5*time.Second)
	p, _ := ParseParams("ubuntu", "", "", "", "")

	for _, left := range []time.Duration{140 * time.Millisecond, 60 * time.Millisecond, 5 * time.Millisecond} {
		hold, err := s.gate.Acquire(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		parent, cancel := context.WithCancel(context.Background())
		ctx := &movingDeadline{Context: parent, dl: time.Now().Add(time.Hour)}
		errc := make(chan error, 1)
		go func() { _, err := s.Search(ctx, p); errc <- err }()
		for s.gate.Waiting() != 1 { // queued: the permit is held by the test
			time.Sleep(time.Millisecond)
		}
		ctx.set(time.Now().Add(left)) // the budget that is left when the slot frees
		hold()
		err = <-errc
		cancel()
		if !errors.Is(err, ErrTimeout) || Outcome(err) != "timeout" {
			t.Fatalf("left %v: got %v, want ErrTimeout", left, err)
		}
		if s.gate.InUse() != 0 || s.gate.Waiting() != 0 {
			t.Fatalf("left %v: gate not drained", left)
		}
	}
	// Deadline consumed entirely while queued: also a timeout, not "busy". The slot
	// stays held until Search has returned, so the outcome cannot depend on timing.
	hold, _ := s.gate.Acquire(context.Background(), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := s.Search(ctx, p)
	cancel()
	hold()
	if !errors.Is(err, ErrTimeout) || errors.Is(err, ErrBusy) {
		t.Fatalf("expired while queued: %v", err)
	}
	if got := s.Stats().Timeout; got != 4 {
		t.Fatalf("timeouts counted: %d", got)
	}
}

func TestNeverStartsMoreThanTheConfiguredStatements(t *testing.T) {
	var cur, peak atomic.Int64
	st := &concurrencyStore{cur: &cur, peak: &peak}
	s := newSvc(t, st, 3, 5*time.Second)
	p, _ := ParseParams("ubuntu", "", "", "", "")
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.Search(context.Background(), p) }()
	}
	wg.Wait()
	if peak.Load() > 3 || peak.Load() == 0 {
		t.Fatalf("peak concurrent statements = %d, want 1..3", peak.Load())
	}
}

type concurrencyStore struct{ cur, peak *atomic.Int64 }

func (c *concurrencyStore) SearchTorrents(context.Context, persistence.SearchSpec) (persistence.SearchPage, error) {
	n := c.cur.Add(1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(2 * time.Millisecond)
	c.cur.Add(-1)
	return persistence.SearchPage{Items: rows(1)}, nil
}
func (c *concurrencyStore) TorrentByInfoHash(context.Context, []byte) (persistence.Torrent, bool, error) {
	return persistence.Torrent{}, false, nil
}

func TestNewValidates(t *testing.T) {
	if _, err := New(nil, Config{Concurrency: 1}); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := New(&fakeStore{}, Config{Concurrency: 0}); err == nil {
		t.Error("zero slots accepted")
	}
}
