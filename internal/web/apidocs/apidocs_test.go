package apidocs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/skobkin/magnetico-web/internal/web/apidocs"
	"github.com/skobkin/magnetico-web/internal/web/apiv2"
	"github.com/skobkin/magnetico-web/internal/web/feed"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/torrents"
)

type fakeBackend struct{}

func (fakeBackend) Verify(context.Context) error { return nil }
func (fakeBackend) Ping(context.Context) error   { return nil }
func (fakeBackend) Close()                       {}

// Nil-embedding fakes satisfy the interfaces for route registration only; no
// request in this file reaches them.
type (
	fakeSearch  struct{ torrents.Searcher }
	fakeDetails struct{ torrents.Details }
	fakeView    struct{ torrents.View }
	fakeRows    struct{ feed.Rows }
)

type apiAuth struct{ apiv2.Authenticator }
type rssAuth struct{ feed.Authenticator }

// newServer builds a server with every route the API surface consists of.
func newServer(t *testing.T) *server.Server {
	t.Helper()
	pu, _ := url.Parse("https://magnet.example.org")
	srv, err := server.New(server.Config{
		PublicURL: pu, TrustedProxies: []netip.Prefix{}, RequestTimeout: 10 * time.Second, SearchTimeout: time.Minute,
		ShutdownTimeout: 5 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := apiv2.Install(srv, apiv2.Config{Auth: apiAuth{}, Search: fakeSearch{}, Details: fakeDetails{}, View: fakeView{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := feed.Install(srv, feed.Config{Auth: rssAuth{}, Rows: fakeRows{}, PublicBase: pu}); err != nil {
		t.Fatal(err)
	}
	// A stand-in for the session guard: the cookie "sess=ok" is a signed-in
	// user; everything else is redirected to the sign-in page.
	srv.Router().SetGuard(server.Session, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c, err := r.Cookie("sess"); err != nil || c.Value != "ok" {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	apidocs.Install(srv)
	return srv
}

// operations returns "METHOD /full/path" for every operation of a document,
// with its server base path prepended.
func operations(t *testing.T, doc []byte) []string {
	t.Helper()
	var d struct {
		Servers []struct {
			URL string `yaml:"url"`
		} `yaml:"servers"`
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(doc, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Servers) != 1 {
		t.Fatalf("expected exactly one server entry, got %d", len(d.Servers))
	}
	var ops []string
	for p, item := range d.Paths {
		for m := range item {
			switch m {
			case "get", "put", "post", "delete", "patch", "head", "options", "trace":
				ops = append(ops, strings.ToUpper(m)+" "+strings.TrimRight(d.Servers[0].URL, "/")+p)
			}
		}
	}
	sort.Strings(ops)
	return ops
}

func TestRoutesAndDocumentsDescribeEachOtherExactly(t *testing.T) {
	srv := newServer(t)
	byPrefix := map[string][]string{"/api/v1/": nil, "/api/v2/": nil}
	docRoutes := map[string]bool{
		"GET /api": true, "GET /api/v1": true, "GET /api/v2": true,
		"GET /api/v1/openapi.yaml": true, "GET /api/v2/openapi.yaml": true,
		"GET /api/v2/": true, // the unknown-path catch-all is not an endpoint
	}
	for _, r := range srv.Router().Routes() {
		_, path, _ := strings.Cut(r.Pattern, " ")
		if !strings.HasPrefix(path, "/api") || docRoutes[r.Pattern] {
			continue
		}
		matched := false
		for prefix := range byPrefix {
			if strings.HasPrefix(path, prefix) {
				byPrefix[prefix] = append(byPrefix[prefix], r.Pattern)
				matched = true
			}
		}
		if !matched {
			t.Errorf("route %s belongs to no documented API", r.Pattern)
		}
	}
	for prefix, doc := range map[string][]byte{"/api/v1/": apidocs.V1(), "/api/v2/": apidocs.V2()} {
		implemented := byPrefix[prefix]
		sort.Strings(implemented)
		documented := operations(t, doc)
		if strings.Join(implemented, "\n") != strings.Join(documented, "\n") {
			t.Errorf("%s\nimplemented:\n  %s\ndocumented:\n  %s", prefix, strings.Join(implemented, "\n  "), strings.Join(documented, "\n  "))
		}
	}
}

func TestEveryAPIRouteHasItsOwnAccessClass(t *testing.T) {
	srv := newServer(t)
	for _, r := range srv.Router().Routes() {
		_, path, _ := strings.Cut(r.Pattern, " ")
		want := ""
		switch {
		case path == "/api/v1/feed/rss/last" || path == "/api/v2/feeds/latest.rss":
			want = "feed"
		case strings.HasPrefix(path, "/api/v2/torrents") || path == "/api/v2/":
			want = "api-token"
		case strings.HasPrefix(path, "/api"):
			want = "session"
		}
		if want != "" && r.Access.String() != want {
			t.Errorf("%s: access %s, want %s", r.Pattern, r.Access, want)
		}
		if r.Access.String() == "api-token" || r.Access.String() == "feed" {
			if m, _, _ := strings.Cut(r.Pattern, " "); m != "GET" {
				t.Errorf("%s is not GET", r.Pattern)
			}
		}
	}
}

// ---- schema validation against the official OpenAPI 3.2 schema, offline ----

type offlineLoader map[string][]byte

func (l offlineLoader) Load(u string) (any, error) {
	b, ok := l[u]
	if !ok {
		return nil, fmt.Errorf("refusing to load %q: only the vendored OpenAPI schema files are available", u)
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(b))
}

func oasValidator(t *testing.T) *jsonschema.Schema {
	t.Helper()
	files := map[string]string{
		"https://spec.openapis.org/oas/3.2/schema-base/2025-09-17": "3.2_schema-base_2025-09-17.json",
		"https://spec.openapis.org/oas/3.2/schema/2025-09-17":      "3.2_schema_2025-09-17.json",
		"https://spec.openapis.org/oas/3.2/dialect/2025-09-17":     "3.2_dialect_2025-09-17.json",
		"https://spec.openapis.org/oas/3.2/meta/2025-09-17":        "3.2_meta_2025-09-17.json",
	}
	l := offlineLoader{}
	for u, f := range files {
		b, err := os.ReadFile(filepath.Join("testdata", "oas", f))
		if err != nil {
			t.Fatal(err)
		}
		l[u] = b
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(l)
	sch, err := c.Compile("https://spec.openapis.org/oas/3.2/schema-base/2025-09-17")
	if err != nil {
		t.Fatalf("compiling the OpenAPI schema: %v", err)
	}
	return sch
}

func asJSONInstance(t *testing.T, doc []byte) any {
	t.Helper()
	j, err := yaml.YAMLToJSON(doc)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestEmbeddedDocumentsAreValidOpenAPI32(t *testing.T) {
	sch := oasValidator(t)
	for name, doc := range map[string][]byte{"v1": apidocs.V1(), "v2": apidocs.V2()} {
		if err := sch.Validate(asJSONInstance(t, doc)); err != nil {
			t.Errorf("%s: %#v", name, err)
		}
		if !regexp.MustCompile(`(?m)^openapi: 3\.2\.0$`).Match(doc) {
			t.Errorf("%s: not an OpenAPI 3.2.0 document", name)
		}
	}
}

// The validator is real: a broken document is rejected.
func TestTheValidatorRejectsAnInvalidDocument(t *testing.T) {
	sch := oasValidator(t)
	bad := strings.Replace(string(apidocs.V2()), "operationId: listTorrents", "operationId: 42", 1)
	bad = strings.Replace(bad, "type: integer, format: int32, minimum: 1}", "type: bogus-type}", 1)
	if err := sch.Validate(asJSONInstance(t, []byte(bad))); err == nil {
		t.Fatal("an invalid document was accepted")
	}
	if err := sch.Validate(asJSONInstance(t, []byte("openapi: 3.2.0\ninfo: {title: x}\n"))); err == nil {
		t.Fatal("a document without version in info was accepted")
	}
}

func TestOperationsAreCompleteAndConsistent(t *testing.T) {
	for name, doc := range map[string][]byte{"v1": apidocs.V1(), "v2": apidocs.V2()} {
		var d struct {
			Paths map[string]map[string]struct {
				OperationID string         `yaml:"operationId"`
				Summary     string         `yaml:"summary"`
				Responses   map[string]any `yaml:"responses"`
				Security    []any          `yaml:"security"`
			} `yaml:"paths"`
		}
		if err := yaml.Unmarshal(doc, &d); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for p, item := range d.Paths {
			for m, op := range item {
				if op.OperationID == "" || op.Summary == "" || len(op.Responses) == 0 || seen[op.OperationID] {
					t.Errorf("%s %s %s: incomplete or duplicate operation %+v", name, m, p, op)
				}
				seen[op.OperationID] = true
				if _, ok := op.Responses["200"]; !ok {
					t.Errorf("%s %s %s: no 200 response", name, m, p)
				}
				if _, ok := op.Responses["401"]; !ok {
					t.Errorf("%s %s %s: no 401 response", name, m, p)
				}
			}
		}
	}
}

func TestV1DescribesOnlyTheRSSEndpointAndItsAuthentication(t *testing.T) {
	doc := string(apidocs.V1())
	if ops := operations(t, apidocs.V1()); len(ops) != 1 || ops[0] != "GET /api/v1/feed/rss/last" {
		t.Fatalf("v1 operations: %v", ops)
	}
	for _, want := range []string{"deprecated: true", "in: header", "in: cookie", "in: query", "name: api-token", "RSS token", "Referrer-Policy", "page=1", "never"} {
		if !strings.Contains(doc, want) {
			t.Errorf("v1 lacks %q", want)
		}
	}
	for _, removed := range []string{"/login", "/logout", "/torrents", "operationId: login"} {
		if strings.Contains(doc, removed) {
			t.Errorf("v1 mentions the removed %q", removed)
		}
	}
}

func TestV2DocumentsTheFileViewContractAndTheCredentialSplit(t *testing.T) {
	doc := string(apidocs.V2())
	for _, want := range []string{
		"full_tree_available", "FileTree:", "FileTreeDir:", "FileTreeFile:", "FileDirPage:", "DirEntry:", "FileEntry:", "Ancestor:", "DirSummary:",
		"name_base64", "next_offset", "urn:magnetico:problem:full-tree-unavailable", "urn:magnetico:problem:index-too-complex",
		"urn:magnetico:problem:invalid-parameter", "urn:magnetico:problem:not-found", "urn:magnetico:problem:temporarily-unavailable",
		"^((0|[1-9][0-9]{0,5})(_(0|[1-9][0-9]{0,5})){0,255})?$", "maximum: 1000", "default: 160", "alternate",
		"bearerAuth", "feedToken", "name: feed_token", "scheme: bearer", "/feeds/latest.rss",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("v2 lacks %q", want)
		}
	}
}

// ---- the public pages ----

func get(h http.Handler, path string, hdr map[string]string) *http.Response {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "203.0.113.9:1"
	if hdr["X-Anonymous"] == "" {
		req.AddCookie(&http.Cookie{Name: "sess", Value: "ok"})
	}
	delete(hdr, "X-Anonymous")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func body(r *http.Response) string { b, _ := io.ReadAll(r.Body); return string(b) }

func TestDocumentationRequiresASession(t *testing.T) {
	h := newServer(t).Handler()
	for _, path := range []string{"/api", "/api/v1", "/api/v2", "/api/v1/openapi.yaml", "/api/v2/openapi.yaml"} {
		r := get(h, path, map[string]string{"X-Anonymous": "1"})
		if r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), "/login?next=") {
			t.Errorf("%s anonymous: %d %q", path, r.StatusCode, r.Header.Get("Location"))
		}
		// A bearer token is not a session.
		r = get(h, path, map[string]string{"X-Anonymous": "1", "Authorization": "Bearer mgt_xxxx"})
		if r.StatusCode != http.StatusSeeOther {
			t.Errorf("%s with a bearer token: %d", path, r.StatusCode)
		}
	}
}

func TestDocumentationIsNoindexAndSelfContained(t *testing.T) {
	h := newServer(t).Handler()
	for path, wantCT := range map[string]string{
		"/api": "text/html", "/api/v1": "text/html", "/api/v2": "text/html",
		"/api/v1/openapi.yaml": "application/yaml", "/api/v2/openapi.yaml": "application/yaml",
	} {
		r := get(h, path, nil) // signed in, nothing else
		b := body(r)
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), wantCT) {
			t.Errorf("%s: %d %q", path, r.StatusCode, r.Header.Get("Content-Type"))
			continue
		}
		if r.Header.Get("X-Robots-Tag") != "noindex, nofollow" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: indexing/security headers missing", path)
		}
		if len(r.Cookies()) != 0 {
			t.Errorf("%s sets a cookie", path)
		}
		if wantCT == "text/html" {
			if !strings.Contains(b, `name="robots" content="noindex, nofollow"`) {
				t.Errorf("%s lacks the robots meta tag", path)
			}
			// Nothing is loaded from, or submitted to, another origin. Plain
			// anchors (the footer's source-code link) are navigation only.
			if m := regexp.MustCompile(`(?i)(\ssrc|\saction|<link[^>]*\shref)="https?://[^"]*"`).FindString(b); m != "" {
				t.Errorf("%s references another origin: %s", path, m)
			}
			if regexp.MustCompile(`(?i)\sstyle\s*=|\son[a-z]+\s*=|<style|<script[^>]*>[^<]`).MatchString(b) {
				t.Errorf("%s contains inline script or style", path)
			}
			if strings.Contains(b, "mgt_") || strings.Contains(b, "mgf_") || strings.Contains(strings.ToLower(b), "bearer mg") {
				t.Errorf("%s contains token-like text", path)
			}
		}
	}
	// Extra credentials change nothing: the page is the same static document.
	a, b := body(get(h, "/api/v2", nil)), body(get(h, "/api/v2", map[string]string{"Authorization": "Bearer mgt_xxxx", "api-token": "secret"}))
	if a != b {
		t.Error("the documentation page depends on request credentials")
	}
}

func TestInteractiveDocsLoadOnlyVendoredSameOriginAssetsUnderAStrictCSP(t *testing.T) {
	h := newServer(t).Handler()
	for _, tc := range []struct{ path, spec string }{{"/api/v1", "/api/v1/openapi.yaml"}, {"/api/v2", "/api/v2/openapi.yaml"}} {
		r := get(h, tc.path, nil)
		page := body(r)
		csp := r.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "img-src 'self' data:") || !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("%s CSP = %q", tc.path, csp)
		}
		for _, bad := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:", "*", "blob:"} {
			if strings.Contains(csp, bad) {
				t.Errorf("%s CSP contains %q: %s", tc.path, bad, csp)
			}
		}
		if !strings.Contains(csp, "form-action 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s CSP = %q", tc.path, csp)
		}
		if !strings.Contains(page, `data-spec="`+tc.spec+`"`) {
			t.Errorf("%s lacks its spec URL", tc.path)
		}
		srcs := regexp.MustCompile(`(?:src|href)="(/static/[^"]+)"`).FindAllStringSubmatch(page, -1)
		if len(srcs) < 3 {
			t.Fatalf("%s: assets = %v", tc.path, srcs)
		}
		for _, m := range srcs {
			a := get(h, m[1], nil)
			if a.StatusCode != 200 || !strings.Contains(a.Header.Get("Cache-Control"), "immutable") {
				t.Errorf("%s: asset %s: %d %q", tc.path, m[1], a.StatusCode, a.Header.Get("Cache-Control"))
			}
		}
		if !strings.Contains(page, "swagger-ui-bundle.") || !strings.Contains(page, "docs.") || !strings.Contains(page, "swagger-ui.") {
			t.Errorf("%s does not load the vendored files", tc.path)
		}
	}
	// The relaxation is not on any other route.
	if csp := get(h, "/api", nil).Header.Get("Content-Security-Policy"); strings.Contains(csp, "data:") {
		t.Errorf("/api CSP carries the docs relaxation: %s", csp)
	}
	if csp := get(h, "/healthz", nil).Header.Get("Content-Security-Policy"); strings.Contains(csp, "data:") {
		t.Errorf("/healthz CSP carries the docs relaxation: %s", csp)
	}
}

func TestDocsInitializerKeepsTryItOutLocalSafeAndStateless(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "ui", "static", "docs.js"))
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{
		"validatorUrl: null", "persistAuthorization: false", `supportedSubmitMethods: ["get"]`, "tryItOutEnabled: false",
		"withCredentials: false", `req.credentials = "omit"`, "deepLinking: false", `getAttribute("data-spec")`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("docs.js lacks %q", want)
		}
	}
	for _, bad := range []string{"http://", "https://", "localStorage", "sessionStorage", "document.cookie", "eval(", `"Authorization"`, "mgt_", "mgf_"} {
		if strings.Contains(js, bad) {
			t.Errorf("docs.js contains %q", bad)
		}
	}
}

func TestVendoredFilesMatchTheRecordedHashes(t *testing.T) {
	vendor, err := os.ReadFile(filepath.Join("..", "..", "..", "third_party", "swagger-ui-dist-5.34.1", "VENDOR.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"swagger-ui-bundle.js", "swagger-ui.css"} {
		b, err := os.ReadFile(filepath.Join("..", "ui", "static", f))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if !strings.Contains(string(vendor), hex.EncodeToString(sum[:])) {
			t.Errorf("%s does not match the hash recorded in VENDOR.md", f)
		}
	}
	for _, f := range []string{"LICENSE", "NOTICE", "swagger-ui-bundle.js.LICENSE.txt"} {
		if _, err := os.Stat(filepath.Join("..", "..", "..", "third_party", "swagger-ui-dist-5.34.1", f)); err != nil {
			t.Errorf("licence file %s missing: %v", f, err)
		}
	}
	// The recorded OAS assets match too.
	readme, _ := os.ReadFile(filepath.Join("testdata", "oas", "README.md"))
	entries, _ := os.ReadDir(filepath.Join("testdata", "oas"))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, _ := os.ReadFile(filepath.Join("testdata", "oas", e.Name()))
		sum := sha256.Sum256(b)
		if !strings.Contains(string(readme), hex.EncodeToString(sum[:])) {
			t.Errorf("%s does not match the hash in the README", e.Name())
		}
	}
}

func TestSpecEndpointsHonourConditionalRequests(t *testing.T) {
	h := newServer(t).Handler()
	r := get(h, "/api/v2/openapi.yaml", nil)
	etag := r.Header.Get("ETag")
	if etag == "" || r.Header.Get("Cache-Control") != "private, max-age=300" {
		t.Fatalf("etag=%q cc=%q", etag, r.Header.Get("Cache-Control"))
	}
	if r2 := get(h, "/api/v2/openapi.yaml", map[string]string{"If-None-Match": etag}); r2.StatusCode != 304 {
		t.Fatalf("conditional: %d", r2.StatusCode)
	}
	if body(r) != string(apidocs.V2()) {
		t.Fatal("the served document differs from the embedded one")
	}
	// The documents are static: no credential-looking strings.
	for _, doc := range []string{string(apidocs.V1()), string(apidocs.V2())} {
		if regexp.MustCompile(`m[gt][tf]_[A-Za-z0-9_-]{20,}`).MatchString(doc) {
			t.Error("a document embeds a token-like value")
		}
	}
}
