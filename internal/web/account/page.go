package account

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	webauth "github.com/skobkin/magnetico-web/internal/web/auth"
	mailer "github.com/skobkin/magnetico-web/internal/web/mail"
	"github.com/skobkin/magnetico-web/internal/web/password"
	"github.com/skobkin/magnetico-web/internal/web/ratelimit"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/ui"
)

// notices are the fixed texts the account page can show after a redirect
// (?done=<slug>). Unknown slugs show nothing.
var notices = map[string]string{
	"password":       "Password changed. Your other sessions were signed out and this session was renewed. Any email confirmation link requested earlier no longer works; the email address on your account is unchanged, and you can request a new confirmation email.",
	"email-sent":     "A confirmation link was sent to the new address. Your current address stays in force until it is confirmed.",
	"email-resent":   "The confirmation email was sent again.",
	"email-removed":  "Email address removed. Password recovery by email is unavailable until you add and verify an address.",
	"api-revoked":    "API token revoked.",
	"rss-revoked":    "RSS token revoked.",
	"invite-revoked": "Invitation revoked.",
}

var tabs = map[string]bool{"profile": true, "invitations": true, "rss": true, "api": true}

func tabOf(s string) string {
	if tabs[s] {
		return s
	}
	return "profile"
}

func (h *Handlers) fmtTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

func (h *Handlers) fmtTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return h.fmtTime(*t)
}

// pageData loads what the tab shows. It never reads another user's rows: every
// query is keyed by the principal's id.
func (h *Handlers) pageData(ctx context.Context, p webauth.Principal, tab string) (ui.AccountPage, error) {
	pg := ui.AccountPage{Username: p.Username, Tab: tab, MailEnabled: h.cfg.Mail != nil, Role: p.Role, IsAdmin: p.Role == "admin", RSSV1Disabled: h.cfg.RSSV1Disabled}
	switch tab {
	case "profile":
		u, err := h.cfg.Store.GetUserByID(ctx, p.UserID)
		if err != nil {
			return pg, err
		}
		pg.Email, pg.EmailVerified = u.Email, u.EmailVerified
		if pending, ok, err := h.cfg.Store.PendingEmail(ctx, p.UserID); err != nil {
			return pg, err
		} else if ok {
			pg.PendingEmail = pending
		}
		switch {
		case u.Email == "" && pg.PendingEmail == "":
			pg.EmailState = "none"
		case u.Email == "":
			pg.EmailState = "pending"
		case u.EmailVerified:
			pg.EmailState = "verified"
		default:
			pg.EmailState = "unverified"
		}
		// Matches the forgot-password worker: a verified address of an active
		// account, and a server that can send the message.
		pg.RecoveryAvailable = pg.EmailState == "verified" && pg.MailEnabled
	case "invitations":
		invs, err := h.cfg.Store.ListInvitesByOwner(ctx, p.UserID)
		if err != nil {
			return pg, err
		}
		for _, v := range invs {
			row := ui.InviteRow{ID: v.ID, UsedBy: v.UsedBy, Created: h.fmtTime(v.CreatedAt)}
			switch {
			case v.UsedBy != "" || v.UsedAt != nil:
				row.Status = "Used"
			case v.RevokedAt != nil:
				row.Status = "Revoked"
			case v.Expired:
				row.Status = "Expired"
			default:
				row.Status, row.Usable = "Available", true
				row.URL = h.absolute("/register/" + v.Code)
				pg.InvitesAvailable++
			}
			pg.Invites = append(pg.Invites, row)
		}
	case "rss", "api":
		list := h.cfg.Store.ListRSSTokens
		if tab == "api" {
			list = h.cfg.Store.ListAPITokens
		}
		toks, err := list(ctx, p.UserID)
		if err != nil {
			return pg, err
		}
		rows := make([]ui.TokenRow, 0, len(toks))
		for _, t := range toks {
			rows = append(rows, ui.TokenRow{
				ID: t.ID, Label: t.Label, Created: h.fmtTime(t.CreatedAt), LastUsed: h.fmtTimePtr(t.LastUsedAt),
				Expires: h.fmtTimePtr(t.ExpiresAt), Expired: t.Expired, Legacy: t.Legacy,
			})
		}
		if tab == "api" {
			pg.APITokens = rows
		} else {
			pg.RSSTokens = rows
		}
	}
	return pg, nil
}

// renderAccount renders the account page with a status, an optional fixed
// notice or error, and an optional one-time secret.
func (h *Handlers) renderAccount(w http.ResponseWriter, r *http.Request, status int, tab, notice, errText string, secret *ui.NewSecret) {
	secretPage(w)
	p, ok := webauth.Current(r.Context())
	if !ok {
		h.srv.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}
	pg, err := h.pageData(r.Context(), p, tab)
	if err != nil {
		h.unavailable(w, r, err, "account-page")
		return
	}
	pg.CSRFToken = webauth.SessionFormToken(r)
	pg.Notice, pg.Error, pg.Secret = notice, errText, secret
	h.render(w, status, "account", pg)
}

func (h *Handlers) getAccount(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h.renderAccount(w, r, http.StatusOK, tabOf(q.Get("tab")), notices[q.Get("done")], "", nil)
}

func (h *Handlers) principal(w http.ResponseWriter, r *http.Request) (webauth.Principal, bool) {
	p, ok := webauth.Current(r.Context())
	if !ok {
		h.srv.ErrorPage(w, r, http.StatusInternalServerError)
	}
	return p, ok
}

func (h *Handlers) accountKey(id int64) ratelimit.Key {
	return ratelimit.KeyOf(kindAccount, strconv.FormatInt(id, 10))
}

// verifyCurrent checks the user's current password through the shared limiter.
// ok=false means the response was written.
func (h *Handlers) verifyCurrent(w http.ResponseWriter, r *http.Request, p webauth.Principal, tab, pw string) (accounts.User, bool) {
	u, err := h.cfg.Store.GetUserByID(r.Context(), p.UserID)
	if err != nil {
		h.unavailable(w, r, err, "load-user")
		return u, false
	}
	if pw == "" || len(pw) > password.MaxLoginLength || u.PasswordHash == "" {
		h.renderAccount(w, r, http.StatusBadRequest, tab, "", "The current password is not correct.", nil)
		return u, false
	}
	ok, err := h.cfg.Passwords.Verify(r.Context(), pw, u.PasswordHash)
	if err != nil {
		h.unavailable(w, r, err, "verify-current")
		return u, false
	}
	if !ok {
		h.renderAccount(w, r, http.StatusBadRequest, tab, "", "The current password is not correct.", nil)
		return u, false
	}
	return u, true
}

// --- password ---

func (h *Handlers) postPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.srv.ErrorPage(w, r, http.StatusBadRequest)
		return
	}
	// The current password is verified here, so this action is limited per
	// account (a hijacked session must not become a password-guessing oracle).
	if h.limited(w, h.userLim, h.accountKey(p.UserID)) {
		return
	}
	pw := r.PostForm.Get("new_password")
	if msg, ok := newPassword(pw, r.PostForm.Get("new_password2")); !ok {
		h.renderAccount(w, r, http.StatusBadRequest, "profile", "", msg, nil)
		return
	}
	u, ok := h.verifyCurrent(w, r, p, "profile", r.PostForm.Get("current_password"))
	if !ok {
		return
	}
	newHash, err := h.cfg.Passwords.Hash(r.Context(), pw)
	if err != nil {
		h.unavailable(w, r, err, "change-hash")
		return
	}
	sessionHash, ok := webauth.CurrentSessionHash(r.Context())
	if !ok {
		h.srv.ErrorPage(w, r, http.StatusInternalServerError)
		return
	}
	res, err := h.cfg.Store.ChangePassword(r.Context(), accounts.ChangePasswordParams{
		UserID: p.UserID, VerifiedHash: u.PasswordHash, NewHash: newHash, CurrentSession: sessionHash,
		RevokeAPI: r.PostForm.Get("revoke_api") != "", RevokeRSS: r.PostForm.Get("revoke_rss") != "",
	})
	switch {
	case errors.Is(err, accounts.ErrLoginStale):
		h.renderAccount(w, r, http.StatusConflict, "profile", "", "Your account changed while you were doing this. Nothing was changed; please try again.", nil)
		return
	case errors.Is(err, accounts.ErrUnauthenticated):
		redirect(w, "/login")
		return
	case err != nil:
		h.unavailable(w, r, err, "change-password")
		return
	}
	h.cfg.Auth.SetRotatedSession(w, res.Token, res.Remember, res.ExpiresAt, res.Now)
	h.log.Info("password changed", "request_id", server.RequestID(r.Context()), "user_id", p.UserID,
		"other_sessions_revoked", res.Sessions, "api_tokens_revoked", res.APITokens, "rss_tokens_revoked", res.RSSTokens,
		"email_links_cancelled", res.EmailLinks)
	redirect(w, "/account?tab=profile&done=password")
}

// --- email ---

// requireMail answers when email is not configured; ok=false means the
// response was written.
func (h *Handlers) requireMail(w http.ResponseWriter, r *http.Request) bool {
	if h.cfg.Mail != nil {
		return true
	}
	h.renderAccount(w, r, http.StatusConflict, "profile", "", "This server cannot send email, so email addresses cannot be confirmed.", nil)
	return false
}

// queueVerification reserves a mail slot first (so a full or closed queue is
// reported before anything is written), then issues the link and hands it to
// the workers. ok=false means the response was written.
func (h *Handlers) queueVerification(w http.ResponseWriter, r *http.Request, userID int64, email string) bool {
	session, ok := webauth.CurrentSessionHash(r.Context())
	if !ok {
		h.srv.ErrorPage(w, r, http.StatusInternalServerError)
		return false
	}
	slot, err := h.cfg.Mail.Reserve()
	if err != nil {
		w.Header().Set("Retry-After", "30")
		h.renderAccount(w, r, http.StatusServiceUnavailable, "profile", "",
			"The confirmation email could not be sent right now. Nothing was changed; please try again later.", nil)
		return false
	}
	link, err := h.cfg.Store.IssueEmailVerifyLinkForSession(r.Context(), userID, email, session)
	if err != nil {
		slot.Release()
		if errors.Is(err, accounts.ErrUnauthenticated) {
			// The session was revoked (a password change or reset) after the
			// request was authorized: no pending link is created.
			redirect(w, "/login")
			return false
		}
		if errors.Is(err, accounts.ErrInvalid) {
			h.renderAccount(w, r, http.StatusBadRequest, "profile", "", "Enter a valid email address.", nil)
			return false
		}
		h.unavailable(w, r, err, "issue-email-link")
		return false
	}
	url := h.absolute("/email/verify/" + link.Token)
	slot.Submit(func(ctx context.Context, send func(mailer.Message) error) {
		_ = send(mailer.VerifyMessage(email, url, accounts.EmailLinkTTL))
	})
	return true
}

func (h *Handlers) postEmail(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.srv.ErrorPage(w, r, http.StatusBadRequest)
		return
	}
	if !h.requireMail(w, r) || h.limited(w, h.userLim, h.accountKey(p.UserID)) {
		return
	}
	email := strings.TrimSpace(r.PostForm.Get("email"))
	if accounts.ValidateEmail(email) != nil {
		h.renderAccount(w, r, http.StatusBadRequest, "profile", "", "Enter a valid email address.", nil)
		return
	}
	// Changing the address needs the current password.
	u, ok := h.verifyCurrent(w, r, p, "profile", r.PostForm.Get("current_password"))
	if !ok {
		return
	}
	if u.EmailVerified && strings.EqualFold(u.Email, email) {
		h.renderAccount(w, r, http.StatusBadRequest, "profile", "", "That address is already confirmed on your account.", nil)
		return
	}
	if !h.queueVerification(w, r, p.UserID, email) {
		return
	}
	h.log.Info("email confirmation requested", "request_id", server.RequestID(r.Context()), "user_id", p.UserID)
	redirect(w, "/account?tab=profile&done=email-sent")
}

// postEmailRemove removes the account's address (verified or not) and cancels
// a pending verification. It does not need SMTP: removing an address is exactly
// what a user of a server that cannot send email must be able to do. It needs
// the current password like every email change.
func (h *Handlers) postEmailRemove(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.srv.ErrorPage(w, r, http.StatusBadRequest)
		return
	}
	if h.limited(w, h.userLim, h.accountKey(p.UserID)) {
		return
	}
	if _, ok := h.verifyCurrent(w, r, p, "profile", r.PostForm.Get("current_password")); !ok {
		return
	}
	res, err := h.cfg.Store.RemoveEmail(r.Context(), p.UserID)
	if err != nil {
		h.unavailable(w, r, err, "remove-email")
		return
	}
	if res.Email == "" && !res.PendingCancelled {
		h.renderAccount(w, r, http.StatusBadRequest, "profile", "", "There is no email address to remove.", nil)
		return
	}
	h.log.Info("email removed", "request_id", server.RequestID(r.Context()), "user_id", p.UserID,
		"verified", res.WasVerified, "links_cancelled", res.RecoveryLinksCancelled)
	if res.WasVerified && h.cfg.Mail != nil {
		// Best effort notice to the removed address; a full queue drops it.
		if slot, err := h.cfg.Mail.Reserve(); err == nil {
			removed := res.Email
			slot.Submit(func(ctx context.Context, send func(mailer.Message) error) {
				_ = send(mailer.EmailRemovedNotice(removed))
			})
		}
	}
	redirect(w, "/account?tab=profile&done=email-removed")
}

func (h *Handlers) postEmailResend(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	if !h.requireMail(w, r) || h.limited(w, h.userLim, h.accountKey(p.UserID)) {
		return
	}
	target, pending, err := h.cfg.Store.PendingEmail(r.Context(), p.UserID)
	if err != nil {
		h.unavailable(w, r, err, "pending-email")
		return
	}
	if !pending {
		u, err := h.cfg.Store.GetUserByID(r.Context(), p.UserID)
		if err != nil {
			h.unavailable(w, r, err, "load-user")
			return
		}
		if u.Email == "" || u.EmailVerified {
			h.renderAccount(w, r, http.StatusBadRequest, "profile", "", "There is no address waiting for confirmation.", nil)
			return
		}
		target = u.Email
	}
	if !h.queueVerification(w, r, p.UserID, target) {
		return
	}
	redirect(w, "/account?tab=profile&done=email-resent")
}

// --- tokens ---

const (
	apiTab = "api"
	rssTab = "rss"
)

func tabFor(kind byte) string {
	if kind == kindAPITokens {
		return apiTab
	}
	return rssTab
}

func (h *Handlers) postCreateToken(kind byte) http.HandlerFunc {
	tab := tabFor(kind)
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.principal(w, r)
		if !ok {
			return
		}
		if err := r.ParseForm(); err != nil {
			h.srv.ErrorPage(w, r, http.StatusBadRequest)
			return
		}
		if h.limited(w, h.tokenLim, ratelimit.KeyOf(kind, strconv.FormatInt(p.UserID, 10))) {
			return
		}
		label := strings.TrimSpace(r.PostForm.Get("label"))
		if label != "" && accounts.ValidateLabel(label) != nil {
			h.renderAccount(w, r, http.StatusBadRequest, tab, "", "Use a label of at most 100 characters without control characters.", nil)
			return
		}
		var expires *time.Time
		switch d := r.PostForm.Get("expires"); d {
		case "":
		case "30", "90":
			days, _ := strconv.Atoi(d)
			t := h.now().Add(time.Duration(days) * 24 * time.Hour)
			expires = &t
		default:
			h.renderAccount(w, r, http.StatusBadRequest, tab, "", "Choose one of the listed expiry options.", nil)
			return
		}
		create := h.cfg.Store.CreateRSSToken
		if kind == kindAPITokens {
			create = h.cfg.Store.CreateAPIToken
		}
		tok, err := create(r.Context(), p.UserID, label, expires)
		switch {
		case errors.Is(err, accounts.ErrTokenLimit):
			h.renderAccount(w, r, http.StatusBadRequest, tab, "",
				"You already have the maximum of "+strconv.Itoa(accounts.MaxActiveTokens)+" active tokens of this type. Revoke some first.", nil)
			return
		case errors.Is(err, accounts.ErrNotFound):
			redirect(w, "/login")
			return
		case err != nil:
			h.unavailable(w, r, err, "create-token")
			return
		}
		h.log.Info("token created", "request_id", server.RequestID(r.Context()), "user_id", p.UserID, "type", tab, "token_id", tok.ID)
		secret := &ui.NewSecret{Kind: tab, Label: label, Token: tok.Plaintext}
		if kind == kindRSSTokens {
			secret.URL = h.absolute("/api/v2/feeds/latest.rss?feed_token=" + url.QueryEscape(tok.Plaintext))
		}
		h.renderAccount(w, r, http.StatusOK, tab, "", "", secret)
	}
}

func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0 && strconv.FormatInt(id, 10) == s
}

func (h *Handlers) postRevokeToken(kind byte) http.HandlerFunc {
	tab := tabFor(kind)
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.principal(w, r)
		if !ok {
			return
		}
		id, ok := parseID(r.PathValue("id"))
		if !ok {
			h.renderAccount(w, r, http.StatusNotFound, tab, "", "That token no longer exists.", nil)
			return
		}
		revoke := h.cfg.Store.RevokeRSSToken
		if kind == kindAPITokens {
			revoke = h.cfg.Store.RevokeAPIToken
		}
		switch err := revoke(r.Context(), p.UserID, id); {
		case errors.Is(err, accounts.ErrNotFound):
			h.renderAccount(w, r, http.StatusNotFound, tab, "", "That token no longer exists.", nil)
		case err != nil:
			h.unavailable(w, r, err, "revoke-token")
		default:
			h.log.Info("token revoked", "request_id", server.RequestID(r.Context()), "user_id", p.UserID, "type", tab, "token_id", id)
			redirect(w, "/account?tab="+tab+"&done="+tab+"-revoked")
		}
	}
}

// --- invitations ---

func (h *Handlers) postRevokeInvite(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		h.renderAccount(w, r, http.StatusNotFound, "invitations", "", "That invitation no longer exists.", nil)
		return
	}
	out, err := h.cfg.Store.RevokeOwnInvite(r.Context(), p.UserID, id)
	switch {
	case errors.Is(err, accounts.ErrNotFound):
		h.renderAccount(w, r, http.StatusNotFound, "invitations", "", "That invitation no longer exists.", nil)
	case err != nil:
		h.unavailable(w, r, err, "revoke-invite")
	case out == accounts.InviteAlreadyUsed:
		h.renderAccount(w, r, http.StatusConflict, "invitations", "", "That invitation was already used and cannot be revoked.", nil)
	default:
		h.log.Info("invitation revoked", "request_id", server.RequestID(r.Context()), "user_id", p.UserID, "invite_id", id)
		redirect(w, "/account?tab=invitations&done=invite-revoked")
	}
}
