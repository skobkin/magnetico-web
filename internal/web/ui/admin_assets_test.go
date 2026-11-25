package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func vendorDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "third_party", "uplot-1.6.32", "VENDOR.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestVendoredUPlotChecksumsMatchVendorMd(t *testing.T) {
	t.Parallel()
	vendor := vendorDoc(t)
	for _, f := range []string{"uPlot.iife.min.js", "uPlot.min.css"} {
		data, err := fs.ReadFile(embedded, "static/"+f)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if !strings.Contains(vendor, hex.EncodeToString(sum[:])) {
			t.Errorf("the SHA-256 of %s is not in third_party/uplot-1.6.32/VENDOR.md: the vendored file was modified or the document is stale", f)
		}
	}
	lic, err := os.ReadFile(filepath.Join("..", "..", "..", "third_party", "uplot-1.6.32", "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(lic)
	if !strings.Contains(vendor, hex.EncodeToString(sum[:])) {
		t.Error("the SHA-256 of the uPlot LICENSE is not in VENDOR.md")
	}
	if !strings.Contains(string(lic), "MIT License") {
		t.Error("the vendored licence is not the MIT licence")
	}
	for _, want := range []string{"uplot@1.6.32", "sha512-KIMVnG68zvu5XXUbC4LQEPnhwOxBuLyW1AHtpm6IKTXImkbLgkMy+jabjLgSLMasNuGGzQm/ep3tOkyTxpiQIw==", "MIT"} {
		if !strings.Contains(vendor, want) {
			t.Errorf("VENDOR.md lacks %q", want)
		}
	}
}

func TestVendoredUPlotHasNoEvalOrFunctionConstructor(t *testing.T) {
	t.Parallel()
	data, err := fs.ReadFile(embedded, "static/uPlot.iife.min.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	for _, bad := range []string{"eval(", "new Function", "Function(", "document.write", "innerHTML", "outerHTML", "insertAdjacentHTML", "setAttribute", "insertRule", "cssText", "XMLHttpRequest", "fetch(", "importScripts", "sendBeacon", "WebSocket"} {
		if strings.Contains(src, bad) {
			t.Errorf("the vendored uPlot contains %q", bad)
		}
	}
	for _, m := range regexp.MustCompile(`https?://[^\s"')]+`).FindAllString(src, -1) {
		// The SVG namespace identifier and the project URL in the licence banner comment are not requests.
		if m != "http://www.w3.org/2000/svg" && m != "https://github.com/leeoniya/uPlot" {
			t.Errorf("the vendored uPlot references %q", m)
		}
	}
}

// The first-party chart code obeys the same rules as app.js, and the Content-Security-Policy
// stays untouched: styles go through the CSSOM, never through markup.
func TestAdminFrontendSourceInvariants(t *testing.T) {
	t.Parallel()
	js, err := fs.ReadFile(embedded, "static/admin.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "createContextualFragment", "javascript:", "XMLHttpRequest", "importScripts", "WebSocket", "sendBeacon", "localStorage", "sessionStorage", "setAttribute(\"style\"", "cssText"} {
		if strings.Contains(src, bad) {
			t.Errorf("admin.js contains %q", bad)
		}
	}
	if n := strings.Count(src, "fetch("); n != 1 {
		t.Errorf("admin.js has %d fetch call sites, want exactly one", n)
	}
	if !strings.Contains(src, `"/admin/api/series?`) {
		t.Error("admin.js does not fetch the same-origin series endpoint")
	}
	for _, m := range regexp.MustCompile(`https?://[^\s"')]+`).FindAllString(src, -1) {
		t.Errorf("admin.js references %q", m)
	}
	// Dynamic strings only reach the page through textContent.
	if strings.Count(src, "textContent") < 6 {
		t.Error("admin.js shows too few strings through textContent")
	}
	css, _ := fs.ReadFile(embedded, "static/admin.css")
	for _, bad := range []string{"@import", "url(http", "url(\"http", "expression("} {
		if strings.Contains(string(css), bad) {
			t.Errorf("admin.css contains %q", bad)
		}
	}
	for _, want := range []string{"prefers-color-scheme", `[data-theme="light"]`, "prefers-reduced-motion", "--chart-1"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("admin.css lacks %q", want)
		}
	}
	// The self-tests that run under Node exist and still cover the contract.
	st, err := os.ReadFile(filepath.Join("jstest", "admin_test.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nullBucketsRenderAsGaps", "noDataStateShown", "errorStateShown", "untrustedStringsAreTextContent", "summaryTableMatchesSeries"} {
		if !strings.Contains(string(st), name) {
			t.Errorf("jstest/admin_test.js lacks the self-test %s", name)
		}
	}
}

func TestAdminTemplateLoadsOnlySameOriginAssetsAndNoInlineCode(t *testing.T) {
	t.Parallel()
	b, err := fs.ReadFile(embedded, "templates/admin_overview.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if regexp.MustCompile(`(?i)<script[^>]*>[^<]|\son[a-z]+\s*=|\sstyle\s*=|<style|javascript:`).MatchString(src) {
		t.Error("the administrator template has inline script, inline style or an event handler")
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(src, -1) {
		u := m[1]
		if strings.HasPrefix(u, "http") || strings.HasPrefix(u, "//") {
			t.Errorf("the administrator template loads %q from another origin", u)
		}
	}
	for _, want := range []string{`asset "uPlot.iife.min.js"`, `asset "admin.js"`, `asset "uPlot.min.css"`, `asset "admin.css"`} {
		if !strings.Contains(src, want) {
			t.Errorf("the administrator template does not reference %s", want)
		}
	}
	// The data is fetched; it is never embedded in the page as script.
	if strings.Contains(src, "data-series") || strings.Contains(strings.ToLower(src), "application/json") {
		t.Error("chart data is embedded in the page")
	}
}
