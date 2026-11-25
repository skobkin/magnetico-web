package account

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	mailer "github.com/skobkin/magnetico-web/internal/web/mail"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/ui"
)

// One generic text for every unusable invitation (§4.7.4) and one for every
// unusable link.
const (
	msgInviteGone = "This invitation is invalid or no longer available."
	msgLinkGone   = "This link is invalid, has expired or was already used. Ask for a new one."
)

// --- registration ---

func (h *Handlers) getRegister(w http.ResponseWriter, r *http.Request) {
	secretPage(w)
	ok, err := h.cfg.Store.CheckInvite(r.Context(), r.PathValue("code"))
	if err != nil {
		h.unavailable(w, r, err, "check-invite")
		return
	}
	status := http.StatusOK
	if !ok {
		status = http.StatusNotFound
	}
	h.render(w, status, "register", ui.RegisterPage{
		CSRFToken: h.cfg.Auth.PublicFormToken(w, r), Valid: ok, EmailHint: h.cfg.Mail != nil,
	})
}

func (h *Handlers) postRegister(w http.ResponseWriter, r *http.Request) {
	secretPage(w)
	if err := r.ParseForm(); err != nil {
		h.srv.ErrorPage(w, r, http.StatusBadRequest)
		return
	}
	if h.limited(w, h.regLim, h.ipKey(r)) {
		return
	}
	code := r.PathValue("code")
	username := r.PostForm.Get("username")
	email := strings.TrimSpace(r.PostForm.Get("email"))
	page := func(msg string) ui.RegisterPage {
		return ui.RegisterPage{
			CSRFToken: h.cfg.Auth.PublicFormToken(w, r), Valid: true, Username: username, Email: email,
			Error: msg, EmailHint: h.cfg.Mail != nil,
		}
	}
	if msg, ok := newPassword(r.PostForm.Get("password"), r.PostForm.Get("password2")); !ok {
		h.render(w, http.StatusBadRequest, "register", page(msg))
		return
	}
	if err := accounts.ValidateUsername(username); err != nil {
		h.render(w, http.StatusBadRequest, "register", page("Choose a username of 2 to 25 characters without control characters."))
		return
	}
	if email != "" && accounts.ValidateEmail(email) != nil {
		h.render(w, http.StatusBadRequest, "register", page("Enter a valid email address, or leave it empty."))
		return
	}
	hash, err := h.cfg.Passwords.Hash(r.Context(), r.PostForm.Get("password"))
	if err != nil {
		h.unavailable(w, r, err, "register-hash")
		return
	}
	id, err := h.cfg.Store.RegisterWithInvite(r.Context(), accounts.RegisterParams{
		Code: code, Username: username, Email: email, Hash: hash, Quota: h.cfg.InviteQuota,
	})
	switch {
	case errors.Is(err, accounts.ErrInvitationUnavailable):
		h.render(w, http.StatusNotFound, "register", ui.RegisterPage{Valid: false})
		return
	case errors.Is(err, accounts.ErrUsernameTaken):
		h.render(w, http.StatusConflict, "register", page("That username is not available."))
		return
	case errors.Is(err, accounts.ErrInvalid):
		h.render(w, http.StatusBadRequest, "register", page("Please check the details and try again."))
		return
	case err != nil:
		h.unavailable(w, r, err, "register")
		return
	}
	h.log.Info("account registered", "request_id", server.RequestID(r.Context()), "user_id", id)

	// The account is committed. Offering the verification email is best effort:
	// a full or closed queue never undoes the registration, and the user can
	// request it later from the account page.
	notice := "registered"
	if email != "" && h.cfg.Mail != nil {
		if !h.sendVerification(r.Context(), id, email) {
			notice = "registered-mailfail"
		}
	}
	redirect(w, "/login?notice="+notice)
}

// --- password setup and reset links ---

func (h *Handlers) passwordLinkForm(setup bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secretPage(w)
		// The token is not looked up here: showing the form for every token and
		// refusing on submit reveals nothing about which links exist.
		h.render(w, http.StatusOK, "password_link", ui.PasswordLinkPage{
			CSRFToken: h.cfg.Auth.PublicFormToken(w, r), Setup: setup,
		})
	}
}

func (h *Handlers) passwordLinkSubmit(setup bool) http.HandlerFunc {
	purpose := accounts.PurposeReset
	if setup {
		purpose = accounts.PurposeSetup
	}
	return func(w http.ResponseWriter, r *http.Request) {
		secretPage(w)
		if err := r.ParseForm(); err != nil {
			h.srv.ErrorPage(w, r, http.StatusBadRequest)
			return
		}
		if h.limited(w, h.linkLim, h.ipKey(r)) {
			return
		}
		pw := r.PostForm.Get("password")
		if msg, ok := newPassword(pw, r.PostForm.Get("password2")); !ok {
			h.render(w, http.StatusBadRequest, "password_link", ui.PasswordLinkPage{
				CSRFToken: h.cfg.Auth.PublicFormToken(w, r), Setup: setup, Error: msg,
			})
			return
		}
		hash, err := h.cfg.Passwords.Hash(r.Context(), pw)
		if err != nil {
			h.unavailable(w, r, err, "link-hash")
			return
		}
		res, err := h.cfg.Store.CompletePasswordLink(r.Context(), r.PathValue("token"), purpose, hash)
		switch {
		case errors.Is(err, accounts.ErrLinkInvalid):
			h.message(w, http.StatusBadRequest, "Link unavailable", msgLinkGone, "/login", "Go to sign in")
			return
		case err != nil:
			h.unavailable(w, r, err, "complete-link")
			return
		}
		h.log.Info("password link completed", "request_id", server.RequestID(r.Context()), "user_id", res.UserID,
			"purpose", purpose, "sessions_revoked", res.Sessions, "api_tokens_revoked", res.APITokens, "rss_tokens_revoked", res.RSSTokens,
			"email_links_cancelled", res.EmailLinks)
		redirect(w, "/login?notice=password-set")
	}
}

// --- forgot password ---

func (h *Handlers) getForgot(w http.ResponseWriter, r *http.Request) {
	h.render(w, http.StatusOK, "password_forgot", ui.ForgotPage{
		CSRFToken: h.cfg.Auth.PublicFormToken(w, r), Available: h.cfg.Mail != nil,
	})
}

// postForgot answers the same page for every address: unknown, unverified,
// disabled and valid accounts, and also when the queue is full or the address
// is rate limited. The handler never touches the database and never waits for
// SMTP: it reserves a queue slot and hands the job over. The worker does the
// lookup, issues the link and sends, so the request has the same shape and
// duration whatever the account is. Only the per-IP limiter answers
// differently (429), and it does not depend on the address.
func (h *Handlers) postForgot(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Mail == nil {
		h.render(w, http.StatusOK, "password_forgot", ui.ForgotPage{Available: false})
		return
	}
	if err := r.ParseForm(); err != nil {
		h.srv.ErrorPage(w, r, http.StatusBadRequest)
		return
	}
	if h.limited(w, h.forgotIPLim, h.ipKey(r)) {
		return
	}
	addr := strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))
	if bare, ok := bareAddress(addr); ok {
		// A per-address limit that is exhausted silently skips the work: the
		// answer is identical, so it cannot be used to probe.
		if h.forgotMailLim.Reserve(ratelimit.KeyOf(kindForgotEmail, bare)).OK {
			if slot, err := h.cfg.Mail.Reserve(); err == nil {
				slot.Submit(h.recoveryJob(bare))
			}
		}
	}
	h.render(w, http.StatusOK, "password_forgot", ui.ForgotPage{Available: true, Sent: true})
}

func bareAddress(s string) (string, bool) {
	if s == "" || len(s) > maxEmailLen {
		return "", false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s {
		return "", false
	}
	return s, true
}

// recoveryJob runs on a mail worker: issue the link for the verified, active
// account behind the address, then send it. An address that matches nothing
// ends the job silently.
func (h *Handlers) recoveryJob(addr string) mailer.Job {
	return func(ctx context.Context, send func(mailer.Message) error) {
		dctx, cancel := context.WithTimeout(ctx, dbStepTimeout)
		defer cancel()
		link, err := h.cfg.Store.IssueRecoveryLink(dctx, addr)
		if err != nil {
			if !errors.Is(err, accounts.ErrNotFound) {
				h.log.Warn("recovery link could not be issued", "error_type", typeName(err))
			}
			return
		}
		setup := link.Purpose == accounts.PurposeSetup
		ttl := accounts.ResetLinkTTL
		if setup {
			ttl = accounts.SetupLinkTTL
		}
		path := "/password/reset/"
		if setup {
			path = "/password/setup/"
		}
		_ = send(mailer.PasswordLinkMessage(link.Email, h.absolute(path+link.Token), setup, ttl))
	}
}

// --- email confirmation ---

func (h *Handlers) getVerify(w http.ResponseWriter, r *http.Request) {
	secretPage(w)
	h.render(w, http.StatusOK, "email_verify", ui.VerifyPage{CSRFToken: h.cfg.Auth.PublicFormToken(w, r)})
}

func (h *Handlers) postVerify(w http.ResponseWriter, r *http.Request) {
	secretPage(w)
	if err := r.ParseForm(); err != nil {
		h.srv.ErrorPage(w, r, http.StatusBadRequest)
		return
	}
	if h.limited(w, h.linkLim, h.ipKey(r)) {
		return
	}
	res, err := h.cfg.Store.CompleteEmailVerify(r.Context(), r.PathValue("token"))
	switch {
	case errors.Is(err, accounts.ErrLinkInvalid), errors.Is(err, accounts.ErrEmailUnavailable):
		// One message for both: the second would reveal that another account
		// holds the address.
		h.message(w, http.StatusBadRequest, "Link unavailable", "This confirmation link cannot be used. You can request a new one from your account page.", "/account", "Open account")
		return
	case err != nil:
		h.unavailable(w, r, err, "verify-email")
		return
	}
	h.log.Info("email confirmed", "request_id", server.RequestID(r.Context()), "user_id", res.UserID)
	if res.PreviousEmail != "" && h.cfg.Mail != nil {
		// Best effort notice to the replaced address; a full queue drops it.
		if slot, err := h.cfg.Mail.Reserve(); err == nil {
			prev := res.PreviousEmail
			slot.Submit(func(ctx context.Context, send func(mailer.Message) error) {
				_ = send(mailer.EmailChangedNotice(prev))
			})
		}
	}
	h.message(w, http.StatusOK, "Email address confirmed", "This address can now be used to recover your password.", "/account", "Open account")
}

// sendVerification queues a confirmation email for userID/email. The slot is
// reserved BEFORE the link is issued, so a full or closed queue changes
// nothing; if issuing fails the slot is released. It reports whether the
// message was queued.
func (h *Handlers) sendVerification(ctx context.Context, userID int64, email string) bool {
	slot, err := h.cfg.Mail.Reserve()
	if err != nil {
		return false
	}
	link, err := h.cfg.Store.IssueEmailVerifyLink(ctx, userID, email)
	if err != nil {
		slot.Release()
		return false
	}
	url := h.absolute("/email/verify/" + link.Token)
	slot.Submit(func(ctx context.Context, send func(mailer.Message) error) {
		_ = send(mailer.VerifyMessage(email, url, accounts.EmailLinkTTL))
	})
	return true
}
