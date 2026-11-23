// Package ui holds the embedded HTML templates, the static assets and the
// rendering infrastructure of the web service. W1 ships only the generic
// error page and a foundation stylesheet; the real pages belong to later
// milestones.
package ui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

//go:embed templates static
var embedded embed.FS

// StaticPrefix is the URL prefix assets are served under.
const StaticPrefix = "/static/"

// Assets serves the embedded static files under content-hashed names
// (app.css → app.<hash>.css) with immutable caching: a changed file gets a
// new URL, so a cached copy can never be stale.
type Assets struct {
	byName   map[string]string // original name → hashed name
	byHashed map[string]asset
}

type asset struct {
	data []byte
	etag string
	ctyp string
}

// NewAssets hashes every embedded static file.
func NewAssets() (*Assets, error) {
	a := &Assets{byName: map[string]string{}, byHashed: map[string]asset{}}
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		return nil, err
	}
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		h := hex.EncodeToString(sum[:])[:12]
		ext := path.Ext(p)
		hashed := strings.TrimSuffix(p, ext) + "." + h + ext
		ctyp := mime.TypeByExtension(ext)
		if ctyp == "" {
			ctyp = "application/octet-stream"
		}
		a.byName[p] = hashed
		a.byHashed[hashed] = asset{data: data, etag: `"` + h + `"`, ctyp: ctyp}
		return nil
	})
	return a, err
}

// URL returns the hashed URL of an embedded asset.
func (a *Assets) URL(name string) (string, error) {
	h, ok := a.byName[name]
	if !ok {
		return "", fmt.Errorf("unknown static asset %q", name)
	}
	return StaticPrefix + h, nil
}

// Names lists the original asset names (sorted).
func (a *Assets) Names() []string {
	out := make([]string, 0, len(a.byName))
	for n := range a.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ServeFile serves the asset called file (the last URL segment). Only exact
// hashed names exist; everything else is 404.
func (a *Assets) ServeFile(w http.ResponseWriter, r *http.Request, file string) {
	as, ok := a.byHashed[file]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", as.ctyp)
	h.Set("ETag", as.etag)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(as.data))
}

// Renderer renders the embedded page templates. Each page is parsed together
// with the layout into its own set, so pages may define "title" and
// "content" independently.
type Renderer struct {
	pages map[string]*template.Template
}

// ErrorPage is the data of the generic error page.
type ErrorPage struct {
	Status    int
	Heading   string
	Message   string
	RequestID string
}

// DefaultSiteTitle is the site name when none is configured.
const DefaultSiteTitle = "Magnetico"

// NewRenderer parses every page template against the layout.
// siteTitle is the site name templates print (empty selects DefaultSiteTitle).
func NewRenderer(assets *Assets, siteTitle string) (*Renderer, error) {
	if siteTitle == "" {
		siteTitle = DefaultSiteTitle
	}
	funcs := template.FuncMap{"asset": assets.URL, "siteTitle": func() string { return siteTitle }}
	entries, err := fs.ReadDir(embedded, "templates")
	if err != nil {
		return nil, err
	}
	r := &Renderer{pages: map[string]*template.Template{}}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "layout.html" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(embedded, "templates/layout.html", "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		r.pages[strings.TrimSuffix(name, ".html")] = t
	}
	return r, nil
}

// Render executes the page into a buffer first, so a template failure yields
// a clean error instead of a half-written page, then writes it with status.
func (r *Renderer) Render(w http.ResponseWriter, status int, page string, data any) error {
	t, ok := r.pages[page]
	if !ok {
		return fmt.Errorf("unknown page template %q", page)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := w.Write(buf.Bytes())
	return err
}

// LoginPage is the data of the sign-in form (W3's minimal presentation; W5
// owns the real design).
type LoginPage struct {
	CSRFToken string
	Next      string // validated local path or ""
	Username  string // echoed after a failed attempt (escaped by the template); never the password
	Remember  bool
	Error     string // generic message, "" for none
	// Notice is a fixed success text chosen from a whitelist by slug ("" for
	// none); never user input.
	Notice string
	// ForgotLink offers /password/forgot (only when email is configured).
	ForgotLink bool
}

// MessagePage is a plain notice (CSRF failure, temporary unavailability).
type MessagePage struct {
	Heading  string
	Message  string
	LinkHref string // optional same-origin link
	LinkText string
}
