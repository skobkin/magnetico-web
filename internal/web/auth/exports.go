package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
)

// PublicFormToken returns the CSRF token of a public form (registration,
// password links, forgot-password, email confirmation). It issues the
// pre-session cookie that binds the token when the request has none, so call
// it before the response is written.
func (a *Auth) PublicFormToken(w http.ResponseWriter, r *http.Request) string {
	return preCSRF(a.ensurePre(w, r))
}

// CurrentSessionHash returns the stored hash of the session cookie that
// authenticated the request, for a request that passed the Session guard.
func CurrentSessionHash(ctx context.Context) ([]byte, bool) {
	au, ok := authedOf(ctx)
	if !ok {
		return nil, false
	}
	return au.tokenHash, true
}

// SetRotatedSession installs the cookie of a session that replaced the
// request's own (password change). The lifetime is the old session's, as the
// store kept it; expiresAt and now come from the same database clock.
func (a *Auth) SetRotatedSession(w http.ResponseWriter, token string, remember bool, expiresAt, now time.Time) {
	a.setSessionCookie(w, accounts.LoginResult{Token: token, Remember: remember, ExpiresAt: expiresAt, Now: now})
}
