// Package apiv2 serves the public REST API under /api/v2 (docs/specs/web-v1.md
// §7): torrent search, details and the two file-view operations, authenticated
// by a bearer API token only.
//
// It is a thin adapter over the same services the web UI uses: the search
// service (keyset cursors, SQL, timeouts), the shared file-view service (via
// torrents.FileAPI: eligibility, limits, positional paths, serialization) and
// the tracker snapshot. It contains no SQL and no second tree implementation.
package apiv2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/problem"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/search"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/torrents"
	"github.com/skobkin/magnetico-web/internal/web/trackers"
	"github.com/skobkin/magnetico-web/persistence"
)

// Limits of the list endpoint (§7.2).
const (
	DefaultLimit = 50
	MaxLimit     = 100
)

// Config wires the API.
type Config struct {
	Auth     Authenticator
	Search   torrents.Searcher
	Details  torrents.Details
	View     torrents.View
	Trackers torrents.TrackerList // optional

	// Limiters override the defaults (tests).
	FailLimiter, UseLimiter *ratelimit.Limiter
}

// Handlers is the installed API.
type Handlers struct {
	cfg   Config
	srv   *server.Server
	log   *slog.Logger
	files *torrents.FileAPI
}

// APIPrefix is the base path of every route.
const APIPrefix = "/api/v2"

// Install registers the routes (all APIToken class) and the bearer guard.
func Install(srv *server.Server, cfg Config) (*Handlers, error) {
	if cfg.Auth == nil || cfg.Search == nil || cfg.Details == nil || cfg.View == nil {
		return nil, errors.New("apiv2: authenticator, search, details and view are required")
	}
	g := &guard{auth: cfg.Auth, log: srv.Logger(), failLim: cfg.FailLimiter, useLim: cfg.UseLimiter}
	var err error
	if g.failLim == nil {
		if g.failLim, err = ratelimit.New(ratelimit.Config{Burst: failBurst, Interval: failInterval, MaxKeys: limiterKeys}); err != nil {
			return nil, err
		}
	}
	if g.useLim == nil {
		if g.useLim, err = ratelimit.New(ratelimit.Config{Burst: useBurst, Interval: useInterval, MaxKeys: limiterKeys}); err != nil {
			return nil, err
		}
	}
	h := &Handlers{cfg: cfg, srv: srv, log: srv.Logger()}
	h.files = &torrents.FileAPI{Details: cfg.Details, View: cfg.View, Log: srv.Logger(),
		DirURL: func(id int32) string { return APIPrefix + "/torrents/" + strconv.FormatInt(int64(id), 10) + "/files" }}

	rt := srv.Router()
	rt.SetGuard(server.APIToken, g.handle)
	rt.Handle("GET "+APIPrefix+"/torrents", server.KindSearch, server.APIToken, http.HandlerFunc(h.list))
	rt.Handle("GET "+APIPrefix+"/torrents/{id}", server.KindRequest, server.APIToken, http.HandlerFunc(h.details))
	rt.Handle("GET "+APIPrefix+"/torrents/{id}/files/tree", server.KindRequest, server.APIToken, http.HandlerFunc(h.files.Tree))
	rt.Handle("GET "+APIPrefix+"/torrents/{id}/files", server.KindRequest, server.APIToken, http.HandlerFunc(h.files.Dir))
	// Unknown paths under the prefix answer problem+json (after authentication,
	// so an anonymous caller cannot tell real routes from unknown ones).
	rt.Handle("GET "+APIPrefix+"/", server.KindRequest, server.APIToken, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, problem.Problem{Slug: problem.NotFound, Title: "Not found", Status: http.StatusNotFound, Detail: "No such endpoint."})
	}))
	return h, nil
}

// ---- JSON documents --------------------------------------------------------

type torrentJSON struct {
	ID           int32  `json:"id"`
	InfoHash     string `json:"info_hash"`
	Name         string `json:"name"`
	TotalSize    int64  `json:"total_size"`
	FileCount    int32  `json:"file_count"`
	DiscoveredAt string `json:"discovered_at"`
}

type detailsJSON struct {
	torrentJSON
	MagnetURI         string `json:"magnet_uri"`
	FullTreeAvailable bool   `json:"full_tree_available"`
}

type pageJSON struct {
	Items      []torrentJSON `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

func toJSON(t persistence.Torrent) torrentJSON {
	return torrentJSON{
		ID: t.ID, InfoHash: trackers.InfoHashHex(t.InfoHash), Name: t.Name, TotalSize: t.TotalSize, FileCount: t.FileCount,
		DiscoveredAt: time.Unix(t.DiscoveredOn, 0).UTC().Format(time.RFC3339),
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		problem.Write(w, problem.Problem{Slug: problem.InternalError, Title: "Internal error", Status: http.StatusInternalServerError})
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}

func invalid(w http.ResponseWriter, detail string) {
	problem.Write(w, problem.Problem{Slug: problem.InvalidParameter, Title: "Invalid parameter", Status: http.StatusBadRequest, Detail: detail})
}

// ---- GET /torrents -----------------------------------------------------------

// parseList validates the query string strictly: a repeated parameter, a
// malformed limit, an invalid info hash, or info_hash together with another
// search parameter is a 400. Unknown sort/order values keep the documented
// fallback (§6.3.1) and unknown parameters are ignored.
func parseList(rawQuery string) (search.Params, string, bool) {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return search.Params{}, "The query string is not valid.", false
	}
	for _, k := range []string{"q", "sort", "order", "limit", "cursor", "info_hash"} {
		if len(q[k]) > 1 {
			return search.Params{}, "The parameter " + k + " may be given only once.", false
		}
	}
	limit := DefaultLimit
	if v := q.Get("limit"); q.Has("limit") {
		n, err := strconv.Atoi(v)
		if err != nil || strconv.Itoa(n) != v || n < 1 || n > MaxLimit {
			return search.Params{}, fmt.Sprintf("limit must be an integer from 1 to %d.", MaxLimit), false
		}
		limit = n
	}
	text := q.Get("q")
	if q.Has("info_hash") {
		if q.Has("q") || q.Has("sort") || q.Has("order") || q.Has("cursor") {
			return search.Params{}, "info_hash cannot be combined with q, sort, order or cursor.", false
		}
		text = q.Get("info_hash")
		if parsed, err := search.ParseQuery(text); err != nil || parsed.InfoHash == nil {
			return search.Params{}, "info_hash must be exactly 40 hexadecimal characters.", false
		}
	}
	p, err := search.ParseParams(text, q.Get("sort"), q.Get("order"), "", q.Get("cursor"))
	if err != nil {
		return search.Params{}, "The search text is not valid.", false
	}
	p.PageSize = limit
	return p, "", true
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	p, detail, ok := parseList(r.URL.RawQuery)
	if !ok {
		invalid(w, detail)
		return
	}
	res, err := h.cfg.Search.Search(r.Context(), p)
	outcome := search.Outcome(err)
	h.log.Info("api search", "request_id", server.RequestID(r.Context()), "outcome", outcome,
		"terms", len(p.Query.Terms), "sort", p.Sort.String(), "rows", len(res.Items), "duration_ms", time.Since(start).Milliseconds())
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		return
	case errors.Is(err, search.ErrInvalidQuery):
		invalid(w, "The search text is not valid.")
		return
	case errors.Is(err, search.ErrInvalidCursor), errors.Is(err, search.ErrCursorExpired):
		invalid(w, "The cursor is not valid for this search. Start again without a cursor.")
		return
	case errors.Is(err, search.ErrBusy):
		problem.Write(w, problem.Problem{Slug: problem.TemporarilyBusy, Title: "The server is busy",
			Status: http.StatusServiceUnavailable, Detail: "Please try again shortly.", RetryAfterSeconds: 5})
		return
	case errors.Is(err, search.ErrTimeout):
		problem.Write(w, problem.Problem{Slug: problem.TemporarilyBusy, Title: "The search timed out",
			Status: http.StatusServiceUnavailable, Detail: "The search did not finish in the configured time and may be retried.", RetryAfterSeconds: 30})
		return
	case errors.Is(err, persistence.ErrStorageIO):
		h.log.Warn("api search storage error", "request_id", server.RequestID(r.Context()))
		problem.Write(w, problem.Problem{Slug: problem.TemporarilyBusy, Title: "Temporarily unavailable",
			Status: http.StatusServiceUnavailable, Detail: "Please try again shortly.", RetryAfterSeconds: 5})
		return
	default:
		h.log.Error("api search failed", "request_id", server.RequestID(r.Context()), "error_type", fmt.Sprintf("%T", err))
		problem.Write(w, problem.Problem{Slug: problem.InternalError, Title: "Internal error", Status: http.StatusInternalServerError,
			Detail: "Something went wrong on our side."})
		return
	}
	out := pageJSON{Items: make([]torrentJSON, 0, len(res.Items))}
	for _, t := range res.Items {
		out.Items = append(out.Items, toJSON(t))
	}
	if res.Next != "" {
		out.NextCursor = &res.Next
	}
	writeJSON(w, out)
}

// ---- GET /torrents/{id} ------------------------------------------------------

func (h *Handlers) details(w http.ResponseWriter, r *http.Request) {
	id, ok := torrents.ParseID(r.PathValue("id"))
	if !ok {
		problem.Write(w, problem.Problem{Slug: problem.NotFound, Title: "Not found", Status: http.StatusNotFound, Detail: "No such torrent."})
		return
	}
	t, err := h.cfg.Details.TorrentByID(r.Context(), id)
	if err != nil {
		h.files.Fail(w, r, id, err)
		return
	}
	// full_tree_available is the shared service's stage-1 decision. A failed
	// peek yields false (use /files), as on the details page.
	dec, derr := h.cfg.View.Decide(r.Context(), t)
	if derr != nil {
		if errors.Is(derr, context.Canceled) {
			return
		}
		h.log.Warn("file view decision unavailable", "request_id", server.RequestID(r.Context()),
			"torrent_id", id, "error_type", fmt.Sprintf("%T", derr))
	}
	var tr []string
	if h.cfg.Trackers != nil {
		tr = h.cfg.Trackers.URLs()
	}
	name := t.Name
	writeJSON(w, detailsJSON{
		torrentJSON:       toJSON(t),
		MagnetURI:         trackers.MagnetURI(trackers.InfoHashHex(t.InfoHash), &name, tr),
		FullTreeAvailable: derr == nil && dec.FullTreeAvailable(),
	})
}
