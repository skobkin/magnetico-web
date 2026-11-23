// Package torrents holds the web HTTP handlers: torrent search and listing,
// torrent details and the two session-authenticated JSON file-view endpoints
// that feed the details page's file browser (docs/wip/07E-web-search-details-plan.md
// §4, docs/wip/07F-web-ui-report.md). Handlers translate between HTTP and the
// search service and the shared file-view service; they hold no SQL and make
// no decisions about memory.
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
	"github.com/skobkin/magnetico-web/internal/web/fileview"
	"github.com/skobkin/magnetico-web/internal/web/gate"
	"github.com/skobkin/magnetico-web/internal/web/pathfmt"
	"github.com/skobkin/magnetico-web/internal/web/problem"
	"github.com/skobkin/magnetico-web/internal/web/search"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/ui"
	"github.com/skobkin/magnetico-web/persistence"
)

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

// View is *fileview.Service.
type View interface {
	Decide(ctx context.Context, t persistence.Torrent) (fileview.Decision, error)
	Tree(ctx context.Context, t persistence.Torrent) (*fileview.Body, error)
	Dir(ctx context.Context, id int32, path string, pg fileview.Page) (*fileview.Body, error)
}

var (
	_ Searcher = (*search.Service)(nil)
	_ Details  = (*persistence.Reader)(nil)
	_ View     = (*fileview.Service)(nil)
)

// Deps are the handlers' collaborators.
type Deps struct {
	Search  Searcher
	Details Details
	View    View
}

// Handlers serves the W4 routes.
type Handlers struct {
	d   Deps
	srv *server.Server
	log *slog.Logger
}

// Install registers the routes on srv's router: the HTML pages are Session
// routes (the list is a KindSearch route so it gets the search deadline) and
// the file-view endpoints are SessionJSON routes (401 problem+json when
// anonymous).
func Install(srv *server.Server, d Deps) (*Handlers, error) {
	if d.Search == nil || d.Details == nil || d.View == nil {
		return nil, errors.New("torrents: search, details and view are required")
	}
	h := &Handlers{d: d, srv: srv, log: srv.Logger()}
	rt := srv.Router()
	rt.Handle("GET /torrents", server.KindSearch, server.Session, http.HandlerFunc(h.list))
	rt.Handle("GET /torrents/{id}", server.KindRequest, server.Session, http.HandlerFunc(h.details))
	rt.Handle("GET /torrents/{id}/files/tree", server.KindRequest, server.SessionJSON, http.HandlerFunc(h.fileTree))
	rt.Handle("GET /torrents/{id}/files", server.KindRequest, server.SessionJSON, http.HandlerFunc(h.fileDir))
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
	Magnet                    template.URL // tracker-less magnet link: hex hash and query-escaped name only
	Name                      []pathfmt.Segment
	Size                      string
	Files                     int32
	Discovered, DiscoveredISO string
}

type listPage struct {
	chrome
	Q                      string
	Query                  bool // a usable query is active
	Lookup                 bool
	Ignored                int
	Error                  string
	Rows                   []listRow
	Prev, Next             string
	ByOptions, SizeOptions []option
	Heads                  []head
}

// head is one column header of the results table. Sortable columns are plain
// links (so sorting works without JavaScript); the link flips the direction
// of the active column and starts the others in their natural direction.
type head struct {
	Label, Class    string
	Href            string // "" for a column that cannot be sorted now
	AriaSort, Arrow string
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	q := r.URL.Query()
	page := listPage{chrome: h.chrome(r), Q: truncate(q.Get("q"), search.MaxQueryBytes)}

	sortParam, orderParam := q.Get("sort"), q.Get("order")
	if by := q.Get("by"); by != "" { // the view-options form sends "sort:order"
		if sp, op, found := strings.Cut(by, ":"); found {
			sortParam, orderParam = sp, op
		}
	}
	p, err := search.ParseParams(q.Get("q"), sortParam, orderParam, q.Get("size"), q.Get("cursor"))
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
		hash := fmt.Sprintf("%x", t.InfoHash)
		page.Rows = append(page.Rows, listRow{
			Href:   "/torrents/" + strconv.FormatInt(int64(t.ID), 10),
			Magnet: template.URL("magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(t.Name)), // #nosec: hex and escaped text only
			Name:   pathfmt.Display(t.Name), Size: formatSizeShort(t.TotalSize), Files: t.FileCount,
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
	type by struct {
		sort  search.Sort
		desc  bool
		label string
	}
	opts := []by{
		{search.SortDiscovered, true, "Newest first"}, {search.SortDiscovered, false, "Oldest first"},
		{search.SortSize, true, "Largest first"}, {search.SortSize, false, "Smallest first"},
	}
	if len(p.Query.Terms) > 0 {
		opts = append(opts, by{search.SortName, false, "Name A–Z"}, by{search.SortName, true, "Name Z–A"})
	}
	for _, o := range opts {
		pg.ByOptions = append(pg.ByOptions, option{o.sort.String() + ":" + map[bool]string{true: "desc", false: "asc"}[o.desc], o.label,
			o.sort == p.Sort && o.desc == p.Desc})
	}
	for _, n := range search.PageSizes {
		pg.SizeOptions = append(pg.SizeOptions, option{strconv.Itoa(n), strconv.Itoa(n) + " per page", n == p.PageSize})
	}
	pg.Heads = columnHeads(p, pg.Q)
}

func columnHeads(p search.Params, rawQ string) []head {
	lookup := p.Query.InfoHash != nil
	col := func(label, class string, sort search.Sort, enabled bool) head {
		h := head{Label: label, Class: class}
		if lookup || !enabled {
			return h
		}
		desc := sort != search.SortName // natural direction of a column that is not active
		if p.Sort == sort {
			h.AriaSort = map[bool]string{true: "descending", false: "ascending"}[p.Desc]
			h.Arrow = map[bool]string{true: "↓", false: "↑"}[p.Desc]
			desc = !p.Desc
		}
		v := url.Values{"sort": {sort.String()}, "order": {map[bool]string{true: "desc", false: "asc"}[desc]},
			"size": {strconv.Itoa(p.PageSize)}}
		if rawQ != "" {
			v.Set("q", rawQ)
		}
		h.Href = "/torrents?" + v.Encode()
		return h
	}
	return []head{
		col("Name", "col-name", search.SortName, len(p.Query.Terms) > 0),
		col("Size", "col-size", search.SortSize, true),
		{Label: "Files", Class: "col-files"},
		col("Discovered", "col-discovered", search.SortDiscovered, true),
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
	// File browser: the backend picks the mode; the page only carries it.
	Mode    fileview.Mode
	TreeURL string
	DirURL  string
}

func torrentPath(id int32) string { return "/torrents/" + strconv.FormatInt(int64(id), 10) }

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
	// The mode decision needs at most a 12-byte header peek and never blocks
	// the page: when it fails the browser starts in the always-valid
	// directory mode and the file view reports the real problem itself.
	dec, derr := h.d.View.Decide(r.Context(), t)
	if derr != nil && !errors.Is(derr, context.Canceled) {
		h.log.Warn("file view decision unavailable", "request_id", server.RequestID(r.Context()),
			"torrent_id", id, "error_type", fmt.Sprintf("%T", derr))
	}
	hd, iso := when(t.DiscoveredOn)
	hash := fmt.Sprintf("%x", t.InfoHash)
	h.render(w, r, http.StatusOK, "torrent", detailPage{
		chrome: h.chrome(r), Name: pathfmt.Display(t.Name), Size: FormatSize(t.TotalSize), InfoHash: hash,
		Files: t.FileCount, Discovered: hd, DiscoveredISO: iso,
		Magnet:  template.URL("magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(t.Name)), // #nosec: hex and escaped text only
		Mode:    dec.Mode,
		TreeURL: torrentPath(id) + "/files/tree",
		DirURL:  torrentPath(id) + "/files",
	})
}

// ---- file view JSON ----------------------------------------------------------

func writeBody(w http.ResponseWriter, body *fileview.Body) {
	defer body.Release()
	b := body.Bytes()
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}

// fileTree serves GET /torrents/{id}/files/tree.
func (h *Handlers) fileTree(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		jsonProblem(w, problem.NotFound, "Not found", http.StatusNotFound, "No such torrent.")
		return
	}
	t, err := h.d.Details.TorrentByID(r.Context(), id)
	if err != nil {
		h.failJSON(w, r, id, err)
		return
	}
	body, err := h.d.View.Tree(r.Context(), t)
	if err != nil {
		h.logFileView(r, id, "tree", err, 0)
		h.failJSON(w, r, id, err)
		return
	}
	h.logFileView(r, id, "tree", nil, len(body.Bytes()))
	writeBody(w, body)
}

// fileDir serves GET /torrents/{id}/files?dir=&offset=&limit=.
func (h *Handlers) fileDir(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		jsonProblem(w, problem.NotFound, "Not found", http.StatusNotFound, "No such torrent.")
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q["dir"]) > 1 || len(q["offset"]) > 1 || len(q["limit"]) > 1 {
		jsonProblem(w, problem.InvalidParameter, "Invalid parameter", http.StatusBadRequest, "The query string is not valid.")
		return
	}
	pg, err := fileview.ParsePage(q.Get("offset"), q.Get("limit"))
	if err != nil {
		h.failJSON(w, r, id, err)
		return
	}
	body, err := h.d.View.Dir(r.Context(), id, q.Get("dir"), pg)
	if err != nil {
		h.logFileView(r, id, "dir", err, 0)
		h.failJSON(w, r, id, err)
		return
	}
	h.logFileView(r, id, "dir", nil, len(body.Bytes()))
	writeBody(w, body)
}

func (h *Handlers) logFileView(r *http.Request, id int32, op string, err error, n int) {
	outcome := "ok"
	if err != nil {
		outcome = fmt.Sprintf("%T", err)
	}
	h.log.Info("file view", "request_id", server.RequestID(r.Context()), "torrent_id", id, "op", op, "outcome", outcome, "bytes", n)
}

func jsonProblem(w http.ResponseWriter, slug, title string, status int, detail string) {
	problem.Write(w, problem.Problem{Slug: slug, Title: title, Status: status, Detail: detail})
}

// failJSON maps a read-path error to a problem+json response. It mirrors fail
// (same categories, same log safety) for the JSON endpoints.
func (h *Handlers) failJSON(w http.ResponseWriter, r *http.Request, id int32, err error) {
	rid := server.RequestID(r.Context())
	var le *codec.LimitError
	var ue *fileview.UnavailableError
	busy := func(title string) {
		problem.Write(w, problem.Problem{Slug: problem.TemporarilyBusy, Title: title,
			Status: http.StatusServiceUnavailable, Detail: "Please try again shortly.", RetryAfterSeconds: 5})
	}
	switch {
	case errors.Is(err, context.Canceled):
		return
	case errors.Is(err, persistence.ErrTorrentNotFound):
		jsonProblem(w, problem.NotFound, "Not found", http.StatusNotFound, "No such torrent.")
	case errors.Is(err, fileview.ErrBadPath), errors.Is(err, fileview.ErrBadPage):
		jsonProblem(w, problem.InvalidParameter, "Invalid parameter", http.StatusBadRequest,
			"The directory path, offset or limit is not valid.")
	case errors.Is(err, fileview.ErrNoSuchDir):
		jsonProblem(w, problem.NotFound, "Not found", http.StatusNotFound, "No such directory in this torrent.")
	case errors.As(err, &ue):
		problem.Write(w, problem.Problem{Slug: problem.FullTreeUnavailable, Title: "Full file tree unavailable",
			Status:    http.StatusUnprocessableEntity,
			Detail:    "The complete file tree is not available for this torrent. Browse it directory by directory instead.",
			Alternate: torrentPath(id) + "/files"})
	case errors.Is(err, filetree.ErrIndexComplexity):
		h.log.Warn("file hierarchy exceeds the web index limits", "request_id", rid, "torrent_id", id)
		jsonProblem(w, problem.IndexTooComplex, "File structure too complex", http.StatusUnprocessableEntity,
			"The file structure of this torrent exceeds this server's browsing limits.")
	case errors.Is(err, gate.ErrBusy):
		busy("The server is busy")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, persistence.ErrStatementTimeout),
		errors.Is(err, persistence.ErrBudgetExhausted):
		busy("The request timed out")
	case errors.Is(err, persistence.ErrBlobChanged), errors.Is(err, filecache.ErrClosed), errors.Is(err, persistence.ErrStorageIO):
		h.log.Warn("file list temporarily unavailable", "request_id", rid, "torrent_id", id, "error_type", fmt.Sprintf("%T", err))
		busy("Temporarily unavailable")
	case errors.Is(err, persistence.ErrIntegrityMissingBlob):
		h.log.Error("file list blob missing", "request_id", rid, "torrent_id", id, "category", "missing_blob")
		jsonProblem(w, problem.InternalError, "File list unavailable", http.StatusInternalServerError,
			"The file list of this torrent is missing from the database.")
	case errors.Is(err, filecache.ErrTooLargeToIndex):
		jsonProblem(w, problem.InternalError, "File list too large", http.StatusInternalServerError,
			"This file list is too large to browse.")
	case errors.Is(err, persistence.ErrCorruptBlob):
		attrs := []any{"request_id", rid, "torrent_id", id, "category", "corrupt_blob"}
		if errors.As(err, &le) {
			attrs = append(attrs, "limit", le.Limit, "actual", le.Actual, "allowed", le.Allowed)
		}
		h.log.Error("file list blob rejected", attrs...)
		jsonProblem(w, problem.InternalError, "File list unavailable", http.StatusInternalServerError,
			"The stored file list of this torrent is damaged or exceeds the supported limits.")
	default:
		h.log.Error("request failed", "request_id", rid, "torrent_id", id, "error_type", fmt.Sprintf("%T", err))
		jsonProblem(w, problem.InternalError, "Internal error", http.StatusInternalServerError, "Something went wrong on our side.")
	}
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
