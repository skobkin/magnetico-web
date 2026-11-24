package torrents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/filecache"
	"github.com/skobkin/magnetico-web/internal/web/filetree"
	"github.com/skobkin/magnetico-web/internal/web/fileview"
	"github.com/skobkin/magnetico-web/internal/web/gate"
	"github.com/skobkin/magnetico-web/internal/web/problem"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/persistence"
)

// FileAPI is the single JSON adapter over the shared file-view service
// (docs/specs/web-v1.md §6.4.7, §7.5). The session-authenticated UI endpoints
// and the bearer-authenticated /api/v2 endpoints are thin authentication
// wrappers around the same value of this type, so request parsing, limits,
// error mapping and logging cannot drift apart.
type FileAPI struct {
	Details Details
	View    View
	Log     *slog.Logger
	// DirURL is the URL of the paginated file view of a torrent, offered as the
	// alternative in the full-tree-unavailable problem.
	DirURL func(id int32) string
}

// ParseID accepts only a canonical positive int32 (no sign, no leading zeros).
func ParseID(s string) (int32, bool) { return parseID(s) }

func writeBody(w http.ResponseWriter, body *fileview.Body) {
	defer body.Release()
	b := body.Bytes()
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b) //nolint:gosec // G705: a JSON document encoded by the fileview package, served as application/json (the server adds nosniff)
}

// Tree serves the complete hierarchy of the torrent in {id}, only when it is
// eligible.
func (f *FileAPI) Tree(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		jsonProblem(w, problem.NotFound, "Not found", http.StatusNotFound, "No such torrent.")
		return
	}
	t, err := f.Details.TorrentByID(r.Context(), id)
	if err != nil {
		f.fail(w, r, id, err)
		return
	}
	body, err := f.View.Tree(r.Context(), t)
	if err != nil {
		f.logOp(r, id, "tree", err, 0)
		f.fail(w, r, id, err)
		return
	}
	f.logOp(r, id, "tree", nil, len(body.Bytes()))
	writeBody(w, body)
}

// Dir serves one page of one directory (?dir=&offset=&limit=) of any torrent.
func (f *FileAPI) Dir(w http.ResponseWriter, r *http.Request) {
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
		f.fail(w, r, id, err)
		return
	}
	body, err := f.View.Dir(r.Context(), id, q.Get("dir"), pg)
	if err != nil {
		f.logOp(r, id, "dir", err, 0)
		f.fail(w, r, id, err)
		return
	}
	f.logOp(r, id, "dir", nil, len(body.Bytes()))
	writeBody(w, body)
}

func (f *FileAPI) logOp(r *http.Request, id int32, op string, err error, n int) {
	outcome := "ok"
	if err != nil {
		outcome = fmt.Sprintf("%T", err)
	}
	f.Log.Info("file view", "request_id", server.RequestID(r.Context()), "torrent_id", id, "op", op, "outcome", outcome, "bytes", n)
}

func jsonProblem(w http.ResponseWriter, slug, title string, status int, detail string) {
	problem.Write(w, problem.Problem{Slug: slug, Title: title, Status: status, Detail: detail})
}

// Fail maps a read-path error of the torrent or file-view services to a
// problem+json response (§6.4.13). Bodies never carry names, paths or internal
// detail; logs carry only the request ID, torrent ID and a category.
func (f *FileAPI) Fail(w http.ResponseWriter, r *http.Request, id int32, err error) {
	f.fail(w, r, id, err)
}

func (f *FileAPI) fail(w http.ResponseWriter, r *http.Request, id int32, err error) {
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
			Alternate: f.DirURL(id)})
	case errors.Is(err, filetree.ErrIndexComplexity):
		f.Log.Warn("file hierarchy exceeds the web index limits", "request_id", rid, "torrent_id", id)
		jsonProblem(w, problem.IndexTooComplex, "File structure too complex", http.StatusUnprocessableEntity,
			"The file structure of this torrent exceeds this server's browsing limits.")
	case errors.Is(err, gate.ErrBusy):
		busy("The server is busy")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, persistence.ErrStatementTimeout),
		errors.Is(err, persistence.ErrBudgetExhausted):
		busy("The request timed out")
	case errors.Is(err, persistence.ErrStorageIO):
		// The database itself is failing: not backpressure, so it is logged (and
		// access-logged) at ERROR although the client gets a retryable 503.
		server.MarkFault(r.Context())
		f.Log.Error("file list storage error", "request_id", rid, "torrent_id", id, "error_type", fmt.Sprintf("%T", err))
		busy("Temporarily unavailable")
	case errors.Is(err, persistence.ErrBlobChanged), errors.Is(err, filecache.ErrClosed):
		f.Log.Warn("file list temporarily unavailable", "request_id", rid, "torrent_id", id, "error_type", fmt.Sprintf("%T", err))
		busy("Temporarily unavailable")
	case errors.Is(err, persistence.ErrIntegrityMissingBlob):
		f.Log.Error("file list blob missing", "request_id", rid, "torrent_id", id, "category", "missing_blob")
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
		f.Log.Error("file list blob rejected", attrs...)
		jsonProblem(w, problem.InternalError, "File list unavailable", http.StatusInternalServerError,
			"The stored file list of this torrent is damaged or exceeds the supported limits.")
	default:
		f.Log.Error("request failed", "request_id", rid, "torrent_id", id, "error_type", fmt.Sprintf("%T", err))
		jsonProblem(w, problem.InternalError, "Internal error", http.StatusInternalServerError, "Something went wrong on our side.")
	}
}
