package cli

// Regression tests for the load client itself (not gated: they need no
// database and no binary). The load numbers are only as honest as the client's
// classification, so these pin it: a response that cannot be read to the end is
// a transport failure, a 422 counts as expected only when it is the documented
// full-tree answer, and the search workload really follows keyset continuation
// links.

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type errAfter struct {
	data string
	err  error
	done bool
}

func (r *errAfter) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), nil
}

func TestReadBodyReportsReadErrors(t *testing.T) {
	boom := errors.New("connection reset")
	if _, err := readBody(&errAfter{data: "partial", err: boom}); !errors.Is(err, boom) {
		t.Fatalf("readBody swallowed the read error: %v", err)
	}
	b, err := readBody(strings.NewReader(strings.Repeat("x", harnessBodyKeep+10)))
	if err != nil || len(b) != harnessBodyKeep {
		t.Fatalf("an oversized body keeps %d bytes and no error, got %d / %v", harnessBodyKeep, len(b), err)
	}
}

func harnessTestLoad(addr string, timeout time.Duration) (*harnessLoad, *harnessIdent) {
	hc := &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &harnessLoad{addr: addr, hc: hc}, &harnessIdent{xff: "10.0.0.1"}
}

func TestTruncatedAndStalledBodiesAreTransportFailures(t *testing.T) {
	// Truncated: the promised length is never delivered.
	truncated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 1000\r\n\r\nshort")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	defer truncated.Close()
	l, id := harnessTestLoad(strings.TrimPrefix(truncated.URL, "http://"), 5*time.Second)
	out, _, _ := l.fetch(harnessReq{class: "details", path: "/x", expect: []int{200}}, id)
	if out != oTransport {
		t.Fatalf("a truncated 200 response was classified %q, want %q", out, oTransport)
	}

	// Stalled: status line and headers arrive, the body stops past the client timeout.
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("short"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer stalled.Close()
	defer close(release)
	l, id = harnessTestLoad(strings.TrimPrefix(stalled.URL, "http://"), 300*time.Millisecond)
	out, _, _ = l.fetch(harnessReq{class: "details", path: "/x", expect: []int{200}}, id)
	if out != oTransport {
		t.Fatalf("a body that timed out mid-transfer was classified %q, want %q", out, oTransport)
	}

	// A complete body is still ok.
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "fine") }))
	defer okSrv.Close()
	l, id = harnessTestLoad(strings.TrimPrefix(okSrv.URL, "http://"), 5*time.Second)
	if out, body, _ := l.fetch(harnessReq{class: "details", path: "/x", expect: []int{200}}, id); out != oOK || string(body) != "fine" {
		t.Fatalf("complete response: %q %q", out, body)
	}
}

func TestConnectionRefusedIsATransportFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	l, id := harnessTestLoad(addr, time.Second)
	if out, _, _ := l.fetch(harnessReq{class: "details", path: "/x", expect: []int{200}}, id); out != oTransport {
		t.Fatalf("refused connection classified %q", out)
	}
}

func TestClassifyTreeUnprocessable(t *testing.T) {
	expect := []int{200, 422}
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"complete tree", 200, "{}", oOK},
		{"documented full-tree problem", 422, `{"type":"https://example.test/problems/full-tree-unavailable"}`, oOK},
		{"file index failure", 422, `{"type":"https://example.test/problems/index-too-complex"}`, oUnexpect},
		{"empty 422", 422, ``, oUnexpect},
		{"busy", 503, `{"title":"The server is busy"}`, oBusy},
		{"timeout", 503, `{"title":"The request timed out"}`, oTimeout},
		{"limiter", 429, ``, o429},
		{"other", 404, ``, oUnexpect},
	} {
		if got := classify(c.status, []byte(c.body), expect); got != c.want {
			t.Errorf("%s: classify(%d) = %q, want %q", c.name, c.status, got, c.want)
		}
	}
	// A 422 is never acceptable for a request that did not expect it.
	if got := classify(422, []byte("full-tree-unavailable"), []int{200}); got != oUnexpect {
		t.Errorf("unexpected 422 classified %q", got)
	}
}

func TestSearchContinuationFollowsTheNextLink(t *testing.T) {
	page := []byte(`<a class="btn small" href="/torrents?q=1080p&amp;sort=discovered&amp;cursor=AbC_-9&amp;size=20" rel="next">Next</a>`)
	next, ok := nextPage(page, map[string]string{"Cookie": "s=1"})
	if !ok || next.path != "/torrents?q=1080p&sort=discovered&cursor=AbC_-9&size=20" || next.class != "searchpage" || next.hdr["Cookie"] != "s=1" {
		t.Fatalf("continuation = %+v ok=%v", next, ok)
	}
	for _, bad := range []string{
		`<span class="btn small" aria-disabled="true">Next</span>`,             // last page
		`<a href="https://evil.example/torrents?cursor=x" rel="next">Next</a>`, // other host
		`<a href="/account?cursor=x" rel="next">Next</a>`,                      // other route
		`<a href="/torrents?q=x" rel="next">Next</a>`,                          // no cursor
	} {
		if _, ok := nextPage([]byte(bad), nil); ok {
			t.Errorf("followed %q", bad)
		}
	}

	// Search page, then its continuation, as the worker loop does: the server sees
	// the cursor requests.
	var seen, cont atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		if r.URL.Query().Get("cursor") != "" {
			cont.Add(1)
		}
		_, _ = io.WriteString(w, `<a href="/torrents?cursor=zz&amp;q=a" rel="next">Next</a>`)
	}))
	defer srv.Close()
	l, id := harnessTestLoad(strings.TrimPrefix(srv.URL, "http://"), 5*time.Second)
	for i := 0; i < 20; i++ {
		out, body, _ := l.fetch(harnessReq{class: "search", path: "/torrents?q=a", expect: []int{200}}, id)
		if out != oOK {
			t.Fatal(out)
		}
		if n, ok := nextPage(body, nil); !ok {
			t.Fatal("no continuation")
		} else if out, _, _ := l.fetch(n, id); out != oOK {
			t.Fatal(out)
		}
	}
	if cont.Load() != 20 || seen.Load() != 40 {
		t.Fatalf("server saw %d requests, %d of them continuations", seen.Load(), cont.Load())
	}
}

// ---- the ingest drain ----------------------------------------------------------------

// harnessClientTimeout bounds one load-client request.
const harnessClientTimeout = 90 * time.Second

// outcome classes, reported separately and never merged.
const (
	oOK        = "ok"
	o429       = "limited429"
	oBusy      = "busy503"
	oTimeout   = "timeout503"
	oOther5xx  = "other5xx"
	oUnexpect  = "unexpected4xx"
	oTransport = "transport"
)

// ---- processes in their own scopes --------------------------------------------------

type harnessIdent struct {
	user    string
	xff     string
	session string // "name=value"
	api     []string
	rss     []string
}

type harnessPools struct {
	small, medium, large, huge []int
	idents                     []*harnessIdent
	apiSeq, rssSeq             atomic.Int64
}

func (p *harnessPools) nextAPI() string {
	n := p.apiSeq.Add(1)
	id := p.idents[int(n)%len(p.idents)]
	return id.api[int(n/int64(len(p.idents)))%len(id.api)]
}

func (p *harnessPools) nextRSS() string {
	n := p.rssSeq.Add(1)
	id := p.idents[int(n)%len(p.idents)]
	return id.rss[int(n/int64(len(p.idents)))%len(id.rss)]
}

type harnessReq struct {
	class  string
	path   string
	hdr    map[string]string
	expect []int
}

func harnessPickClass(rng *rand.Rand) string {
	n := rng.IntN(100)
	for _, m := range harnessMixes {
		if n < m.weight {
			return m.class
		}
		n -= m.weight
	}
	return "search"
}

func harnessTerm(rng *rand.Rand) string {
	switch n := rng.IntN(100); {
	case n < 50:
		return harnessCommon[rng.IntN(len(harnessCommon))]
	case n < 60:
		return harnessCommon[rng.IntN(len(harnessCommon))] + " " + harnessCommon[rng.IntN(len(harnessCommon))]
	case n < 80:
		return "m" + strconv.Itoa(int(math.Pow(rng.Float64(), 2)*400))
	case n < 92:
		return "w" + strconv.Itoa(rng.IntN(200000))
	case n < 94:
		return "zzqxj"
	case n < 97:
		return []string{"日本語", "Кириллица"}[rng.IntN(2)]
	}
	return ""
}

func (p *harnessPools) pick(rng *rand.Rand, id *harnessIdent) harnessReq {
	ses := map[string]string{"Cookie": id.session}
	multi := func() int {
		switch n := rng.IntN(100); {
		case n < 40:
			return p.small[rng.IntN(len(p.small))]
		case n < 75:
			return p.medium[rng.IntN(len(p.medium))]
		case n < 95:
			return p.large[rng.IntN(len(p.large))]
		}
		return p.huge[rng.IntN(len(p.huge))]
	}
	cls := harnessPickClass(rng)
	switch cls {
	case "search":
		q := url.Values{}
		if t := harnessTerm(rng); t != "" {
			q.Set("q", t)
		}
		q.Set("sort", []string{"discovered", "discovered", "discovered", "size", "name"}[rng.IntN(5)])
		q.Set("order", []string{"asc", "desc"}[rng.IntN(2)])
		q.Set("size", []string{"20", "50", "100"}[rng.IntN(3)])
		return harnessReq{cls, "/torrents?" + q.Encode(), ses, []int{200}}
	case "details":
		return harnessReq{cls, "/torrents/" + strconv.Itoa(multi()), ses, []int{200}}
	case "dirs":
		return harnessReq{cls, fmt.Sprintf("/torrents/%d/files?dir=&limit=160", multi()), ses, []int{200}}
	case "tree":
		// Torrents the backend serves as a complete tree (up to 4,000 files); the
		// rest answer 422 and the client falls back to pages, both are expected.
		return harnessReq{cls, fmt.Sprintf("/torrents/%d/files/tree", multi()), ses, []int{200, 422}}
	case "api":
		q := url.Values{"limit": {[]string{"20", "50", "100"}[rng.IntN(3)]}}
		if t := harnessTerm(rng); t != "" {
			q.Set("q", t)
		}
		return harnessReq{cls, "/api/v2/torrents?" + q.Encode(), map[string]string{"Authorization": "Bearer " + p.nextAPI()}, []int{200}}
	case "apifiles":
		return harnessReq{cls, fmt.Sprintf("/api/v2/torrents/%d/files?limit=200", multi()), map[string]string{"Authorization": "Bearer " + p.nextAPI()}, []int{200}}
	case "rss":
		if rng.IntN(2) == 0 {
			return harnessReq{cls, "/api/v2/feeds/latest.rss?feed_token=" + p.nextRSS() + "&page=" + strconv.Itoa(1+rng.IntN(3)), nil, []int{200}}
		}
		return harnessReq{cls, "/api/v1/feed/rss/last?api-token=" + p.nextRSS(), nil, []int{200}}
	}
	return harnessReq{"magnet", "/magnet/" + hex.EncodeToString(sha1Sum(strconv.Itoa(rng.IntN(1000000)))), nil, []int{307}}
}

func sha1Sum(s string) []byte { h := sha1.Sum([]byte(s)); return h[:] }

// ---- accumulators -------------------------------------------------------------------

type harnessAcc struct {
	mu   sync.Mutex
	n    map[string]int            // outcome -> count
	durs map[string][]float32      // class -> ms (ok only)
	cls  map[string]map[string]int // class -> outcome -> count
}

func newHarnessAcc() *harnessAcc {
	return &harnessAcc{n: map[string]int{}, durs: map[string][]float32{}, cls: map[string]map[string]int{}}
}

func (a *harnessAcc) add(class, outcome string, d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n[outcome]++
	if a.cls[class] == nil {
		a.cls[class] = map[string]int{}
	}
	a.cls[class][outcome]++
	if outcome == oOK {
		a.durs[class] = append(a.durs[class], float32(d.Seconds()*1000))
	}
}

func pct(v []float32, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float32(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return float64(c[min(len(c)-1, int(math.Ceil(p/100*float64(len(c))))-1)])
}

type harnessClassStat struct {
	N        int            `json:"n"`
	Outcomes map[string]int `json:"outcomes"`
	P50      float64        `json:"p50_ms"`
	P95      float64        `json:"p95_ms"`
	P99      float64        `json:"p99_ms"`
	Max      float64        `json:"max_ms"`
}

func (a *harnessAcc) stats() (total map[string]int, classes map[string]harnessClassStat) {
	a.mu.Lock()
	defer a.mu.Unlock()
	total = map[string]int{}
	for k, v := range a.n {
		total[k] = v
	}
	classes = map[string]harnessClassStat{}
	for c, o := range a.cls {
		st := harnessClassStat{Outcomes: map[string]int{}}
		for k, v := range o {
			st.Outcomes[k] = v
			st.N += v
		}
		d := a.durs[c]
		st.P50, st.P95, st.P99 = pct(d, 50), pct(d, 95), pct(d, 99)
		for _, x := range d {
			st.Max = max(st.Max, float64(x))
		}
		classes[c] = st
	}
	return
}

// ---- load generation ----------------------------------------------------------------

type harnessLoad struct {
	// noContinuation switches the paging follow-ups off (MAGNETICO_BENCH_NO_CONTINUATION=1):
	// the workload of the earlier campaign, for comparisons across revisions.
	noContinuation bool
	addr           string
	pools          *harnessPools
	hc             *http.Client
}

func newHarnessLoad(addr string, pools *harnessPools) *harnessLoad {
	tr := &http.Transport{MaxIdleConnsPerHost: 256, MaxConnsPerHost: 0, IdleConnTimeout: 90 * time.Second}
	return &harnessLoad{noContinuation: os.Getenv("MAGNETICO_BENCH_NO_CONTINUATION") == "1", addr: addr, pools: pools, hc: &http.Client{Transport: tr, Timeout: harnessClientTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// harnessFullTreeSlug identifies the one 422 the workload expects: the documented
// "complete tree unavailable, browse by directory" answer. Any other 422
// (notably index-too-complex) is a file-index failure and must stay visible.
const harnessFullTreeSlug = "full-tree-unavailable"

// harnessBodyKeep is how much of a response body the client keeps for classification
// and link extraction; the rest is still read, to detect truncation.
const harnessBodyKeep = 4 << 20

// readBody reads the whole body. A read error (truncation, reset, a timeout
// while the body is transferred) is returned, never swallowed: the caller counts
// it as a transport failure.
func readBody(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.LimitReader(r, harnessBodyKeep)); err != nil {
		return nil, err
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// fetch performs one request and returns the outcome class and the body.
func (l *harnessLoad) fetch(rq harnessReq, id *harnessIdent) (string, []byte, time.Duration) {
	req, err := http.NewRequest("GET", "http://"+l.addr+rq.path, nil)
	if err != nil {
		return oTransport, nil, 0
	}
	req.Header.Set("X-Forwarded-For", id.xff)
	for k, v := range rq.hdr {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := l.hc.Do(req)
	if err != nil {
		return oTransport, nil, time.Since(start)
	}
	defer resp.Body.Close()
	body, err := readBody(resp.Body)
	if err != nil {
		return oTransport, nil, time.Since(start)
	}
	return classify(resp.StatusCode, body, rq.expect), body, time.Since(start)
}

// nextPageRe finds the "Next" link of a search results page.
var nextPageRe = regexp.MustCompile(`href="([^"]+)" rel="next"`)

// nextPage returns the continuation request of a search page, if it has one.
func nextPage(body []byte, ses map[string]string) (harnessReq, bool) {
	m := nextPageRe.FindSubmatch(body)
	if m == nil {
		return harnessReq{}, false
	}
	href := html.UnescapeString(string(m[1]))
	if !strings.HasPrefix(href, "/torrents?") || !strings.Contains(href, "cursor=") {
		return harnessReq{}, false
	}
	return harnessReq{class: "searchpage", path: href, hdr: ses, expect: []int{200}}, true
}

func classify(status int, body []byte, expect []int) string {
	for _, e := range expect {
		if status != e {
			continue
		}
		if status == http.StatusUnprocessableEntity && !bytes.Contains(body, []byte(harnessFullTreeSlug)) {
			return oUnexpect
		}
		return oOK
	}
	switch {
	case status == 429:
		return o429
	case status == 503:
		b := string(body)
		switch {
		case strings.Contains(b, "timed out"):
			return oTimeout
		case strings.Contains(b, "busy"):
			return oBusy
		}
		return oOther5xx
	case status >= 500:
		return oOther5xx
	}
	return oUnexpect
}

// run drives conc workers for d and returns what they saw. A worker stops
// starting requests at the deadline and finishes the one in flight.
func (l *harnessLoad) run(conc int, d time.Duration, think time.Duration, acc *harnessAcc) {
	stop := time.Now().Add(d)
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(i)+1, uint64(time.Now().UnixNano())))
			id := l.pools.idents[i%len(l.pools.idents)]
			for time.Now().Before(stop) {
				rq := l.pools.pick(rng, id)
				outcome, body, d := l.fetch(rq, id)
				acc.add(rq.class, outcome, d)
				// A third of the searches continue like a user paging on: up to two
				// keyset continuation requests from the page just received.
				if rq.class == "search" && outcome == oOK && !l.noContinuation && rng.IntN(3) == 0 {
					for depth := 0; depth < 2; depth++ {
						next, ok := nextPage(body, rq.hdr)
						if !ok {
							break
						}
						outcome, body, d = l.fetch(next, id)
						acc.add(next.class, outcome, d)
						if outcome != oOK {
							break
						}
					}
				}
				if think > 0 {
					// An interactive user: an exponentially distributed pause (mean = think).
					time.Sleep(time.Duration(rng.ExpFloat64() * float64(think)))
				}
			}
		}(i)
	}
	wg.Wait()
}

// ---- ingest producer ----------------------------------------------------------------

var (
	harnessCommon = []string{"1080p", "720p", "x264", "x265", "bluray", "webrip", "complete", "season", "ubuntu", "linux", "multi", "remux"}
	harnessMixes  = []struct {
		class  string
		weight int
	}{{"search", 45}, {"details", 20}, {"dirs", 12}, {"tree", 5}, {"api", 10}, {"rss", 3}, {"magnet", 3}, {"apifiles", 2}}
)
