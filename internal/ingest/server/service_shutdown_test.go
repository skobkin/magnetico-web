package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/internal/pgtest"
	"github.com/skobkin/magnetico-web/persistence"
)

// Bounded shutdown (Task 06 review correction 2). The consumer's record in
// flight used to run under context.WithoutCancel with a two-minute timeout
// while Run waited for it unconditionally, so a stalled AddTorrent made
// shutdown take minutes — far beyond a container stop timeout.

// stallStore blocks the first AddTorrent. With honorCtx it returns when its
// context ends (a stalled database call that can be interrupted); without, it
// ignores the context until released (a call that cannot be interrupted).
type stallStore struct {
	real     pgqueue.Store
	honorCtx bool
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func newStallStore(real pgqueue.Store, honorCtx bool) *stallStore {
	return &stallStore{real: real, honorCtx: honorCtx, entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *stallStore) AddTorrent(ctx context.Context, m persistence.Torrent, f []persistence.File) (bool, int32, error) {
	first := false
	s.once.Do(func() { first = true; close(s.entered) })
	if !first {
		return s.real.AddTorrent(ctx, m, f)
	}
	if s.honorCtx {
		select {
		case <-ctx.Done():
			return false, 0, ctx.Err()
		case <-s.release:
		}
	} else {
		<-s.release
	}
	return false, 0, errors.New("stalled call released")
}

func (s *stallStore) unblock() {
	select {
	case <-s.release:
	default:
		close(s.release)
	}
}

func shutdownOptions(st **stallStore, honorCtx bool) func(*Options) {
	return func(o *Options) {
		o.HTTPGrace = 2 * time.Second
		o.ConsumerGrace = 300 * time.Millisecond
		o.AbortGrace = 300 * time.Millisecond
		o.HandlerGrace = time.Second
		o.consumerStore = func(real pgqueue.Store) pgqueue.Store {
			*st = newStallStore(real, honorCtx)
			return *st
		}
	}
}

func waitEntered(t *testing.T, s *stallStore) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the consumer never reached AddTorrent")
	}
}

// An interruptible stall: the record is aborted after the consumer grace, Run
// returns cleanly within the budget, the pools are closed, the queue row is
// untouched and a restarted service settles it idempotently.
func TestShutdownInterruptsStalledAddTorrent(t *testing.T) {
	dsn := prepareDB(t, true)
	var st *stallStore
	r := startService(t, dsn, shutdownOptions(&st, true))

	if code, b, err := r.post("/torrents", records(recJSON(t, 1))); err != nil || code != 202 {
		t.Fatalf("submit: %v %d %s", err, code, b)
	}
	waitEntered(t, st)

	begin := time.Now()
	r.cancel()
	err, ok := r.wait(15 * time.Second)
	took := time.Since(begin)
	if !ok {
		t.Fatalf("Run did not return within 15 s of the shutdown signal (stalled record)")
	}
	if err != nil {
		t.Fatalf("Run = %v, want a clean shutdown after interrupting the record", err)
	}
	if took > 5*time.Second {
		t.Errorf("shutdown took %v, want about ConsumerGrace (0.3 s)", took)
	}
	t.Logf("shutdown with a stalled, interruptible AddTorrent took %v", took.Round(time.Millisecond))

	if n := countRows(t, dsn, `SELECT count(*) FROM magnetico_ingest.queue`); n != 1 {
		t.Fatalf("queue rows = %d, want the interrupted record kept", n)
	}
	if n := countRows(t, dsn, `SELECT count(*) FROM magneticod.torrents`); n != 0 {
		t.Fatalf("torrents = %d, nothing should be stored yet", n)
	}

	// Restart without the stall: reprocessing completes the record.
	r2 := startService(t, dsn, nil)
	eventually(t, "reprocessing after restart", func() bool {
		return countRows(t, dsn, `SELECT count(*) FROM magneticod.torrents`) == 1 &&
			countRows(t, dsn, `SELECT count(*) FROM magnetico_ingest.queue`) == 0
	})
	r2.cancel()
	if err, ok := r2.wait(30 * time.Second); !ok || err != nil {
		t.Fatalf("second shutdown: %v ok=%v", err, ok)
	}
}

// A stall that ignores its context cannot be interrupted: Run must still
// return within the budget, report ErrShutdownIncomplete, and must not close
// the pools under the stuck goroutine (which would block forever).
func TestShutdownAbandonsUninterruptibleAddTorrent(t *testing.T) {
	dsn := prepareDB(t, true)
	var st *stallStore
	r := startService(t, dsn, shutdownOptions(&st, false))
	t.Cleanup(func() { st.unblock() })

	if code, b, err := r.post("/torrents", records(recJSON(t, 1))); err != nil || code != 202 {
		t.Fatalf("submit: %v %d %s", err, code, b)
	}
	waitEntered(t, st)

	begin := time.Now()
	r.cancel()
	err, ok := r.wait(15 * time.Second)
	took := time.Since(begin)
	if !ok {
		t.Fatal("Run blocked on an uninterruptible stalled record instead of abandoning it")
	}
	if !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("Run = %v, want ErrShutdownIncomplete", err)
	}
	if took > 5*time.Second {
		t.Errorf("shutdown took %v, want about ConsumerGrace+AbortGrace (0.6 s)", took)
	}
	t.Logf("shutdown with an uninterruptible AddTorrent took %v and abandoned the consumer", took.Round(time.Millisecond))

	// The abandoned goroutine is still inside AddTorrent; releasing it must
	// not panic on a closed pool (they were deliberately left open) and the
	// row is still queued for the next start.
	st.unblock()
	time.Sleep(300 * time.Millisecond)
	// The abandoned instance keeps its pools open by design (the point of this
	// test): its connections, all open by now, may outlive the test. The
	// replacement instance below is not covered and must close its own.
	t.Logf("abandoned instance: %d connections left open by design", pgtest.AllowConnectionsOpenNow(t, dsn))
	if n := countRows(t, dsn, `SELECT count(*) FROM magnetico_ingest.queue`); n != 1 {
		t.Fatalf("queue rows = %d, want the record kept", n)
	}
	// A replacement instance (the old advisory-lock connection is released as
	// the abandoned consumer unwinds) completes the record.
	r2 := startService(t, dsn, nil)
	eventually(t, "reprocessing after restart", func() bool {
		return countRows(t, dsn, `SELECT count(*) FROM magneticod.torrents`) == 1
	})
	r2.cancel()
	if err, ok := r2.wait(30 * time.Second); !ok {
		t.Fatal("the replacement instance did not shut down")
	} else if err != nil {
		t.Fatalf("the replacement instance's Run = %v, want a clean shutdown", err)
	}
}
