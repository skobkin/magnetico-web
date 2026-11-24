// Package account serves the browser side of account management
// (docs/specs/web-v1.md §4.5, §4.9, §6.1, §8.2.7, §11.4): registration through
// an invitation, password setup and reset pages, forgot-password, email
// confirmation, and the signed-in account page with password, email, token and
// invitation management.
//
// It is a thin layer over accounts.Store, the password service, the mail queue
// and the auth package (sessions, CSRF). It owns no business rule that the
// store enforces under row locks. Every state-changing action is a POST behind
// the router's CSRF guard (the pre-session token for public forms, the session
// token for the account page).
//
// Logging: only request IDs, user IDs and fixed outcome slugs. Never a
// password, token, link, invitation code, email address or username typed into
// a public form.
package account

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	webauth "github.com/skobkin/magnetico-web/internal/web/auth"
	"github.com/skobkin/magnetico-web/internal/web/mail"
	"github.com/skobkin/magnetico-web/internal/web/password"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/ui"
)

// Store is the part of accounts.Store the handlers use.
type Store interface {
	GetUserByID(ctx context.Context, id int64) (accounts.User, error)
	CheckInvite(ctx context.Context, code string) (bool, error)
	RegisterWithInvite(ctx context.Context, p accounts.RegisterParams) (int64, error)
	CompletePasswordLink(ctx context.Context, token, purpose, newHash string) (accounts.CompleteResult, error)
	CompleteEmailVerify(ctx context.Context, token string) (accounts.EmailVerifyResult, error)
	IssueEmailVerifyLink(ctx context.Context, userID int64, email string) (accounts.Link, error)
	IssueRecoveryLink(ctx context.Context, email string) (accounts.RecoveryLink, error)
	PendingEmail(ctx context.Context, userID int64) (string, bool, error)
	RemoveEmail(ctx context.Context, userID int64) (accounts.EmailRemoveResult, error)
	ChangePassword(ctx context.Context, p accounts.ChangePasswordParams) (accounts.ChangePasswordResult, error)
	CreateAPIToken(ctx context.Context, userID int64, label string, expiresAt *time.Time) (accounts.CreatedToken, error)
	CreateRSSToken(ctx context.Context, userID int64, label string, expiresAt *time.Time) (accounts.CreatedToken, error)
	ListAPITokens(ctx context.Context, userID int64) ([]accounts.TokenInfo, error)
	ListRSSTokens(ctx context.Context, userID int64) ([]accounts.TokenInfo, error)
	RevokeAPIToken(ctx context.Context, userID, id int64) error
	RevokeRSSToken(ctx context.Context, userID, id int64) error
	ListInvitesByOwner(ctx context.Context, ownerID int64) ([]accounts.Invite, error)
	RevokeOwnInvite(ctx context.Context, ownerID, id int64) (accounts.RevokeOutcome, error)
}

// Passwords is the part of password.Service the handlers use.
type Passwords interface {
	Hash(ctx context.Context, pw string) (string, error)
	Verify(ctx context.Context, pw, hash string) (bool, error)
}

// Config wires the handlers.
type Config struct {
	Store     Store
	Passwords Passwords
	Auth      *webauth.Auth
	// Mail is nil when SMTP is not configured: every email-dependent control
	// is then shown as unavailable and no recovery mail is ever queued.
	Mail *mail.Queue
	// PublicBase is the configured PUBLIC_URL; every absolute link is built
	// from it, never from the Host header.
	PublicBase  *url.URL
	InviteQuota int

	// Test seams.
	Now func() time.Time
	// Limiters override the defaults when non-nil.
	RegisterLimiter, LinkLimiter, ForgotIPLimiter, ForgotEmailLimiter, UserLimiter, TokenLimiter *ratelimit.Limiter
}

// Fixed limits (constants, not configuration).
const (
	maxEmailLen   = 254
	dbStepTimeout = 15 * time.Second
)

// Rate-limit kinds for ratelimit.KeyOf (kinds 0, 4, 6 and 'u' belong to IPKey
// and UserKey).
const (
	kindForgotEmail = 'e'
	kindAccount     = 'a'
	kindAPITokens   = 'A'
	kindRSSTokens   = 'R'
)

// Handlers is the installed account feature.
type Handlers struct {
	cfg Config
	srv *server.Server
	log *slog.Logger
	now func() time.Time

	regLim, linkLim, forgotIPLim, forgotMailLim, userLim, tokenLim *ratelimit.Limiter
}

func newLimiter(override *ratelimit.Limiter, burst int, every time.Duration) (*ratelimit.Limiter, error) {
	if override != nil {
		return override, nil
	}
	return ratelimit.New(ratelimit.Config{Burst: burst, Interval: every, MaxKeys: 10000})
}

// Install registers the routes. The auth package must have been installed
// first (it installs the access guards).
func Install(srv *server.Server, cfg Config) (*Handlers, error) {
	if cfg.Store == nil || cfg.Passwords == nil || cfg.Auth == nil || cfg.PublicBase == nil {
		return nil, errors.New("account: store, passwords, auth and public base URL are required")
	}
	h := &Handlers{cfg: cfg, srv: srv, log: srv.Logger(), now: cfg.Now}
	if h.now == nil {
		h.now = time.Now
	}
	var err error
	for _, l := range []struct {
		dst      **ratelimit.Limiter
		override *ratelimit.Limiter
		burst    int
		every    time.Duration
	}{
		{&h.regLim, cfg.RegisterLimiter, 10, 30 * time.Second},
		{&h.linkLim, cfg.LinkLimiter, 20, 15 * time.Second},
		{&h.forgotIPLim, cfg.ForgotIPLimiter, 5, time.Minute},
		{&h.forgotMailLim, cfg.ForgotEmailLimiter, 3, 20 * time.Minute},
		{&h.userLim, cfg.UserLimiter, 10, 30 * time.Second},
		// Token creation: burst 10, one more every 6 minutes (about ten an hour)
		// per account and type. It limits creation/revocation churn; the number
		// of usable tokens is capped separately by the store.
		{&h.tokenLim, cfg.TokenLimiter, 10, 6 * time.Minute},
	} {
		if *l.dst, err = newLimiter(l.override, l.burst, l.every); err != nil {
			return nil, err
		}
	}

	rt := srv.Router()
	pub := func(pattern string, f http.HandlerFunc) {
		rt.Handle(pattern, server.KindRequest, server.PublicForm, f)
	}
	pub("GET /register/{code}", h.getRegister)
	pub("POST /register/{code}", h.postRegister)
	pub("GET /password/forgot", h.getForgot)
	pub("POST /password/forgot", h.postForgot)
	pub("GET /password/setup/{token}", h.passwordLinkForm(true))
	pub("POST /password/setup/{token}", h.passwordLinkSubmit(true))
	pub("GET /password/reset/{token}", h.passwordLinkForm(false))
	pub("POST /password/reset/{token}", h.passwordLinkSubmit(false))
	pub("GET /email/verify/{token}", h.getVerify)
	pub("POST /email/verify/{token}", h.postVerify)

	sess := func(pattern string, f http.HandlerFunc) {
		rt.Handle(pattern, server.KindRequest, server.Session, f)
	}
	sess("GET /account", h.getAccount)
	sess("POST /account/password", h.postPassword)
	sess("POST /account/email", h.postEmail)
	sess("POST /account/email/resend", h.postEmailResend)
	sess("POST /account/email/remove", h.postEmailRemove)
	sess("POST /account/api-tokens", h.postCreateToken(kindAPITokens))
	sess("POST /account/api-tokens/{id}/revoke", h.postRevokeToken(kindAPITokens))
	sess("POST /account/rss-tokens", h.postCreateToken(kindRSSTokens))
	sess("POST /account/rss-tokens/{id}/revoke", h.postRevokeToken(kindRSSTokens))
	sess("POST /account/invites/{id}/revoke", h.postRevokeInvite)
	return h, nil
}

// --- shared helpers ---

func redirect(w http.ResponseWriter, to string) {
	w.Header().Set("Location", to)
	w.WriteHeader(http.StatusSeeOther)
}

// secretPage marks a response that carries or is reached through a credential:
// never stored, never leaked through Referer.
func secretPage(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "private, no-store")
	h.Set("Referrer-Policy", "no-referrer")
}

func (h *Handlers) render(w http.ResponseWriter, status int, page string, data any) {
	if err := h.srv.Renderer().Render(w, status, page, data); err != nil {
		h.log.Error("render failed", "page", page)
	}
}

func (h *Handlers) message(w http.ResponseWriter, status int, heading, msg, href, text string) {
	h.render(w, status, "message", ui.MessagePage{Heading: heading, Message: msg, LinkHref: href, LinkText: text})
}

// limited answers 429 with Retry-After when the bucket is empty.
func (h *Handlers) limited(w http.ResponseWriter, lim *ratelimit.Limiter, key ratelimit.Key) bool {
	res := lim.Reserve(key)
	if res.OK {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(res.RetryAfter)))
	h.message(w, http.StatusTooManyRequests, "Too many attempts", "Please wait a little and try again.", "", "")
	return true
}

func (h *Handlers) ipKey(r *http.Request) ratelimit.Key {
	return ratelimit.IPKey(server.ClientIP(r.Context()))
}

func (h *Handlers) unavailable(w http.ResponseWriter, r *http.Request, err error, op string) {
	if errors.Is(err, password.ErrBusy) {
		w.Header().Set("Retry-After", "5")
	}
	h.log.Warn("account operation failed", "op", op, "request_id", server.RequestID(r.Context()), "error_type", typeName(err))
	h.message(w, http.StatusServiceUnavailable, "Temporarily unavailable", "The request could not be completed right now. Please try again in a moment.", "", "")
}

func typeName(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, password.ErrBusy):
		return "busy"
	}
	return "error"
}

// absolute builds a link under the configured public URL.
func (h *Handlers) absolute(path string) string {
	return strings.TrimRight(h.cfg.PublicBase.String(), "/") + path
}

// newPassword validates the two new-password fields. The message is safe to
// show; it never includes the password.
func newPassword(pw, pw2 string) (string, bool) {
	switch {
	case pw != pw2:
		return "The two passwords do not match.", false
	case password.ValidateNewPassword(pw) != nil:
		return "Choose a password of at least 8 characters.", false
	}
	return "", true
}
