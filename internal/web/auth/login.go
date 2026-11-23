package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/password"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/ui"
)

// Generic messages. Every failed attempt, whatever the account condition,
// shows the same one (07A §7.2).
const (
	msgLoginFailed  = "Wrong username or password, or the account is unavailable."
	msgLoginLimited = "Too many sign-in attempts. Please wait and try again."
	busyRetryAfter  = "5"
)

// loginNotices are the fixed texts the sign-in page can show after another
// flow redirected to /login?notice=<slug>. Unknown slugs show nothing.
var loginNotices = map[string]string{
	"registered":          "Your account was created. Sign in to continue.",
	"registered-mailfail": "Your account was created, but the confirmation email could not be sent right now. Sign in and request it again from your account page.",
	"password-set":        "Your password was set and every session, API token and RSS token of the account was revoked. Sign in, then create new API and RSS tokens and update your torrent clients.",
}

func (a *Auth) renderLogin(w http.ResponseWriter, status int, p ui.LoginPage) {
	p.ForgotLink = a.cfg.RecoveryLink
	_ = a.srv.Renderer().Render(w, status, "login", p)
}

// getLogin shows the sign-in form. A visitor who already has a valid session
// goes to the home page; otherwise the pre-session cookie that binds the form's
// CSRF token is issued or reused.
func (a *Auth) getLogin(w http.ResponseWriter, r *http.Request) {
	if _, hash, _, wellFormed := a.readSession(r); wellFormed {
		_, err := a.cfg.Store.LookupSession(r.Context(), hash, a.cfg.Lifetimes.Idle)
		switch {
		case err == nil:
			redirect(w, "/")
			return
		case !errors.Is(err, accounts.ErrUnauthenticated):
			a.log.Warn("session lookup failed", "request_id", server.RequestID(r.Context()), "error_type", typeName(err))
			a.unavailable(w, r, "")
			return
		}
	}
	pre := a.ensurePre(w, r)
	a.renderLogin(w, http.StatusOK, ui.LoginPage{CSRFToken: preCSRF(pre), Next: validNext(r.URL.Query().Get("next")), Notice: loginNotices[r.URL.Query().Get("notice")]})
}

type outcome int

const (
	outOK outcome = iota
	outFail
	outBusy
	outUnavailable
)

// postLogin handles a sign-in. The router has already verified the pre-session
// CSRF token. Order: rate limit, then (outside any database transaction)
// verification, rehash, and finally one conditional commit that creates the
// session (accounts.CompleteLogin).
func (a *Auth) postLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.srv.ErrorPage(w, r, http.StatusBadRequest)
		return
	}
	username := r.PostForm.Get("username")
	pw := r.PostForm.Get("password")
	remember := r.PostForm.Get("remember") != ""
	next := validNext(r.PostForm.Get("next"))
	pre, _ := a.readPre(r) // present: the guard validated it
	page := func() ui.LoginPage {
		return ui.LoginPage{CSRFToken: preCSRF(pre), Next: next, Username: username, Remember: remember}
	}

	// Rate limit before any database access or hashing: per client IP and per
	// normalised username, identical for existing and unknown names.
	pair := ratelimit.ReservePair(a.ipLim, ratelimit.IPKey(server.ClientIP(r.Context())), a.userLim, ratelimit.UserKey(username))
	if !pair.OK {
		w.Header().Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(pair.RetryAfter)))
		p := page()
		p.Error = msgLoginLimited
		a.renderLogin(w, http.StatusTooManyRequests, p)
		return
	}

	var replace []byte
	if _, h, _, ok := a.readSession(r); ok {
		replace = h // a presented session is deleted in the login transaction (fixation)
	}
	res, userID, out := a.attempt(r.Context(), username, pw, replace, remember, server.RequestID(r.Context()))
	switch out {
	case outBusy:
		a.unavailable(w, r, busyRetryAfter)
	case outUnavailable:
		a.unavailable(w, r, "")
	case outFail:
		p := page()
		p.Error = msgLoginFailed
		a.renderLogin(w, http.StatusUnauthorized, p)
	case outOK:
		pair.B.Cancel() // a success does not count against the username
		a.setSessionCookie(w, res)
		a.clearPreCookie(w)
		a.log.Info("login succeeded", "request_id", server.RequestID(r.Context()), "user_id", userID, "remember", remember)
		if next == "" {
			next = "/"
		}
		redirect(w, next)
	}
}

// dummy spends one verification's worth of work for an account that cannot log
// in (unknown, inactive, no password, unsupported hash).
func (a *Auth) dummy(ctx context.Context, pw string, reqID string) outcome {
	err := a.cfg.Verifier.DummyVerify(ctx, pw)
	switch {
	case err == nil:
		return outFail
	case errors.Is(err, password.ErrBusy):
		return outBusy
	case errors.Is(err, password.ErrNotPrepared):
		a.log.Error("password service not prepared", "request_id", reqID)
	}
	return outUnavailable
}

// attempt verifies the password and commits the login. It never reveals which
// account condition failed: every refusal is outFail after exactly one
// limiter-admitted verification (Verify or DummyVerify); infrastructure
// failures are outBusy/outUnavailable.
func (a *Auth) attempt(ctx context.Context, username, pw string, replace []byte, remember bool, reqID string) (accounts.LoginResult, int64, outcome) {
	var none accounts.LoginResult
	if pw == "" || len(pw) > password.MaxLoginLength {
		return none, 0, outFail // account-independent: nothing to verify
	}
	cred, err := a.cfg.Store.LoginCredential(ctx, username)
	switch {
	case errors.Is(err, accounts.ErrNotFound):
		return none, 0, a.dummy(ctx, pw, reqID)
	case err != nil:
		a.log.Warn("login credential lookup failed", "request_id", reqID, "error_type", typeName(err))
		return none, 0, outUnavailable
	case cred.Hash == nil || !cred.Active:
		return none, 0, a.dummy(ctx, pw, reqID)
	}

	if o := a.verify(ctx, pw, cred, reqID); o != outOK {
		return none, 0, o
	}

	// Rehash outside any transaction. If the limiter refuses the second
	// computation the login still completes, unchanged hash, upgraded at a
	// later login (approved exception to spec §4.3.4).
	var newHash string
	if a.cfg.Verifier.NeedsRehash(*cred.Hash) {
		nh, rerr := a.cfg.Verifier.Rehash(ctx, pw)
		switch {
		case rerr == nil:
			newHash = nh
		case errors.Is(rerr, password.ErrBusy):
			a.log.Info("password rehash skipped: hashing capacity busy", "request_id", reqID, "user_id", cred.UserID)
		default:
			return none, 0, outUnavailable
		}
	}

	params := accounts.LoginParams{
		UserID: cred.UserID, VerifiedHash: *cred.Hash, NewHash: newHash,
		Remember: remember, Lifetimes: a.cfg.Lifetimes, ReplaceSession: replace,
	}
	res, err := a.cfg.Store.CompleteLogin(ctx, params)
	if errors.Is(err, accounts.ErrLoginStale) {
		return a.retryStale(ctx, username, pw, params, reqID)
	}
	if err != nil {
		a.log.Warn("login commit failed", "request_id", reqID, "error_type", typeName(err))
		return none, 0, outUnavailable
	}
	return res, cred.UserID, outOK
}

// verify checks pw against the stored hash. An unsupported stored hash is
// never accepted and, to keep the cost equal to any other failure, is followed
// by a dummy verification.
func (a *Auth) verify(ctx context.Context, pw string, cred accounts.Credential, reqID string) outcome {
	ok, err := a.cfg.Verifier.Verify(ctx, pw, *cred.Hash)
	switch {
	case errors.Is(err, password.ErrBusy):
		return outBusy
	case errors.Is(err, password.ErrUnsupportedHash):
		a.log.Warn("login refused: stored hash is in an unsupported format (account needs a password reset)",
			"request_id", reqID, "user_id", cred.UserID)
		return a.dummy(ctx, pw, reqID)
	case err != nil:
		return outUnavailable
	case !ok:
		return outFail
	}
	return outOK
}

// retryStale is the single bounded retry after ErrLoginStale: the account's
// hash changed between verification and commit. If it was replaced by a
// password change or reset the password no longer verifies and the attempt
// fails generically; if it was only rehashed by a concurrent login with the
// same password (two tabs) the retry succeeds. Never more than one retry.
func (a *Auth) retryStale(ctx context.Context, username, pw string, first accounts.LoginParams, reqID string) (accounts.LoginResult, int64, outcome) {
	var none accounts.LoginResult
	cred, err := a.cfg.Store.LoginCredential(ctx, username)
	switch {
	case errors.Is(err, accounts.ErrNotFound):
		return none, 0, outFail
	case err != nil:
		return none, 0, outUnavailable
	case cred.Hash == nil || !cred.Active || *cred.Hash == first.VerifiedHash || cred.UserID != first.UserID:
		return none, 0, outFail
	}
	if o := a.verify(ctx, pw, cred, reqID); o != outOK {
		return none, 0, o
	}
	second := first
	second.VerifiedHash, second.NewHash = *cred.Hash, "" // already current; no second rehash
	res, err := a.cfg.Store.CompleteLogin(ctx, second)
	switch {
	case errors.Is(err, accounts.ErrLoginStale):
		return none, 0, outFail
	case err != nil:
		a.log.Warn("login commit failed", "request_id", reqID, "error_type", typeName(err))
		return none, 0, outUnavailable
	}
	return res, cred.UserID, outOK
}

// postLogout deletes the server-side session and clears the cookie. A failed
// delete is reported and the cookie is kept, so a retry can still invalidate
// the session. A replay with an already deleted session never reaches here
// (the Session guard redirects it).
func (a *Auth) postLogout(w http.ResponseWriter, r *http.Request) {
	au, _ := authedOf(r.Context())
	if err := a.cfg.Store.DeleteSession(r.Context(), au.tokenHash); err != nil {
		a.log.Warn("logout failed", "request_id", server.RequestID(r.Context()), "error_type", typeName(err))
		w.Header().Set("Retry-After", busyRetryAfter)
		_ = a.srv.Renderer().Render(w, http.StatusServiceUnavailable, "message", ui.MessagePage{
			Heading: "Sign-out could not be completed", Message: "Please try again.", LinkHref: "/", LinkText: "Back",
		})
		return
	}
	a.clearSessionCookie(w)
	redirect(w, "/login")
}

// home redirects the exact root of a signed-in session to the torrent list.
func (a *Auth) home(w http.ResponseWriter, r *http.Request) {
	redirect(w, "/torrents")
}

// SessionFormToken returns the session-bound CSRF token for forms on pages
// served on Session routes (the logout form). It is empty outside them.
func SessionFormToken(r *http.Request) string {
	au, ok := authedOf(r.Context())
	if !ok {
		return ""
	}
	return sessionCSRF(au.token)
}
