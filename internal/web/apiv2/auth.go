package apiv2

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/problem"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/server"
)

// Authenticator resolves and touches API tokens (accounts.Store).
type Authenticator interface {
	AuthenticateAPIToken(ctx context.Context, presented string) (accounts.Identity, error)
	TouchAPIToken(ctx context.Context, id int64) error
}

// Rate limits of the bearer guard (constants). Failures are limited per client
// address BEFORE the token is looked up and refunded on success, so a client
// with a valid token is never throttled by its own successful requests;
// successes are limited per token.
const (
	failBurst    = 20
	failInterval = 10 * time.Second
	useBurst     = 120
	useInterval  = 500 * time.Millisecond
	limiterKeys  = 10000
	touchTimeout = 2 * time.Second

	kindToken = 'T'
)

// tokenFromHeader extracts the credential from exactly one Authorization
// header of the form "Bearer <token>" (scheme case-insensitive, one space).
// Nothing else is a credential: not a cookie, not a query parameter, not
// another scheme. present reports that an Authorization header was sent.
func tokenFromHeader(r *http.Request) (token string, present, ok bool) {
	vals := r.Header.Values("Authorization")
	if len(vals) == 0 {
		return "", false, false
	}
	if len(vals) != 1 {
		return "", true, false
	}
	scheme, rest, found := strings.Cut(vals[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || rest == "" || strings.ContainsAny(rest, " \t") || len(rest) > 256 {
		return "", true, false
	}
	if !strings.HasPrefix(rest, accounts.APITokenPrefix) {
		return "", true, false
	}
	return rest, true, true
}

type guard struct {
	auth    Authenticator
	failLim *ratelimit.Limiter
	useLim  *ratelimit.Limiter
	log     *slog.Logger
}

func unauthorized(w http.ResponseWriter, presented bool) {
	challenge := `Bearer realm="magnetico"`
	if presented {
		challenge += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
	problem.Write(w, problem.Problem{Slug: problem.Unauthenticated, Title: "Authentication required",
		Status: http.StatusUnauthorized, Detail: "A valid API token is required in the Authorization header (Bearer scheme)."})
}

func limited(w http.ResponseWriter, retry time.Duration) {
	problem.Write(w, problem.Problem{Slug: problem.RateLimited, Title: "Too many requests",
		Status: http.StatusTooManyRequests, Detail: "Slow down and retry later.",
		RetryAfterSeconds: ratelimit.RetryAfterSeconds(retry)})
}

// handle is the server.Guard of the APIToken access class: Bearer API tokens
// only, checked in one lookup (existence, expiry, revocation, owner active) on
// every request, with no authentication cache.
func (g *guard) handle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := g.failLim.Reserve(ratelimit.IPKey(server.ClientIP(r.Context())))
		if !fail.OK {
			limited(w, fail.RetryAfter)
			return
		}
		token, present, ok := tokenFromHeader(r)
		if !ok {
			unauthorized(w, present)
			return
		}
		id, err := g.auth.AuthenticateAPIToken(r.Context(), token)
		switch {
		case errors.Is(err, accounts.ErrUnauthenticated):
			unauthorized(w, true)
			return
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			g.log.Warn("api token lookup failed", "request_id", server.RequestID(r.Context()), "error_type", errKind(err))
			fail.Cancel() // not the client's failure
			problem.Write(w, problem.Problem{Slug: problem.TemporarilyBusy, Title: "Temporarily unavailable",
				Status: http.StatusServiceUnavailable, Detail: "Please try again shortly.", RetryAfterSeconds: 5})
			return
		}
		fail.Cancel() // a success does not count against the address
		if use := g.useLim.Reserve(ratelimit.KeyOf(kindToken, strconv.FormatInt(id.TokenID, 10))); !use.OK {
			limited(w, use.RetryAfter)
			return
		}
		server.SetUserID(r.Context(), id.UserID)
		if id.TouchDue {
			g.touch(r, id.TokenID)
		}
		next.ServeHTTP(w, r)
	})
}

// touch records the use at most once per accounts.TokenTouchInterval. It is
// best effort: a failure is logged by category and never fails the request.
func (g *guard) touch(r *http.Request, tokenID int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), touchTimeout)
	defer cancel()
	if err := g.auth.TouchAPIToken(ctx, tokenID); err != nil {
		g.log.Warn("api token last-use update failed", "request_id", server.RequestID(r.Context()), "error_type", errKind(err))
	}
}

func errKind(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	return "error"
}
