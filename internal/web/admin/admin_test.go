package admin

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/admin/access"
	"github.com/skobkin/magnetico-web/internal/web/admin/stats"
	"github.com/skobkin/magnetico-web/internal/web/server"
)

type fakeBackend struct{}

func (fakeBackend) Verify(context.Context) error { return nil }
func (fakeBackend) Ping(context.Context) error   { return nil }
func (fakeBackend) Close()                       {}

func newServer(t *testing.T) *server.Server {
	t.Helper()
	pub, _ := url.Parse("https://magnet.example.org")
	srv, err := server.New(server.Config{
		PublicURL: pub, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		RequestTimeout: 5 * time.Second, SearchTimeout: time.Minute, ShutdownTimeout: 5 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// passThrough is a guard that lets everything through WITHOUT a Principal: the
// handlers must still refuse (defence in depth).
func passThrough(next http.Handler) http.Handler { return next }

func TestAdminRoutesAreGetOnlyAndInTheAdminClasses(t *testing.T) {
	srv := newServer(t)
	srv.Router().SetGuard(server.Admin, passThrough)
	srv.Router().SetGuard(server.AdminJSON, passThrough)
	Install(srv, stats.NewService(nil))
	var n int
	for _, r := range srv.Router().Routes() {
		if !strings.Contains(r.Pattern, " /admin") {
			continue
		}
		n++
		if !strings.HasPrefix(r.Pattern, "GET ") {
			t.Errorf("admin route %q is not GET-only", r.Pattern)
		}
		if r.Access != server.Admin && r.Access != server.AdminJSON {
			t.Errorf("admin route %q has access class %v", r.Pattern, r.Access)
		}
		if strings.Contains(r.Pattern, "/api/") && r.Access != server.AdminJSON {
			t.Errorf("JSON route %q is not AdminJSON", r.Pattern)
		}
	}
	if n < 3 {
		t.Fatalf("only %d admin routes registered", n)
	}
	// Registering a state-changing admin route is a programming error.
	for _, a := range []server.Access{server.Admin, server.AdminJSON} {
		for _, p := range []string{"POST /admin/x", "PUT /admin/x", "DELETE /admin/x", "PATCH /admin/x", "/admin/x"} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("%v %s did not panic", a, p)
					}
				}()
				srv.Router().Handle(p, server.KindRequest, a, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			}()
		}
	}
}

func TestMutatingMethodsAreRefusedAndRunNoHandler(t *testing.T) {
	srv := newServer(t)
	var guarded int
	count := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { guarded++; next.ServeHTTP(w, r) })
	}
	srv.Router().SetGuard(server.Admin, count)
	srv.Router().SetGuard(server.AdminJSON, count)
	Install(srv, stats.NewService(nil))
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		for _, p := range []string{"/admin", "/admin/api/overview", "/admin/api/series"} {
			req := httptest.NewRequest(m, "https://magnet.example.org"+p, strings.NewReader("x=1"))
			req.RemoteAddr = "203.0.113.9:1"
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %d, want 405", m, p, rec.Code)
			}
		}
	}
	if guarded != 0 {
		t.Fatalf("a mutating request reached a guard or handler %d times", guarded)
	}
}

func TestHandlersRefuseARequestThatDidNotPassTheGuard(t *testing.T) {
	srv := newServer(t)
	srv.Router().SetGuard(server.Admin, passThrough)
	srv.Router().SetGuard(server.AdminJSON, passThrough)
	Install(srv, stats.NewService(nil))
	for _, p := range []string{"/admin", "/admin/api/overview", "/admin/api/series?chart=queue&g=day"} {
		req := httptest.NewRequest("GET", "https://magnet.example.org"+p, nil)
		req.RemoteAddr = "203.0.113.9:1"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without a Principal: %d, want 403", p, rec.Code)
		}
	}
}

// Principals are granted by exactly one non-test file: the guard.
func TestOnlyTheGuardGrantsAPrincipal(t *testing.T) {
	var offenders []string
	_ = filepath.Walk("../..", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "access.Grant(") && filepath.ToSlash(path) != "../../web/auth/admin.go" {
			offenders = append(offenders, path)
		}
		return nil
	})
	if len(offenders) > 0 {
		t.Fatalf("access.Grant is called outside the guard: %v", offenders)
	}
	if access.Grant(0, "x").Valid() || (access.Principal{}).Valid() {
		t.Fatal("a Principal without a positive user id or the zero value is valid")
	}
}

func TestFormattingNeverTurnsUnavailableIntoZero(t *testing.T) {
	if fmtInt(stats.Measured[int64]{}) != dash || fmtBytes(stats.Measured[int64]{}) != dash ||
		fmtFloat(stats.Measured[float64]{}, "/s") != dash || fmtPercent(stats.Measured[float64]{}) != dash || fmtAge(stats.Measured[time.Duration]{}) != dash {
		t.Fatal("an unavailable value is not a dash")
	}
	if got := fmtInt(stats.Measured[int64]{Value: 0, Valid: true}); got != "0" {
		t.Fatalf("a measured zero renders as %q", got)
	}
	if got := fmtInt(stats.Measured[int64]{Value: 1234567, Valid: true, Approx: true}); got != "≈ 1,234,567" {
		t.Fatalf("approximate count: %q", got)
	}
	if got := bytesIEC(1536); got != "1.5 KiB" {
		t.Fatalf("bytesIEC: %q", got)
	}
}
