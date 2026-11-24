package app

// An in-process web service for the W9 security sweeps: the exact wiring of the
// binary (assemble.Web + Run, the same code path as runWeb), over a disposable
// database, with the log captured so tests can scan it and the route table
// readable so a sweep cannot miss a route.

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/assemble"
	"github.com/skobkin/magnetico-web/internal/web/server"
	"github.com/skobkin/magnetico-web/internal/web/webdb"
	"github.com/skobkin/magnetico-web/opflags"
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type inprocWeb struct {
	t      *testing.T
	srv    *server.Server
	addr   string
	logs   *syncBuf
	cancel context.CancelFunc
	done   chan error
}

// startInprocWeb runs the production wiring against dsn. mod adjusts the
// options before assembly (trusted proxies, SMTP, lifetimes, …).
func startInprocWeb(t *testing.T, dsn string, mod func(o *opflags.WebOptions)) *inprocWeb {
	t.Helper()
	addr := freeLoopbackAddr(t)
	pub, _ := url.Parse("http://" + addr)
	o := &opflags.WebOptions{
		Listen: addr, PublicURL: pub.String(), PublicBaseURL: pub, DatabaseURL: dsn, PoolMaxConns: 10, SiteTitle: "Magnetico",
		SessionIdleTimeout: 12 * time.Hour, SessionAbsoluteLifetime: 168 * time.Hour, SessionRememberLifetime: 2160 * time.Hour,
		SearchTimeout: 20 * time.Second, SearchConcurrency: 4, RequestTimeout: 20 * time.Second, ShutdownTimeout: 10 * time.Second,
		TrackerCacheTTL: time.Minute, LogLevel: "debug", LogFormat: "text", DevMode: true,
	}
	if mod != nil {
		mod(o)
	}
	logs := &syncBuf{}
	logger, err := server.NewLogger(logs, o.LogLevel, o.LogFormat)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	backend, err := webdb.Open(ctx, o.DatabaseURL, o.PoolMaxConns)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	srv, err := assemble.Web(o, backend, logger, nil)
	if err != nil {
		backend.Close()
		cancel()
		t.Fatal(err)
	}
	w := &inprocWeb{t: t, srv: srv, addr: o.Listen, logs: logs, cancel: cancel, done: make(chan error, 1)}
	go func() { w.done <- srv.Run(ctx) }()
	t.Cleanup(w.stop)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if resp, err := http.Get("http://" + w.addr + "/readyz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return w
			}
		}
		select {
		case err := <-w.done:
			t.Fatalf("web service stopped during startup: %v\n%s", err, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (w *inprocWeb) stop() {
	w.cancel()
	select {
	case err := <-w.done:
		if err != nil {
			w.t.Errorf("web service stopped with an error: %v", err)
		}
		w.done <- nil
	case <-time.After(30 * time.Second):
		w.t.Error("the in-process web service did not stop")
	}
}

// routes returns the registered route table.
func (w *inprocWeb) routes() []server.Route { return w.srv.Router().Routes() }
