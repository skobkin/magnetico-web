package stats

import (
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/ingest/crawlers"
)

// TestOverviewCountsRegisteredEnabledAndRecentlyActiveCrawlers (Task 09A): the
// dashboard states how many crawlers exist, how many are enabled and how many of
// those were active recently. Active means: enabled and the statistics last saw
// an authenticated request from it within ActiveWindow (15 minutes). A disabled
// crawler and one that never connected are never active.
func TestOverviewCountsRegisteredEnabledAndRecentlyActiveCrawlers(t *testing.T) {
	e := newEnv(t, flags{stats: true, ingest: true})
	store := crawlers.New(e.pool)
	ids := map[string]string{}
	for _, n := range []string{"alpha", "bravo", "charlie", "delta"} {
		c, err := store.Add(bg, n)
		if err != nil {
			t.Fatal(err)
		}
		ids[n] = c.ID
	}
	if err := store.SetEnabled(bg, "charlie", false); err != nil {
		t.Fatal(err)
	}
	seen := func(id, ago string) {
		e.exec(`INSERT INTO magnetico_stats.crawler_activity (crawler_id, first_seen_at, last_seen_at, last_ip, records_received)
VALUES ($1, now() - interval '1 day', now() - $2::interval, '192.0.2.1', 1)`, id, ago)
	}
	seen(ids["alpha"], "2 minutes")  // active
	seen(ids["bravo"], "1 hour")     // enabled but inactive
	seen(ids["charlie"], "1 minute") // recent, but disabled
	// delta never connected
	o, _, err := e.svc.Overview(bg, admin)
	if err != nil {
		t.Fatal(err)
	}
	c := o.Crawlers
	if !c.Registered.Valid || c.Registered.Value != 4 || c.Enabled.Value != 3 || !c.Active.Valid || c.Active.Value != 1 {
		t.Fatalf("registered %+v enabled %+v active %+v, want 4 / 3 / 1", c.Registered, c.Enabled, c.Active)
	}
	if ActiveWindow != 15*time.Minute {
		t.Fatalf("the documented activity window is 15 minutes, got %v", ActiveWindow)
	}
}

func TestCrawlerCountsAreUnavailableWithoutTheIngestSchemaAndActivityWithoutStatistics(t *testing.T) {
	e := newEnv(t, flags{stats: true})
	o, _, err := e.svc.Overview(bg, admin)
	if err != nil {
		t.Fatal(err)
	}
	if o.Crawlers.Registered.Valid || o.Crawlers.Enabled.Valid || o.Crawlers.Active.Valid {
		t.Fatalf("counts shown without an ingest schema: %+v", o.Crawlers)
	}
	e2 := newEnv(t, flags{ingest: true}) // registry readable, no statistics schema
	if _, err := crawlers.New(e2.pool).Add(bg, "solo"); err != nil {
		t.Fatal(err)
	}
	o2, _, err := e2.svc.Overview(bg, admin)
	if err != nil {
		t.Fatal(err)
	}
	if o2.Crawlers.Active.Valid {
		t.Fatal("activity is unavailable without the statistics schema, not zero")
	}
}

func TestCrawlerCountsAreCappedSoTheirCostIsBounded(t *testing.T) {
	e := newEnv(t, flags{stats: true, ingest: true})
	e.exec(`INSERT INTO magnetico_ingest.crawlers (crawler_id, name, token_hash)
SELECT gen_random_uuid(), 'c' || lpad(g::text, 6, '0'), decode(md5(g::text) || md5(g::text || 'x'), 'hex') FROM generate_series(1, 10100) g`)
	o, _, err := e.svc.Overview(bg, admin)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Crawlers.Capped || o.Crawlers.Registered.Value != CrawlerCountCap {
		t.Fatalf("registered %+v capped=%v, want the cap %d reported as 'at least'", o.Crawlers.Registered, o.Crawlers.Capped, CrawlerCountCap)
	}
}

func TestUsersCountsIncludeDisabledAccounts(t *testing.T) {
	e := newEnv(t, flags{stats: true, web: true})
	e.exec(`INSERT INTO magnetico_web.users (username, role) VALUES ('a', 'admin'), ('b', 'user'), ('c', 'user'), ('d', 'user')`)
	e.exec(`UPDATE magnetico_web.users SET active = false WHERE username IN ('c', 'd')`)
	o, _, err := e.svc.Overview(bg, admin)
	if err != nil || !o.Users.Disabled.Valid || o.Users.Disabled.Value != 2 || o.Users.Active.Value != 2 {
		t.Fatalf("%+v %v", o.Users, err)
	}
}

// TestCrawlerActivityIsClassified: disabled, inactive, never connected and
// unavailable are four different things.
func TestCrawlerActivityIsClassified(t *testing.T) {
	recent, old := &Activity{SeenAgo: 2 * time.Minute}, &Activity{SeenAgo: time.Hour}
	cases := []struct {
		name  string
		row   CrawlerRow
		stats bool
		want  ActivityClass
	}{
		{"active", CrawlerRow{Enabled: true, Activity: recent}, true, ActivityActive},
		{"inactive", CrawlerRow{Enabled: true, Activity: old}, true, ActivityInactive},
		{"never connected", CrawlerRow{Enabled: true}, true, ActivityNever},
		{"disabled although recent", CrawlerRow{Enabled: false, Activity: recent}, true, ActivityDisabled},
		{"disabled and never connected", CrawlerRow{Enabled: false}, true, ActivityDisabled},
		{"statistics unavailable", CrawlerRow{Enabled: true}, false, ActivityUnavailable},
		{"statistics unavailable, disabled", CrawlerRow{Enabled: false}, false, ActivityDisabled},
	}
	for _, c := range cases {
		if got := c.row.ActivityClass(c.stats); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if (&Activity{SeenAgo: ActiveWindow}).Recent() || !(&Activity{SeenAgo: ActiveWindow - time.Second}).Recent() {
		t.Error("the activity window is exclusive at its end")
	}
}

// TestCachedCrawlerPageAgesItsActivity: the page is cached; a crawler seen two
// minutes before the read must read as inactive once the page is old enough, and
// the cached value itself must not be modified.
func TestCachedCrawlerPageAgesItsActivity(t *testing.T) {
	fetched := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	page := CrawlerPage{StatsAvailable: true, Items: []CrawlerRow{{Name: "a", Enabled: true, Activity: &Activity{SeenAgo: 2 * time.Minute}}}}
	aged := page.aged(fetched, fetched.Add(20*time.Minute))
	if got := aged.Items[0].Activity.SeenAgo; got != 22*time.Minute {
		t.Fatalf("aged SeenAgo = %v, want 22m", got)
	}
	if aged.Items[0].ActivityClass(true) != ActivityInactive {
		t.Fatal("a crawler last seen 22 minutes ago must be inactive")
	}
	if page.Items[0].Activity.SeenAgo != 2*time.Minute {
		t.Fatal("aging modified the cached page")
	}
}

// Review findings of Task 09A.

// A cached overview cannot recompute "recently active" (it holds a count, not the last-seen
// times), so once a whole activity window has passed since the read the count says nothing
// any more and is unavailable, not the old number.
func TestCachedRecentlyActiveCountExpiresWithTheActivityWindow(t *testing.T) {
	o := agedFixture()
	o.Crawlers = CrawlerCounts{Registered: known(int64(4)), Enabled: known(int64(3)), Active: known(int64(2))}
	fetched := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
	if got := o.aged(fetched, fetched.Add(ActiveWindow-time.Second)); !got.Crawlers.Active.Valid {
		t.Fatal("inside the window the count stays")
	}
	got := o.aged(fetched, fetched.Add(ActiveWindow))
	if got.Crawlers.Active.Valid {
		t.Fatalf("after the window the old count %d must not be shown as current", got.Crawlers.Active.Value)
	}
	if !got.Crawlers.Registered.Valid || !got.Crawlers.Enabled.Valid || o.Crawlers.Active.Valid != true {
		t.Fatal("the registry counts stay and the cached value is not modified")
	}
}

// A last-seen time in the future (the ingest host's clock ahead) is not recent: it came from
// a wrong clock, like a source far in the future on the freshness clocks.
func TestAFutureLastSeenIsNotRecentActivity(t *testing.T) {
	if (&Activity{SeenAgo: -time.Hour}).Recent() {
		t.Fatal("a crawler 'seen' an hour in the future is active")
	}
	if !(&Activity{SeenAgo: -5 * time.Second}).Recent() {
		t.Fatal("a few seconds of skew between hosts must not make a live crawler inactive")
	}
}

func TestActiveCountIgnoresALastSeenFarInTheFuture(t *testing.T) {
	e := newEnv(t, flags{stats: true, ingest: true})
	store := crawlers.New(e.pool)
	c, err := store.Add(bg, "skewed")
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO magnetico_stats.crawler_activity (crawler_id, first_seen_at, last_seen_at, last_ip, records_received)
VALUES ($1, now(), now() + interval '1 day', '192.0.2.1', 1)`, c.ID)
	o, _, err := e.svc.Overview(bg, admin)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Crawlers.Active.Valid || o.Crawlers.Active.Value != 0 {
		t.Fatalf("active = %+v, want 0: a last-seen a day ahead is a wrong clock, not activity", o.Crawlers.Active)
	}
}
