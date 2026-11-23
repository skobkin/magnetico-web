package feed

import (
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
)

const legacyEnvelope = `{"code":401,"status":"error","message":"Invalid credentials.","data":null}`

// seedGolden inserts the three fixture torrents and returns their ids.
func seedGolden(e *env) (id1, id2, id3 int32) {
	id1 = e.seedFixed(strings.Repeat("a", 40), "First", 1000, 1_700_000_000)
	id2 = e.seedFixed(strings.Repeat("b", 40), "Второй торрент", 2000, 1_700_000_060)
	id3 = e.seedFixed(strings.Repeat("c", 40), "Third & last <one>", 3000, 1_700_000_120)
	return
}

func golden(t *testing.T, self string, id1, id2, id3 int32) string {
	t.Helper()
	b, err := os.ReadFile("testdata/feed-golden.xml")
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer("{SELF}", self, "{ID1}", fmt.Sprint(id1), "{ID2}", fmt.Sprint(id2), "{ID3}", fmt.Sprint(id3)).Replace(string(b))
}

func TestStructuralGoldenForBothFeeds(t *testing.T) {
	e := newEnv(t)
	id1, id2, id3 := seedGolden(e)
	tok := e.rssToken()
	for _, tc := range []struct{ path, self string }{
		{V1Path + "?api-token=" + tok.Plaintext, V1Path},
		{V2Path + "?feed_token=" + tok.Plaintext, V2Path},
	} {
		r := e.do(tc.path, nil)
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", tc.path, r.Status, r.Body)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/rss+xml; charset=utf-8" {
			t.Fatalf("content type %q", ct)
		}
		if !strings.HasPrefix(string(r.Body), `<?xml version="1.0" encoding="UTF-8"?>`) {
			t.Fatalf("no XML declaration:\n%.100s", r.Body)
		}
		for _, must := range []string{`xmlns:dc="http://purl.org/dc/elements/1.1/"`, `<dc:creator>Magnetico Web</dc:creator>`} {
			if !strings.Contains(string(r.Body), must) {
				t.Fatalf("%s: legacy author metadata lost, missing %s", tc.path, must)
			}
		}
		got, want := canon(t, r.Body), canon(t, []byte(golden(t, tc.self, id1, id2, id3)))
		if got != want {
			t.Fatalf("%s: structure differs\n got: %s\nwant: %s", tc.path, got, want)
		}
		if strings.Contains(string(r.Body), tok.Plaintext) || strings.Contains(string(r.Body), strings.TrimPrefix(tok.Plaintext, "mgf_")) {
			t.Fatal("the document echoes the token")
		}
	}
	// Both feeds share the generator: apart from their self link the items are identical.
	a := e.do(V1Path+"?api-token="+tok.Plaintext, nil)
	b := e.do(V2Path+"?feed_token="+tok.Plaintext, nil)
	strip := func(s string) string { return strings.NewReplacer(V1Path, "{SELF}", V2Path, "{SELF}").Replace(s) }
	if canon(t, []byte(strip(string(a.Body)))) != canon(t, []byte(strip(string(b.Body)))) {
		t.Fatal("v1 and v2 documents differ beyond the self link")
	}
}

func TestItemIdentityAndWellFormednessWithHostileNames(t *testing.T) {
	e := newEnv(t)
	e.seedFixed(strings.Repeat("d", 40), "control\x01char\x08 and � and ]]> and \"quotes\" 'x'", 5, 1_700_000_000)
	e.seedFixed(strings.Repeat("e", 40), "tab\there\nnewline", 6, 1_700_000_001)
	tok := e.rssToken()
	r := e.do(V2Path+"?feed_token="+tok.Plaintext, nil)
	var doc struct {
		Channel struct {
			Items []struct {
				Title string `xml:"title"`
				GUID  string `xml:"guid"`
				Desc  string `xml:"description"`
				Link  string `xml:"link"`
				Enc   struct {
					URL    string `xml:"url,attr"`
					Length string `xml:"length,attr"`
					Type   string `xml:"type,attr"`
				} `xml:"enclosure"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(r.Body, &doc); err != nil {
		t.Fatalf("not well-formed XML: %v\n%s", err, r.Body)
	}
	if len(doc.Channel.Items) != 2 {
		t.Fatalf("%d items", len(doc.Channel.Items))
	}
	for _, it := range doc.Channel.Items {
		if it.GUID != it.Desc || len(it.GUID) != 40 || it.Link != it.Enc.URL || it.Enc.Type != "application/x-bittorrent" {
			t.Fatalf("item identity: %+v", it)
		}
		if !strings.HasPrefix(it.Link, "magnet:?xt=urn:btih:"+it.GUID+"&dn=") || !strings.Contains(it.Link, "&tr=udp%3A%2F%2Ftracker.one") {
			t.Fatalf("magnet = %s", it.Link)
		}
	}
	// XML 1.0 has no representation for control characters: they appear as U+FFFD.
	if strings.ContainsAny(doc.Channel.Items[1].Title, "\x01\x08") || !strings.Contains(doc.Channel.Items[1].Title, "control") {
		t.Fatalf("title = %q", doc.Channel.Items[1].Title)
	}
	if !strings.Contains(doc.Channel.Items[1].Title, "]]>") || !strings.Contains(doc.Channel.Items[1].Title, `"quotes"`) {
		t.Fatalf("title = %q", doc.Channel.Items[1].Title)
	}
	if doc.Channel.Items[0].Enc.Length != "6" {
		t.Fatalf("length = %s", doc.Channel.Items[0].Enc.Length)
	}
}

func TestPagesAreNewestFirstWithTheDocumentedBoundaries(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.noTrackers = true })
	e.exec(`INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
	    SELECT decode(md5(i::text) || substr(md5((i+1)::text),1,8),'hex'), 'torrent ' || i, i, 1700000000 + i, 1 FROM generate_series(1, 1005) i`)
	tok := e.rssToken()
	guids := func(path string) []string {
		r := e.do(path, nil)
		if r.Status != 200 {
			t.Fatalf("%s: %d", path, r.Status)
		}
		var doc struct {
			Items []struct {
				Title string `xml:"title"`
			} `xml:"channel>item"`
		}
		if err := xml.Unmarshal(r.Body, &doc); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range doc.Items {
			out = append(out, it.Title)
		}
		return out
	}
	q := "?feed_token=" + tok.Plaintext
	p1 := guids(V2Path + q)
	if len(p1) != 1000 || p1[0] != "torrent 1005" || p1[999] != "torrent 6" {
		t.Fatalf("page 1: %d items, %s … %s", len(p1), p1[0], p1[len(p1)-1])
	}
	p2 := guids(V2Path + q + "&page=2")
	if len(p2) != 5 || p2[0] != "torrent 5" || p2[4] != "torrent 1" {
		t.Fatalf("page 2: %v", p2)
	}
	for _, same := range []string{"&page=0", "&page=abc", "&page=", "&page=-3", "&page=1.5"} {
		if got := guids(V2Path + q + same); len(got) != 1000 || got[0] != p1[0] {
			t.Errorf("%s did not fall back to page 1", same)
		}
	}
	for _, clamp := range []string{"&page=10", "&page=11", "&page=99999"} {
		if got := guids(V2Path + q + clamp); len(got) != 0 {
			t.Errorf("%s: %d items beyond the data", clamp, len(got))
		}
	}
	// v1 has the same page semantics.
	if got := guids(V1Path + "?api-token=" + tok.Plaintext); len(got) != 1000 || got[0] != "torrent 1005" {
		t.Fatalf("v1 page 1: %d", len(got))
	}
	// Without trackers the magnet carries no tr parameter.
	r := e.do(V2Path+q+"&page=2", nil)
	if strings.Contains(string(r.Body), "&amp;tr=") {
		t.Fatal("trackers present although none are configured")
	}
}

func TestCredentialPoliciesAreIndependent(t *testing.T) {
	e := newEnv(t)
	seedGolden(e)
	rss := e.rssToken()
	api := e.apiToken()
	const legacy = "0123456789abcdef0123456789abcdef"
	lh := sha256.Sum256([]byte(legacy))
	e.exec(`INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy) VALUES ($1,$2,true)`, lh[:], e.user.ID)

	type tc struct {
		name string
		path string
		hdr  map[string]string
		ck   []*http.Cookie
		want int
	}
	cookie := func(v string) []*http.Cookie { return []*http.Cookie{{Name: "api-token", Value: v}} }
	v1 := func(q string) string { return V1Path + q }
	v2 := func(q string) string { return V2Path + q }
	for _, c := range []tc{
		// v1: three transports, RSS tokens only.
		{"v1 query rss", v1("?api-token=" + rss.Plaintext), nil, nil, 200},
		{"v1 header rss", v1(""), map[string]string{"api-token": rss.Plaintext}, nil, 200},
		{"v1 cookie rss", v1(""), nil, cookie(rss.Plaintext), 200},
		{"v1 query legacy", v1("?api-token=" + legacy), nil, nil, 200},
		{"v1 header legacy", v1(""), map[string]string{"api-token": legacy}, nil, 200},
		{"v1 cookie legacy", v1(""), nil, cookie(legacy), 200},
		{"v1 header wins over a bad query", v1("?api-token=bad"), map[string]string{"api-token": rss.Plaintext}, nil, 200},
		{"v1 a bad header is not rescued by a good query", v1("?api-token=" + rss.Plaintext), map[string]string{"api-token": "bad"}, nil, 401},
		{"v1 none", v1(""), nil, nil, 401},
		{"v1 api token (query)", v1("?api-token=" + api.Plaintext), nil, nil, 401},
		{"v1 api token (header)", v1(""), map[string]string{"api-token": api.Plaintext}, nil, 401},
		{"v1 api token (cookie)", v1(""), nil, cookie(api.Plaintext), 401},
		{"v1 api token as Bearer", v1(""), map[string]string{"Authorization": "Bearer " + api.Plaintext}, nil, 401},
		{"v1 rss token as Bearer", v1(""), map[string]string{"Authorization": "Bearer " + rss.Plaintext}, nil, 401},
		{"v1 feed_token is not a v1 transport", v1("?feed_token=" + rss.Plaintext), nil, nil, 401},
		// v2: feed_token only, RSS tokens only.
		{"v2 feed_token rss", v2("?feed_token=" + rss.Plaintext), nil, nil, 200},
		{"v2 feed_token legacy", v2("?feed_token=" + legacy), nil, nil, 200},
		{"v2 none", v2(""), nil, nil, 401},
		{"v2 api token in feed_token", v2("?feed_token=" + api.Plaintext), nil, nil, 401},
		{"v2 legacy transport query", v2("?api-token=" + rss.Plaintext), nil, nil, 401},
		{"v2 legacy transport header", v2(""), map[string]string{"api-token": rss.Plaintext}, nil, 401},
		{"v2 legacy transport cookie", v2(""), nil, cookie(rss.Plaintext), 401},
		{"v2 feed_token header", v2(""), map[string]string{"feed_token": rss.Plaintext}, nil, 401},
		{"v2 feed_token cookie", v2(""), nil, []*http.Cookie{{Name: "feed_token", Value: rss.Plaintext}}, 401},
		{"v2 Bearer rss", v2(""), map[string]string{"Authorization": "Bearer " + rss.Plaintext}, nil, 401},
		{"v2 Bearer api", v2(""), map[string]string{"Authorization": "Bearer " + api.Plaintext}, nil, 401},
		{"v2 repeated feed_token", v2("?feed_token=bad&feed_token=" + rss.Plaintext), nil, nil, 401},
		{"v2 empty feed_token", v2("?feed_token="), nil, nil, 401},
	} {
		r := e.do(c.path, c.hdr, c.ck...)
		if r.Status != c.want {
			t.Errorf("%s: %d, want %d", c.name, r.Status, c.want)
			continue
		}
		if c.want != 401 {
			continue
		}
		// Failure envelopes: v1 the legacy JSON, v2 problem+json; never mixed.
		if strings.HasPrefix(c.path, V1Path) {
			if string(r.Body) != legacyEnvelope || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("%s: v1 envelope = %q (%s)", c.name, r.Body, r.Header.Get("Content-Type"))
			}
		} else {
			var p map[string]any
			if json.Unmarshal(r.Body, &p) != nil || p["type"] != "urn:magnetico:problem:unauthenticated" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/problem+json") {
				t.Errorf("%s: v2 problem = %q (%s)", c.name, r.Body, r.Header.Get("Content-Type"))
			}
			if _, ok := p["code"]; ok {
				t.Errorf("%s: v2 failure uses the legacy envelope", c.name)
			}
		}
	}
}

func TestRegisteredFeedPoliciesAreFixedPerRoute(t *testing.T) {
	e := newEnv(t)
	got := map[string]string{}
	for _, r := range e.srv.Router().Routes() {
		if r.Access.String() == "feed" {
			got[r.Pattern] = "feed"
		}
	}
	if len(got) != 2 || got["GET "+V1Path] == "" || got["GET "+V2Path] == "" {
		t.Fatalf("feed routes = %v", got)
	}
	if v1Policy.name == v2Policy.name || v1Policy.path == v2Policy.path {
		t.Fatal("policies are not distinct")
	}
}

func TestEveryResponseCarriesTheCredentialSafeHeaders(t *testing.T) {
	e := newEnv(t)
	seedGolden(e)
	tok := e.rssToken()
	for name, path := range map[string]string{
		"v1 ok": V1Path + "?api-token=" + tok.Plaintext, "v2 ok": V2Path + "?feed_token=" + tok.Plaintext,
		"v1 401": V1Path + "?api-token=nope", "v2 401": V2Path + "?feed_token=nope", "v2 none": V2Path,
	} {
		r := e.do(path, nil)
		if r.Header.Get("Referrer-Policy") != "no-referrer" || r.Header.Get("Cache-Control") != "private, no-store" {
			t.Errorf("%s: Referrer-Policy=%q Cache-Control=%q", name, r.Header.Get("Referrer-Policy"), r.Header.Get("Cache-Control"))
		}
		if r.Header.Get("X-Robots-Tag") != "noindex, nofollow" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing indexing/security headers", name)
		}
		if strings.Contains(r.Header.Get("Location"), tok.Plaintext) {
			t.Errorf("%s: token in Location", name)
		}
	}
}

func TestRevocationOwnerStateAndExpiryApplyBeforeTheCache(t *testing.T) {
	e := newEnv(t)
	seedGolden(e)
	tok := e.rssToken()
	path := V2Path + "?feed_token=" + tok.Plaintext
	if e.do(path, nil).Status != 200 || e.do(path, nil).Status != 200 {
		t.Fatal("baseline")
	}
	if e.rows.calls.Load() != 1 {
		t.Fatalf("the second request must be served from the cache (%d reads)", e.rows.calls.Load())
	}
	// The page is cached, yet each change takes effect on the very next request.
	if _, err := e.store.Disable(tctx(t), "alice"); err != nil {
		t.Fatal(err)
	}
	if r := e.do(path, nil); r.Status != 401 {
		t.Fatalf("disabled owner: %d", r.Status)
	}
	if err := e.store.Enable(tctx(t), "alice"); err != nil {
		t.Fatal(err)
	}
	if e.do(path, nil).Status != 200 {
		t.Fatal("not usable after enable")
	}
	list, _ := e.store.ListRSSTokens(tctx(t), e.user.ID)
	if err := e.store.RevokeRSSToken(tctx(t), e.user.ID, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if r := e.do(path, nil); r.Status != 401 {
		t.Fatalf("revoked: %d", r.Status)
	}
	e.store.Disable(tctx(t), "alice")
	e.store.Enable(tctx(t), "alice")
	if e.do(path, nil).Status != 401 {
		t.Fatal("enable revived a revoked token")
	}
	// Expiry.
	tok2 := e.rssToken()
	p2 := V2Path + "?feed_token=" + tok2.Plaintext
	h := sha256.Sum256([]byte(tok2.Plaintext))
	e.exec(`UPDATE magnetico_web.rss_tokens SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, h[:])
	if e.do(p2, nil).Status != 401 {
		t.Fatal("expired token accepted")
	}
}

func TestMigratedLegacyTokenSurvivesDisableEnableAndDiesOnReset(t *testing.T) {
	e := newEnv(t)
	seedGolden(e)
	const legacy = "00ff00ff00ff00ff00ff00ff00ff00ff"
	lh := sha256.Sum256([]byte(legacy))
	e.exec(`INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy, created_at) VALUES ($1,$2,true,'2019-05-06 07:08:09+00')`, lh[:], e.user.ID)
	dump := func() string {
		var s string
		e.pool.QueryRow(tctx(t), `SELECT t::text FROM magnetico_web.rss_tokens t WHERE token_hash = $1`, lh[:]).Scan(&s)
		return s
	}
	before := dump()
	url := V1Path + "?api-token=" + legacy
	if e.do(url, nil).Status != 200 {
		t.Fatal("legacy token rejected")
	}
	before = dump() // last_used_at is written by the first use
	if _, err := e.store.Disable(tctx(t), "alice"); err != nil {
		t.Fatal(err)
	}
	if r := e.do(url, nil); r.Status != 401 || string(r.Body) != legacyEnvelope {
		t.Fatalf("disabled: %d %s", r.Status, r.Body)
	}
	if dump() != before {
		t.Fatal("disable changed the legacy token row")
	}
	if err := e.store.Enable(tctx(t), "alice"); err != nil {
		t.Fatal(err)
	}
	if dump() != before || e.do(url, nil).Status != 200 {
		t.Fatal("the unchanged v1 URL does not work again after enable")
	}
	// A password reset revokes legacy tokens too.
	h, _ := hashFor(t)
	if _, err := e.store.SetPassword(tctx(t), "alice", h); err != nil {
		t.Fatal(err)
	}
	if e.do(url, nil).Status != 401 {
		t.Fatal("a password reset left the legacy token valid")
	}
}

func TestCacheSingleFlightTTLAndTrackerVersion(t *testing.T) {
	e := newEnv(t)
	seedGolden(e)
	tok := e.rssToken()
	path := V2Path + "?feed_token=" + tok.Plaintext

	// Single flight: many concurrent requests, one read.
	e.rows.gate = make(chan struct{})
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.do(path, nil).Status == 200 {
				ok.Add(1)
			}
		}()
	}
	for e.rows.calls.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(e.rows.gate)
	wg.Wait()
	if ok.Load() != 24 || e.rows.calls.Load() != 1 {
		t.Fatalf("ok=%d reads=%d", ok.Load(), e.rows.calls.Load())
	}
	e.rows.gate = nil
	// Cached within the TTL; rebuilt after it.
	e.do(path, nil)
	if e.rows.calls.Load() != 1 {
		t.Fatal("not cached within the TTL")
	}
	e.clock.Advance(2 * time.Minute)
	e.do(path, nil)
	if e.rows.calls.Load() != 2 {
		t.Fatalf("not rebuilt after the TTL (%d)", e.rows.calls.Load())
	}
	// A tracker change shows up without waiting for the TTL (new snapshot version).
	urls := []string{"udp://tracker.new:6969/announce"}
	e.source.urls.Store(&urls)
	e.trackers.Invalidate()
	ctx := tctx(t)
	go e.trackers.Run(ctx)
	for i := 0; i < 200 && e.trackers.Snapshot().Version < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	r := e.do(path, nil)
	if !strings.Contains(string(r.Body), "tracker.new") || strings.Contains(string(r.Body), "tracker.one") {
		t.Fatalf("tracker change not visible:\n%.400s", r.Body)
	}
	// The two feeds do not share cache entries.
	n := e.rows.calls.Load()
	e.do(V1Path+"?api-token="+tok.Plaintext, nil)
	if e.rows.calls.Load() != n+1 {
		t.Fatal("v1 was served from the v2 cache entry")
	}
}

func TestStorageFailuresAreNotCachedAndMapToTheRightEnvelope(t *testing.T) {
	e := newEnv(t)
	seedGolden(e)
	tok := e.rssToken()
	boom := fmt.Errorf("pq: connection to secret-host failed")
	e.rows.err.Store(&boom)
	r := e.do(V2Path+"?feed_token="+tok.Plaintext, nil)
	if r.Status != 500 || strings.Contains(string(r.Body), "secret-host") {
		t.Fatalf("v2 failure: %d %s", r.Status, r.Body)
	}
	r = e.do(V1Path+"?api-token="+tok.Plaintext, nil)
	if r.Status != 500 || !strings.Contains(string(r.Body), `"status":"fail"`) || strings.Contains(string(r.Body), "secret-host") {
		t.Fatalf("v1 failure: %d %s", r.Status, r.Body)
	}
	var a []string
	_ = a
	e.rows.err.Store(nil)
	if r := e.do(V2Path+"?feed_token="+tok.Plaintext, nil); r.Status != 200 {
		t.Fatalf("a failure was cached: %d", r.Status)
	}
	if strings.Contains(e.log.String(), "secret-host") || strings.Contains(e.log.String(), tok.Plaintext) {
		t.Fatal("the log carries the driver text or the token")
	}
}

func TestRateLimits(t *testing.T) {
	fail, _ := ratelimit.New(ratelimit.Config{Burst: 2, Interval: time.Hour, MaxKeys: 10})
	use, _ := ratelimit.New(ratelimit.Config{Burst: 2, Interval: time.Hour, MaxKeys: 10})
	e := newEnv(t, func(o *envOpts) { o.failLim = fail; o.useLim = use })
	seedGolden(e)
	tok := e.rssToken()
	for i := 0; i < 2; i++ {
		if r := e.do(V2Path+"?feed_token="+tok.Plaintext, nil); r.Status != 200 {
			t.Fatalf("use %d: %d", i, r.Status)
		}
	}
	r := e.do(V2Path+"?feed_token="+tok.Plaintext, nil)
	if r.Status != 429 || r.Header.Get("Retry-After") == "" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("v2 token limit: %d", r.Status)
	}
	r = e.do(V1Path+"?api-token="+tok.Plaintext, nil)
	if r.Status != 429 || !strings.Contains(string(r.Body), `"code":429`) {
		t.Fatalf("v1 token limit: %d %s", r.Status, r.Body)
	}
	// Failures: burst of two, then limited, whatever the credential.
	var got []int
	for i := 0; i < 4; i++ {
		got = append(got, e.do(V2Path+"?feed_token=bad", nil).Status)
	}
	if got[0] != 401 || got[1] != 401 || got[2] != 429 || got[3] != 429 {
		t.Fatalf("failure statuses = %v", got)
	}
}

func TestGenerationGateBoundsConcurrentBuilds(t *testing.T) {
	e := newEnv(t)
	seedGolden(e)
	tok := e.rssToken()
	e.rows.gate = make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]int, 3)
	for i := 0; i < 3; i++ { // three distinct pages: two build, the third waits and gives up
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = e.do(fmt.Sprintf("%s?feed_token=%s&page=%d", V2Path, tok.Plaintext, i+1), nil).Status
		}(i)
		time.Sleep(30 * time.Millisecond)
	}
	time.Sleep(genWait + 500*time.Millisecond)
	close(e.rows.gate)
	wg.Wait()
	busy, ok := 0, 0
	for _, s := range statuses {
		switch s {
		case 503:
			busy++
		case 200:
			ok++
		}
	}
	if busy != 1 || ok != 2 {
		t.Fatalf("statuses = %v", statuses)
	}
}

func TestLogsCarryNoTokensOrQueries(t *testing.T) {
	e := newEnv(t)
	seedGolden(e)
	tok := e.rssToken()
	api := e.apiToken()
	e.do(V1Path+"?api-token="+tok.Plaintext, nil)
	e.do(V2Path+"?feed_token="+tok.Plaintext+"&page=2", nil)
	e.do(V1Path, map[string]string{"api-token": tok.Plaintext})
	e.do(V1Path, nil, &http.Cookie{Name: "api-token", Value: tok.Plaintext})
	e.do(V2Path+"?feed_token="+api.Plaintext, nil)
	e.do(V1Path+"?api-token=wrong-token-value", nil)
	logs := e.log.String()
	for _, secret := range []string{tok.Plaintext, strings.TrimPrefix(tok.Plaintext, "mgf_"), api.Plaintext, strings.TrimPrefix(api.Plaintext, "mgt_"), "wrong-token-value", "feed_token", "api-token="} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
	if !strings.Contains(logs, "user_id=") || !strings.Contains(logs, "feed=v1") || !strings.Contains(logs, "feed=v2") {
		t.Errorf("expected structured feed log lines:\n%s", logs)
	}
	_ = url.QueryEscape
}
