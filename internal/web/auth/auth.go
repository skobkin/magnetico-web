// Package auth is the HTTP authentication layer of the web service (W3): the
// session and pre-session cookies, the cookie-bound CSRF tokens, the access
// class guards installed on the server Router, the login and logout handlers
// and the background purger. It holds no SQL (internal/web/accounts) and no
// hashing (internal/web/password); see docs/wip/07D-web-auth-session-plan.md.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/password"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/server"
)

// Store is what the auth layer needs from accounts.Store; tests substitute
// fakes for handler-shape tests and use the real store for flows.
type Store interface {
	LookupSession(ctx context.Context, tokenHash []byte, idle time.Duration) (accounts.Session, error)
	TouchSession(ctx context.Context, tokenHash []byte, idle time.Duration) (bool, error)
	DeleteSession(ctx context.Context, tokenHash []byte) error
	LoginCredential(ctx context.Context, username string) (accounts.Credential, error)
	CompleteLogin(ctx context.Context, p accounts.LoginParams) (accounts.LoginResult, error)
	PurgeSessions(ctx context.Context, idle time.Duration) (int64, error)
	PurgeExpiredLinks(ctx context.Context) (int64, error)
}

// Verifier is the password service (*password.Service).
type Verifier interface {
	Prepare(ctx context.Context) error
	Verify(ctx context.Context, pw, hash string) (bool, error)
	DummyVerify(ctx context.Context, pw string) error
	NeedsRehash(hash string) bool
	Rehash(ctx context.Context, pw string) (string, error)
}

var _ Verifier = (*password.Service)(nil)
var _ Store = (*accounts.Store)(nil)

// ErrPrepare wraps a failure to prepare the password service at startup (main
// maps it to a fixed message).
var ErrPrepare = errors.New("the password hashing service could not be prepared")

// Initial login limits (spec §4.6.1a, approved at the W3 plan review).
const (
	ipBurst      = 20
	ipInterval   = 30 * time.Second
	userBurst    = 5
	userInterval = 60 * time.Second
	limiterKeys  = 10_000

	defaultPurgeFirst = time.Minute
	defaultPurgeEvery = 15 * time.Minute
	purgeRunTimeout   = 30 * time.Second
)

// Config configures Install.
type Config struct {
	Store     Store
	Verifier  Verifier
	Lifetimes accounts.SessionLifetimes
	// DevMode selects the loopback development cookies (no __Host- prefix, no
	// Secure). It is derived from the public URL by opflags.
	DevMode bool

	// Optional overrides (tests). Nil/zero means the documented defaults.
	// RootHandler replaces the redirect from the exact root to /torrents.
	RootHandler http.Handler

	// RecoveryLink shows the "Forgot password?" link on the sign-in page; set
	// when email (and therefore /password/forgot) is available.
	RecoveryLink bool
	Now          func() time.Time // clock of the pre-session cookie
	IPLimiter    *ratelimit.Limiter
	UserLimiter  *ratelimit.Limiter
	PurgeFirst   time.Duration
	PurgeEvery   time.Duration
}

// Auth is the installed authentication layer.
type Auth struct {
	cfg     Config
	srv     *server.Server
	log     *slog.Logger
	now     func() time.Time
	ipLim   *ratelimit.Limiter
	userLim *ratelimit.Limiter
}

// Install wires the auth layer into srv: the access-class guards, the W3
// routes (GET/POST /login, POST /logout, GET /{$}), the startup hook that
// prepares the dummy password hash before any traffic is accepted, and the
// purger as the server's background task.
func Install(srv *server.Server, cfg Config) (*Auth, error) {
	if cfg.Store == nil || cfg.Verifier == nil {
		return nil, errors.New("auth: store and verifier are required")
	}
	if cfg.Lifetimes.Idle <= 0 || cfg.Lifetimes.Absolute <= 0 || cfg.Lifetimes.Remember <= 0 {
		return nil, errors.New("auth: session lifetimes must be positive")
	}
	a := &Auth{cfg: cfg, srv: srv, log: srv.Logger(), now: cfg.Now, ipLim: cfg.IPLimiter, userLim: cfg.UserLimiter}
	if a.now == nil {
		a.now = time.Now
	}
	var err error
	if a.ipLim == nil {
		if a.ipLim, err = ratelimit.New(ratelimit.Config{Burst: ipBurst, Interval: ipInterval, MaxKeys: limiterKeys}); err != nil {
			return nil, err
		}
	}
	if a.userLim == nil {
		if a.userLim, err = ratelimit.New(ratelimit.Config{Burst: userBurst, Interval: userInterval, MaxKeys: limiterKeys}); err != nil {
			return nil, err
		}
	}
	if a.cfg.PurgeFirst <= 0 {
		a.cfg.PurgeFirst = defaultPurgeFirst
	}
	if a.cfg.PurgeEvery <= 0 {
		a.cfg.PurgeEvery = defaultPurgeEvery
	}

	rt := srv.Router()
	rt.SetGuards(map[server.Access]server.Guard{
		server.PublicForm:  a.formGuard,
		server.Session:     a.sessionGuard,
		server.SessionJSON: a.sessionJSONGuard,
	})
	rt.Handle("GET /login", server.KindRequest, server.PublicForm, http.HandlerFunc(a.getLogin))
	rt.Handle("POST /login", server.KindRequest, server.PublicForm, http.HandlerFunc(a.postLogin))
	rt.Handle("POST /logout", server.KindRequest, server.Session, http.HandlerFunc(a.postLogout))
	// Exact root only: "GET /" would be a subtree pattern and would answer every
	// unknown GET path (/torrents before W4, legacy routes) instead of 404.
	var root http.Handler = http.HandlerFunc(a.home)
	if cfg.RootHandler != nil {
		root = cfg.RootHandler
	}
	rt.Handle("GET /{$}", server.KindRequest, server.Session, root)

	srv.OnStartup(a.startup)
	srv.AddBackground(a.Background)
	return a, nil
}

// startup runs before the listener opens. No login traffic is accepted before
// the dummy hash exists; a failure here fails startup.
func (a *Auth) startup(ctx context.Context) error {
	if err := a.cfg.Verifier.Prepare(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrPrepare, err)
	}
	if a.cfg.DevMode {
		a.log.Warn("development mode: session cookies are not Secure and carry no __Host- prefix; use only on a loopback address")
	}
	return nil
}

// Principal is the authenticated user of a request on a Session route.
type Principal struct {
	UserID   int64
	Username string
	Role     string
	Remember bool
}

type principalKey struct{}

type authed struct {
	Principal
	token     string // the cookie value (for the CSRF token)
	tokenHash []byte
}

// Current returns the authenticated principal of a request that passed the
// Session guard.
func Current(ctx context.Context) (Principal, bool) {
	a, ok := ctx.Value(principalKey{}).(authed)
	return a.Principal, ok
}

func authedOf(ctx context.Context) (authed, bool) {
	a, ok := ctx.Value(principalKey{}).(authed)
	return a, ok
}
