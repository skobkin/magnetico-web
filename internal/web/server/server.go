// Package server is the HTTP foundation of `magnetico web` (milestone W1):
// lifecycle, middleware, per-route deadlines, health and robots routes and
// the embedded static assets. It contains no authentication, search or
// account handling; later milestones register their routes on the Router.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/ui"
)

// ErrShutdownIncomplete means the grace period expired with work still
// running (or a resource refusing to close); the caller should exit with a
// distinct non-zero status, as the other services do.
var ErrShutdownIncomplete = errors.New("shutdown incomplete: the grace period expired before every in-flight request finished")

// Fixed transport safeguards. There is deliberately no WriteTimeout: write
// deadlines are per route (spec §2.9).
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 16 << 10

	verifyTimeout    = 30 * time.Second
	readyPingTimeout = 2 * time.Second
	inflightWait     = 2 * time.Second
	backendCloseWait = 5 * time.Second
)

// Backend is what the foundation needs from the database layer. Production
// uses webdb; tests inject fakes.
type Backend interface {
	// Verify is the read-only startup verification. Any failure is fatal.
	Verify(ctx context.Context) error
	// Ping reports liveness of the database for readiness probes.
	Ping(ctx context.Context) error
	// Close releases the resources; it may block until connections return.
	Close()
}

// Config is the runtime configuration of the server.
type Config struct {
	Listen   string       // used when Listener is nil
	Listener net.Listener // optional pre-bound listener (tests)

	PublicURL      *url.URL // scheme://host only
	TrustedProxies []netip.Prefix

	RequestTimeout  time.Duration
	SearchTimeout   time.Duration
	ShutdownTimeout time.Duration

	Logger *slog.Logger
}

// Server is the web service foundation.
type Server struct {
	cfg      Config
	log      *slog.Logger
	backend  Backend
	router   *Router
	handler  http.Handler
	assets   *ui.Assets
	renderer *ui.Renderer

	draining atomic.Bool
	inflight atomic.Int64

	baseCtx    context.Context
	baseCancel context.CancelFunc
}

// Router registers routes with a deadline class. It is the single place new
// milestones add handlers, so none can forget the per-route deadlines.
type Router struct {
	mux *http.ServeMux
	dl  deadlines
}

// Handle registers pattern (a Go 1.22 method+path pattern) with the deadline
// class k.
func (r *Router) Handle(pattern string, k Kind, h http.Handler) {
	r.mux.Handle(pattern, routeDeadline(pattern, k, r.dl)(h))
}

// New builds the server and its foundation routes. It does not touch the
// network or the database.
func New(cfg Config, backend Backend) (*Server, error) {
	if cfg.Logger == nil || backend == nil || cfg.PublicURL == nil {
		return nil, errors.New("server: logger, backend and public URL are required")
	}
	if cfg.RequestTimeout <= 0 || cfg.SearchTimeout <= 0 || cfg.ShutdownTimeout <= 0 {
		return nil, errors.New("server: timeouts must be positive")
	}
	assets, err := ui.NewAssets()
	if err != nil {
		return nil, fmt.Errorf("server: static assets: %w", err)
	}
	renderer, err := ui.NewRenderer(assets)
	if err != nil {
		return nil, fmt.Errorf("server: templates: %w", err)
	}

	s := &Server{cfg: cfg, log: cfg.Logger, backend: backend, assets: assets, renderer: renderer}
	s.baseCtx, s.baseCancel = context.WithCancel(context.Background())
	s.router = &Router{mux: http.NewServeMux(), dl: deadlines{
		request: cfg.RequestTimeout, search: cfg.SearchTimeout,
		requestMargin: requestWriteMargin, searchMargin: searchWriteMargin,
	}}
	s.registerFoundationRoutes()

	origin := (&url.URL{Scheme: cfg.PublicURL.Scheme, Host: cfg.PublicURL.Host}).String()
	cross, err := crossOrigin(origin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.errorPage(w, r, http.StatusForbidden)
	}))
	if err != nil {
		return nil, fmt.Errorf("server: public origin: %w", err)
	}

	// Outermost first. Deviation from the spec §3.1 listing: the access log
	// wraps the panic recoverer (not the reverse) so that a recovered panic is
	// logged with its final 500 status; the response headers are set before
	// the handler runs, so the recovered response still carries them.
	chain := []func(http.Handler) http.Handler{
		s.track,
		withRequestID,
		accessLog(s.log),
		recoverer(s.log, s.errorPage),
		securityHeaders(cfg.PublicURL.Scheme == "https"),
		resolveClientIP(cfg.TrustedProxies),
		cross,
		limitBody,
	}
	var h http.Handler = s.router.mux
	for i := len(chain) - 1; i >= 0; i-- {
		h = chain[i](h)
	}
	s.handler = h
	return s, nil
}

// Router returns the route registry.
func (s *Server) Router() *Router { return s.router }

// Handler returns the complete handler chain (for tests and Run).
func (s *Server) Handler() http.Handler { return s.handler }

// track counts in-flight requests for the shutdown report.
func (s *Server) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.inflight.Add(1)
		defer s.inflight.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// errorPage renders the generic error page; details never reach the client.
func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int) {
	heading, msg := http.StatusText(status), "The request could not be completed."
	if status >= 500 {
		msg = "Something went wrong on our side."
	}
	err := s.renderer.Render(w, status, "error", ui.ErrorPage{
		Status: status, Heading: heading, Message: msg, RequestID: RequestID(r.Context()),
	})
	if err != nil {
		s.log.Error("render error page failed", "request_id", RequestID(r.Context()))
		http.Error(w, heading, status)
	}
}

func (s *Server) registerFoundationRoutes() {
	rt := s.router
	rt.Handle("GET /healthz", KindRequest, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plain(w, http.StatusOK, "ok\n")
	}))
	rt.Handle("GET /readyz", KindRequest, http.HandlerFunc(s.ready))
	rt.Handle("GET /robots.txt", KindRequest, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plain(w, http.StatusOK, "User-agent: *\nDisallow: /\n")
	}))
	rt.Handle("GET "+ui.StaticPrefix+"{file}", KindRequest, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.assets.ServeFile(w, r, r.PathValue("file"))
	}))
}

func plain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// ready is the readiness probe: the process must not be draining and the
// database must answer. The body never carries version, configuration or
// database detail; the reason goes to the log only.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		plain(w, http.StatusServiceUnavailable, "not ready\n")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readyPingTimeout)
	defer cancel()
	if err := s.backend.Ping(ctx); err != nil {
		s.log.Warn("readiness check failed", "request_id", RequestID(r.Context()), "error_type", typeName(err))
		plain(w, http.StatusServiceUnavailable, "not ready\n")
		return
	}
	plain(w, http.StatusOK, "ready\n")
}

// Run verifies the database, serves until ctx is cancelled and then shuts
// down within the grace period. Run owns the backend and always closes it.
// Verification failures are returned wrapped (errors.Is/As keep working)
// before anything listens. After a forced stop the error is
// ErrShutdownIncomplete.
func (s *Server) Run(ctx context.Context) error {
	defer s.baseCancel()

	vctx, vcancel := context.WithTimeout(ctx, verifyTimeout)
	err := s.backend.Verify(vctx)
	vcancel()
	if err != nil {
		s.backend.Close()
		return fmt.Errorf("startup verification: %w", err)
	}

	ln := s.cfg.Listener
	if ln == nil {
		if ln, err = net.Listen("tcp", s.cfg.Listen); err != nil {
			s.log.Error("listen failed", "error", err.Error()) // address and errno only
			s.backend.Close()
			return fmt.Errorf("listen: %w", err)
		}
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return s.baseCtx },
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	s.logStartup(ln.Addr())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		// Serve returns only on failure here (Shutdown has not been called).
		s.backend.Close()
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "grace", s.cfg.ShutdownTimeout.String())
	return s.shutdown(srv)
}

func (s *Server) shutdown(srv *http.Server) error {
	s.draining.Store(true)
	incomplete := false

	grace, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(grace); err != nil {
		// Grace expired: cancel the remaining request contexts and drop
		// their connections.
		incomplete = true
		s.log.Warn("grace period expired; cancelling in-flight requests", "in_flight", s.inflight.Load())
		s.baseCancel()
		_ = srv.Close()
		deadline := time.Now().Add(inflightWait)
		for s.inflight.Load() > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}

	closed := make(chan struct{})
	go func() { s.backend.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(backendCloseWait):
		incomplete = true
		s.log.Warn("database resources did not close in time")
	}

	if incomplete {
		return ErrShutdownIncomplete
	}
	s.log.Info("stopped cleanly")
	return nil
}

// logStartup records the effective deadlines (spec §2.9) and the proxy
// posture. It never prints the database URL or any secret.
func (s *Server) logStartup(addr net.Addr) {
	reqHandler, reqWrite := s.router.dl.of(KindRequest)
	searchHandler, searchWrite := s.router.dl.of(KindSearch)
	s.log.Info("web service listening",
		"addr", addr.String(),
		"public_origin", s.cfg.PublicURL.String(),
		"trusted_proxies", len(s.cfg.TrustedProxies),
		"request_deadline", reqHandler.String(), "request_write_deadline", reqWrite.String(),
		"search_deadline", searchHandler.String(), "search_write_deadline", searchWrite.String(),
	)
	s.log.Info("operational requirement: reverse-proxy response and idle timeouts must exceed the search write deadline",
		"minimum", searchWrite.String())
}
