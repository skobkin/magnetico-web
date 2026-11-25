// Package admin is the read-only administrator section: a dashboard of the
// ingestion statistics and (see pages.go) listings of crawlers, quarantine,
// users, invitations and trackers. Every route is registered with the Admin or
// AdminJSON access class, so the role is enforced by the router for every
// request, and every data-access call takes the access.Principal that only the
// guard can produce. Nothing here mutates anything.
package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/skobkin/magnetico-web/internal/opstats"
	"github.com/skobkin/magnetico-web/internal/web/admin/access"
	"github.com/skobkin/magnetico-web/internal/web/admin/data"
	"github.com/skobkin/magnetico-web/internal/web/admin/stats"
	"github.com/skobkin/magnetico-web/internal/web/auth"
	"github.com/skobkin/magnetico-web/internal/web/problem"
	"github.com/skobkin/magnetico-web/internal/web/server"
)

// Handlers serves the administrator section.
type Handlers struct {
	srv   *server.Server
	stats *stats.Service
	store *data.Store
}

// Install registers the administrator routes: all GET, all behind the Admin or
// AdminJSON guard of the auth package.
func Install(srv *server.Server, svc *stats.Service, store *data.Store) *Handlers {
	h := &Handlers{srv: srv, stats: svc, store: store}
	rt := srv.Router()
	rt.Handle("GET /admin", server.KindRequest, server.Admin, http.HandlerFunc(h.overview))
	rt.Handle("GET /admin/api/overview", server.KindRequest, server.AdminJSON, http.HandlerFunc(h.overviewJSON))
	rt.Handle("GET /admin/api/series", server.KindRequest, server.AdminJSON, http.HandlerFunc(h.seriesJSON))
	h.installPages(rt)
	return h
}

// principal returns the Principal the guard attached; a request that somehow
// reached a handler without one is refused (defence in depth).
func (h *Handlers) principal(w http.ResponseWriter, r *http.Request, json bool) (access.Principal, bool) {
	p, ok := access.FromContext(r.Context())
	if !ok {
		if json {
			problem.Write(w, problem.Problem{Slug: problem.Forbidden, Title: "Administrator access required", Status: http.StatusForbidden})
		} else {
			h.srv.ErrorPage(w, r, http.StatusForbidden)
		}
	}
	return p, ok
}

func noStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

type tab struct {
	Label, Href string
	Active      bool
}

func tabs(active string) []tab {
	all := []struct{ key, label, href string }{
		{"overview", "Overview", "/admin"},
	}
	all = append(all, extraTabs...)
	out := make([]tab, len(all))
	for i, t := range all {
		out[i] = tab{Label: t.label, Href: t.href, Active: t.key == active}
	}
	return out
}

// extraTabs are appended by the page files.
var extraTabs []struct{ key, label, href string }

// chrome is the data every administrator page shares with the site layout.
type chrome struct {
	Username, CSRFToken string
	IsAdmin             bool
	Tab                 string
	Tabs                []tab
}

func (h *Handlers) chrome(r *http.Request, active string) chrome {
	p, _ := auth.Current(r.Context())
	return chrome{Username: p.Username, CSRFToken: auth.SessionFormToken(r), IsAdmin: true, Tab: active, Tabs: tabs(active)}
}

func (h *Handlers) render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	noStore(w)
	if err := h.srv.Renderer().Render(w, status, page, data); err != nil {
		h.srv.Logger().Error("render admin page failed", "request_id", server.RequestID(r.Context()))
		h.srv.ErrorPage(w, r, http.StatusInternalServerError)
	}
}

// ---- the overview page ------------------------------------------------------------

type instanceView struct {
	ID, Started, State, StateClass  string
	Owns                            bool
	InFlight, Stalled               string
	Warnings                        []string
	Received, Acknowledged, Settled string
}

// rateRow is one rolling window. V holds the formatted rate of every metric by
// its name (stats.MetricNames) plus "auth_hit_rate", the cache efficiency of
// the same window.
type rateRow struct {
	Window, Unit   string
	V              map[string]string
	Included       int
	ExcludedNotice []string
}

// chartCard is one chart group. A wide card spans the whole grid row and lays
// its plots out side by side.
type chartCard struct {
	Key, Title string
	Wide       bool
}

// charts are the five chart groups of the dashboard (docs/specs/statistics-v1.md §9).
// The two-plot groups (queue, storage) share a row, and traffic, which has six
// plots, comes last at full width, so no card leaves a hole beside a taller one.
var charts = []chartCard{
	{Key: stats.ChartThroughput, Title: "Throughput"}, {Key: stats.ChartOutcomes, Title: "Outcomes of processing"},
	{Key: stats.ChartQueue, Title: "Queue and quota"}, {Key: stats.ChartStorage, Title: "Storage"},
	{Key: stats.ChartTraffic, Title: "Traffic and efficiency", Wide: true},
}

type overviewPage struct {
	chrome
	Charts                               []chartCard
	Schema, SchemaClass, Problem         string
	Ingest, IngestClass                  string
	Queue, QueueClass                    string
	Storage, StorageClass                string
	FetchedAt                            string
	Stale                                bool
	Instances                            []instanceView
	Rates                                []rateRow
	QueueRows, QueueBytes                string
	QuotaUsed, QuotaMax, Quota           string
	Quarantine, QuarantineBytes          string
	Oldest                               string
	Backoff                              bool
	Torrents, Blobs, Indexes             string
	Other, Magneticod, Ingestdb          string
	Database, Indexed, PerTorrent        string
	ReconNote                            string
	HourReceived, HourEnqueued           string
	HourAcknowledged, HourSettled        string
	UsersTotal, UsersActive, UsersAdmins string
	UsersDisabled                        string
	CrawlersRegistered                   string
	CrawlersEnabled, CrawlersActive      string
	ActiveWindow                         string
}

func (h *Handlers) overview(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r, false)
	if !ok {
		return
	}
	o, f, err := h.stats.Overview(r.Context(), p)
	if err != nil {
		if r.Context().Err() == nil {
			h.srv.Logger().Warn("admin overview failed", "request_id", server.RequestID(r.Context()), "error_type", errType(err))
		}
		w.Header().Set("Retry-After", "5")
		h.srv.ErrorPage(w, r, http.StatusServiceUnavailable)
		return
	}
	h.render(w, r, http.StatusOK, "admin_overview", buildOverview(h.chrome(r, "overview"), o, f))
}

func errType(err error) string { return strings.TrimPrefix(typeName(err), "*") }

func typeName(v any) string { return fmt.Sprintf("%T", v) }

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

func buildOverview(c chrome, o stats.Overview, f stats.Freshness) overviewPage {
	pg := overviewPage{chrome: c, Charts: charts, Problem: o.Problem, Stale: f.Stale, FetchedAt: f.FetchedAt.UTC().Format("2006-01-02 15:04:05 UTC")}
	pg.Schema = stateText(o.Schema, 0)
	pg.SchemaClass = stateClass(o.Schema)
	pg.Ingest, pg.IngestClass = stateText(f.Ingest, 0), stateClass(f.Ingest)
	pg.Queue, pg.QueueClass = stateText(f.Queue, o.Queue.Age), stateClass(f.Queue)
	pg.Storage, pg.StorageClass = stateText(f.Storage, o.Storage.Age), stateClass(f.Storage)
	pg.Ingest = stateText(f.Ingest, 0)
	for _, in := range o.Instances {
		v := instanceView{ID: shortID(in.ID), Started: in.StartedAt.UTC().Format("2006-01-02 15:04 UTC"),
			State: stateText(in.State, in.Age), StateClass: stateClass(in.State), Owns: in.OwnsConsumer,
			InFlight: groupDigits(in.InFlight), Stalled: fmtDuration(time.Duration(in.FlushStalledSeconds) * time.Second),
			Received: groupDigits(in.TotalReceived), Acknowledged: groupDigits(in.TotalAcknowledged), Settled: groupDigits(in.TotalSettled)}
		if in.FlushStalledSeconds > 120 {
			v.Warnings = append(v.Warnings, "persisting statistics is stalled")
		}
		if in.SequenceAnomalies > 0 {
			v.Warnings = append(v.Warnings, groupDigits(in.SequenceAnomalies)+" sequence anomaly(ies)")
		}
		if in.HoursDropped > 0 {
			v.Warnings = append(v.Warnings, groupDigits(in.HoursDropped)+" hour(s) of statistics dropped")
		}
		if in.BatchesDroppedUnresolved > 0 {
			v.Warnings = append(v.Warnings, groupDigits(in.BatchesDroppedUnresolved)+" batch(es) dropped unresolved")
		}
		if in.ClockAnomaly {
			v.Warnings = append(v.Warnings, "clock anomaly")
		}
		pg.Instances = append(pg.Instances, v)
	}
	labels := []string{"last minute", "last 15 minutes", "last hour"}
	for i, w := range o.Rates.Windows {
		row := rateRow{Window: labels[i], Unit: w.Unit, Included: w.Included, V: map[string]string{}}
		for k, name := range stats.MetricNames {
			if name == "bytes_body" {
				row.V[name] = bytesRate(w.Values[k])
			} else {
				row.V[name] = fmtFloat(w.Values[k], "")
			}
		}
		row.V["auth_hit_rate"] = authHitRate(w)
		for _, e := range w.Excluded {
			row.ExcludedNotice = append(row.ExcludedNotice, shortID(e.InstanceID)+": "+e.Reason)
		}
		pg.Rates = append(pg.Rates, row)
	}
	q := o.Queue
	pg.QueueRows, pg.QueueBytes = fmtInt(q.Rows), fmtBytes(q.Bytes)
	pg.QuotaUsed, pg.QuotaMax, pg.Quota = fmtBytes(q.QuotaUsed), fmtBytes(q.QuotaMax), fmtPercent(q.Utilisation)
	pg.Quarantine, pg.QuarantineBytes, pg.Oldest, pg.Backoff = fmtInt(q.Quarantine), fmtBytes(q.QuarantineBy), fmtAge(q.OldestAge), q.Backoff
	s := o.Storage
	pg.Torrents, pg.Blobs, pg.Indexes, pg.Other = fmtBytes(s.Torrents), fmtBytes(s.Blobs), fmtBytes(s.Indexes), fmtBytes(s.Other)
	pg.Magneticod, pg.Ingestdb, pg.Database = fmtBytes(s.Magneticod), fmtBytes(s.Ingest), fmtBytes(s.Database)
	pg.Indexed = fmtInt(s.IndexedCount)
	if s.BytesPerItem.Valid {
		pg.PerTorrent = approx(true, bytesIEC(int64(s.BytesPerItem.Value)))
	} else {
		pg.PerTorrent = dash
	}
	if s.State == stats.StateUp {
		switch s.Reconciliation {
		case opstats.ReconMismatch:
			pg.ReconNote = "The storage categories did not add up to the total in the last sample."
		case opstats.ReconNotEvaluated:
			pg.ReconNote = "The storage categories were not checked against the total in the last sample because a measure was unavailable."
		}
	}
	if o.ThisHourAt.IsZero() {
		pg.HourReceived, pg.HourEnqueued, pg.HourAcknowledged, pg.HourSettled = dash, dash, dash, dash
	} else {
		pg.HourReceived, pg.HourEnqueued = groupDigits(o.ThisHour[0]), groupDigits(o.ThisHour[1])
		pg.HourAcknowledged, pg.HourSettled = groupDigits(o.ThisHour[2]), groupDigits(o.ThisHour[3])
	}
	pg.UsersTotal, pg.UsersActive, pg.UsersAdmins = fmtInt(o.Users.Total), fmtInt(o.Users.Active), fmtInt(o.Users.Admins)
	pg.UsersDisabled = fmtInt(o.Users.Disabled)
	pg.CrawlersRegistered, pg.CrawlersEnabled, pg.CrawlersActive = fmtCount(o.Crawlers.Registered, o.Crawlers.Capped), fmtCount(o.Crawlers.Enabled, o.Crawlers.Capped), fmtCount(o.Crawlers.Active, o.Crawlers.Capped)
	pg.ActiveWindow = fmtDuration(stats.ActiveWindow)
	return pg
}

func bytesRate(m stats.Measured[float64]) string {
	if !m.Valid {
		return dash
	}
	return bytesIEC(int64(m.Value))
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// ---- JSON ---------------------------------------------------------------------------

type measured[T any] struct {
	Value  *T   `json:"value"`
	Valid  bool `json:"valid"`
	Approx bool `json:"approx,omitempty"`
}

func mj[T any](m stats.Measured[T]) measured[T] {
	out := measured[T]{Valid: m.Valid, Approx: m.Approx}
	if m.Valid {
		v := m.Value
		out.Value = &v
	}
	return out
}

func seconds(m stats.Measured[time.Duration]) measured[float64] {
	return mj(stats.Measured[float64]{Value: m.Value.Seconds(), Valid: m.Valid, Approx: m.Approx})
}

type freshnessJSON struct {
	Ingest    string `json:"ingest"`
	Queue     string `json:"queue"`
	Storage   string `json:"storage"`
	FetchedAt string `json:"fetched_at"`
	Stale     bool   `json:"stale"`
}

func freshJSON(f stats.Freshness) freshnessJSON {
	return freshnessJSON{Ingest: f.Ingest.String(), Queue: f.Queue.String(), Storage: f.Storage.String(),
		FetchedAt: f.FetchedAt.UTC().Format(time.RFC3339), Stale: f.Stale}
}

type overviewJSON struct {
	Schema    string        `json:"schema"`
	Problem   string        `json:"problem,omitempty"`
	Freshness freshnessJSON `json:"freshness"`
	Instances []struct {
		ID           string  `json:"id"`
		State        string  `json:"state"`
		AgeSeconds   float64 `json:"age_seconds"`
		OwnsConsumer bool    `json:"owns_consumer"`
		InFlight     int64   `json:"in_flight_records"`
		Received     int64   `json:"total_received"`
		Acknowledged int64   `json:"total_acknowledged"`
		Settled      int64   `json:"total_settled"`
	} `json:"instances"`
	Rates []struct {
		Seconds  int                          `json:"window_seconds"`
		Unit     string                       `json:"unit"`
		Included int                          `json:"instances_included"`
		Excluded int                          `json:"instances_excluded"`
		Values   map[string]measured[float64] `json:"values"`
	} `json:"rates"`
	Queue struct {
		State       string            `json:"state"`
		Rows        measured[int64]   `json:"rows"`
		Bytes       measured[int64]   `json:"bytes"`
		QuotaUsed   measured[int64]   `json:"quota_used_bytes"`
		QuotaMax    measured[int64]   `json:"quota_max_bytes"`
		Utilisation measured[float64] `json:"quota_utilisation"`
		Quarantine  measured[int64]   `json:"quarantine_rows"`
		OldestAge   measured[float64] `json:"oldest_age_seconds"`
	} `json:"queue"`
	Storage struct {
		State      string            `json:"state"`
		Torrents   measured[int64]   `json:"torrents_table_bytes"`
		Blobs      measured[int64]   `json:"blobs_table_bytes"`
		Indexes    measured[int64]   `json:"indexes_bytes"`
		Other      measured[int64]   `json:"other_bytes"`
		Magneticod measured[int64]   `json:"magneticod_total_bytes"`
		Ingest     measured[int64]   `json:"ingest_total_bytes"`
		Database   measured[int64]   `json:"database_total_bytes"`
		Indexed    measured[int64]   `json:"indexed_torrents_estimate"`
		PerTorrent measured[float64] `json:"bytes_per_torrent"`
	} `json:"storage"`
	Users struct {
		Total    measured[int64] `json:"total"`
		Active   measured[int64] `json:"active"`
		Disabled measured[int64] `json:"disabled"`
		Admins   measured[int64] `json:"admins"`
	} `json:"users"`
	// Crawlers are the registry counts; "recently active" means the statistics
	// saw an authenticated request within ActiveWindowSeconds. Capped: the counts
	// are lower bounds.
	Crawlers struct {
		Registered          measured[int64] `json:"registered"`
		Enabled             measured[int64] `json:"enabled"`
		Active              measured[int64] `json:"recently_active"`
		Capped              bool            `json:"capped"`
		ActiveWindowSeconds int             `json:"active_window_seconds"`
	} `json:"crawlers"`
}

func buildOverviewJSON(o stats.Overview, f stats.Freshness) overviewJSON {
	var j overviewJSON
	j.Schema, j.Problem, j.Freshness = o.Schema.String(), o.Problem, freshJSON(f)
	for _, in := range o.Instances {
		j.Instances = append(j.Instances, struct {
			ID           string  `json:"id"`
			State        string  `json:"state"`
			AgeSeconds   float64 `json:"age_seconds"`
			OwnsConsumer bool    `json:"owns_consumer"`
			InFlight     int64   `json:"in_flight_records"`
			Received     int64   `json:"total_received"`
			Acknowledged int64   `json:"total_acknowledged"`
			Settled      int64   `json:"total_settled"`
		}{shortID(in.ID), in.State.String(), in.Age.Seconds(), in.OwnsConsumer, in.InFlight, in.TotalReceived, in.TotalAcknowledged, in.TotalSettled})
	}
	for _, w := range o.Rates.Windows {
		vals := map[string]measured[float64]{}
		for i, n := range stats.MetricNames {
			vals[n] = mj(w.Values[i])
		}
		j.Rates = append(j.Rates, struct {
			Seconds  int                          `json:"window_seconds"`
			Unit     string                       `json:"unit"`
			Included int                          `json:"instances_included"`
			Excluded int                          `json:"instances_excluded"`
			Values   map[string]measured[float64] `json:"values"`
		}{w.Seconds, w.Unit, w.Included, len(w.Excluded), vals})
	}
	q := o.Queue
	j.Queue.State, j.Queue.Rows, j.Queue.Bytes, j.Queue.QuotaUsed, j.Queue.QuotaMax = q.State.String(), mj(q.Rows), mj(q.Bytes), mj(q.QuotaUsed), mj(q.QuotaMax)
	j.Queue.Utilisation, j.Queue.Quarantine, j.Queue.OldestAge = mj(q.Utilisation), mj(q.Quarantine), seconds(q.OldestAge)
	s := o.Storage
	j.Storage.State, j.Storage.Torrents, j.Storage.Blobs, j.Storage.Indexes, j.Storage.Other = s.State.String(), mj(s.Torrents), mj(s.Blobs), mj(s.Indexes), mj(s.Other)
	j.Storage.Magneticod, j.Storage.Ingest, j.Storage.Database, j.Storage.Indexed, j.Storage.PerTorrent = mj(s.Magneticod), mj(s.Ingest), mj(s.Database), mj(s.IndexedCount), mj(s.BytesPerItem)
	j.Users.Total, j.Users.Active, j.Users.Admins = mj(o.Users.Total), mj(o.Users.Active), mj(o.Users.Admins)
	j.Users.Disabled = mj(o.Users.Disabled)
	j.Crawlers.Registered, j.Crawlers.Enabled, j.Crawlers.Active, j.Crawlers.Capped = mj(o.Crawlers.Registered), mj(o.Crawlers.Enabled), mj(o.Crawlers.Active), o.Crawlers.Capped
	j.Crawlers.ActiveWindowSeconds = int(stats.ActiveWindow / time.Second)
	return j
}

func writeJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		problem.Write(w, problem.Problem{Slug: problem.InternalError, Title: "Internal error", Status: http.StatusInternalServerError})
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(body)
}

func (h *Handlers) overviewJSON(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r, true)
	if !ok {
		return
	}
	o, f, err := h.stats.Overview(r.Context(), p)
	if err != nil {
		h.unavailableJSON(w, r, err)
		return
	}
	writeJSON(w, buildOverviewJSON(o, f))
}

func (h *Handlers) unavailableJSON(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	h.srv.Logger().Warn("admin data failed", "request_id", server.RequestID(r.Context()), "error_type", errType(err))
	problem.Write(w, problem.Problem{Slug: problem.TemporarilyBusy, Title: "Statistics temporarily unavailable", Status: http.StatusServiceUnavailable, RetryAfterSeconds: 5})
}

type seriesJSON struct {
	Chart       string   `json:"chart"`
	Granularity string   `json:"granularity"`
	Period      string   `json:"period"`
	Previous    string   `json:"previous,omitempty"`
	Next        string   `json:"next,omitempty"`
	Buckets     []string `json:"buckets"`
	Partial     []bool   `json:"partial"`
	// Notes lists, per bucket, what a reader must know about its values
	// (approximate_counts, incomplete_breakdown, reconciliation_mismatch,
	// reconciliation_not_evaluated); an entry is null when there is nothing to say.
	Notes [][]string `json:"notes"`
	Lines []struct {
		Key    string     `json:"key"`
		Label  string     `json:"label"`
		Unit   string     `json:"unit"`
		Group  string     `json:"group"`
		Stack  bool       `json:"stack,omitempty"`
		Values []*float64 `json:"values"`
	} `json:"lines"`
	Freshness freshnessJSON `json:"freshness"`
}

// maxParam bounds every query parameter value.
const maxParam = 16

func (h *Handlers) seriesJSON(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r, true)
	if !ok {
		return
	}
	qv := r.URL.Query()
	get := func(k string) (string, bool) {
		vs := qv[k]
		if len(vs) > 1 || (len(vs) == 1 && len(vs[0]) > maxParam) {
			return "", false
		}
		if len(vs) == 0 {
			return "", true
		}
		return vs[0], true
	}
	chart, ok1 := get("chart")
	gran, ok2 := get("g")
	period, ok3 := get("period")
	if !ok1 || !ok2 || !ok3 || chart == "" || gran == "" {
		problem.Write(w, problem.Problem{Slug: problem.InvalidParameter, Title: "Invalid parameter", Status: http.StatusBadRequest,
			Detail: "chart and g are required; period is optional; each value is a short string."})
		return
	}
	s, f, err := h.stats.Series(r.Context(), p, stats.Query{Chart: chart, Granularity: gran, Period: period})
	switch {
	case err == nil:
	case errors.Is(err, stats.ErrBadQuery):
		problem.Write(w, problem.Problem{Slug: problem.InvalidParameter, Title: "Invalid parameter", Status: http.StatusBadRequest,
			Detail: "chart is one of throughput, outcomes, queue, traffic, storage; g is day, month or year; period matches it (2026-11-25, 2026-11, 2026)."})
		return
	case errors.Is(err, stats.ErrPeriodOutOfRange):
		problem.Write(w, problem.Problem{Slug: problem.NotFound, Title: "No data for that period", Status: http.StatusNotFound})
		return
	default:
		h.unavailableJSON(w, r, err)
		return
	}
	writeJSON(w, buildSeriesJSON(s, f))
}

// buildSeriesJSON is the answer of the series endpoint.
func buildSeriesJSON(s stats.Series, f stats.Freshness) seriesJSON {
	var out seriesJSON
	out.Chart, out.Granularity, out.Period, out.Previous, out.Next = s.Query.Chart, s.Query.Granularity, s.Query.Period, s.Previous, s.Next
	for _, b := range s.Buckets {
		out.Buckets = append(out.Buckets, b.UTC().Format(time.RFC3339))
	}
	out.Partial = s.Partial
	for _, l := range s.Lines {
		out.Lines = append(out.Lines, struct {
			Key    string     `json:"key"`
			Label  string     `json:"label"`
			Unit   string     `json:"unit"`
			Group  string     `json:"group"`
			Stack  bool       `json:"stack,omitempty"`
			Values []*float64 `json:"values"`
		}{l.Key, l.Label, l.Unit, l.Group, l.Stack, l.Values})
	}
	out.Notes = s.Notes
	out.Freshness = freshJSON(f)
	return out
}
