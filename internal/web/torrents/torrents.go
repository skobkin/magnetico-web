// Package torrents holds the W4 HTTP handlers: torrent search and listing,
// torrent details and the file browser (docs/wip/07E-web-search-details-plan.md
// §4). Handlers translate between HTTP and the search service and the
// file-index cache; they hold no SQL and make no decisions about memory.
//
// Logs never carry search text, torrent names, file paths or cursors; they
// carry ids, outcome categories and counts only.
package torrents

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/auth"
	"github.com/skobkin/magnetico-web/internal/web/filecache"
	"github.com/skobkin/magnetico-web/internal/web/filetree"
	"github.com/skobkin/magnetico-web/internal/web/gate"
	"github.com/skobkin/magnetico-web/internal/web/pathfmt"
	"github.com/skobkin/magnetico-web/internal/web/search"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/ui"
	"github.com/skobkin/magnetico-web/persistence"
)

// PageRows is the fixed number of rows of a file-browser page.
const PageRows = 500

// maxNameDisplay bounds the bytes of one name rendered in a row; the cut is
// shown explicitly by pathfmt (the exact bytes stay reachable by directory).
const maxNameDisplay = 2048

const (
	retryBusy    = "5"
	retryTimeout = "30"
)

// Searcher is *search.Service.
type Searcher interface {
	Search(ctx context.Context, p search.Params) (search.Result, error)
}

// Details reads one torrent row without touching its blob.
type Details interface {
	TorrentByID(ctx context.Context, id int32) (persistence.Torrent, error)
}

// Files is *filecache.Cache.
type Files interface {
	Get(ctx context.Context, id int32) (filecache.Result, error)
}

var (
	_ Searcher = (*search.Service)(nil)
	_ Details  = (*persistence.Reader)(nil)
	_ Files    = (*filecache.Cache)(nil)
)

// Deps are the handlers' collaborators.
type Deps struct {
	Search  Searcher
	Details Details
	Files   Files
}

// Handlers serves the W4 routes.
type Handlers struct {
	d   Deps
	srv *server.Server
	log *slog.Logger
}

// Install registers the W4 routes on srv's router (all Session routes; the
// list is a KindSearch route so it gets the search deadline).
func Install(srv *server.Server, d Deps) (*Handlers, error) {
	if d.Search == nil || d.Details == nil || d.Files == nil {
		return nil, errors.New("torrents: search, details and files are required")
	}
	h := &Handlers{d: d, srv: srv, log: srv.Logger()}
	rt := srv.Router()
	rt.Handle("GET /torrents", server.KindSearch, server.Session, http.HandlerFunc(h.list))
	rt.Handle("GET /torrents/{id}", server.KindRequest, server.Session, http.HandlerFunc(h.details))
	rt.Handle("GET /torrents/{id}/files", server.KindRequest, server.Session, http.HandlerFunc(h.files))
	return h, nil
}

// ---- shared view pieces -----------------------------------------------------

type chrome struct{ Username, CSRFToken string }

func (h *Handlers) chrome(r *http.Request) chrome {
	p, _ := auth.Current(r.Context())
	return chrome{Username: p.Username, CSRFToken: auth.SessionFormToken(r)}
}

func (h *Handlers) render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	if err := h.srv.Renderer().Render(w, status, page, data); err != nil {
		h.log.Error("render failed", "request_id", server.RequestID(r.Context()), "page", page)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

func (h *Handlers) notice(w http.ResponseWriter, r *http.Request, status int, retryAfter, heading, msg, href, text string) {
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	h.render(w, r, status, "message", ui.MessagePage{Heading: heading, Message: msg, LinkHref: href, LinkText: text})
}

// FormatSize renders n in IEC units with the exact byte count.
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB (%d bytes)", float64(n)/float64(div), "KMGTPE"[exp], n)
}

func formatSizeShort(n int64) string {
	s := FormatSize(n)
	if i := strings.Index(s, " ("); i >= 0 {
		return s[:i]
	}
	return s
}

func when(unix int64) (human, iso string) {
	t := time.Unix(unix, 0).UTC()
	return t.Format("2006-01-02 15:04 UTC"), t.Format(time.RFC3339)
}

// parseID accepts only canonical decimal int32 ≥ 1 ("007", "+1", "1e3" are 404).
func parseID(s string) (int32, bool) {
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != s {
		return 0, false
	}
	return int32(n), true
}

// ---- search -----------------------------------------------------------------

type option struct {
	Value, Label string
	Selected     bool
}

type listRow struct {
	Href                      string
	Name                      []pathfmt.Segment
	Size                      string
	Files                     int32
	Discovered, DiscoveredISO string
}

type listPage struct {
	chrome
	Q                                      string
	Query                                  bool // a usable query is active
	Lookup                                 bool
	Ignored                                int
	Error                                  string
	Rows                                   []listRow
	Prev, Next                             string
	SortOptions, OrderOptions, SizeOptions []option
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	q := r.URL.Query()
	page := listPage{chrome: h.chrome(r), Q: truncate(q.Get("q"), search.MaxQueryBytes)}

	p, err := search.ParseParams(q.Get("q"), q.Get("sort"), q.Get("order"), q.Get("size"), q.Get("cursor"))
	var res search.Result
	if err == nil {
		page.Query = !p.Query.Empty()
		page.Ignored = p.Query.Ignored
		page.setOptions(p)
		res, err = h.d.Search.Search(r.Context(), p)
	} else {
		page.setOptions(search.Params{PageSize: search.DefaultPageSize})
	}
	outcome := search.Outcome(err)
	defer func() {
		h.log.Info("search", "request_id", server.RequestID(r.Context()), "outcome", outcome,
			"terms", len(p.Query.Terms), "sort", p.Sort.String(), "rows", len(res.Items),
			"duration_ms", time.Since(start).Milliseconds())
	}()

	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		return // the client is gone: nothing to render
	case errors.Is(err, search.ErrInvalidQuery):
		page.Error = "The search text is not valid: " + invalidReason(err) + "."
		h.render(w, r, http.StatusBadRequest, "torrents", page)
		return
	case errors.Is(err, search.ErrInvalidCursor), errors.Is(err, search.ErrCursorExpired):
		page.Error = "This page link is no longer valid for the current search. Start again from the first page."
		h.render(w, r, http.StatusBadRequest, "torrents", page)
		return
	case errors.Is(err, search.ErrBusy):
		w.Header().Set("Retry-After", retryBusy)
		page.Error = "The server is busy. Please try again shortly."
		h.render(w, r, http.StatusServiceUnavailable, "torrents", page)
		return
	case errors.Is(err, search.ErrTimeout):
		w.Header().Set("Retry-After", retryTimeout)
		page.Error = "The search did not finish in the configured time and may be retried."
		h.render(w, r, http.StatusServiceUnavailable, "torrents", page)
		return
	case errors.Is(err, persistence.ErrStorageIO):
		w.Header().Set("Retry-After", retryBusy)
		h.log.Warn("search storage error", "request_id", server.RequestID(r.Context()))
		page.Error = "The torrent database is temporarily unavailable."
		h.render(w, r, http.StatusServiceUnavailable, "torrents", page)
		return
	default:
		h.log.Error("search failed", "request_id", server.RequestID(r.Context()), "error_type", fmt.Sprintf("%T", err))
		h.notice(w, r, http.StatusInternalServerError, "", "Search failed", "Something went wrong on our side.", "/torrents", "Back")
		return
	}

	page.Lookup = res.Lookup
	for _, t := range res.Items {
		hd, iso := when(t.DiscoveredOn)
		page.Rows = append(page.Rows, listRow{
			Href: "/torrents/" + strconv.FormatInt(int64(t.ID), 10),
			Name: pathfmt.Display(t.Name), Size: formatSizeShort(t.TotalSize), Files: t.FileCount,
			Discovered: hd, DiscoveredISO: iso,
		})
	}
	page.Next = listLink(p, q.Get("q"), res.Next)
	page.Prev = listLink(p, q.Get("q"), res.Prev)
	h.render(w, r, http.StatusOK, "torrents", page)
}

func invalidReason(err error) string {
	switch {
	case errors.Is(err, search.ErrQueryTooLong):
		return "it is too long"
	case errors.Is(err, search.ErrTooManyTerms):
		return "it has too many words"
	}
	return "it is not valid text"
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// listLink builds a page link that preserves every search parameter. The
// effective sort and direction are written explicitly so the link means the
// same thing as the page it was made on.
func listLink(p search.Params, rawQ, cursor string) string {
	if cursor == "" {
		return ""
	}
	v := url.Values{}
	if p.Query.InfoHash == nil {
		v.Set("sort", p.Sort.String())
		v.Set("order", map[bool]string{true: "desc", false: "asc"}[p.Desc])
	}
	if rawQ != "" {
		v.Set("q", rawQ)
	}
	v.Set("size", strconv.Itoa(p.PageSize))
	v.Set("cursor", cursor)
	return "/torrents?" + v.Encode()
}

func (pg *listPage) setOptions(p search.Params) {
	for _, s := range []search.Sort{search.SortDiscovered, search.SortSize, search.SortName} {
		pg.SortOptions = append(pg.SortOptions, option{s.String(), map[search.Sort]string{
			search.SortDiscovered: "Newest", search.SortSize: "Size", search.SortName: "Name"}[s], s == p.Sort})
	}
	pg.OrderOptions = []option{{"desc", "Descending", p.Desc}, {"asc", "Ascending", !p.Desc}}
	for _, n := range search.PageSizes {
		pg.SizeOptions = append(pg.SizeOptions, option{strconv.Itoa(n), strconv.Itoa(n) + " per page", n == p.PageSize})
	}
}

// ---- details ----------------------------------------------------------------

type detailPage struct {
	chrome
	Name                      []pathfmt.Segment
	Size, InfoHash            string
	Files                     int32
	Discovered, DiscoveredISO string
	Magnet                    template.URL // built here from hex and a query-escaped name
	FilesHref                 string
}

func (h *Handlers) details(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		h.srv.ErrorPage(w, r, http.StatusNotFound)
		return
	}
	t, err := h.d.Details.TorrentByID(r.Context(), id)
	if err != nil {
		h.fail(w, r, id, err)
		return
	}
	hd, iso := when(t.DiscoveredOn)
	hash := fmt.Sprintf("%x", t.InfoHash)
	h.render(w, r, http.StatusOK, "torrent", detailPage{
		chrome: h.chrome(r), Name: pathfmt.Display(t.Name), Size: FormatSize(t.TotalSize), InfoHash: hash,
		Files: t.FileCount, Discovered: hd, DiscoveredISO: iso,
		Magnet:    template.URL("magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(t.Name)), // #nosec: hex and escaped text only
		FilesHref: "/torrents/" + strconv.FormatInt(int64(id), 10) + "/files",
	})
}

// ---- files ------------------------------------------------------------------

type crumb struct {
	Name []pathfmt.Segment
	Href string // "" for the current directory
}

type fileRow struct {
	Name  []pathfmt.Segment
	Dir   bool
	Href  string
	Size  string
	Files uint32
	Dup   bool
}

type filesPage struct {
	chrome
	Name                  []pathfmt.Segment
	TorrentHref, RootHref string
	Crumbs                []crumb
	DirFiles              uint32
	DirSize               string
	Rows                  []fileRow
	Total, First, Last    int
	Prev, Next            string
}

func filesHref(id int32, dir string, from int) string {
	u := "/torrents/" + strconv.FormatInt(int64(id), 10) + "/files"
	var q []string
	if dir != "" {
		q = append(q, pathfmt.DirQuery(dir))
	}
	if from > 0 {
		q = append(q, "from="+strconv.Itoa(from))
	}
	if len(q) > 0 {
		u += "?" + strings.Join(q, "&")
	}
	return u
}

func (h *Handlers) files(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		h.srv.ErrorPage(w, r, http.StatusNotFound)
		return
	}
	// r.URL.Query() has already percent-decoded the value exactly once; it is
	// the raw directory prefix and must not be decoded again.
	q := r.URL.Query()
	dir := q.Get(pathfmt.DirParam)
	from := 0
	if s := q.Get("from"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || strconv.Itoa(n) != s {
			h.notice(w, r, http.StatusBadRequest, "", "Invalid page", "The page position is not valid.", filesHref(id, dir, 0), "Back to the directory")
			return
		}
		from = n
	}

	res, err := h.d.Files.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, id, err)
		return
	}
	d, found := res.Index.Lookup(dir)
	if !found {
		h.notice(w, r, http.StatusNotFound, "", "Directory not found", "That directory does not exist in this torrent.", filesHref(id, "", 0), "Back to the root")
		return
	}
	info := res.Index.Info(d)
	total := info.Total()
	if from > 0 && from >= total {
		h.notice(w, r, http.StatusBadRequest, "", "Invalid page", "The page position is beyond the end of the directory.", filesHref(id, dir, 0), "Back to the directory")
		return
	}

	pg := filesPage{
		chrome: h.chrome(r), Name: pathfmt.Display(res.Torrent.Name),
		TorrentHref: "/torrents/" + strconv.FormatInt(int64(id), 10), RootHref: filesHref(id, "", 0),
		DirFiles: info.Files, DirSize: FormatSize(info.Size), Total: total,
	}
	if dir != "" {
		comps := strings.Split(dir[:len(dir)-1], "/")
		for i, c := range comps {
			cr := crumb{Name: pathfmt.Component(c, maxNameDisplay)}
			if i < len(comps)-1 {
				cr.Href = filesHref(id, strings.Join(comps[:i+1], "/")+"/", 0)
			}
			pg.Crumbs = append(pg.Crumbs, cr)
		}
	}
	entries := res.Index.Page(d, from, PageRows, make([]filetree.Entry, 0, PageRows))
	for _, e := range entries {
		row := fileRow{Name: pathfmt.Component(e.Name, maxNameDisplay), Dir: e.Dir, Dup: e.Dup}
		if e.Dir {
			row.Href = filesHref(id, dir+e.Name+"/", 0)
			row.Files = e.Files
		}
		row.Size = formatSizeShort(e.Size)
		pg.Rows = append(pg.Rows, row)
	}
	if len(entries) > 0 {
		pg.First, pg.Last = from+1, from+len(entries)
	}
	if from > 0 {
		pg.Prev = filesHref(id, dir, max(0, from-PageRows))
	}
	if from+len(entries) < total {
		pg.Next = filesHref(id, dir, from+len(entries))
	}
	h.render(w, r, http.StatusOK, "files", pg)
}

// ---- error mapping ----------------------------------------------------------

// fail maps a read-path error to a distinguishable response. Integrity and
// limit failures are 500 with a safe structured log; saturation and timeouts
// are 503 with Retry-After; a vanished client gets nothing.
func (h *Handlers) fail(w http.ResponseWriter, r *http.Request, id int32, err error) {
	rid := server.RequestID(r.Context())
	var le *codec.LimitError
	switch {
	case errors.Is(err, context.Canceled):
		return
	case errors.Is(err, persistence.ErrTorrentNotFound):
		h.srv.ErrorPage(w, r, http.StatusNotFound)
	case errors.Is(err, gate.ErrBusy):
		h.notice(w, r, http.StatusServiceUnavailable, retryBusy, "The server is busy",
			"Too many large file lists are being prepared. Please try again shortly.", r.URL.RequestURI(), "Try again")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, persistence.ErrStatementTimeout),
		errors.Is(err, persistence.ErrBudgetExhausted):
		h.notice(w, r, http.StatusServiceUnavailable, retryBusy, "The request timed out",
			"The request did not finish in time and may be retried.", r.URL.RequestURI(), "Try again")
	case errors.Is(err, persistence.ErrBlobChanged), errors.Is(err, filecache.ErrClosed), errors.Is(err, persistence.ErrStorageIO):
		h.log.Warn("file list temporarily unavailable", "request_id", rid, "torrent_id", id, "error_type", fmt.Sprintf("%T", err))
		h.notice(w, r, http.StatusServiceUnavailable, retryBusy, "Temporarily unavailable",
			"The file list is temporarily unavailable. Please try again.", r.URL.RequestURI(), "Try again")
	case errors.Is(err, persistence.ErrIntegrityMissingBlob):
		h.log.Error("file list blob missing", "request_id", rid, "torrent_id", id, "category", "missing_blob")
		h.notice(w, r, http.StatusInternalServerError, "", "File list unavailable",
			"The file list of this torrent is missing from the database.", "/torrents/"+strconv.FormatInt(int64(id), 10), "Back")
	case errors.Is(err, filecache.ErrTooLargeToIndex):
		h.notice(w, r, http.StatusInternalServerError, "", "File list too large",
			"This file list is too large to browse.", "/torrents/"+strconv.FormatInt(int64(id), 10), "Back")
	case errors.Is(err, persistence.ErrCorruptBlob):
		attrs := []any{"request_id", rid, "torrent_id", id, "category", "corrupt_blob"}
		if errors.As(err, &le) {
			attrs = append(attrs, "limit", le.Limit, "actual", le.Actual, "allowed", le.Allowed)
		}
		h.log.Error("file list blob rejected", attrs...)
		h.notice(w, r, http.StatusInternalServerError, "", "File list unavailable",
			"The stored file list of this torrent is damaged or exceeds the supported limits.", "/torrents/"+strconv.FormatInt(int64(id), 10), "Back")
	default:
		h.log.Error("request failed", "request_id", rid, "torrent_id", id, "error_type", fmt.Sprintf("%T", err))
		h.srv.ErrorPage(w, r, http.StatusInternalServerError)
	}
}
