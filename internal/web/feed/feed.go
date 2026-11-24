// Package feed serves the two RSS endpoints (docs/specs/web-v1.md §8.2):
// the deprecated v1 compatibility feed GET /api/v1/feed/rss/last and the v2
// feed GET /api/v2/feeds/latest.rss. They share one generator (same rows,
// same tracker snapshot, same document) and differ only in a fixed credential
// policy bound at registration:
//
//	v1: RSS token from the api-token header, cookie or query parameter (that
//	    order); failures use the legacy JSON envelope.
//	v2: RSS token from the feed_token query parameter only; failures are
//	    RFC 9457 problem+json.
//
// Both accept RSS tokens only (new mgf_ and migrated legacy tokens, looked up
// in rss_tokens) and never API tokens or the Authorization header. The token
// is checked, with owner-active, expiry and revocation, on every request and
// BEFORE the page cache. It is never echoed, logged or sent in a Referer.
package feed

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

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/problem"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/trackers"
	"github.com/skobkin/magnetico-web/persistence"
)

// Paths of the two feeds.
const (
	V1Path = "/api/v1/feed/rss/last"
	V2Path = "/api/v2/feeds/latest.rss"
)

// Page geometry: 1,000 items a page, pages 1 to 10 (values above 10 clamp).
const (
	PerPage = 1000
	MaxPage = 10
)

// Authenticator resolves and touches RSS tokens (accounts.Store).
type Authenticator interface {
	AuthenticateRSSToken(ctx context.Context, presented string) (accounts.Identity, error)
	TouchRSSToken(ctx context.Context, id int64) error
}

// Rows reads the newest torrents (persistence.Reader).
type Rows interface {
	LatestTorrents(ctx context.Context, offset, limit int) ([]persistence.Torrent, error)
}

// TrackerSource is the tracker snapshot cache.
type TrackerSource interface {
	Snapshot() *trackers.Snapshot
}

// Config wires the feeds.
type Config struct {
	Auth       Authenticator
	Rows       Rows
	Trackers   TrackerSource // optional: no trackers without it
	PublicBase *url.URL

	CacheTTL time.Duration // 0 → 60 s
	Now      func() time.Time

	// Limiters override the defaults (tests).
	FailLimiter, UseLimiter *ratelimit.Limiter
}

// Limits and bounds (constants).
const (
	defaultCacheTTL = 60 * time.Second
	failBurst       = 20
	failInterval    = 10 * time.Second
	useBurst        = 30
	useInterval     = 2 * time.Second
	limiterKeys     = 10000
	touchTimeout    = 2 * time.Second
	genSlots        = 2
	genWait         = 2 * time.Second
	kindFeedToken   = 'F'
)

// policy is the fixed credential policy and failure vocabulary of one feed.
type policy struct {
	name string // "v1" or "v2", for logs
	path string
	// token extracts the credential; present reports that something was sent.
	token func(r *http.Request) string
	// reply writes a failure.
	reply func(w http.ResponseWriter, status int, retryAfter int)
}

// v1Policy: header, then cookie, then query, all named api-token (legacy order);
// the legacy JSON envelope for every failure.
var v1Policy = policy{
	name: "v1",
	path: V1Path,
	token: func(r *http.Request) string {
		if v := r.Header.Get("api-token"); v != "" {
			return v
		}
		if c, err := r.Cookie("api-token"); err == nil && c.Value != "" {
			return c.Value
		}
		return r.URL.Query().Get("api-token")
	},
	reply: func(w http.ResponseWriter, status, retryAfter int) {
		msg, st := "Invalid credentials.", "error"
		switch status {
		case http.StatusTooManyRequests:
			msg = "Too many requests."
		case http.StatusServiceUnavailable:
			msg, st = "Service unavailable.", "fail"
		case http.StatusInternalServerError:
			msg, st = "Internal error.", "fail"
		}
		if retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		}
		body, _ := json.Marshal(struct {
			Code    int     `json:"code"`
			Status  string  `json:"status"`
			Message string  `json:"message"`
			Data    *string `json:"data"`
		}{status, st, msg, nil})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	},
}

// v2Policy: feed_token query parameter only (exactly one value); problem+json.
var v2Policy = policy{
	name: "v2",
	path: V2Path,
	token: func(r *http.Request) string {
		vals := r.URL.Query()["feed_token"]
		if len(vals) != 1 {
			return ""
		}
		return vals[0]
	},
	reply: func(w http.ResponseWriter, status, retryAfter int) {
		switch status {
		case http.StatusTooManyRequests:
			problem.Write(w, problem.Problem{Slug: problem.RateLimited, Title: "Too many requests", Status: status,
				Detail: "Slow down and retry later.", RetryAfterSeconds: retryAfter})
		case http.StatusServiceUnavailable:
			problem.Write(w, problem.Problem{Slug: problem.TemporarilyBusy, Title: "Temporarily unavailable", Status: status,
				Detail: "Please try again shortly.", RetryAfterSeconds: retryAfter})
		case http.StatusInternalServerError:
			problem.Write(w, problem.Problem{Slug: problem.InternalError, Title: "Internal error", Status: status})
		default:
			problem.Write(w, problem.Problem{Slug: problem.Unauthenticated, Title: "Authentication required", Status: status,
				Detail: "A valid RSS token is required in the feed_token parameter."})
		}
	},
}

// Handlers is the installed feed.
type Handlers struct {
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	failLim *ratelimit.Limiter
	useLim  *ratelimit.Limiter
	cache   *pageCache
	gen     chan struct{}
}

// Install registers both feeds (Feed access class).
func Install(srv *server.Server, cfg Config) (*Handlers, error) {
	if cfg.Auth == nil || cfg.Rows == nil || cfg.PublicBase == nil {
		return nil, errors.New("feed: authenticator, rows and public base URL are required")
	}
	h := &Handlers{cfg: cfg, log: srv.Logger(), now: cfg.Now, failLim: cfg.FailLimiter, useLim: cfg.UseLimiter, gen: make(chan struct{}, genSlots)}
	if h.now == nil {
		h.now = time.Now
	}
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	h.cache = newPageCache(ttl, h.now)
	var err error
	if h.failLim == nil {
		if h.failLim, err = ratelimit.New(ratelimit.Config{Burst: failBurst, Interval: failInterval, MaxKeys: limiterKeys}); err != nil {
			return nil, err
		}
	}
	if h.useLim == nil {
		if h.useLim, err = ratelimit.New(ratelimit.Config{Burst: useBurst, Interval: useInterval, MaxKeys: limiterKeys}); err != nil {
			return nil, err
		}
	}
	rt := srv.Router()
	// The Feed class carries no credential logic: each route below binds its own
	// fixed policy, so the transports of one feed can never leak into the other.
	rt.SetGuard(server.Feed, func(next http.Handler) http.Handler { return next })
	rt.Handle("GET "+V1Path, server.KindRequest, server.Feed, h.handler(v1Policy))
	rt.Handle("GET "+V2Path, server.KindRequest, server.Feed, h.handler(v2Policy))
	return h, nil
}

func pageOf(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	switch {
	case err != nil || n < 1:
		return 1
	case n > MaxPage:
		return MaxPage
	}
	return n
}

func (h *Handlers) handler(p policy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "private, no-store")
		rid := server.RequestID(r.Context())

		fail := h.failLim.Reserve(ratelimit.IPKey(server.ClientIP(r.Context())))
		if !fail.OK {
			p.reply(w, http.StatusTooManyRequests, ratelimit.RetryAfterSeconds(fail.RetryAfter))
			return
		}
		token := p.token(r)
		if token == "" {
			p.reply(w, http.StatusUnauthorized, 0)
			return
		}
		id, err := h.cfg.Auth.AuthenticateRSSToken(r.Context(), token)
		switch {
		case errors.Is(err, accounts.ErrUnauthenticated):
			p.reply(w, http.StatusUnauthorized, 0)
			return
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			h.log.Warn("rss token lookup failed", "request_id", rid, "feed", p.name, "error_type", errKind(err))
			fail.Cancel()
			p.reply(w, http.StatusServiceUnavailable, 5)
			return
		}
		fail.Cancel()
		if use := h.useLim.Reserve(ratelimit.KeyOf(kindFeedToken, strconv.FormatInt(id.TokenID, 10))); !use.OK {
			p.reply(w, http.StatusTooManyRequests, ratelimit.RetryAfterSeconds(use.RetryAfter))
			return
		}
		server.SetUserID(r.Context(), id.UserID)
		if id.TouchDue {
			h.touch(r, id.TokenID)
		}

		page := pageOf(r)
		var version int64
		var urls []string
		if h.cfg.Trackers != nil {
			s := h.cfg.Trackers.Snapshot()
			version, urls = s.Version, s.URLs
		}
		body, hit, err := h.cache.get(r.Context(), cacheKey{page: page, version: version, feed: p.name}, func(ctx context.Context) ([]byte, error) {
			return h.build(ctx, p, page, urls)
		})
		switch {
		case err == nil:
		case errors.Is(err, context.Canceled):
			return
		case errors.Is(err, persistence.ErrStorageIO):
			// The database itself is failing: logged at ERROR, not as backpressure.
			server.MarkFault(r.Context())
			h.log.Error("rss feed storage error", "request_id", rid, "feed", p.name, "error_type", errKind(err))
			p.reply(w, http.StatusServiceUnavailable, 5)
			return
		case errors.Is(err, errBusy), errors.Is(err, context.DeadlineExceeded), errors.Is(err, persistence.ErrStatementTimeout),
			errors.Is(err, persistence.ErrBudgetExhausted):
			h.log.Warn("rss feed unavailable", "request_id", rid, "feed", p.name, "error_type", errKind(err))
			p.reply(w, http.StatusServiceUnavailable, 5)
			return
		default:
			h.log.Error("rss feed failed", "request_id", rid, "feed", p.name, "error_type", fmt.Sprintf("%T", err))
			p.reply(w, http.StatusInternalServerError, 0)
			return
		}
		h.log.Info("rss feed", "request_id", rid, "feed", p.name, "user_id", id.UserID, "page", page, "cache_hit", hit, "bytes", len(body))
		hd.Set("Content-Type", "application/rss+xml; charset=utf-8")
		hd.Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	})
}

var errBusy = errors.New("feed: generation capacity exhausted")

// build reads the page and renders it, within the generation gate (at most
// genSlots builds at once, bounded wait).
func (h *Handlers) build(ctx context.Context, p policy, page int, urls []string) ([]byte, error) {
	select {
	case h.gen <- struct{}{}:
		defer func() { <-h.gen }()
	case <-time.After(genWait):
		return nil, errBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	rows, err := h.cfg.Rows.LatestTorrents(ctx, (page-1)*PerPage, PerPage)
	if err != nil {
		return nil, err
	}
	return render(rows, urls, h.cfg.PublicBase, p.path, p.name == "v1", h.now())
}

func (h *Handlers) touch(r *http.Request, tokenID int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), touchTimeout)
	defer cancel()
	if err := h.cfg.Auth.TouchRSSToken(ctx, tokenID); err != nil {
		h.log.Warn("rss token last-use update failed", "request_id", server.RequestID(r.Context()), "error_type", errKind(err))
	}
}

func errKind(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	return "error"
}
