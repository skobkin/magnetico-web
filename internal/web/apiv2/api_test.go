package apiv2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/search"
	"github.com/skobkin/magnetico-web/internal/web/torrents"
)

const unauth = "urn:magnetico:problem:unauthenticated"

func TestBearerIsTheOnlyCredentialAndFailuresAreIdentical(t *testing.T) {
	e := newEnv(t)
	id := e.seed("x", 5, 1, nil)
	path := fmt.Sprintf("/api/v2/torrents/%d", id)
	rssTok, _ := e.store.CreateRSSToken(tctx(t), e.user.ID, "", nil)
	const legacy = "0123456789abcdef0123456789abcdef"
	lh := sha256.Sum256([]byte(legacy))
	e.exec(`INSERT INTO magnetico_web.rss_tokens (token_hash, user_id, legacy) VALUES ($1,$2,true)`, lh[:], e.user.ID)
	cookie := e.sessionCookie()

	if r := e.api(path); r.Status != 200 {
		t.Fatalf("valid token: %d %s", r.Status, r.Body)
	}
	if r := e.do("GET", path, map[string]string{"Authorization": "bearer " + e.token}); r.Status != 200 {
		t.Fatalf("lower-case scheme: %d", r.Status)
	}

	type try struct {
		name   string
		path   string
		hdr    map[string]string
		cookie *http.Cookie
	}
	bearer := func(v string) map[string]string { return map[string]string{"Authorization": v} }
	var first resp
	for i, tc := range []try{
		{"no credential", path, nil, nil},
		{"session cookie only", path, nil, cookie},
		{"rss token as bearer", path, bearer("Bearer " + rssTok.Plaintext), nil},
		{"legacy token as bearer", path, bearer("Bearer " + legacy), nil},
		{"unknown token", path, bearer("Bearer mgt_" + strings.Repeat("A", 43)), nil},
		{"wrong scheme", path, bearer("Basic " + e.token), nil},
		{"token scheme", path, bearer("Token " + e.token), nil},
		{"no scheme", path, bearer(e.token), nil},
		{"double space", path, bearer("Bearer  " + e.token), nil},
		{"trailing junk", path, bearer("Bearer " + e.token + " x"), nil},
		{"huge token", path, bearer("Bearer mgt_" + strings.Repeat("A", 400)), nil},
		{"token in access_token", path + "?access_token=" + e.token, nil, nil},
		{"token in api_token", path + "?api_token=" + e.token, nil, nil},
		{"token in token", path + "?token=" + e.token, nil, nil},
		{"token in api-token", path + "?api-token=" + e.token, nil, nil},
		{"rss token as feed_token", path + "?feed_token=" + rssTok.Plaintext, nil, nil},
		{"api token in a cookie", path, nil, &http.Cookie{Name: "api-token", Value: e.token}},
		{"api token in api-token header", path, map[string]string{"api-token": e.token}, nil},
	} {
		r := e.do("GET", tc.path, tc.hdr, tc.cookie)
		if r.Status != 401 {
			t.Errorf("%s: %d", tc.name, r.Status)
			continue
		}
		if typ, _ := r.problem(t); typ != unauth {
			t.Errorf("%s: type %s", tc.name, typ)
		}
		if !strings.HasPrefix(r.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: WWW-Authenticate = %q", tc.name, r.Header.Get("WWW-Authenticate"))
		}
		if i == 0 {
			first = r
		}
		if tc.name != "no credential" && tc.name != "session cookie only" && !bytes.Equal(r.Body, first.Body) {
			t.Errorf("%s: body differs from the missing-credential body", tc.name)
		}
	}
	// A duplicated Authorization header is not a credential.
	req := e.do("GET", path, nil)
	_ = req
	if r := e.doDup(path, "Bearer "+e.token, "Bearer "+e.token); r.Status != 401 {
		t.Fatalf("two Authorization headers: %d", r.Status)
	}
	// The API token does not work as an RSS credential either direction is
	// asserted by the feed tests; here the RSS token must not open any JSON route.
	for _, p := range []string{"/api/v2/torrents", path, path + "/files", path + "/files/tree"} {
		if r := e.do("GET", p, bearer("Bearer "+rssTok.Plaintext)); r.Status != 401 {
			t.Errorf("rss token on %s: %d", p, r.Status)
		}
	}
}

func (e *env) doDup(path string, v1, v2 string) resp {
	e.t.Helper()
	req := newReq(path)
	req.Header.Add("Authorization", v1)
	req.Header.Add("Authorization", v2)
	return e.serve(req)
}

func TestRevocationExpiryAndDisableTakeEffectOnTheNextRequest(t *testing.T) {
	e := newEnv(t)
	path := "/api/v2/torrents?limit=1"
	body401 := func() []byte {
		r := e.do("GET", path, map[string]string{"Authorization": "Bearer mgt_" + strings.Repeat("Z", 43)})
		return r.Body
	}()
	if e.api(path).Status != 200 {
		t.Fatal("baseline")
	}
	// Disable suspends: 401 while disabled (same body as any failure), usable again after enable.
	if _, err := e.store.Disable(tctx(t), "alice"); err != nil {
		t.Fatal(err)
	}
	r := e.api(path)
	if r.Status != 401 || !bytes.Equal(r.Body, body401) {
		t.Fatalf("disabled: %d", r.Status)
	}
	if err := e.store.Enable(tctx(t), "alice"); err != nil {
		t.Fatal(err)
	}
	if e.api(path).Status != 200 {
		t.Fatal("not usable after enable")
	}
	// Explicit revocation is permanent, also across disable/enable.
	list, _ := e.store.ListAPITokens(tctx(t), e.user.ID)
	if err := e.store.RevokeAPIToken(tctx(t), e.user.ID, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if r := e.api(path); r.Status != 401 || !bytes.Equal(r.Body, body401) {
		t.Fatalf("revoked: %d", r.Status)
	}
	if _, err := e.store.Disable(tctx(t), "alice"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Enable(tctx(t), "alice"); err != nil {
		t.Fatal(err)
	}
	if e.api(path).Status != 401 {
		t.Fatal("enable revived a revoked token")
	}
	// Expired.
	tok, _ := e.store.CreateAPIToken(tctx(t), e.user.ID, "short", nil)
	e.exec(`UPDATE magnetico_web.api_tokens SET expires_at = now() + interval '1 hour' WHERE token_hash = $1`, sha256sum(tok.Plaintext))
	hdr := map[string]string{"Authorization": "Bearer " + tok.Plaintext}
	if e.do("GET", path, hdr).Status != 200 {
		t.Fatal("unexpired")
	}
	e.exec(`UPDATE magnetico_web.api_tokens SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, sha256sum(tok.Plaintext))
	if r := e.do("GET", path, hdr); r.Status != 401 || !bytes.Equal(r.Body, body401) {
		t.Fatalf("expired: %d", r.Status)
	}
}

func sha256sum(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func TestConcurrentDisableVersusRequests(t *testing.T) {
	e := newEnv(t)
	var disabled, enabling, enabled atomic.Bool
	var wg sync.WaitGroup
	var bad atomic.Int64
	stop := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				wasDisabled, wasEnabling, wasEnabled := disabled.Load(), enabling.Load(), enabled.Load()
				r := e.api("/api/v2/torrents?limit=1")
				// A request that STARTED after Disable returned (and before Enable
				// began) must fail; one that started after Enable returned must work.
				if wasDisabled && !wasEnabling && r.Status == 200 && !enabling.Load() {
					bad.Add(1)
				}
				if wasEnabled && r.Status != 200 {
					bad.Add(1)
				}
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := e.store.Disable(tctx(t), "alice"); err != nil {
		t.Fatal(err)
	}
	disabled.Store(true)
	time.Sleep(150 * time.Millisecond)
	enabling.Store(true) // from here a request may legitimately succeed, before Enable returns
	if err := e.store.Enable(tctx(t), "alice"); err != nil {
		t.Fatal(err)
	}
	enabled.Store(true)
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
	if n := bad.Load(); n != 0 {
		t.Fatalf("%d requests violated the disable/enable ordering", n)
	}
}

func TestLastUseIsRecordedAndThrottled(t *testing.T) {
	e := newEnv(t)
	var before *time.Time
	if err := e.pool.QueryRow(tctx(t), `SELECT last_used_at FROM magnetico_web.api_tokens WHERE user_id = $1`, e.user.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != nil {
		t.Fatal("last_used_at preset")
	}
	e.api("/api/v2/torrents?limit=1")
	var t1, t2 time.Time
	if err := e.pool.QueryRow(tctx(t), `SELECT last_used_at FROM magnetico_web.api_tokens WHERE user_id = $1`, e.user.ID).Scan(&t1); err != nil {
		t.Fatal(err)
	}
	if t1.IsZero() {
		t.Fatal("last use not recorded")
	}
	e.api("/api/v2/torrents?limit=1")
	if err := e.pool.QueryRow(tctx(t), `SELECT last_used_at FROM magnetico_web.api_tokens WHERE user_id = $1`, e.user.ID).Scan(&t2); err != nil {
		t.Fatal(err)
	}
	if !t1.Equal(t2) {
		t.Fatal("last use written again within the throttle interval")
	}
}

func TestFailureAndUseRateLimits(t *testing.T) {
	fail, _ := ratelimit.New(ratelimit.Config{Burst: 3, Interval: time.Hour, MaxKeys: 10})
	use, _ := ratelimit.New(ratelimit.Config{Burst: 2, Interval: time.Hour, MaxKeys: 10})
	e := newEnv(t, func(o *envOpts) { o.failLim = fail; o.useLim = use })
	// Successful requests refund the address bucket but spend the token bucket.
	for i := 0; i < 2; i++ {
		if r := e.api("/api/v2/torrents?limit=1"); r.Status != 200 {
			t.Fatalf("use %d: %d", i, r.Status)
		}
	}
	r := e.api("/api/v2/torrents?limit=1")
	if r.Status != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("token limit: %d", r.Status)
	}
	if typ, _ := r.problem(t); typ != "urn:magnetico:problem:rate-limited" {
		t.Fatalf("type %s", typ)
	}
	// Failures from one address are limited before the token is looked up.
	bad := map[string]string{"Authorization": "Bearer mgt_" + strings.Repeat("Q", 43)}
	var got []int
	for i := 0; i < 5; i++ {
		got = append(got, e.do("GET", "/api/v2/torrents", bad).Status)
	}
	// 2 successes earlier were refunded; bursts of 3 failures, then 429.
	if got[0] != 401 || got[1] != 401 || got[2] != 401 || got[3] != 429 || got[4] != 429 {
		t.Fatalf("failure statuses = %v", got)
	}
}

func TestListContractAndKeysetPagination(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.trackers = nil })
	var ids []int32
	for i := 0; i < 7; i++ {
		ids = append(ids, e.seed(fmt.Sprintf("Alpha release %d", i), int64(100+i), int64(1_700_000_000+i), nil))
	}
	e.seed("Something else", 5000, 1_600_000_000, nil)

	type page struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"next_cursor"`
	}
	var seen []int32
	cursor := ""
	for n := 0; ; n++ {
		if n > 10 {
			t.Fatal("pagination does not end")
		}
		p := "/api/v2/torrents?q=alpha&limit=3"
		if cursor != "" {
			p += "&cursor=" + url.QueryEscape(cursor)
		}
		r := e.api(p)
		var pg page
		r.json(t, &pg)
		if r.Header.Get("Cache-Control") != "private, no-store" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("headers: %v", r.Header)
		}
		for _, it := range pg.Items {
			seen = append(seen, int32(it["id"].(float64)))
		}
		if pg.NextCursor == nil {
			break
		}
		cursor = *pg.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("seen %v", seen)
	}
	for i := 1; i < len(seen); i++ { // newest first, ids from newest to oldest
		if seen[i] >= seen[i-1] {
			t.Fatalf("order: %v", seen)
		}
	}
	// Field contract.
	var one page
	e.api("/api/v2/torrents?q=something&limit=1").json(t, &one)
	it := one.Items[0]
	for _, k := range []string{"id", "info_hash", "name", "total_size", "file_count", "discovered_at"} {
		if _, ok := it[k]; !ok {
			t.Fatalf("missing %s in %v", k, it)
		}
	}
	if len(it) != 6 || it["name"] != "Something else" || it["total_size"].(float64) != 5000 || it["discovered_at"] != "2020-09-13T12:26:40Z" {
		t.Fatalf("item = %v", it)
	}
	if h := it["info_hash"].(string); len(h) != 40 || h != strings.ToLower(h) {
		t.Fatalf("info_hash = %q", h)
	}
	if one.NextCursor != nil {
		t.Fatal("next_cursor set on the last page")
	}
	// Sorting.
	var by page
	e.api("/api/v2/torrents?sort=size&order=asc&limit=2").json(t, &by)
	if by.Items[0]["total_size"].(float64) != 100 {
		t.Fatalf("sort=size asc: %v", by.Items)
	}
	e.api("/api/v2/torrents?q=alpha&sort=name&limit=1").json(t, &by)
	if !strings.HasPrefix(by.Items[0]["name"].(string), "Alpha release 0") {
		t.Fatalf("sort=name: %v", by.Items)
	}
	// Short words are ignored: everything is listed.
	var all page
	e.api("/api/v2/torrents?q=ab&limit=100").json(t, &all)
	if len(all.Items) != 8 {
		t.Fatalf("ignored short word: %d items", len(all.Items))
	}
	// An empty result is a valid page.
	var none page
	e.api("/api/v2/torrents?q=nomatchhere").json(t, &none)
	if none.Items == nil || len(none.Items) != 0 || none.NextCursor != nil {
		t.Fatalf("empty = %+v", none)
	}
	_ = ids
}

func TestInfoHashLookupAndParameterValidation(t *testing.T) {
	e := newEnv(t)
	id := e.seed("Lookup me", 10, 1, nil)
	var hash string
	if err := e.pool.QueryRow(tctx(t), `SELECT encode(info_hash,'hex') FROM magneticod.torrents WHERE id=$1`, id).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"next_cursor"`
	}
	for _, h := range []string{hash, strings.ToUpper(hash)} {
		e.api("/api/v2/torrents?info_hash="+h).json(t, &out)
		if len(out.Items) != 1 || out.Items[0]["info_hash"] != hash || out.NextCursor != nil {
			t.Fatalf("lookup %s: %+v", h, out)
		}
	}
	e.api("/api/v2/torrents?info_hash="+strings.Repeat("0", 40)).json(t, &out)
	if len(out.Items) != 0 {
		t.Fatal("unknown hash returned items")
	}
	for name, q := range map[string]string{
		"short hash":      "info_hash=abc",
		"non-hex":         "info_hash=" + strings.Repeat("g", 40),
		"empty hash":      "info_hash=",
		"hash with q":     "info_hash=" + hash + "&q=lookup",
		"hash with sort":  "info_hash=" + hash + "&sort=size",
		"hash with order": "info_hash=" + hash + "&order=asc",
		"hash with cur":   "info_hash=" + hash + "&cursor=abc",
		"limit zero":      "limit=0",
		"limit 101":       "limit=101",
		"limit text":      "limit=abc",
		"limit float":     "limit=5.0",
		"limit negative":  "limit=-1",
		"limit space":     "limit=%205",
		"limit leading 0": "limit=05",
		"limit empty":     "limit=",
		"repeated q":      "q=abc&q=def",
		"repeated limit":  "limit=1&limit=2",
		"repeated hash":   "info_hash=" + hash + "&info_hash=" + hash,
		"bad cursor":      "cursor=not-a-cursor",
		"long query":      "q=" + strings.Repeat("a", 600),
		"too many terms":  "q=aaa+bbb+ccc+ddd+eee+fff+ggg+hhh+iii",
		"bad escape":      "q=%zz",
	} {
		r := e.api("/api/v2/torrents?" + q)
		if r.Status != 400 {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
			continue
		}
		if typ, _ := r.problem(t); typ != "urn:magnetico:problem:invalid-parameter" {
			t.Errorf("%s: %s", name, typ)
		}
	}
	// Unknown sort/order keep the documented fallback; unknown parameters are ignored.
	if r := e.api("/api/v2/torrents?sort=bogus&order=sideways&unknown=1&limit=100"); r.Status != 200 {
		t.Fatalf("fallbacks: %d", r.Status)
	}
	// A cursor of one search does not work for another.
	for i := 0; i < 4; i++ {
		e.seed(fmt.Sprintf("Cursor test %d", i), 1, int64(10+i), nil)
	}
	e.api("/api/v2/torrents?q=cursor&limit=1").json(t, &out)
	if out.NextCursor == nil {
		t.Fatal("no cursor")
	}
	if r := e.api("/api/v2/torrents?q=lookup&limit=1&cursor=" + url.QueryEscape(*out.NextCursor)); r.Status != 400 {
		t.Fatalf("foreign cursor: %d", r.Status)
	}
}

type failingSearch struct {
	torrents.Searcher
	err error
}

func (f failingSearch) Search(context.Context, search.Params) (search.Result, error) {
	return search.Result{}, f.err
}

func TestSearchFailuresMapToProblems(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		retry  string
		typ    string
	}{
		{search.ErrBusy, 503, "5", "temporarily-unavailable"},
		{search.ErrTimeout, 503, "30", "temporarily-unavailable"},
		{fmt.Errorf("wrapped: %w", search.ErrTimeout), 503, "30", "temporarily-unavailable"},
		{search.ErrInvalidCursor, 400, "", "invalid-parameter"},
		{search.ErrInvalidQuery, 400, "", "invalid-parameter"},
		{fmt.Errorf("boom SECRET-DETAIL"), 500, "", "internal-error"},
	} {
		e := newEnv(t, func(o *envOpts) {
			o.search = func(s torrents.Searcher) torrents.Searcher { return failingSearch{s, tc.err} }
		})
		r := e.api("/api/v2/torrents")
		if r.Status != tc.status || r.Header.Get("Retry-After") != tc.retry {
			t.Errorf("%v: %d retry=%q", tc.err, r.Status, r.Header.Get("Retry-After"))
			continue
		}
		if typ, _ := r.problem(t); typ != "urn:magnetico:problem:"+tc.typ {
			t.Errorf("%v: %s", tc.err, typ)
		}
		if strings.Contains(string(r.Body), "SECRET-DETAIL") {
			t.Errorf("body leaks the error text")
		}
	}
}

func TestDetailsContract(t *testing.T) {
	tr := staticTrackers{"udp://tracker.one:1337/announce", "http://tracker.two:80/announce"}
	e := newEnv(t, func(o *envOpts) { o.trackers = tr; o.view.MaxTreeFiles = 3 })
	small := e.seed("Small & Co", 1536, 1_700_000_000, []codec.File{{Path: "a/b", Size: 1000}, {Path: "c", Size: 536}})
	big := e.seed("Big", 0, 1, []codec.File{{Path: "a", Size: 1}, {Path: "b", Size: 1}, {Path: "c", Size: 1}, {Path: "d", Size: 1}})
	single := e.seed("Single", 77, 1, nil)
	nopeek := e.seed("NoBlob", 3, 1, nil)
	e.exec(`UPDATE magneticod.torrents SET file_count=3 WHERE id=$1`, nopeek)

	var d map[string]any
	e.api(fmt.Sprintf("/api/v2/torrents/%d", small)).json(t, &d)
	for _, k := range []string{"id", "info_hash", "name", "total_size", "file_count", "discovered_at", "magnet_uri", "full_tree_available"} {
		if _, ok := d[k]; !ok {
			t.Fatalf("missing %s: %v", k, d)
		}
	}
	if len(d) != 8 || d["name"] != "Small & Co" || d["file_count"].(float64) != 2 || d["discovered_at"] != "2023-11-14T22:13:20Z" {
		t.Fatalf("details = %v", d)
	}
	wantMagnet := "magnet:?xt=urn:btih:" + d["info_hash"].(string) + "&dn=Small+%26+Co&tr=udp%3A%2F%2Ftracker.one%3A1337%2Fannounce&tr=http%3A%2F%2Ftracker.two%3A80%2Fannounce"
	if d["magnet_uri"] != wantMagnet {
		t.Fatalf("magnet = %v\nwant %s", d["magnet_uri"], wantMagnet)
	}
	for id, want := range map[int32]bool{small: true, big: false, single: true, nopeek: false} {
		e.api(fmt.Sprintf("/api/v2/torrents/%d", id)).json(t, &d)
		if d["full_tree_available"] != want {
			t.Errorf("torrent %d: full_tree_available = %v, want %v", id, d["full_tree_available"], want)
		}
		// The flag agrees with what the tree endpoint does.
		r := e.api(fmt.Sprintf("/api/v2/torrents/%d/files/tree", id))
		if want && r.Status != 200 && id != nopeek {
			t.Errorf("torrent %d: tree %d although available", id, r.Status)
		}
		if !want && id == big && r.Status != 422 {
			t.Errorf("torrent %d: tree %d although unavailable", id, r.Status)
		}
	}
	if s := e.cache.Stats(); s.Builds > 4 {
		t.Fatalf("details decoded blobs: %+v", s)
	}
	for _, p := range []string{"/api/v2/torrents/0", "/api/v2/torrents/-1", "/api/v2/torrents/007", "/api/v2/torrents/1e3", "/api/v2/torrents/99999999999", "/api/v2/torrents/abc", "/api/v2/torrents/424242", "/api/v2/torrents/%2B1"} {
		for _, suffix := range []string{"", "/files", "/files/tree"} {
			r := e.api(p + suffix)
			if r.Status != 404 {
				t.Errorf("GET %s%s: %d", p, suffix, r.Status)
				continue
			}
			if typ, _ := r.problem(t); typ != "urn:magnetico:problem:not-found" {
				t.Errorf("GET %s%s: %s", p, suffix, typ)
			}
		}
	}
	// Unknown endpoints under the prefix are problem+json after authentication.
	if r := e.api("/api/v2/nothing/here"); r.Status != 404 {
		t.Fatalf("unknown path: %d", r.Status)
	} else if typ, _ := r.problem(t); typ != "urn:magnetico:problem:not-found" {
		t.Fatal(typ)
	}
	if r := e.do("GET", "/api/v2/nothing/here", nil); r.Status != 401 {
		t.Fatalf("anonymous unknown path: %d", r.Status)
	}
}

// The API and the UI endpoints are two authentication wrappers around one
// adapter: their responses are byte-identical for every shape and parameter.
func TestFileViewParityWithTheUIEndpoints(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.view.MaxTreeFiles = 150 })
	odd := []codec.File{
		{Path: "100%/x", Size: 1}, {Path: "a%2Fb/x", Size: 2}, {Path: "../up/x", Size: 4}, {Path: "/lead/x", Size: 6},
		{Path: "mid//dle/x", Size: 7}, {Path: "bad\xff\xfe/x", Size: 8}, {Path: "ctl\x01‮/x", Size: 9},
		{Path: "dup/same", Size: 10}, {Path: "dup/same", Size: 10}, {Path: "dup/same", Size: 11},
		{Path: "тест/файл", Size: 12}, {Path: "end/", Size: 13}, {Path: "q\"uo\\te</>& ", Size: 14},
		{Path: "nul\x00byte/y", Size: 15}, {Path: "real�char/z", Size: 16},
	}
	var hundred, large []codec.File
	for i := 0; i < 100; i++ {
		hundred = append(hundred, codec.File{Path: fmt.Sprintf("d%d/f%03d", i%4, i), Size: int64(i)})
	}
	for i := 0; i < 400; i++ {
		large = append(large, codec.File{Path: fmt.Sprintf("d%d/f%03d", i%7, i), Size: 1})
	}
	deep := []codec.File{{Path: strings.Repeat("d/", 300) + "f", Size: 1}, {Path: "other", Size: 2}}
	shapes := map[string]int32{
		"odd": e.seed("odd", 0, 1, odd), "hundred": e.seed("hundred", 0, 1, hundred), "large": e.seed("large", 0, 1, large),
		"deep": e.seed("deep", 0, 1, deep), "single": e.seed("single", 9, 1, nil),
	}
	cookie := e.sessionCookie()
	queries := []string{"", "?dir=0", "?dir=0_0", "?dir=1&offset=1&limit=2", "?limit=1", "?offset=9999", "?limit=1000", "?limit=0", "?limit=1001",
		"?dir=01", "?dir=a", "?dir=99", "?offset=-1", "?dir=0&dir=1", "?limit=1&limit=2", "?offset=1.5"}
	compared := 0
	for name, id := range shapes {
		for _, suffix := range []string{"/files/tree", "/files"} {
			for _, q := range queries {
				if suffix == "/files/tree" && q != "" {
					continue
				}
				api := e.api(fmt.Sprintf("/api/v2/torrents/%d%s%s", id, suffix, q))
				ui := e.do("GET", fmt.Sprintf("/torrents/%d%s%s", id, suffix, q), nil, cookie)
				if api.Status != ui.Status {
					t.Errorf("%s %s%s: API %d, UI %d", name, suffix, q, api.Status, ui.Status)
					continue
				}
				a, u := string(api.Body), string(ui.Body)
				// The only intended difference: the alternate URL of the 422 problem.
				u = strings.ReplaceAll(u, fmt.Sprintf("/torrents/%d/files", id), fmt.Sprintf("/api/v2/torrents/%d/files", id))
				if a != u {
					t.Errorf("%s %s%s: bodies differ\nAPI: %.200s\nUI:  %.200s", name, suffix, q, a, u)
				}
				if api.Header.Get("Content-Type") != ui.Header.Get("Content-Type") || api.Header.Get("Retry-After") != ui.Header.Get("Retry-After") {
					t.Errorf("%s %s%s: headers differ", name, suffix, q)
				}
				compared++
			}
		}
	}
	if compared < 60 {
		t.Fatalf("only %d comparisons", compared)
	}
}

func TestSmallTorrentUsableThroughEitherEndpointAndLargeOneRefusesTheTree(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.view.MaxTreeFiles = 50 })
	var hundred []codec.File
	for i := 0; i < 100; i++ {
		hundred = append(hundred, codec.File{Path: fmt.Sprintf("d%d/f%03d", i%4, i), Size: 1})
	}
	small := e.seed("small", 0, 1, []codec.File{{Path: "a/b", Size: 1}, {Path: "a/c", Size: 2}, {Path: "z", Size: 3}})
	large := e.seed("large", 0, 1, hundred)

	// Small: both operations work and agree on the totals.
	var tree struct {
		Version   int   `json:"version"`
		FileCount int   `json:"file_count"`
		Size      int64 `json:"size"`
	}
	e.api(fmt.Sprintf("/api/v2/torrents/%d/files/tree", small)).json(t, &tree)
	var pg struct {
		Dir struct {
			FileCount int   `json:"file_count"`
			Size      int64 `json:"size"`
		} `json:"dir"`
	}
	e.api(fmt.Sprintf("/api/v2/torrents/%d/files", small)).json(t, &pg)
	if tree.Version != 1 || tree.FileCount != 3 || tree.Size != 6 || pg.Dir.FileCount != 3 || pg.Dir.Size != 6 {
		t.Fatalf("tree %+v page %+v", tree, pg)
	}

	// Large: the tree is refused before any body byte, with the documented problem.
	r := e.api(fmt.Sprintf("/api/v2/torrents/%d/files/tree", large))
	typ, p := r.problem(t)
	if r.Status != 422 || typ != "urn:magnetico:problem:full-tree-unavailable" || p["alternate"] != fmt.Sprintf("/api/v2/torrents/%d/files", large) {
		t.Fatalf("%d %v", r.Status, p)
	}
	if _, ok := p["detail"].(string); !ok {
		t.Fatal("no detail")
	}
	// A 100-file torrent paginates regardless of the tree limit and every file is reachable.
	seen := map[int]bool{}
	var walk func(dir string)
	type entry struct {
		Type      string `json:"type"`
		Path      string `json:"path"`
		FileIndex int    `json:"file_index"`
	}
	walk = func(dir string) {
		offset := 0
		for {
			var page struct {
				Entries    []entry `json:"entries"`
				NextOffset *int    `json:"next_offset"`
			}
			e.api(fmt.Sprintf("/api/v2/torrents/%d/files?dir=%s&offset=%d&limit=7", large, dir, offset)).json(t, &page)
			for _, en := range page.Entries {
				if en.Type == "file" {
					seen[en.FileIndex] = true
				} else {
					walk(en.Path)
				}
			}
			if page.NextOffset == nil {
				return
			}
			offset = *page.NextOffset
		}
	}
	walk("")
	if len(seen) != 100 {
		t.Fatalf("pagination reached %d of 100 files", len(seen))
	}
	// The large torrent says so in its details.
	var d map[string]any
	e.api(fmt.Sprintf("/api/v2/torrents/%d", large)).json(t, &d)
	if d["full_tree_available"] != false {
		t.Fatal("full_tree_available should be false")
	}
	// Direct requests cannot bypass the limit.
	if r := e.api(fmt.Sprintf("/api/v2/torrents/%d/files/tree", large)); r.Status != 422 {
		t.Fatal("limit bypassed")
	}
}

func TestExactSizeCapAndIndexComplexityOverTheAPI(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.view.MaxTreeBytes = 8 << 10; o.view.MaxPageBytes = 4 << 10 })
	var files []codec.File
	for i := 0; i < 400; i++ {
		files = append(files, codec.File{Path: fmt.Sprintf("dir/file-number-%04d.bin", i), Size: int64(i)})
	}
	id := e.seed("sizecap", 0, 1, files)
	r := e.api(fmt.Sprintf("/api/v2/torrents/%d/files/tree", id))
	if typ, _ := r.problem(t); r.Status != 422 || typ != "urn:magnetico:problem:full-tree-unavailable" {
		t.Fatalf("exact cap: %d", r.Status)
	}
	// Pages are cut at the byte cap and continue with next_offset.
	var pg struct {
		Entries    []json.RawMessage `json:"entries"`
		NextOffset *int              `json:"next_offset"`
		Limit      int               `json:"limit"`
	}
	r = e.api(fmt.Sprintf("/api/v2/torrents/%d/files?dir=0&limit=1000", id))
	r.json(t, &pg)
	if len(r.Body) > 4<<10+512 || pg.NextOffset == nil || *pg.NextOffset != len(pg.Entries) {
		t.Fatalf("page bytes %d entries %d next %v", len(r.Body), len(pg.Entries), pg.NextOffset)
	}

	e2 := newEnv(t)
	deep := e2.seed("deep", 0, 1, []codec.File{{Path: strings.Repeat("d/", 300) + "f", Size: 1}, {Path: "other", Size: 2}})
	for _, p := range []string{"/files/tree", "/files", "/files?dir=0_0"} {
		r := e2.api(fmt.Sprintf("/api/v2/torrents/%d%s", deep, p))
		if typ, _ := r.problem(t); r.Status != 422 || typ != "urn:magnetico:problem:index-too-complex" {
			t.Errorf("%s: %d %s", p, r.Status, r.Body)
		}
	}
	if r := e2.api(fmt.Sprintf("/api/v2/torrents/%d", deep)); r.Status != 200 {
		t.Fatalf("details of a too-complex torrent: %d", r.Status)
	}
}

func TestNamesAreByteExactOverTheAPI(t *testing.T) {
	e := newEnv(t)
	id := e.seed("odd", 0, 1, []codec.File{{Path: "bad\xff\xfe/x", Size: 1}, {Path: "ok/y", Size: 2}, {Path: "dup", Size: 3}, {Path: "dup", Size: 3}})
	r := e.api(fmt.Sprintf("/api/v2/torrents/%d/files/tree", id))
	body := string(r.Body)
	if !strings.Contains(body, `"name":null,"name_base64":"YmFk//4="`) || strings.Contains(body, "�") || !json.Valid(r.Body) {
		t.Fatalf("invalid-UTF-8 name: %s", body)
	}
	if strings.Count(body, `"name":"dup"`) != 2 || !strings.Contains(body, `"duplicate":true`) {
		t.Fatalf("duplicates: %s", body)
	}
}

func TestLogsCarryNoTokensNamesOrQueries(t *testing.T) {
	e := newEnv(t)
	id := e.seed("TOPSECRET-NAME", 0, 1, []codec.File{{Path: "SECRET-PATH/file", Size: 1}})
	e.api("/api/v2/torrents?q=SECRETQUERY")
	e.api(fmt.Sprintf("/api/v2/torrents/%d/files?dir=0", id))
	e.do("GET", "/api/v2/torrents?access_token="+e.token, nil)
	e.do("GET", "/api/v2/torrents", map[string]string{"Authorization": "Bearer mgt_" + strings.Repeat("Q", 43)})
	logs := e.log.String()
	for _, secret := range []string{e.token, strings.TrimPrefix(e.token, "mgt_"), "TOPSECRET", "SECRET-PATH", "SECRETQUERY", strings.Repeat("Q", 43)} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
	if !strings.Contains(logs, "user_id=") {
		t.Error("authenticated API requests should log the user id")
	}
}

func TestEveryAPIRouteIsBearerOnlyAndSafeMethodsOnly(t *testing.T) {
	e := newEnv(t)
	n := 0
	for _, r := range e.srv.Router().Routes() {
		if !strings.Contains(r.Pattern, "/api/") {
			continue
		}
		n++
		if r.Access.String() != "api-token" {
			t.Errorf("%s has access %s", r.Pattern, r.Access)
		}
		if m, _, _ := strings.Cut(r.Pattern, " "); m != "GET" {
			t.Errorf("%s is not GET", r.Pattern)
		}
	}
	if n != 5 {
		t.Fatalf("%d API routes", n)
	}
	// Writes are not served, with or without a token.
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if r := e.do(m, "/api/v2/torrents", map[string]string{"Authorization": "Bearer " + e.token}); r.Status == 200 || r.Status == 307 {
			t.Errorf("%s accepted: %d", m, r.Status)
		}
	}
	_ = strconv.Itoa
	_ = accounts.APITokenPrefix
}

// Measurement (synthetic, one local PostgreSQL): the cost of an authenticated
// API request, which includes the token lookup, and of the details endpoint.
func TestMeasureAPILatency(t *testing.T) {
	e := newEnv(t)
	id := e.seed("latency", 0, 1, []codec.File{{Path: "a/b", Size: 1}, {Path: "c", Size: 2}})
	e.exec(`INSERT INTO magneticod.torrents (info_hash, name, total_size, discovered_on, file_count)
	    SELECT decode(md5(i::text) || substr(md5((i+1)::text),1,8),'hex'), 'filler ' || i, i, 1700000000 + i, 1 FROM generate_series(1, 5000) i`)
	for name, path := range map[string]string{
		"details":        fmt.Sprintf("/api/v2/torrents/%d", id),
		"list (50)":      "/api/v2/torrents?limit=50",
		"search (q)":     "/api/v2/torrents?q=filler&limit=50",
		"dir page":       fmt.Sprintf("/api/v2/torrents/%d/files", id),
		"401 (no token)": "",
	} {
		var ds []time.Duration
		for i := 0; i < 200; i++ {
			s := time.Now()
			if path == "" {
				e.do("GET", "/api/v2/torrents", nil)
			} else {
				e.api(path)
			}
			ds = append(ds, time.Since(s))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		t.Logf("%-14s p50 %v  p95 %v  max %v", name, ds[100], ds[190], ds[199])
	}
}
