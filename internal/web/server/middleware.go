package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime/debug"
	"time"
)

// maxBodyBytes bounds request bodies of every route (forms and small JSON).
const maxBodyBytes = 1 << 20

// reqState is the per-request record shared by the middleware: the access
// log (outer) reads what later layers (inner) learn.
type reqState struct {
	id       string
	clientIP netip.Addr
	route    string
}

type ctxKey struct{}

func stateOf(ctx context.Context) *reqState {
	if s, ok := ctx.Value(ctxKey{}).(*reqState); ok {
		return s
	}
	return &reqState{} // outside the chain (unit tests of handlers)
}

// RequestID returns the request ID of ctx ("" outside the middleware chain).
func RequestID(ctx context.Context) string { return stateOf(ctx).id }

// ClientIP returns the resolved client address of ctx (zero outside the chain).
func ClientIP(ctx context.Context) netip.Addr { return stateOf(ctx).clientIP }

// statusWriter records the status and size of a response. Unwrap keeps
// http.ResponseController (write deadlines, flushing) working through it.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// withRequestID is the outermost layer: it always generates a fresh ID (an
// incoming X-Request-ID is never trusted into logs), exposes it as a response
// header, and installs the shared per-request state.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := &reqState{id: newRequestID()}
		w.Header().Set("X-Request-ID", st.id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, st)))
	})
}

// accessLog writes one secret-safe line per request. It logs the method, the
// matched route pattern, status, size, duration and client address, and
// deliberately nothing taken from the URL, headers or cookies: no path (path
// segments can carry codes), no query string (feed_token, api-token), no
// Authorization, Cookie or Referer values.
func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)

			st := stateOf(r.Context())
			route := st.route
			if route == "" {
				route = "-"
			}
			level := slog.LevelInfo
			switch {
			case sw.status >= 500:
				level = slog.LevelError
			case route == "GET /healthz" || route == "GET /readyz":
				level = slog.LevelDebug
			}
			log.LogAttrs(r.Context(), level, "request",
				slog.String("request_id", st.id),
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.Int("status", sw.status),
				slog.Int64("bytes", sw.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("client_ip", st.clientIP.String()),
			)
		})
	}
}

// recoverer turns a handler panic into a generic 500 carrying the request ID;
// the panic value is never rendered to the client or logged (only its type
// and the stack, which carry no request data).
func recoverer(log *slog.Logger, fail func(http.ResponseWriter, *http.Request, int)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w}
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.LogAttrs(r.Context(), slog.LevelError, "panic in handler",
					slog.String("request_id", RequestID(r.Context())),
					slog.String("panic_type", typeName(v)),
					slog.String("stack", string(debug.Stack())),
				)
				if !sw.wrote {
					fail(w, r, http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// securityHeaders sets the headers every response class carries (spec §3.4,
// §3.6), including errors produced further in: they are set before the
// handler runs. Handlers may override Cache-Control (static assets do).
func securityHeaders(hsts bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Robots-Tag", "noindex, nofollow")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			h.Set("Content-Security-Policy",
				"default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cache-Control", "no-store")
			if hsts {
				h.Set("Strict-Transport-Security", "max-age=31536000")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// resolveClientIP stores the trusted-proxy-aware client address.
func resolveClientIP(proxies []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stateOf(r.Context()).clientIP = ResolveClientIP(r.RemoteAddr, r.Header.Values("X-Forwarded-For"), proxies)
			next.ServeHTTP(w, r)
		})
	}
}

// crossOrigin rejects cross-origin state-changing requests (CSRF layer one;
// the per-form synchronizer token is added with authentication in W3). The
// public origin is trusted explicitly because a reverse proxy may rewrite the
// Host header the stdlib compares Origin against.
func crossOrigin(publicOrigin string, deny http.Handler) (func(http.Handler) http.Handler, error) {
	p := http.NewCrossOriginProtection()
	if err := p.AddTrustedOrigin(publicOrigin); err != nil {
		return nil, err
	}
	p.SetDenyHandler(deny)
	return p.Handler, nil
}

// limitBody caps request bodies.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// Kind selects the deadline class of a route.
type Kind int

const (
	// KindRequest routes get the ordinary request deadline.
	KindRequest Kind = iota
	// KindSearch routes (UI and API search) get the SQL search deadline.
	KindSearch
)

// Per-route write margins: time after the handler deadline still allowed to
// write the response (a timeout answer, or rendering after a search).
const (
	requestWriteMargin = 5 * time.Second
	searchWriteMargin  = 10 * time.Second // spec §2.9
)

// deadlines is the effective per-class deadline table.
type deadlines struct {
	request, search             time.Duration
	requestMargin, searchMargin time.Duration
}

func (d deadlines) of(k Kind) (handler, write time.Duration) {
	if k == KindSearch {
		return d.search, d.search + d.searchMargin
	}
	return d.request, d.request + d.requestMargin
}

// routeDeadline labels the request with its route pattern and applies the
// class deadline: a context deadline for the handler and, through
// http.ResponseController, a write deadline. There is no server-wide
// WriteTimeout, which would cut long searches short.
func routeDeadline(pattern string, k Kind, d deadlines) func(http.Handler) http.Handler {
	hd, wd := d.of(k)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stateOf(r.Context()).route = pattern
			ctx, cancel := context.WithTimeout(r.Context(), hd)
			defer cancel()
			// Unsupported writers (tests) simply keep no write deadline.
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wd))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
