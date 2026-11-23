package trackers

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/server"
)

type fakeBackend struct{}

func (fakeBackend) Verify(context.Context) error { return nil }
func (fakeBackend) Ping(context.Context) error   { return nil }
func (fakeBackend) Close()                       {}

func routeServer(t *testing.T, c *Cache) http.Handler {
	t.Helper()
	pu, _ := url.Parse("https://magnet.example.org")
	srv, err := server.New(server.Config{
		PublicURL: pu, TrustedProxies: []netip.Prefix{}, RequestTimeout: 10 * time.Second,
		SearchTimeout: time.Minute, ShutdownTimeout: 5 * time.Second, Logger: slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
	}, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	Install(srv, c)
	return srv.Handler()
}

func TestMagnetRouteRedirectsWithTrackersAndDoesNoLookup(t *testing.T) {
	src := &fakeSource{urls: []string{"udp://tracker.one:1337/announce", "http://tracker.two:80/announce"}}
	c, _, _ := newCache(t, src, time.Hour)
	if err := c.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	// From here on the source is poisoned: the route must not touch it (it is
	// the only thing that could reach the database).
	loadCalls := src.calls.Load()
	src.set(nil, context.DeadlineExceeded)
	h := routeServer(t, c)

	const hash = "0123456789abcdef0123456789abcdef01234567"
	want := "magnet:?xt=urn:btih:" + hash + "&tr=udp%3A%2F%2Ftracker.one%3A1337%2Fannounce&tr=http%3A%2F%2Ftracker.two%3A80%2Fannounce"
	for _, in := range []string{hash, strings.ToUpper(hash)} {
		for _, method := range []string{"GET", "HEAD"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, "/magnet/"+in, nil))
			res := rec.Result()
			if res.StatusCode != 307 || res.Header.Get("Location") != want {
				t.Fatalf("%s %s: %d %q", method, in, res.StatusCode, res.Header.Get("Location"))
			}
			if res.Header.Get("X-Robots-Tag") == "" || len(res.Cookies()) != 0 {
				t.Fatalf("headers/cookies: %v", res.Header)
			}
			if strings.Contains(res.Header.Get("Location"), "dn=") {
				t.Fatal("dn present")
			}
		}
	}
	if src.calls.Load() != loadCalls {
		t.Fatal("the route queried the source")
	}
	// Anything that is not a canonical info hash is a 404, and no session is
	// needed for the valid ones.
	for _, bad := range []string{"abc", strings.Repeat("g", 40), hash + "0", "%20" + hash[1:]} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/magnet/"+bad, nil))
		if rec.Code != 404 {
			t.Fatalf("%q: %d", bad, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/magnet/"+hash, nil))
	if rec.Code == 307 {
		t.Fatal("POST accepted")
	}
}

func TestMagnetRouteSurvivesRefreshFailureAndPicksUpChangesWithinTheTTL(t *testing.T) {
	src := &fakeSource{urls: []string{"udp://a:1/announce"}}
	c, clk, _ := newCache(t, src, time.Minute)
	c.Load(context.Background())
	run(t, c)
	h := routeServer(t, c)
	const hash = "ffffffffffffffffffffffffffffffffffffffff"
	loc := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/magnet/"+hash, nil))
		return rec.Header().Get("Location")
	}
	if !strings.Contains(loc(), "udp%3A%2F%2Fa%3A1") {
		t.Fatal("initial list")
	}
	src.set(nil, context.DeadlineExceeded)
	clk.Advance(2 * time.Minute)
	loc() // triggers a failing reload
	waitFor(t, "failed reload", func() bool { return c.Failures.Load() == 1 })
	if !strings.Contains(loc(), "udp%3A%2F%2Fa%3A1") {
		t.Fatal("last good list lost after a failed refresh")
	}
	src.set([]string{"udp://b:2/announce"}, nil)
	clk.Advance(2 * time.Minute)
	loc()
	waitFor(t, "recovered list", func() bool { return strings.Contains(loc(), "udp%3A%2F%2Fb%3A2") })
}
