package auth

import (
	"net/http"

	"github.com/skobkin/magnetico-web/internal/web/admin/access"
	"github.com/skobkin/magnetico-web/internal/web/problem"
)

// adminGuard is the guard of server.Admin: the session guard (one database
// lookup per request, no cache: a user disabled or demoted a moment ago is
// refused on the very next request) followed by the role check. API and RSS
// tokens are never consulted. This file is the only non-test code that grants
// an access.Principal.
func (a *Auth) adminGuard(next http.Handler) http.Handler {
	return a.sessionGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := Current(r.Context())
		if !ok || p.Role != "admin" {
			w.Header().Set("Cache-Control", "private, no-store")
			a.srv.ErrorPage(w, r, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(access.IntoContext(r.Context(), access.Grant(p.UserID, p.Username))))
	}))
}

// adminJSONGuard is the guard of server.AdminJSON: the same, with problem+json failures.
func (a *Auth) adminJSONGuard(next http.Handler) http.Handler {
	return a.sessionJSONGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := Current(r.Context())
		if !ok || p.Role != "admin" {
			problem.Write(w, problem.Problem{Slug: problem.Forbidden, Title: "Administrator access required", Status: http.StatusForbidden})
			return
		}
		next.ServeHTTP(w, r.WithContext(access.IntoContext(r.Context(), access.Grant(p.UserID, p.Username))))
	}))
}
