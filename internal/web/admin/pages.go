package admin

import (
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/admin/data"
	"github.com/skobkin/magnetico-web/internal/web/admin/stats"
	"github.com/skobkin/magnetico-web/internal/web/server"
)

func init() {
	extraTabs = []struct{ key, label, href string }{
		{"crawlers", "Crawlers", "/admin/crawlers"},
		{"quarantine", "Quarantine", "/admin/quarantine"},
		{"users", "Users", "/admin/users"},
		{"invitations", "Invitations", "/admin/invitations"},
		{"trackers", "Trackers", "/admin/trackers"},
	}
}

func (h *Handlers) installPages(rt *server.Router) {
	rt.Handle("GET /admin/crawlers", server.KindRequest, server.Admin, http.HandlerFunc(h.crawlersPage))
	rt.Handle("GET /admin/quarantine", server.KindRequest, server.Admin, http.HandlerFunc(h.quarantinePage))
	rt.Handle("GET /admin/users", server.KindRequest, server.Admin, http.HandlerFunc(h.usersPage))
	rt.Handle("GET /admin/invitations", server.KindRequest, server.Admin, http.HandlerFunc(h.invitationsPage))
	rt.Handle("GET /admin/trackers", server.KindRequest, server.Admin, http.HandlerFunc(h.trackersPage))
}

// ---- cursors and parameters (strict) -------------------------------------------------------

var (
	cursorAlphabet = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
	nameCursor     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// encodeName and decodeName are the opaque keyset cursor of the crawlers list.
func encodeName(name string) string { return base64.RawURLEncoding.EncodeToString([]byte(name)) }

func decodeName(s string) (string, bool) {
	if !cursorAlphabet.MatchString(s) {
		return "", false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || !nameCursor.Match(b) {
		return "", false
	}
	return string(b), true
}

func encodeID(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

func decodeID(s string) (int64, bool) {
	if !cursorAlphabet.MatchString(s) {
		return 0, false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) == 0 || len(b) > 19 || (len(b) > 1 && b[0] == '0') {
		return 0, false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil || n <= 0 || n >= 1<<62 {
		return 0, false
	}
	return n, true
}

// param returns the single value of a query parameter; more than one value, or
// one longer than max bytes, is invalid.
func param(r *http.Request, key string, max int) (string, bool) {
	vs := r.URL.Query()[key]
	switch {
	case len(vs) == 0:
		return "", true
	case len(vs) > 1 || len(vs[0]) > max:
		return "", false
	}
	return vs[0], true
}

func (h *Handlers) badRequest(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	h.srv.ErrorPage(w, r, http.StatusBadRequest)
}

func (h *Handlers) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() == nil {
		h.srv.Logger().Warn("admin page failed", "request_id", server.RequestID(r.Context()), "error_type", errType(err))
	}
	w.Header().Set("Retry-After", "5")
	noStore(w)
	h.srv.ErrorPage(w, r, http.StatusServiceUnavailable)
}

// readAt formats the time a cached answer was read, in UTC to the second.
func readAt(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") }

func ts(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") }

func optTS(t *time.Time) string {
	if t == nil {
		return dash
	}
	return ts(*t)
}

// ---- crawlers ------------------------------------------------------------------------------

type crawlerView struct {
	ID, Name, State, StateClass, Created, Rotated, Disabled string
	Connected                                               bool
	FirstSeen, LastSeen, LastIP, LastSubmit                 string
	Received, Enqueued, Duplicate, Bytes                    string
	ExistsRequests, ExistsHashes, Rejected                  string
}

type crawlersPage struct {
	chrome
	Problem        string
	Items          []crawlerView
	NextHref       string
	StatsAvailable bool
	// Stale and FetchedAt say how current the page is: the data may come from
	// the cache, and after a failed refresh it is the last known good.
	Stale     bool
	FetchedAt string
}

func (h *Handlers) crawlersPage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r, false)
	if !ok {
		return
	}
	after := stats.Cursor{}
	if v, ok := param(r, "after", 100); !ok {
		h.badRequest(w, r)
		return
	} else if v != "" {
		name, ok := decodeName(v)
		if !ok {
			h.badRequest(w, r)
			return
		}
		after.Name = name
	}
	page, f, err := h.stats.Crawlers(r.Context(), p, after)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_crawlers", buildCrawlersPage(h.chrome(r, "crawlers"), page, f))
}

// buildCrawlersPage turns one page of the registry into its view.
func buildCrawlersPage(c chrome, page stats.CrawlerPage, f stats.Freshness) crawlersPage {
	pg := crawlersPage{chrome: c, StatsAvailable: page.StatsAvailable, Stale: f.Stale, FetchedAt: readAt(f.FetchedAt)}
	if page.Ingest.State != stats.IngestOK {
		pg.Problem = ingestProblem("crawler registry", page.Ingest)
		return pg
	}
	for _, c := range page.Items {
		v := crawlerView{ID: c.ID, Name: c.Name, Created: ts(c.CreatedAt), Rotated: optTS(c.TokenRotatedAt), Disabled: optTS(c.DisabledAt)}
		// Four different things: disabled, inactive, never connected and unknown.
		switch c.ActivityClass(page.StatsAvailable) {
		case stats.ActivityActive:
			v.State, v.StateClass = "active", "ok"
		case stats.ActivityInactive:
			v.State, v.StateClass = "inactive", "warning"
		case stats.ActivityNever:
			v.State, v.StateClass = "never connected", "pending"
		case stats.ActivityDisabled:
			v.State, v.StateClass = "disabled", "error"
		default:
			v.State, v.StateClass = "activity unavailable", "pending"
		}
		if a := c.Activity; a != nil {
			v.Connected = true
			v.FirstSeen, v.LastSeen, v.LastIP, v.LastSubmit = ts(a.FirstSeen), ts(a.LastSeen), orDash(a.LastIP), optTS(a.LastSubmit)
			v.Received, v.Enqueued, v.Duplicate, v.Bytes = groupDigits(a.Received), groupDigits(a.Enqueued), groupDigits(a.Duplicate), bytesIEC(a.BytesBody)
			v.ExistsRequests, v.ExistsHashes, v.Rejected = groupDigits(a.ExistsRequests), groupDigits(a.ExistsHashes), groupDigits(a.Rejected)
		}
		pg.Items = append(pg.Items, v)
	}
	if page.Next != nil {
		pg.NextHref = "/admin/crawlers?after=" + encodeName(page.Next.Name)
	}
	return pg
}

func orDash(s string) string {
	if s == "" {
		return dash
	}
	return s
}

// ingestProblem explains an unusable ingest schema in names only.
func ingestProblem(what string, st stats.IngestStatus) string {
	switch st.State {
	case stats.IngestMissing:
		return "The ingest schema does not exist in this database, so the " + what + " cannot be shown. Run `magnetico migrate ingest-schema-init`."
	case stats.IngestIncompatible:
		return "The ingest schema is not compatible with this version (" + strings.Join(st.Missing, ", ") +
			"), so the " + what + " cannot be shown. An older development schema is reinitialised by hand: see UPGRADE.md."
	}
	return "The web database role lacks privileges needed for the " + what + ": " + strings.Join(st.Missing, "; ") + " (see docs/web-deployment.md)."
}

// ---- quarantine ----------------------------------------------------------------------------

type quarantineView struct {
	ID, Reason, Detail, Attempts, Size, Received, Quarantined, Age string
}

type quarantinePage struct {
	chrome
	Problem  string
	Items    []quarantineView
	NextHref string
}

func (h *Handlers) quarantinePage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r, false)
	if !ok {
		return
	}
	var after int64
	if v, ok := param(r, "after", 100); !ok {
		h.badRequest(w, r)
		return
	} else if v != "" {
		if after, ok = decodeID(v); !ok {
			h.badRequest(w, r)
			return
		}
	}
	pg := quarantinePage{chrome: h.chrome(r, "quarantine")}
	st, err := h.stats.IngestStatus(r.Context(), p)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	if st.State != stats.IngestOK {
		pg.Problem = ingestProblem("quarantine", st)
		h.render(w, r, http.StatusOK, "admin_quarantine", pg)
		return
	}
	page, err := h.store.Quarantine(r.Context(), p, after)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	now := time.Now()
	for _, q := range page.Items {
		pg.Items = append(pg.Items, quarantineView{ID: strconv.FormatInt(q.ID, 10), Reason: q.Reason, Detail: q.Detail, Attempts: strconv.Itoa(int(q.Attempts)),
			Size: bytesIEC(int64(q.PayloadBytes)), Received: ts(q.ReceivedAt), Quarantined: ts(q.QuarantinedAt), Age: fmtDuration(now.Sub(q.QuarantinedAt))})
	}
	if page.Next != 0 {
		pg.NextHref = "/admin/quarantine?after=" + encodeID(page.Next)
	}
	h.render(w, r, http.StatusOK, "admin_quarantine", pg)
}

// ---- users ---------------------------------------------------------------------------------

type userView struct {
	ID, Username, Email, EmailState, Role, Active, ActiveClass, Created, LastLogin, InvitedBy string
}

type usersPage struct {
	chrome
	Query    string
	Items    []userView
	NextHref string
}

func (h *Handlers) usersPage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r, false)
	if !ok {
		return
	}
	q, ok := param(r, "q", 4*data.MaxSearch)
	if !ok || data.ValidSearch(q) != nil {
		h.badRequest(w, r)
		return
	}
	var after int64
	if v, ok := param(r, "after", 100); !ok {
		h.badRequest(w, r)
		return
	} else if v != "" {
		if after, ok = decodeID(v); !ok {
			h.badRequest(w, r)
			return
		}
	}
	page, err := h.store.Users(r.Context(), p, q, after)
	if err != nil {
		if errors.Is(err, data.ErrBadInput) {
			h.badRequest(w, r)
			return
		}
		h.unavailable(w, r, err)
		return
	}
	pg := usersPage{chrome: h.chrome(r, "users"), Query: q}
	for _, u := range page.Items {
		v := userView{ID: strconv.FormatInt(u.ID, 10), Username: u.Username, Email: orDash(u.Email), Role: u.Role, Created: ts(u.CreatedAt),
			LastLogin: optTS(u.LastLoginAt), InvitedBy: orDash(u.InvitedBy), Active: "active", ActiveClass: "ok"}
		switch {
		case u.Email == "":
			v.EmailState = "none"
		case u.EmailVerifiedAt != nil:
			v.EmailState = "verified"
		default:
			v.EmailState = "unverified"
		}
		if !u.Active {
			v.Active, v.ActiveClass = "disabled", "warning"
		}
		pg.Items = append(pg.Items, v)
	}
	if page.Next != 0 {
		href := "/admin/users?after=" + encodeID(page.Next)
		if q != "" {
			href += "&q=" + urlQueryEscape(q)
		}
		pg.NextHref = href
	}
	h.render(w, r, http.StatusOK, "admin_users", pg)
}

// ---- invitations ---------------------------------------------------------------------------

type invitationView struct {
	ID, Owner, UsedBy, Status, StatusClass, Created, Used, Expires, Revoked string
}

type invitationsPage struct {
	chrome
	Items    []invitationView
	NextHref string
}

func (h *Handlers) invitationsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r, false)
	if !ok {
		return
	}
	var after int64
	if v, ok := param(r, "after", 100); !ok {
		h.badRequest(w, r)
		return
	} else if v != "" {
		if after, ok = decodeID(v); !ok {
			h.badRequest(w, r)
			return
		}
	}
	page, err := h.store.Invitations(r.Context(), p, after)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	pg := invitationsPage{chrome: h.chrome(r, "invitations")}
	for _, i := range page.Items {
		cls := map[string]string{"used": "ok", "unused": "ok", "expired": "warning", "revoked": "error"}[i.Status]
		pg.Items = append(pg.Items, invitationView{ID: strconv.FormatInt(i.ID, 10), Owner: i.Owner, UsedBy: orDash(i.UsedBy), Status: i.Status, StatusClass: cls,
			Created: ts(i.CreatedAt), Used: optTS(i.UsedAt), Expires: optTS(i.ExpiresAt), Revoked: optTS(i.RevokedAt)})
	}
	if page.Next != 0 {
		pg.NextHref = "/admin/invitations?after=" + encodeID(page.Next)
	}
	h.render(w, r, http.StatusOK, "admin_invitations", pg)
}

// ---- trackers ------------------------------------------------------------------------------

type trackersPage struct {
	chrome
	Items []data.TrackerRow
}

func (h *Handlers) trackersPage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r, false)
	if !ok {
		return
	}
	rows, err := h.store.Trackers(r.Context(), p)
	if err != nil {
		h.unavailable(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_trackers", trackersPage{chrome: h.chrome(r, "trackers"), Items: rows})
}
