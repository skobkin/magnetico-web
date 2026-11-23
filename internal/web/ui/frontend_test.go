package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Static guarantees about the first-party front end: no HTML string
// injection, no external requests, no inline script or style (the CSP forbids
// them), and a tiny dependency surface.
func TestFrontendSourceInvariants(t *testing.T) {
	t.Parallel()
	js, err := fs.ReadFile(embedded, "static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "createContextualFragment", "javascript:", "XMLHttpRequest", "importScripts", "WebSocket", "sendBeacon"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("app.js contains %q", forbidden)
		}
	}
	// The only absolute URL is the SVG namespace identifier (not a request).
	for _, m := range regexp.MustCompile(`https?://[^\s"')]+`).FindAllString(src, -1) {
		if m != "http://www.w3.org/2000/svg" {
			t.Errorf("app.js references %q", m)
		}
	}
	if n := strings.Count(src, "fetch("); n != 1 {
		t.Errorf("expected exactly one fetch call site, found %d", n)
	}
	// Storage access is always guarded.
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "localStorage") && !strings.Contains(line, "try") && !strings.Contains(line, "t === \"system\"") {
			t.Errorf("unguarded storage use: %s", strings.TrimSpace(line))
		}
	}
	theme, _ := fs.ReadFile(embedded, "static/theme.js")
	if !strings.Contains(string(theme), "try") || len(theme) > 1024 {
		t.Error("theme.js must stay a tiny guarded script")
	}

	css, _ := fs.ReadFile(embedded, "static/app.css")
	for _, forbidden := range []string{"@import", "url(http", "url(\"http", "//fonts.", "expression("} {
		if strings.Contains(string(css), forbidden) {
			t.Errorf("app.css contains %q", forbidden)
		}
	}
	for _, want := range []string{"prefers-color-scheme", "prefers-reduced-motion", ":focus-visible", `[data-theme="light"]`, "max-width:360px"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}

func TestTemplatesHaveNoInlineScriptOrStyle(t *testing.T) {
	t.Parallel()
	entries, _ := fs.ReadDir(embedded, "templates")
	inline := regexp.MustCompile(`(?i)<script[^>]*>[^<]|\son[a-z]+\s*=|\sstyle\s*=|<style|javascript:|<link[^>]+href="https?:|<script[^>]+src="https?:|<img[^>]+src="https?:`)
	for _, e := range entries {
		b, _ := fs.ReadFile(embedded, "templates/"+e.Name())
		if m := inline.Find(b); m != nil {
			t.Errorf("%s: %q", e.Name(), m)
		}
	}
}

func TestEveryIconUsedExistsInTheSprite(t *testing.T) {
	t.Parallel()
	layout, _ := fs.ReadFile(embedded, "templates/layout.html")
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`<symbol id="i-([a-z-]+)"`).FindAllStringSubmatch(string(layout), -1) {
		defined[m[1]] = true
	}
	var used []string
	entries, _ := fs.ReadDir(embedded, "templates")
	for _, e := range entries {
		b, _ := fs.ReadFile(embedded, "templates/"+e.Name())
		for _, m := range regexp.MustCompile(`template "icon" "([a-z-]+)"`).FindAllStringSubmatch(string(b), -1) {
			used = append(used, m[1])
		}
	}
	js, _ := fs.ReadFile(embedded, "static/app.js")
	for _, m := range regexp.MustCompile(`icon\("([a-z-]+)"`).FindAllStringSubmatch(string(js), -1) {
		used = append(used, m[1])
	}
	for _, m := range regexp.MustCompile(`THEME_ICON = \{([^}]*)\}`).FindAllStringSubmatch(string(js), -1) {
		for _, n := range regexp.MustCompile(`"([a-z-]+)"`).FindAllStringSubmatch(m[1], -1) {
			used = append(used, n[1])
		}
	}
	if len(used) < 10 {
		t.Fatalf("found only %d icon uses", len(used))
	}
	for _, name := range used {
		if !defined[name] {
			t.Errorf("icon %q is not in the sprite", name)
		}
	}
}

func TestPagesRenderWithAutoescapingAndNoJSFallback(t *testing.T) {
	t.Parallel()
	a, _ := NewAssets()
	r, err := NewRenderer(a, "")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := r.Render(rec, 200, "login", LoginPage{CSRFToken: "tok", Username: `"><script>x</script>`, Error: "<b>bad</b>"}); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>x") || strings.Contains(body, "<b>bad") || !strings.Contains(body, "&lt;b&gt;bad") {
		t.Fatalf("login page does not escape:\n%s", body)
	}
	for _, want := range []string{`name="csrf_token" value="tok"`, `class="skip-link"`, `id="theme-toggle" hidden`, `<main id="main"`, `app.`, `theme.`} {
		if !strings.Contains(body, want) {
			t.Errorf("login page lacks %q", want)
		}
	}
}

// The in-page self-tests (testdata/selftest.js) run in a real browser when
// MAGNETICO_TEST_BROWSER names a Chromium-compatible binary. They also run in
// any other DOM host: see docs/wip/07F-web-ui-report.md.
func TestFrontendSelfTestsInBrowser(t *testing.T) {
	bin := os.Getenv("MAGNETICO_TEST_BROWSER")
	if bin == "" {
		t.Skip("explicit: set MAGNETICO_TEST_BROWSER=<chromium binary>")
	}
	assets, err := NewAssets()
	if err != nil {
		t.Fatal(err)
	}
	appURL, _ := assets.URL("app.js")
	selftest, err := os.ReadFile("testdata/selftest.js")
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := os.ReadFile("../pathfmt/testdata/name-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/static/{file}", func(w http.ResponseWriter, r *http.Request) { assets.ServeFile(w, r, r.PathValue("file")) })
	mux.HandleFunc("/selftest.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write(selftest)
	})
	mux.HandleFunc("/vectors.json", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(vectors) })
	mux.HandleFunc("/run.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte(`fetch("/vectors.json").then(r=>r.json()).then(v=>window.runSelfTests(v)).then(r=>{document.getElementById("result").textContent=JSON.stringify(r)})`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html data-selftest="1"><head><meta charset="utf-8"><script src="` + appURL + `"></script><script src="/selftest.js"></script></head><body><pre id="result"></pre><script src="/run.js"></script></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--headless", "--no-sandbox", "--disable-gpu", "--virtual-time-budget=60000", "--dump-dom", srv.URL+"/")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("browser: %v", err)
	}
	m := regexp.MustCompile(`(?s)<pre id="result">(.*?)</pre>`).FindSubmatch(out.Bytes())
	if m == nil || len(bytes.TrimSpace(m[1])) == 0 {
		t.Fatalf("no self-test result in the dumped DOM:\n%.500s", out.String())
	}
	var res struct {
		Passed, Failed int
		Results        []struct {
			Name, Detail string
			Ok           bool
		}
	}
	raw := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&", "&quot;", `"`).Replace(string(m[1]))
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Results {
		if !r.Ok {
			t.Errorf("%s: %s", r.Name, r.Detail)
		}
	}
	t.Logf("%d passed, %d failed", res.Passed, res.Failed)
}

// Light and dark must define exactly the same colour tokens, and the
// system-preference light block must repeat the explicit light theme.
func TestThemesDefineTheSameTokens(t *testing.T) {
	t.Parallel()
	b, _ := fs.ReadFile(embedded, "static/app.css")
	css := string(b)
	block := func(start string) string {
		i := strings.Index(css, start)
		if i < 0 {
			t.Fatalf("css lacks %q", start)
		}
		j := strings.Index(css[i:], "}")
		return css[i : i+j]
	}
	tokens := func(s string) []string {
		var out []string
		for _, m := range regexp.MustCompile(`--([a-z-]+):`).FindAllStringSubmatch(s, -1) {
			out = append(out, m[1])
		}
		return out
	}
	dark := tokens(block(":root {"))
	light := tokens(block(`:root[data-theme="light"] {`))
	system := tokens(block(`:root:not([data-theme="dark"]):not([data-theme="light"])`))
	has := func(list []string, n string) bool {
		for _, x := range list {
			if x == n {
				return true
			}
		}
		return false
	}
	for _, n := range light {
		if !has(dark, n) || !has(system, n) {
			t.Errorf("token --%s missing from the dark or the system-light block", n)
		}
	}
	for _, n := range []string{"bg", "surface", "text", "subtext", "muted", "accent", "danger", "focus", "border-strong"} {
		if !has(dark, n) || !has(light, n) {
			t.Errorf("token --%s is not defined in both themes", n)
		}
	}
	for _, bp := range []string{"max-width:900px", "max-width:680px", "max-width:360px"} {
		if !strings.Contains(css, bp) {
			t.Errorf("missing responsive breakpoint %s", bp)
		}
	}
}
