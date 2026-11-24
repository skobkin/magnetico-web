package pgqueue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/persistence"
)

type obsCounts struct {
	attempts, retries, storageErrs, created, storedNew, storedExisting, quarantined int
}

// recObserver counts consumer events.
type recObserver struct {
	mu sync.Mutex
	obsCounts
	durations []time.Duration
}

func (o *recObserver) ProcessAttempt()      { o.mu.Lock(); o.attempts++; o.mu.Unlock() }
func (o *recObserver) ProcessRetry()        { o.mu.Lock(); o.retries++; o.mu.Unlock() }
func (o *recObserver) ProcessStorageError() { o.mu.Lock(); o.storageErrs++; o.mu.Unlock() }
func (o *recObserver) TorrentCreated()      { o.mu.Lock(); o.created++; o.mu.Unlock() }
func (o *recObserver) SettledStoredNew(d time.Duration) {
	o.mu.Lock()
	o.storedNew++
	o.durations = append(o.durations, d)
	o.mu.Unlock()
}
func (o *recObserver) SettledStoredExisting(d time.Duration) {
	o.mu.Lock()
	o.storedExisting++
	o.durations = append(o.durations, d)
	o.mu.Unlock()
}
func (o *recObserver) SettledQuarantined() { o.mu.Lock(); o.quarantined++; o.mu.Unlock() }

func (o *recObserver) get() obsCounts {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.obsCounts
}

func TestAcceptedPayloadBytesCountInsertedRowsOnly(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	a, b := payloadFor(t, 1, 100), payloadFor(t, 2, 300)
	res, err := q.Admit(context.Background(), []Item{b, a, a}) // one in-batch duplicate
	want := int64(len(a.Payload) + len(b.Payload))
	if err != nil || res.Inserted != 2 || res.Duplicates != 1 || res.InsertedBytes != want {
		t.Fatalf("Admit = %+v, %v; want inserted bytes %d", res, err, want)
	}
	// A redelivery inserts nothing and so adds no bytes; a new row adds only its own.
	c := payloadFor(t, 3, 50)
	res, err = q.Admit(context.Background(), []Item{a, b, c})
	if err != nil || res.Inserted != 1 || res.Duplicates != 2 || res.InsertedBytes != int64(len(c.Payload)) {
		t.Fatalf("redelivery = %+v, %v", res, err)
	}
	if s := mustStats(t, q); s.UsedBytes != want+int64(len(c.Payload)) {
		t.Fatalf("used bytes %d disagree with the sum of inserted bytes", s.UsedBytes)
	}
}

func TestTorrentCreatedIsCountedAtTheCreationBoundaryEvenIfTheDeleteFails(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	obs := &recObserver{}
	item := payloadFor(t, 7, 10)
	if _, err := q.Admit(context.Background(), []Item{item}); err != nil {
		t.Fatal(err)
	}
	// First consumer: AddTorrent commits, the row is not deleted (simulated crash).
	ctx, cancel := context.WithCancel(context.Background())
	c1 := NewConsumer(q, e.store, fastOpts(ConsumerOptions{Observer: obs,
		beforeDelete: func([20]byte) error { cancel(); return errors.New("simulated crash") }}))
	done := make(chan struct{})
	go func() { defer close(done); _ = c1.Run(ctx) }()
	<-done
	if g := obs.get(); g.created != 1 || g.storedNew != 0 || g.storedExisting != 0 || g.attempts != 1 {
		t.Fatalf("after the failed delete: %+v, want the creation counted and nothing settled", g)
	}
	// The retry finds the torrent present: it settles as existing and creates nothing.
	c2 := NewConsumer(q, e.store, fastOpts(ConsumerOptions{Observer: obs}))
	runConsumer(t, c2)
	waitFor(t, "reprocess", func() bool { return mustStats(t, q).QueueRows == 0 })
	g := obs.get()
	if g.created != 1 || g.storedNew != 0 || g.storedExisting != 1 || g.attempts != 2 {
		t.Fatalf("after the retry: %+v", g)
	}
	if len(obs.durations) != 1 || obs.durations[0] < 0 {
		t.Fatalf("durations: %v", obs.durations)
	}
}

func TestSettlementOutcomesAreMutuallyExclusiveAtTheQueueBoundary(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	obs := &recObserver{}
	bad, good := payloadFor(t, 1, 10), payloadFor(t, 2, 10)
	fs := &fakeStore{inner: e.store, fail: func(h [20]byte) error {
		if h == bad.InfoHash {
			return fmt.Errorf("%w: test", persistence.ErrInvalidMetadata)
		}
		return nil
	}}
	if _, err := q.Admit(context.Background(), []Item{bad, good}); err != nil {
		t.Fatal(err)
	}
	runConsumer(t, NewConsumer(q, fs, fastOpts(ConsumerOptions{Observer: obs})))
	waitFor(t, "drain", func() bool { s := mustStats(t, q); return s.QueueRows == 0 && s.QuarantineRows == 1 })
	g := obs.get()
	if g.storedNew != 1 || g.quarantined != 1 || g.storedExisting != 0 || g.created != 1 || g.attempts != 2 {
		t.Fatalf("outcomes: %+v", g)
	}
	if len(obs.durations) != 1 {
		t.Fatalf("only the stored row is timed, got %d durations", len(obs.durations))
	}
}

func TestRetriesAndStorageErrorsAreObserved(t *testing.T) {
	e := newEnv(t)
	q := e.open(t, Options{})
	obs := &recObserver{}
	item := payloadFor(t, 3, 10)
	var calls int
	var mu sync.Mutex
	fs := &fakeStore{inner: e.store, fail: func(h [20]byte) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return errors.New("transient store failure") // unclassified: retry the row
		}
		if calls == 2 {
			return fmt.Errorf("%w: down", persistence.ErrStorageIO) // global backoff, row untouched
		}
		return nil
	}}
	if _, err := q.Admit(context.Background(), []Item{item}); err != nil {
		t.Fatal(err)
	}
	opts := fastOpts(ConsumerOptions{Observer: obs})
	runConsumer(t, NewConsumer(q, fs, opts))
	waitFor(t, "retry then success", func() bool { return mustStats(t, q).QueueRows == 0 })
	if g := obs.get(); g.retries != 1 || g.storageErrs < 1 || g.storedNew != 1 {
		t.Fatalf("observer: %+v", g)
	}
}
