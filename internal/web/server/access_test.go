package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func ok(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }

func mustPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", what)
		}
	}()
	f()
}

func TestPublicRoutesAreSafeMethodsOnly(t *testing.T) {
	f := newFixture(t, nil)
	rt := f.srv.Router()
	for _, p := range []string{"POST /x", "PUT /x", "DELETE /x", "PATCH /x", "/x", "OPTIONS /x"} {
		mustPanic(t, "Public "+p, func() { rt.Handle(p, KindRequest, Public, http.HandlerFunc(ok)) })
	}
	rt.Handle("GET /g", KindRequest, Public, http.HandlerFunc(ok))
	rt.Handle("HEAD /h", KindRequest, Public, http.HandlerFunc(ok))
}

func TestGuardedClassesNeedInstalledGuards(t *testing.T) {
	f := newFixture(t, nil)
	rt := f.srv.Router()
	mustPanic(t, "PublicForm without a guard", func() { rt.Handle("POST /f", KindRequest, PublicForm, http.HandlerFunc(ok)) })
	mustPanic(t, "Session without a guard", func() { rt.Handle("GET /s", KindRequest, Session, http.HandlerFunc(ok)) })

	var applied []string
	guard := func(name string) Guard {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				applied = append(applied, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	rt.SetGuards(map[Access]Guard{PublicForm: guard("form"), Session: guard("session")})
	rt.Handle("POST /f", KindRequest, PublicForm, http.HandlerFunc(ok))
	rt.Handle("GET /s", KindRequest, Session, http.HandlerFunc(ok))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "https://magnet.example.org/f", nil)
	req.RemoteAddr = "203.0.113.9:1"
	f.srv.Handler().ServeHTTP(rec, req)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/s", nil)
	req.RemoteAddr = "203.0.113.9:1"
	f.srv.Handler().ServeHTTP(rec, req)
	if strings.Join(applied, ",") != "form,session" {
		t.Fatalf("guards applied: %v", applied)
	}
}

func TestRouteTableRecordsAccessClasses(t *testing.T) {
	f := newFixture(t, nil)
	got := map[string]Access{}
	for _, r := range f.srv.Router().Routes() {
		got[r.Pattern] = r.Access
	}
	for _, p := range []string{"GET /healthz", "GET /readyz", "GET /robots.txt", "GET /static/{file}"} {
		if a, ok := got[p]; !ok || a != Public {
			t.Errorf("%s: %v %v", p, a, ok)
		}
	}
	// The invariant later milestones rely on: no registered state-changing
	// route is Public.
	for _, r := range f.srv.Router().Routes() {
		m, _, _ := strings.Cut(r.Pattern, " ")
		if !safeMethod(m) && r.Access == Public {
			t.Errorf("state-changing public route %s", r.Pattern)
		}
	}
}

func TestAccessLogCarriesUserIDNotName(t *testing.T) {
	f := newFixture(t, nil)
	f.srv.Router().Handle("GET /me", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetUserID(r.Context(), 42)
		w.WriteHeader(204)
	}))
	f.get("/me")
	if !strings.Contains(f.log.String(), "user_id=42") {
		t.Fatalf("no user_id in the access log:\n%s", f.log.String())
	}
	f.get("/healthz")
	for _, line := range strings.Split(f.log.String(), "\n") {
		if strings.Contains(line, "route=\"GET /healthz\"") && strings.Contains(line, "user_id") {
			t.Errorf("anonymous request logged a user_id: %s", line)
		}
	}
}

func TestStartupHookFailureFailsClosedWithoutListening(t *testing.T) {
	f := newFixture(t, nil)
	boom := errors.New("hook failed")
	f.srv.OnStartup(func(context.Context) error { return boom })
	var started atomic.Bool
	f.srv.SetBackground(func(context.Context) { started.Store(true) })
	err := f.srv.Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("Run: %v", err)
	}
	if f.be.closed.Load() != 1 || started.Load() {
		t.Fatalf("backend closed %d times, background started=%v", f.be.closed.Load(), started.Load())
	}
}

func TestBackgroundNeverStartsOnVerificationOrListenFailure(t *testing.T) {
	var started atomic.Bool
	f := newFixture(t, nil)
	f.be.verifyErr = errors.New("bad schema")
	f.srv.SetBackground(func(context.Context) { started.Store(true) })
	if err := f.srv.Run(context.Background()); err == nil || started.Load() {
		t.Fatalf("verification failure: err=%v started=%v", err, started.Load())
	}

	started.Store(false)
	f = newFixture(t, func(c *Config) { c.Listen = "256.0.0.1:0" }) // cannot bind
	f.srv.SetBackground(func(context.Context) { started.Store(true) })
	if err := f.srv.Run(context.Background()); err == nil || started.Load() {
		t.Fatalf("listen failure: err=%v started=%v", err, started.Load())
	}
	if f.be.closed.Load() != 1 {
		t.Fatal("backend not closed")
	}
}

// orderBackend records whether the background task had stopped when the
// backend was closed.
type orderBackend struct {
	fakeBackend
	bgRunning          *atomic.Bool
	closedWhileRunning atomic.Bool
}

func (b *orderBackend) Close() {
	if b.bgRunning.Load() {
		b.closedWhileRunning.Store(true)
	}
	b.fakeBackend.Close()
}

func runWithBackground(t *testing.T, bg func(ctx context.Context, running *atomic.Bool), mutate func(*Server)) (err error, closedWhileRunning bool, be *orderBackend) {
	t.Helper()
	f := newFixture(t, func(c *Config) { c.ShutdownTimeout = time.Second })
	running := &atomic.Bool{}
	be = &orderBackend{bgRunning: running}
	s, e := New(f.srv.cfg, be)
	if e != nil {
		t.Fatal(e)
	}
	entered := make(chan struct{})
	s.SetBackground(func(ctx context.Context) {
		running.Store(true)
		close(entered)
		bg(ctx, running)
	})
	if mutate != nil {
		mutate(s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("background task never started")
	}
	cancel()
	select {
	case err = <-errc:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}
	return err, be.closedWhileRunning.Load(), be
}

func TestBackgroundStopsBeforeTheBackendClosesOnSIGTERM(t *testing.T) {
	err, closedWhileRunning, be := runWithBackground(t, func(ctx context.Context, running *atomic.Bool) {
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // finishing work takes a moment
		running.Store(false)
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if closedWhileRunning || be.closed.Load() != 1 {
		t.Fatalf("backend closed while the background task still ran (%v) / closed=%d", closedWhileRunning, be.closed.Load())
	}
}

func TestStuckBackgroundIsBoundedAndReportsIncompleteShutdown(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	err, _, be := runWithBackground(t, func(ctx context.Context, running *atomic.Bool) {
		<-release // ignores cancellation
	}, func(s *Server) { s.bgStopWait = 200 * time.Millisecond })
	if !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("Run: %v, want ErrShutdownIncomplete", err)
	}
	if be.closed.Load() != 1 {
		t.Fatal("the backend must still be closed after a bounded wait")
	}
}

func TestBackgroundStopsBeforeCloseOnUnexpectedServeFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	running := &atomic.Bool{}
	be := &orderBackend{bgRunning: running}
	f := newFixture(t, func(c *Config) { c.Listener = ln })
	s, err := New(f.srv.cfg, be)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	s.SetBackground(func(ctx context.Context) {
		running.Store(true)
		close(entered)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		running.Store(false)
	})
	errc := make(chan error, 1)
	go func() { errc <- s.Run(context.Background()) }()
	<-entered
	ln.Close() // Serve fails although nobody asked for a shutdown
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "serve") {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the listener failed")
	}
	if be.closedWhileRunning.Load() || be.closed.Load() != 1 {
		t.Fatalf("closedWhileRunning=%v closed=%d", be.closedWhileRunning.Load(), be.closed.Load())
	}
}

func TestBackgroundStopsOnForcedShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	running := &atomic.Bool{}
	be := &orderBackend{bgRunning: running}
	f := newFixture(t, func(c *Config) { c.Listener = ln; c.ShutdownTimeout = 200 * time.Millisecond })
	s, err := New(f.srv.cfg, be)
	if err != nil {
		t.Fatal(err)
	}
	inHandler := make(chan struct{})
	s.Router().Handle("GET /stuck", KindRequest, Public, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inHandler)
		<-r.Context().Done() // only the forced cancel ends it
	}))
	bgEntered := make(chan struct{})
	s.SetBackground(func(ctx context.Context) {
		running.Store(true)
		close(bgEntered)
		<-ctx.Done()
		running.Store(false)
	})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()
	<-bgEntered
	go http.Get("http://" + ln.Addr().String() + "/stuck")
	<-inHandler
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrShutdownIncomplete) {
			t.Fatalf("Run: %v, want ErrShutdownIncomplete", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}
	if be.closedWhileRunning.Load() || running.Load() {
		t.Fatal("background task still running at backend close")
	}
}
