// Package apidocs holds the embedded OpenAPI documents and serves the public
// API documentation (docs/specs/web-v1.md §9): the landing page /api, the
// interactive pages /api/v1 and /api/v2 (the vendored Swagger UI, started by
// the first-party docs.js) and the documents /api/v1/openapi.yaml and
// /api/v2/openapi.yaml.
//
// Everything here is public and static. The pages contain no credentials or
// user data, make no request to any other origin and store nothing. The
// documents describe only implemented endpoints; tests keep them consistent
// with the router and valid against the OpenAPI schema.
package apidocs

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"

	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/ui"
)

//go:embed openapi/v1.yaml
var v1 []byte

//go:embed openapi/v2.yaml
var v2 []byte

// V1 returns the OpenAPI document of /api/v1 (the deprecated RSS feed).
func V1() []byte { return append([]byte(nil), v1...) }

// V2 returns the OpenAPI document of /api/v2.
func V2() []byte { return append([]byte(nil), v2...) }

// docsCSP is the policy of the interactive pages: the application's policy plus
// data: images, which the vendored stylesheet uses for its icons. Nothing else
// is relaxed (no inline script or style, no eval, no other origin).
const docsCSP = "default-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'"

// Install registers the public documentation routes.
func Install(srv *server.Server) {
	rt := srv.Router()
	pub := func(pattern string, h http.HandlerFunc) {
		rt.Handle("GET "+pattern, server.KindRequest, server.Public, h)
	}
	pub("/api", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if err := srv.Renderer().Render(w, http.StatusOK, "api_index", struct{}{}); err != nil {
			srv.ErrorPage(w, r, http.StatusInternalServerError)
		}
	})
	page := func(title, spec string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", docsCSP)
			w.Header().Set("Cache-Control", "no-store")
			if err := srv.Renderer().Render(w, http.StatusOK, "api_docs", ui.DocsPage{Title: title, SpecURL: spec}); err != nil {
				srv.ErrorPage(w, r, http.StatusInternalServerError)
			}
		}
	}
	pub("/api/v1", page("API v1 (deprecated)", "/api/v1/openapi.yaml"))
	pub("/api/v2", page("API v2", "/api/v2/openapi.yaml"))
	pub("/api/v1/openapi.yaml", spec(v1))
	pub("/api/v2/openapi.yaml", spec(v2))
}

func spec(doc []byte) http.HandlerFunc {
	sum := sha256.Sum256(doc)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "application/yaml; charset=utf-8")
		h.Set("ETag", etag)
		h.Set("Cache-Control", "public, max-age=300")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(doc)
	}
}
