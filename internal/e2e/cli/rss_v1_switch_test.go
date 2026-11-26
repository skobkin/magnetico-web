package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// MAGNETICO_WEB_RSS_V1_ENABLED=false through the real binary: the deprecated
// v1 feed answers 404 for every transport of a valid token, the v2 feed serves
// the same token, the account page stops telling users that the v1 address
// works, and the startup log names the switch without any credential.
func TestRealBinaryRSSV1Switch(t *testing.T) {
	w := newHarnessWorld(t, "MAGNETICO_WEB_RSS_V1_ENABLED=false")
	tok := w.mintToken("rss")

	get := func(path string, hdr map[string]string, cookie string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://"+w.web.addr+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "api-token", Value: cookie})
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for name, st := range map[string]int{
		"query":  get("/api/v1/feed/rss/last?api-token="+tok, nil, ""),
		"header": get("/api/v1/feed/rss/last", map[string]string{"api-token": tok}, ""),
		"cookie": get("/api/v1/feed/rss/last", nil, tok),
		"none":   get("/api/v1/feed/rss/last", nil, ""),
	} {
		if st != http.StatusNotFound {
			t.Errorf("v1 %s: %d, want 404", name, st)
		}
	}
	// The documented 404 (OpenAPI v1 getLegacyFeed): the router's plain-text
	// answer for an unregistered path, never an RSS document or the token.
	{
		resp, err := http.Get("http://" + w.web.addr + "/api/v1/feed/rss/last?api-token=" + tok)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") ||
			strings.TrimSpace(string(body)) != "404 page not found" {
			t.Errorf("disabled v1 answer: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
	}
	if st, hdr, _ := w.bearer("", "/api/v2/feeds/latest.rss?feed_token="+tok); st != 200 || !strings.Contains(hdr.Get("Content-Type"), "rss") {
		t.Fatalf("v2 feed: %d %s", st, hdr.Get("Content-Type"))
	}

	w.alice.get("/account?tab=rss")
	if !strings.Contains(w.alice.body, "switched off on this site") || strings.Contains(w.alice.body, "but it still works") {
		t.Error("the RSS tab must say that the v1 address is switched off")
	}
	logs := w.web.out.String()
	if !strings.Contains(logs, "rss v1 feed disabled") {
		t.Error("the startup log does not name the disabled v1 feed")
	}
	if strings.Contains(logs, tok) {
		t.Error("a token reached the web log")
	}
}
