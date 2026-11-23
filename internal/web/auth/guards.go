package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/problem"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/ui"
)

func safeMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead }

// redirect sends a 303 with an exact Location (http.Redirect would rewrite it).
func redirect(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusSeeOther)
}

// csrfDenied renders the generic 403 for a missing, invalid, expired or
// replayed token. Nothing of the submitted form is echoed.
func (a *Auth) csrfDenied(w http.ResponseWriter, r *http.Request, link string) {
	_ = a.srv.Renderer().Render(w, http.StatusForbidden, "message", ui.MessagePage{
		Heading:  "Request not allowed",
		Message:  "This form has expired or is invalid. Go back, reload the page and try again.",
		LinkHref: link, LinkText: "Go back",
	})
}

// unavailable renders the generic 503 (the user is never treated as anonymous
// or as authenticated when the database cannot answer).
func (a *Auth) unavailable(w http.ResponseWriter, r *http.Request, retryAfter string) {
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	a.srv.ErrorPage(w, r, http.StatusServiceUnavailable)
}

// formGuard is the guard of server.PublicForm: safe methods pass; every other
// method must carry the pre-session CSRF token.
func (a *Auth) formGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		pre, ok := a.readPre(r)
		if !ok || !formTokenOK(r, preCSRF(pre)) {
			a.csrfDenied(w, r, r.URL.Path)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sessionGuard is the guard of server.Session: the session is resolved in one
// database lookup per request (no cache); an anonymous or invalid session is
// redirected to /login with its cookie cleared; a database error is 503; and
// every non-safe method must carry the session-bound CSRF token.
func (a *Auth) sessionGuard(next http.Handler) http.Handler {
	return a.sessionGuardWith(next,
		func(w http.ResponseWriter, r *http.Request) { redirect(w, loginLocation(r)) },
		func(w http.ResponseWriter, r *http.Request) { a.unavailable(w, r, "") })
}

// sessionJSONGuard is the guard of server.SessionJSON: the same session
// resolution as sessionGuard, but failures are application/problem+json
// (401 for an anonymous or invalid session, 503 for a database error) rather
// than a redirect or an HTML page.
func (a *Auth) sessionJSONGuard(next http.Handler) http.Handler {
	return a.sessionGuardWith(next,
		func(w http.ResponseWriter, r *http.Request) {
			problem.Write(w, problem.Problem{Slug: problem.Unauthenticated, Title: "Authentication required",
				Status: http.StatusUnauthorized, Detail: "Sign in to use this endpoint."})
		},
		func(w http.ResponseWriter, r *http.Request) {
			problem.Write(w, problem.Problem{Slug: problem.TemporarilyBusy, Title: "Service temporarily unavailable",
				Status: http.StatusServiceUnavailable, RetryAfterSeconds: 5})
		})
}

func (a *Auth) sessionGuardWith(next http.Handler, onAnonymous, onUnavailable http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, hash, present, wellFormed := a.readSession(r)
		var sess accounts.Session
		var err error
		if wellFormed {
			sess, err = a.cfg.Store.LookupSession(r.Context(), hash, a.cfg.Lifetimes.Idle)
		} else {
			err = accounts.ErrUnauthenticated
		}
		if err != nil {
			if !errors.Is(err, accounts.ErrUnauthenticated) {
				a.log.Warn("session lookup failed", "request_id", server.RequestID(r.Context()), "error_type", typeName(err))
				onUnavailable(w, r)
				return
			}
			if present {
				a.clearSessionCookie(w)
			}
			onAnonymous(w, r)
			return
		}

		server.SetUserID(r.Context(), sess.UserID)
		if sess.TouchDue {
			if _, err := a.cfg.Store.TouchSession(r.Context(), hash, a.cfg.Lifetimes.Idle); err != nil {
				a.log.Warn("session touch failed", "request_id", server.RequestID(r.Context()), "error_type", typeName(err))
			}
		}
		if !safeMethod(r.Method) && !formTokenOK(r, sessionCSRF(token)) {
			a.csrfDenied(w, r, "/")
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, authed{
			Principal: Principal{UserID: sess.UserID, Username: sess.Username, Role: sess.Role, Remember: sess.Remember},
			token:     token, tokenHash: hash,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// loginLocation is where an anonymous request is sent: /login, remembering the
// requested page only for safe methods and only if it is a valid local path.
func loginLocation(r *http.Request) string {
	if safeMethod(r.Method) {
		if next := validNext(r.URL.RequestURI()); next != "" {
			return "/login?next=" + url.QueryEscape(next)
		}
	}
	return "/login"
}

// validNext returns s if it is a safe same-origin redirect target and "" otherwise
// (D4). Only a path that starts with exactly one "/" is accepted: no scheme or
// host, no "//" or "/\" (also after percent-decoding, since a decoded
// double slash or backslash could be re-interpreted), no backslash anywhere,
// no control characters, at most 2048 bytes.
func validNext(s string) string {
	if s == "" || len(s) > 2048 || s[0] != '/' {
		return ""
	}
	if len(s) > 1 && (s[1] == '/' || s[1] == '\\') {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f || s[i] == '\\' {
			return ""
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return ""
	}
	if p := u.Path; len(p) > 1 && (p[1] == '/' || p[1] == '\\') {
		return ""
	}
	if dec, err := url.PathUnescape(s); err != nil || (len(dec) > 1 && (dec[1] == '/' || dec[1] == '\\')) {
		return ""
	}
	return s
}

func typeName(v any) string { return typeOf(v) }
