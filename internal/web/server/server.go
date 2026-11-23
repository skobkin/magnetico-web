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
	"strings"
	"sync"
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

	verifyTimeout      = 30 * time.Second
	readyPingTimeout   = 2 * time.Second
	inflightWait       = 2 * time.Second
	backendCloseWait   = 5 * time.Second
	backgroundStopWait = 5 * time.Second
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

	closeWait time.Duration // bound on backend.Close during shutdown

	// Startup hooks and the single background task are installed before Run
	// (auth.Install); see Run for their lifecycle.
	startup    []func(context.Context) error
	background func(context.Context)
	onDrained  []func(context.Context) error
	bgStopWait time.Duration // bound on waiting for the background task at shutdown

	baseCtx    context.Context
	baseCancel context.CancelFunc
}

// Access is the authentication class of a route. Every route is registered
// with one, so authentication and CSRF protection are structural: a handler
// cannot forget them (docs/wip/07D-web-auth-session-plan.md §6.1).
type Access int

const (
	// Public routes need no authentication and no CSRF protection; they may
	// answer only safe methods (GET/HEAD), enforced at registration.
	Public Access = iota
	// PublicForm routes are forms available before sign-in (login, and the
	// W6 registration/reset forms). Non-safe methods are CSRF-protected with
	// the pre-session token.
	PublicForm
	// Session routes require a valid session; non-safe methods are
	// CSRF-protected with the session token.
	Session
)

func (a Access) String() string {
	switch a {
	case Public:
		return "public"
	case PublicForm:
		return "public-form"
	case Session:
		return "session"
	}
	return "unknown"
}

// Guard wraps a handler with the authentication and CSRF behaviour of one
// Access class. They are supplied by the auth package (Router.SetGuards).
type Guard func(next http.Handler) http.Handler

// Route is one registered route, for the route-table tests.
type Route struct {
	Pattern string
	Access  Access
}

// Router registers routes with a deadline class and an access class. It is
// the single place new milestones add handlers, so none can forget the
// per-route deadlines or the authentication class.
type Router struct {
	mux    *http.ServeMux
	dl     deadlines
	guards map[Access]Guard
	routes []Route
}

// SetGuards installs the guards of the non-public access classes. It must be
// called before any route of those classes is registered.
func (r *Router) SetGuards(g map[Access]Guard) { r.guards = g }

// Routes lists the registered routes in registration order.
func (r *Router) Routes() []Route { return append([]Route(nil), r.routes...) }

func safeMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead }

// Handle registers pattern (a Go 1.22 method+path pattern) with the deadline
// class k and the access class a. It panics (at startup, never at request
// time) on a programming error: a Public route that is not restricted to
// GET/HEAD, or a guarded class whose guard was not installed.
func (r *Router) Handle(pattern string, k Kind, a Access, h http.Handler) {
	method, _, hasMethod := strings.Cut(pattern, " ")
	if a == Public && (!hasMethod || !safeMethod(method)) {
		panic("server: public route " + pattern + " must be GET or HEAD only; state-changing routes need PublicForm or Session")
	}
	if a != Public {
		g := r.guards[a]
		if g == nil {
			panic("server: no guard installed for " + a.String() + " route " + pattern)
		}
		h = g(h)
	}
	r.routes = append(r.routes, Route{Pattern: pattern, Access: a})
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

	s := &Server{cfg: cfg, log: cfg.Logger, backend: backend, assets: assets, renderer: renderer, closeWait: backendCloseWait, bgStopWait: backgroundStopWait}
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

// OnStartup registers a hook that runs after the database verification and
// before the listener opens; an error fails startup without listening. Used
// for work that must precede traffic (the password service's Prepare).
func (s *Server) OnStartup(f func(context.Context) error) { s.startup = append(s.startup, f) }

// SetBackground installs the single background task (the session purger). It
// is started by Run only after startup succeeded and the listener is open,
// and is cancelled and awaited before the database is closed.
func (s *Server) SetBackground(f func(context.Context)) { s.background = f }

// OnDrained registers a hook that runs at shutdown after the in-flight
// requests are drained or terminated and before the background task and the
// database are stopped. It lets a component that owns request-detached work
// (the file-index cache) stop it while the database is still open; the hook
// must return within the context it is given or report an error, which makes
// the shutdown incomplete.
func (s *Server) OnDrained(f func(context.Context) error) { s.onDrained = append(s.onDrained, f) }

// ErrorPage renders the generic error page for status (details never reach
// the client).
func (s *Server) ErrorPage(w http.ResponseWriter, r *http.Request, status int) {
	s.errorPage(w, r, status)
}

// Renderer returns the page renderer.
func (s *Server) Renderer() *ui.Renderer { return s.renderer }

// Logger returns the server logger.
func (s *Server) Logger() *slog.Logger { return s.log }

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
	rt.Handle("GET /healthz", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plain(w, http.StatusOK, "ok\n")
	}))
	rt.Handle("GET /readyz", KindRequest, Public, http.HandlerFunc(s.ready))
	rt.Handle("GET /robots.txt", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plain(w, http.StatusOK, "User-agent: *\nDisallow: /\n")
	}))
	rt.Handle("GET "+ui.StaticPrefix+"{file}", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	for i := 0; err == nil && i < len(s.startup); i++ {
		err = s.startup[i](vctx)
	}
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

	// The background task starts only now: verification, the startup hooks and
	// the listener all succeeded. From here every exit path calls
	// stopBackground before backend.Close.
	stopBackground := s.startBackground()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		// Serve returns only on failure here (Shutdown has not been called).
		drained := s.runDrained()
		stopped := stopBackground()
		s.backend.Close()
		if !stopped || !drained {
			// Keep the Serve failure for diagnostics and report the incomplete
			// shutdown too; errors.Is matches both.
			return fmt.Errorf("serve: %w (%w)", err, ErrShutdownIncomplete)
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "grace", s.cfg.ShutdownTimeout.String())
	return s.shutdown(srv, stopBackground)
}

// startBackground starts the background task, if any, under its own context
// (not baseCtx, whose cancellation is ordered by the grace logic) and returns
// a function that cancels it and waits for it, bounded by bgStopWait. The
// function reports whether the task stopped in time and is safe to call more
// than once.
func (s *Server) startBackground() (stop func() bool) {
	if s.background == nil {
		return func() bool { return true }
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.background(ctx) }()
	var stopped atomic.Bool
	var once sync.Once
	return func() bool {
		once.Do(func() {
			cancel()
			select {
			case <-done:
				stopped.Store(true)
			case <-time.After(s.bgStopWait):
				s.log.Warn("background task did not stop in time")
			}
		})
		return stopped.Load()
	}
}

// runDrained runs the OnDrained hooks, each bounded; it reports whether all
// completed.
func (s *Server) runDrained() bool {
	ok := true
	for _, f := range s.onDrained {
		hctx, hcancel := context.WithTimeout(context.Background(), s.bgStopWait)
		if err := f(hctx); err != nil {
			ok = false
			s.log.Warn("shutdown hook did not complete", "error_type", typeName(err))
		}
		hcancel()
	}
	return ok
}

func (s *Server) shutdown(srv *http.Server, stopBackground func() bool) error {
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

	if !s.runDrained() {
		incomplete = true
	}

	// In-flight requests are drained or terminated; now the purger, and only
	// then the database it uses.
	if !stopBackground() {
		incomplete = true
	}

	closed := make(chan struct{})
	go func() { s.backend.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(s.closeWait):
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
