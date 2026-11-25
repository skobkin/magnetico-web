package stats

import (
	"context"
	"testing"
	"time"
)

func agedFixture() Overview {
	in := Instance{ID: "i1", State: StateUp, Age: 5 * time.Second, windowEndAge: 5 * time.Second,
		covered: [3]int{60, 900, 3600}}
	in.counts[0][0] = 600
	return Overview{
		Schema:    StateUp,
		Instances: []Instance{in},
		Rates:     computeRates([]Instance{in}),
		Queue: QueueView{State: StateUp, Age: 60 * time.Second,
			Rows: known(int64(10)), Bytes: known(int64(1000)), QuotaUsed: known(int64(1000)), QuotaMax: known(int64(10000)),
			Utilisation: known(0.1), Quarantine: known(int64(1)), OldestAge: known(time.Minute)},
		Storage: StorageView{State: StateUp, Age: time.Minute, Magneticod: known(int64(5)), Torrents: known(int64(2)),
			IndexedCount: estimated(int64(3))},
		ThisHourAt: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC),
		ThisHour:   [metricCount]int64{5, 5, 5, 5, 5, 5},
	}
}

// TestCachedOverviewAgesWhatItServes (Task 09A): the instance ages and states
// are computed when the overview is read from the database; a copy served from
// the cache (up to 10 s later, or indefinitely after a failed refresh) must
// not keep reporting an old heartbeat as current.
func TestCachedOverviewAgesWhatItServes(t *testing.T) {
	o := agedFixture()
	fetched := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)

	if got := o.aged(fetched, fetched); got.Instances[0].State != StateUp || got.Instances[0].Age != 5*time.Second {
		t.Fatalf("no time passed: %+v", got.Instances[0])
	}
	got := o.aged(fetched, fetched.Add(40*time.Second))
	if in := got.Instances[0]; in.State != StateStale || in.Age != 45*time.Second {
		t.Fatalf("after 40 s: state %v age %v, want stale / 45 s", in.State, in.Age)
	}
	if got.Rates.Windows[0].Included != 0 || got.Rates.Windows[0].Values[0].Valid {
		t.Fatalf("a stale instance must leave the rates: %+v", got.Rates.Windows[0])
	}
	if len(got.Rates.Windows[0].Excluded) != 1 {
		t.Fatalf("the instance must be listed as left out: %+v", got.Rates.Windows[0].Excluded)
	}
	if got := o.aged(fetched, fetched.Add(6*time.Minute)); got.Instances[0].State != StateUnavailable {
		t.Fatalf("after 6 min the heartbeat is unavailable, got %v", got.Instances[0].State)
	}
	if o.Instances[0].State != StateUp || o.Instances[0].Age != 5*time.Second || !o.Rates.Windows[0].Values[0].Valid {
		t.Fatal("aging modified the cached value instead of a copy")
	}
}

func TestCachedQueueAndStorageAgeIndependentlyAndBlankWhenExpired(t *testing.T) {
	o := agedFixture()
	fetched := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)

	got := o.aged(fetched, fetched.Add(2*time.Minute)) // queue age 180 s
	if got.Queue.State != StateStale || !got.Queue.Rows.Valid || got.Storage.State != StateUp {
		t.Fatalf("queue %v rows valid=%v storage %v", got.Queue.State, got.Queue.Rows.Valid, got.Storage.State)
	}
	got = o.aged(fetched, fetched.Add(16*time.Minute)) // queue beyond 15 min, storage beyond 15 min but < 1 h
	if got.Queue.State != StateUnavailable || got.Queue.Rows.Valid || got.Queue.Utilisation.Valid || got.Queue.OldestAge.Valid {
		t.Fatalf("an expired queue sample must not show numbers: %+v", got.Queue)
	}
	if got.Storage.State != StateStale || !got.Storage.Magneticod.Valid {
		t.Fatalf("storage after 16 min: %v valid=%v", got.Storage.State, got.Storage.Magneticod.Valid)
	}
	got = o.aged(fetched, fetched.Add(2*time.Hour))
	if got.Storage.State != StateUnavailable || got.Storage.Magneticod.Valid || got.Storage.IndexedCount.Valid {
		t.Fatalf("an expired storage sample must not show numbers: %+v", got.Storage)
	}
}

func TestCachedOverviewDoesNotKeepTheClosedHourAsTheOpenOne(t *testing.T) {
	o := agedFixture()
	fetched := time.Date(2026, 11, 25, 10, 59, 59, 0, time.UTC)
	if got := o.aged(fetched, fetched.Add(500*time.Millisecond)); got.ThisHourAt.IsZero() {
		t.Fatal("still inside the hour: the count so far stays")
	}
	if got := o.aged(fetched, fetched.Add(2*time.Second)); !got.ThisHourAt.IsZero() {
		t.Fatalf("the hour has ended: the count of the closed hour is not 'this hour', got %v", got.ThisHourAt)
	}
}

// TestFreshnessIsDerivedFromTheAgedOverview: the page and the JSON take their
// state from Freshness, so it must follow the aged values.
func TestFreshnessIsDerivedFromTheAgedOverview(t *testing.T) {
	s := &Service{}
	o := agedFixture()
	fetched := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
	f := s.freshness(o.aged(fetched, fetched.Add(40*time.Second)), fetched, true)
	if f.Ingest != StateStale || !f.Stale {
		t.Fatalf("%+v", f)
	}
}

// TestServiceOverviewServesAnAgedCopyAfterAFailedRefresh is the end-to-end form:
// the refresh fails (no database behind the reader), the last known good is
// served with its own fetch time, and its ages and states have moved on.
func TestServiceOverviewServesAnAgedCopyAfterAFailedRefresh(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)}
	s := NewService(nil, WithClock(clk.now))
	fetched := clk.now()
	if _, _, _, err := s.overview.get(bg, struct{}{}, func(context.Context) (Overview, error) { return agedFixture(), nil }); err != nil {
		t.Fatal(err)
	}
	clk.advance(3 * time.Minute) // the 10 s cache has expired; the reader has no database and fails
	o, f, err := s.Overview(bg, admin)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Stale || !f.FetchedAt.Equal(fetched) {
		t.Fatalf("freshness %+v, want the last known good with its own fetch time %v", f, fetched)
	}
	if f.Ingest != StateStale || o.Instances[0].Age != 5*time.Second+3*time.Minute {
		t.Fatalf("a 5 s old heartbeat read 3 minutes ago must now be stale at 185 s: ingest %v, age %v", f.Ingest, o.Instances[0].Age)
	}
	if f.Queue != StateStale {
		t.Fatalf("queue sample of 60 s + 3 min is stale, got %v", f.Queue)
	}
}
