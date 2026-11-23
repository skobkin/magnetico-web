package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
)

// Cookie names. The production names use the __Host- prefix (Secure, Path=/,
// no Domain), which also stops a sibling subdomain from planting them; the
// development names (loopback http:// only) cannot.
const (
	sessionCookieProd = "__Host-magnetico_session"
	sessionCookieDev  = "magnetico_session"
	preCookieProd     = "__Host-magnetico_presession"
	preCookieDev      = "magnetico_presession"

	// preSessionTTL is the lifetime of the pre-session cookie.
	preSessionTTL = time.Hour
	// preClockSkew tolerates a small forward drift when validating its age.
	preClockSkew = time.Minute
)

func (a *Auth) sessionName() string {
	if a.cfg.DevMode {
		return sessionCookieDev
	}
	return sessionCookieProd
}

func (a *Auth) preName() string {
	if a.cfg.DevMode {
		return preCookieDev
	}
	return preCookieProd
}

// readSession returns the first well-formed session cookie. present reports
// that a cookie with the session name was sent at all (so a malformed or stale
// one can be cleared); a malformed value never reaches the database.
func (a *Auth) readSession(r *http.Request) (token string, hash []byte, present, wellFormed bool) {
	for _, c := range r.Cookies() {
		if c.Name != a.sessionName() {
			continue
		}
		present = true
		if h, ok := accounts.SessionTokenHash(c.Value); ok {
			return c.Value, h, true, true
		}
	}
	return "", nil, present, false
}

func (a *Auth) setSessionCookie(w http.ResponseWriter, res accounts.LoginResult) {
	c := &http.Cookie{
		Name: a.sessionName(), Value: res.Token, Path: "/",
		HttpOnly: true, Secure: !a.cfg.DevMode, SameSite: http.SameSiteLaxMode,
	}
	if res.Remember {
		// The server-side expires_at is authoritative; the cookie carries a
		// matching Expires derived from the same database clock. Ordinary
		// sessions get a browser-session cookie.
		c.Expires = res.ExpiresAt
		c.MaxAge = max(int(res.ExpiresAt.Sub(res.Now).Seconds()), 1)
	}
	http.SetCookie(w, c)
}

func (a *Auth) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: a.sessionName(), Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, Secure: !a.cfg.DevMode, SameSite: http.SameSiteLaxMode,
	})
}

func (a *Auth) clearPreCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: a.preName(), Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, Secure: !a.cfg.DevMode, SameSite: http.SameSiteStrictMode,
	})
}

// Pre-session cookie value: "v1.<base64url(32 random bytes)>.<issue unix time>".
// It is never promoted to a session; it only binds public-form CSRF tokens
// for up to preSessionTTL.
func (a *Auth) newPreValue() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return "v1." + base64.RawURLEncoding.EncodeToString(b[:]) + "." + strconv.FormatInt(a.now().Unix(), 10)
}

// validPre reports whether v is a well-formed, unexpired pre-session value.
func (a *Auth) validPre(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 || parts[0] != "v1" || len(parts[1]) != 43 {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(raw) != 32 {
		return false
	}
	issued, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return false
	}
	age := a.now().Sub(time.Unix(issued, 0))
	return age >= -preClockSkew && age < preSessionTTL
}

// readPre returns the first valid pre-session cookie value.
func (a *Auth) readPre(r *http.Request) (string, bool) {
	for _, c := range r.Cookies() {
		if c.Name == a.preName() && a.validPre(c.Value) {
			return c.Value, true
		}
	}
	return "", false
}

// ensurePre returns a valid pre-session value, issuing a fresh cookie when the
// request has none (or an expired one). Only GETs of public forms call it.
func (a *Auth) ensurePre(w http.ResponseWriter, r *http.Request) string {
	if v, ok := a.readPre(r); ok {
		return v
	}
	v := a.newPreValue()
	http.SetCookie(w, &http.Cookie{
		Name: a.preName(), Value: v, Path: "/", MaxAge: int(preSessionTTL.Seconds()),
		HttpOnly: true, Secure: !a.cfg.DevMode, SameSite: http.SameSiteStrictMode,
	})
	return v
}

// CSRF tokens are a stateless, cookie-bound construction (not a server-stored
// value): a domain-separated SHA-256 of the high-entropy HttpOnly cookie
// secret. Without the cookie the token cannot be computed; the construction
// relies on the __Host- prefix (production) against cookie injection. Session
// forms and pre-session forms use different domains, so a token of one class
// never validates on the other.
const (
	csrfDomainSession = "magnetico/csrf/session/v1\x00"
	csrfDomainPre     = "magnetico/csrf/pre/v1\x00"
)

func csrfToken(domain, secret string) string {
	sum := sha256.Sum256([]byte(domain + secret))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func sessionCSRF(sessionCookie string) string { return csrfToken(csrfDomainSession, sessionCookie) }
func preCSRF(preValue string) string          { return csrfToken(csrfDomainPre, preValue) }

// formTokenOK compares the submitted csrf_token (form body only; never the
// query string) with the expected value in constant time.
func formTokenOK(r *http.Request, want string) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	got := r.PostForm.Get("csrf_token")
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
