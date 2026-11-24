package cli

// Security regression: malformed, oversized and unsupported input. Whatever
// a client sends, the service answers with a controlled 4xx (never a 5xx, never
// a panic), changes no state, and keeps serving afterwards.

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/opflags"
)

// rawTCP sends a request exactly as written (including bytes Go's HTTP client
// refuses to produce) and returns the status and body.
func rawTCP(t *testing.T, addr, request string) (int, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	_, _ = conn.Write([]byte(request))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, err.Error() // the server closed the connection: also a refusal
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil || b.Len() > 1<<16 {
			break
		}
	}
	return resp.StatusCode, b.String()
}

func TestMalformedAndOversizedRequests(t *testing.T) {
	s := newHarnessSec(t, trustLoopbackProxy)
	s.uniqueClients = true
	alice := s.login("alice", secUserPW)
	api := alice.mint("api")
	id := seedBrowse(t, s.dsn, "input torrent", []codec.File{{Path: "a/b/c.txt", Size: 3}, {Path: "a/d.txt", Size: 4}, {Path: "e.txt", Size: 5}})
	for i := 0; i < 5; i++ {
		seedBrowse(t, s.dsn, fmt.Sprintf("input filler %d", i), []codec.File{{Path: "f", Size: int64(i + 1)}})
	}
	alive := func(when string) {
		t.Helper()
		if r := s.raw("GET", "/healthz", nil, ""); r.status != 200 {
			t.Fatalf("the service does not answer after %s: %d", when, r.status)
		}
	}
	big := func(n int) string { return strings.Repeat("A", n) }
	hostLine := "Host: " + s.web.addr + "\r\n"

	type tc struct {
		name   string
		method string
		path   string
		hdr    map[string]string
		body   string
		want   []int // acceptable statuses; never 5xx
	}
	bearer := bearerHdr(api)
	cases := []tc{
		// --- bodies ---
		{"login, 2 MiB body", "POST", "/login", map[string]string{"X-Test": "1"}, "username=alice&password=" + big(2<<20), []int{400, 403, 413}},
		{"login, malformed escapes", "POST", "/login", nil, "username=%zz&password=%&&===&%", []int{400, 403}},
		{"login, NUL in the username", "POST", "/login", nil, "username=ali%00ce&password=x", []int{400, 403}},
		{"login, invalid UTF-8", "POST", "/login", nil, "username=%ff%fe&password=%c3%28", []int{400, 403}},
		{"login, JSON body", "POST", "/login", map[string]string{"Content-Type": "application/json"}, `{"username":"alice","password":"x"}`, []int{400, 403, 415}},
		{"login, multipart without boundary", "POST", "/login", map[string]string{"Content-Type": "multipart/form-data"}, "x", []int{400, 403, 415}},
		// --- search parameters ---
		{"search, malformed escape", "GET", "/torrents?q=%zz", nil, "", []int{200, 303, 400}},
		{"search, invalid UTF-8", "GET", "/torrents?q=%ff%fe", nil, "", []int{200, 303, 400}},
		{"search, NUL", "GET", "/torrents?q=a%00b", nil, "", []int{200, 303, 400}},
		{"search, 4 KiB query", "GET", "/torrents?q=" + big(4096), nil, "", []int{200, 303, 400, 414}},
		{"search, bogus sort and order", "GET", "/torrents?q=input&sort=bogus&order=sideways", nil, "", []int{200, 303}},
		{"search, bogus limit", "GET", "/torrents?q=input&limit=-1", nil, "", []int{200, 303, 400}},
		{"search, junk cursor", "GET", "/torrents?q=input&cursor=%00%00junk", nil, "", []int{200, 303, 400}},
		{"search, 20 KiB cursor", "GET", "/torrents?q=input&cursor=" + big(20000), nil, "", []int{200, 303, 400, 414, 431}},
		// --- API parameters ---
		{"api list, invalid UTF-8", "GET", "/api/v2/torrents?q=%ff", bearer, "", []int{400}},
		{"api list, NUL", "GET", "/api/v2/torrents?q=a%00b", bearer, "", []int{400}},
		{"api list, limit 0", "GET", "/api/v2/torrents?limit=0", bearer, "", []int{400}},
		{"api list, limit 101", "GET", "/api/v2/torrents?limit=101", bearer, "", []int{400}},
		{"api list, limit 1.5", "GET", "/api/v2/torrents?limit=1.5", bearer, "", []int{400}},
		{"api list, limit abc", "GET", "/api/v2/torrents?limit=abc", bearer, "", []int{400}},
		{"api list, repeated limit", "GET", "/api/v2/torrents?limit=1&limit=2", bearer, "", []int{400}},
		{"api list, junk cursor", "GET", "/api/v2/torrents?cursor=!!!", bearer, "", []int{400}},
		{"api list, 20 KiB cursor", "GET", "/api/v2/torrents?cursor=" + big(20000), bearer, "", []int{400, 414, 431}},
		{"api list, cursor of another search", "GET", "/api/v2/torrents?q=zzz&cursor=" + url.QueryEscape("AQEAAAAAAAAAAAAAAAAAAAAAAAAAAAA"), bearer, "", []int{400}},
		{"api list, hash with a word", "GET", "/api/v2/torrents?info_hash=" + strings.Repeat("a", 40) + "&q=x", bearer, "", []int{400}},
		{"api list, short hash", "GET", "/api/v2/torrents?info_hash=" + strings.Repeat("a", 39), bearer, "", []int{400}},
		{"api details, huge id", "GET", "/api/v2/torrents/99999999999999999999", bearer, "", []int{404}},
		{"api details, negative id", "GET", "/api/v2/torrents/-1", bearer, "", []int{404}},
		{"api details, encoded id", "GET", "/api/v2/torrents/%31", bearer, "", []int{200, 404}},
		{"api details, NUL id", "GET", "/api/v2/torrents/1%00", bearer, "", []int{404}},
	}
	// --- directory, offset and limit parameters, on both file endpoints ---
	dirs := []string{"", "0_", "_0", "00", "0__1", "1e3", "-1", "+1", "٠", "１", "%00", "0%20", "0%5f0", "9999999", "1_" + strings.Repeat("1_", 300) + "1", "0_0_0_0_0_0_0_0_0_0", "../", "%2e%2e", "x"}
	for _, d := range dirs {
		for _, ep := range []struct{ name, prefix string }{{"api", "/api/v2"}, {"ui", ""}} {
			hdr := map[string]string(nil)
			if ep.name == "api" {
				hdr = bearer
			} else {
				hdr = map[string]string{"Cookie": sessionCookieOf(alice, s.web.addr)}
			}
			cases = append(cases, tc{fmt.Sprintf("%s dir=%q", ep.name, d), "GET", fmt.Sprintf("%s/torrents/%d/files?dir=%s", ep.prefix, id, d), hdr, "", []int{200, 400, 404}})
		}
	}
	for _, q := range []string{"offset=-1", "offset=abc", "offset=1.5", "offset=99999999999999999999", "offset=1&offset=2", "limit=0", "limit=1001", "limit=abc", "limit=-5", "limit=1&limit=2", "dir=&dir=", "offset=%00"} {
		cases = append(cases, tc{"api files, " + q, "GET", fmt.Sprintf("/api/v2/torrents/%d/files?%s", id, q), bearer, "", []int{400}})
	}
	// --- credentials ---
	for _, h := range []map[string]string{
		{"Authorization": "Bearer"}, {"Authorization": "Bearer "}, {"Authorization": "Bearer " + big(5000)},
		{"Authorization": "Bearer mgt_!!!!"}, {"Authorization": "Bearer\ttoken"}, {"Authorization": "Negotiate abc"}, {"Authorization": big(9000)},
		{"api-token": big(5000)}, {"Cookie": "api-token=" + big(5000)}, {"Cookie": "magnetico_session=" + big(5000)}, {"Cookie": "magnetico_session=%00%00; api-token=%"},
	} {
		cases = append(cases, tc{fmt.Sprintf("credential %.40v", h), "GET", "/api/v2/torrents", h, "", []int{400, 401, 431}})
		cases = append(cases, tc{fmt.Sprintf("feed credential %.40v", h), "GET", "/api/v1/feed/rss/last", h, "", []int{400, 401, 431}})
	}
	for _, p := range []string{"/api/v2/feeds/latest.rss?feed_token=", "/api/v2/feeds/latest.rss?feed_token=" + big(5000), "/api/v2/feeds/latest.rss?feed_token%5B%5D=x", "/api/v2/feeds/latest.rss?feed_token=%00", "/api/v1/feed/rss/last?api-token=%ff"} {
		cases = append(cases, tc{"feed " + p[:min(len(p), 60)], "GET", p, nil, "", []int{400, 401, 414, 431}})
	}
	// --- method restrictions and overrides ---
	cases = append(cases,
		tc{"method override header ignored", "GET", "/torrents?q=input", map[string]string{"X-HTTP-Method-Override": "DELETE", "X-Method-Override": "POST"}, "", []int{200, 303}},
		tc{"method override parameter ignored", "GET", "/torrents?q=input&_method=DELETE", nil, "", []int{200, 303}},
		tc{"POST to a read-only API route", "POST", "/api/v2/torrents", bearer, "x=1", []int{405}},
		tc{"PUT to the feed", "PUT", "/api/v2/feeds/latest.rss", nil, "x=1", []int{405}},
		tc{"TRACE", "TRACE", "/torrents", nil, "", []int{405, 501}},
		tc{"OPTIONS", "OPTIONS", "/api/v2/torrents", bearer, "", []int{405}},
	)

	for _, c := range cases {
		hdr := map[string]string{}
		for k, v := range c.hdr {
			hdr[k] = v
		}
		var r secResp
		if len(c.hdr["Authorization"]) > 8000 || len(c.path) > 8000 {
			// A single header or request line beyond the server's header budget.
			var sb strings.Builder
			fmt.Fprintf(&sb, "%s %s HTTP/1.1\r\n%sConnection: close\r\n", c.method, c.path, hostLine)
			for k, v := range hdr {
				fmt.Fprintf(&sb, "%s: %s\r\n", k, v)
			}
			sb.WriteString("\r\n")
			st, body := rawTCP(t, s.web.addr, sb.String())
			r = secResp{status: st, body: body, hdr: http.Header{}}
		} else {
			r = s.raw(c.method, c.path, hdr, c.body)
		}
		ok := r.status == 0 // the server closed the connection: a refusal
		for _, w := range c.want {
			if r.status == w {
				ok = true
			}
		}
		if !ok || r.status >= 500 {
			t.Errorf("%s: status %d, want one of %v (never 5xx)", c.name, r.status, c.want)
		}
	}
	alive("the table of malformed requests")

	// Header and request-line abuse that Go's client cannot produce.
	for name, req := range map[string]string{
		"NUL in a header value":          "GET /healthz HTTP/1.1\r\n" + hostLine + "X-Bad: a\x00b\r\nConnection: close\r\n\r\n",
		"bare LF line endings":           "GET /healthz HTTP/1.1\n" + "Host: " + s.web.addr + "\n\n",
		"folded header":                  "GET /healthz HTTP/1.1\r\n" + hostLine + "X-A: b\r\n c\r\nConnection: close\r\n\r\n",
		"two Content-Length headers":     "POST /login HTTP/1.1\r\n" + hostLine + "Content-Length: 3\r\nContent-Length: 5\r\nContent-Type: application/x-www-form-urlencoded\r\nConnection: close\r\n\r\nabcde",
		"Transfer-Encoding and length":   "POST /login HTTP/1.1\r\n" + hostLine + "Transfer-Encoding: chunked\r\nContent-Length: 4\r\nContent-Type: application/x-www-form-urlencoded\r\nConnection: close\r\n\r\n0\r\n\r\n",
		"absolute-form request target":   "GET http://evil.example/healthz HTTP/1.1\r\n" + hostLine + "Connection: close\r\n\r\n",
		"missing Host (HTTP/1.1)":        "GET /healthz HTTP/1.1\r\nConnection: close\r\n\r\n",
		"HTTP/0.9 style":                 "GET /healthz\r\n\r\n",
		"20 KiB header value":            "GET /healthz HTTP/1.1\r\n" + hostLine + "X-Big: " + big(20<<10) + "\r\nConnection: close\r\n\r\n",
		"20 KiB request line":            "GET /" + big(20<<10) + " HTTP/1.1\r\n" + hostLine + "Connection: close\r\n\r\n",
		"3000 headers":                   "GET /healthz HTTP/1.1\r\n" + hostLine + strings.Repeat("X-H: 1\r\n", 3000) + "Connection: close\r\n\r\n",
		"chunked body over the limit":    "POST /login HTTP/1.1\r\n" + hostLine + "Transfer-Encoding: chunked\r\nContent-Type: application/x-www-form-urlencoded\r\nConnection: close\r\n\r\n" + chunked(2<<20),
		"declared length, short body":    "POST /login HTTP/1.1\r\n" + hostLine + "Content-Length: 100\r\nContent-Type: application/x-www-form-urlencoded\r\nConnection: close\r\n\r\nabc",
		"pipelined garbage after a body": "GET /healthz HTTP/1.1\r\n" + hostLine + "\r\n\x00\x01\x02GARBAGE\r\n\r\n",
	} {
		st, body := rawTCP(t, s.web.addr, req)
		if st >= 500 {
			t.Errorf("%s: status %d\n%.200s", name, st, body)
		}
		alive(name)
	}

	// A refused oversized change did nothing: the password still works and the
	// account page is unchanged.
	alice.get("/account")
	if resp := alice.post("/account/password", url.Values{"current_password": {secUserPW}, "new_password": {big(2 << 20)}, "new_password2": {big(2 << 20)}, "csrf_token": {alice.token()}}); resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Errorf("a 2 MiB new password: %d, want a 4xx", resp.StatusCode)
	}
	if resp := newBrowser(t, s.web.addr).login("alice", secUserPW); resp.StatusCode != 303 {
		t.Errorf("the password changed through a refused request: login %d", resp.StatusCode)
	}

	// A login with a 5000-character username and an over-long password is a
	// generic failure that does not cost a hash.
	b := newBrowser(t, s.web.addr)
	b.get("/login")
	start := time.Now()
	if resp := b.post("/login", url.Values{"username": {big(5000)}, "password": {big(5000)}, "csrf_token": {b.token()}}); resp.StatusCode >= 500 || resp.StatusCode == 303 {
		t.Errorf("over-long credentials: %d", resp.StatusCode)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("an over-long password took %s to refuse (it must be rejected before hashing)", d)
	}
	alive("the over-long login")
}

// chunked renders n bytes of form data as a chunked body.
func chunked(n int) string {
	var sb strings.Builder
	chunk := "a=" + strings.Repeat("B", 8190)
	for written := 0; written < n; written += len(chunk) {
		fmt.Fprintf(&sb, "%x\r\n%s\r\n", len(chunk), chunk)
	}
	sb.WriteString("0\r\n\r\n")
	return sb.String()
}

// sessionCookieOf returns "name=value" of the browser's session cookie.
func sessionCookieOf(b *webBrowser, addr string) string {
	for _, c := range b.c.Jar.Cookies(&url.URL{Scheme: "http", Host: addr}) {
		if strings.Contains(c.Name, "session") {
			return c.Name + "=" + c.Value
		}
	}
	return ""
}

// The headers of a slow client are bounded in time: a connection that never
// finishes its request line or headers is closed by the server, and the
// service keeps answering others meanwhile.
func TestSlowHeaderConnectionsAreClosed(t *testing.T) {
	s := newHarnessSec(t, func(o *opflags.WebOptions) { o.ShutdownTimeout = 5 * time.Second })
	conn, err := net.DialTimeout("tcp", s.web.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("GET /healthz HTTP/1.1\r\nHost: " + s.web.addr + "\r\nX-Slow: "))
	if r := s.raw("GET", "/healthz", nil, ""); r.status != 200 {
		t.Fatalf("a slow connection blocks other clients: %d", r.status)
	}
	// The server's header read timeout is 10 s.
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	start := time.Now()
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err == nil && n > 0 && !strings.Contains(string(buf[:n]), "408") && !strings.Contains(string(buf[:n]), "400") {
		t.Fatalf("unexpected data from a stalled connection: %q", buf[:n])
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("the stalled connection was held for %s, want it closed after the 10 s header timeout", d)
	}
}
