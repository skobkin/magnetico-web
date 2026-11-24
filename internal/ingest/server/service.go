package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/skobkin/magnetico-web/internal/ingest/cache"
	"github.com/skobkin/magnetico-web/internal/ingest/existence"
	"github.com/skobkin/magnetico-web/internal/ingest/pgqueue"
	"github.com/skobkin/magnetico-web/persistence"
)

// HTTP server limits (spec §5.5).
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 16 << 10
	summaryInterval   = 5 * time.Minute
)

// Shutdown budgets (see Run). The documented guarantee is that Run returns
// within max(HTTPGrace, ConsumerGrace+AbortGrace) + HandlerGrace plus at most
// 5 s for the final summary: 28 s with the defaults.
const (
	DefaultHTTPGrace     = 20 * time.Second
	DefaultConsumerGrace = 10 * time.Second
	DefaultAbortGrace    = 5 * time.Second
	DefaultHandlerGrace  = 3 * time.Second
)

// ErrShutdownIncomplete is returned by Run when a goroutine using the database
// pools could not be stopped within its budget. The pools were left open and
// the process must exit; interrupted work is reprocessed after the next start.
var ErrShutdownIncomplete = errors.New("ingest: shutdown incomplete; a stuck goroutine was abandoned")

type graces struct{ httpGrace, consumerGrace, abortGrace, handlerGrace time.Duration }

func (o Options) durations() graces {
	g := graces{o.HTTPGrace, o.ConsumerGrace, o.AbortGrace, o.HandlerGrace}
	if g.httpGrace <= 0 {
		g.httpGrace = DefaultHTTPGrace
	}
	if g.consumerGrace <= 0 {
		g.consumerGrace = DefaultConsumerGrace
	}
	if g.abortGrace <= 0 {
		g.abortGrace = DefaultAbortGrace
	}
	if g.handlerGrace <= 0 {
		g.handlerGrace = DefaultHandlerGrace
	}
	return g
}

// Options configures Run.
type Options struct {
	Listen           string
	DatabaseURL      string // the 0.8.0 store
	QueueDatabaseURL string // optional least-privilege DSN for magnetico_ingest (default DatabaseURL)
	Token            string

	QueueMaxBytes   int64
	CacheMaxBytes   int64
	CacheDisabled   bool
	AllowNonDurable bool

	Logger *log.Logger
	// Ready, when set, receives the bound listen address once the service
	// accepts requests (tests use ":0").
	Ready func(addr net.Addr)

	// SummaryInterval overrides the counter-summary period (tests).
	SummaryInterval time.Duration

	// Shutdown budgets; zero selects the defaults above.
	HTTPGrace, ConsumerGrace, AbortGrace, HandlerGrace time.Duration

	// consumerStore, set by tests only, wraps the consumer's store.
	consumerStore func(real pgqueue.Store) pgqueue.Store
}

// Run opens the store and queue, starts the consumer and the HTTP server and
// blocks until ctx is cancelled. Shutdown is bounded (see the Default*Grace
// constants and the end of this function): the consumer finishes or is
// interrupted at its current record while the HTTP server drains, and the
// pools are closed only if nothing that uses them had to be abandoned. It
// returns nil on a clean shutdown and ErrShutdownIncomplete otherwise.
func Run(ctx context.Context, o Options) error {
	lg := o.Logger
	if lg == nil {
		lg = log.Default()
	}

	// abandoned is set when a goroutine that uses the pools could not be
	// stopped within its shutdown budget. Closing a pool with a connection
	// still checked out blocks forever and would yank it from under the stuck
	// goroutine, so in that case the pools are left to process exit.
	var abandoned atomic.Bool

	store, err := persistence.MakeDatabase(ctx, o.DatabaseURL)
	if err != nil {
		return fmt.Errorf("opening the database: %w", err)
	}
	defer func() {
		if !abandoned.Load() {
			_ = store.Close()
		}
	}()

	queueDSN := o.QueueDatabaseURL
	if queueDSN == "" {
		queueDSN = o.DatabaseURL
	}
	q, err := pgqueue.Open(ctx, queueDSN, pgqueue.Options{
		MaxBytes: o.QueueMaxBytes, AllowNonDurable: o.AllowNonDurable,
	})
	if err != nil {
		return fmt.Errorf("opening the ingest queue: %w", err)
	}
	defer func() {
		if !abandoned.Load() {
			q.Close()
		}
	}()

	if st, err := q.Stats(ctx); err == nil {
		lg.Printf("ingest queue: %d pending (%d bytes), %d quarantined (%d bytes)",
			st.QueueRows, st.QueueBytes, st.QuarantineRows, st.QuarantineBytes)
		if !st.UsageConsistent() {
			lg.Printf("WARNING: magnetico_ingest.usage.used_bytes (%d) differs from the table sums (%d); "+
				"admissions keep using used_bytes", st.UsedBytes, st.QueueBytes+st.QuarantineBytes)
		}
	}
	lg.Printf("note: records ingested from now on are not covered by earlier migration verification verdicts")

	var c cache.ExistenceCache = &cache.Disabled{}
	if !o.CacheDisabled {
		c = cache.New(cache.Options{MaxBytes: o.CacheMaxBytes})
	}
	exist := existence.New(store, c, existence.Options{})
	var consumerStore pgqueue.Store = store
	if o.consumerStore != nil {
		consumerStore = o.consumerStore(store)
	}
	consumer := pgqueue.NewConsumer(q, consumerStore, pgqueue.ConsumerOptions{OnStored: exist.MarkExisting})

	srv, err := New(Config{
		Token: o.Token, Queue: q, Exist: exist, OnAdmitted: consumer.Wake, Logger: lg,
	})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", o.Listen, err)
	}
	// Requests in flight are counted so shutdown can tell when no handler can
	// still be using the pools.
	var inflight atomic.Int64
	handler := srv.Handler()
	httpSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inflight.Add(1)
			defer inflight.Add(-1)
			handler.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          lg,
	}

	// Cancelling consumerCtx asks the consumer to stop after its current
	// record; consumer.Abort() interrupts that record (see shutdown below).
	consumerCtx, stopConsumer := context.WithCancel(context.WithoutCancel(ctx))
	consumerDone := make(chan struct{})
	summaryDone := make(chan struct{})
	go func() { defer close(consumerDone); _ = consumer.Run(consumerCtx) }()
	go func() {
		defer close(summaryDone)
		interval := o.SummaryInterval
		if interval <= 0 {
			interval = summaryInterval
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-consumerCtx.Done():
				return
			case <-t.C:
				logSummary(lg, srv, exist, consumer, q)
			}
		}
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	lg.Printf("ingest listening on %s", ln.Addr())
	if o.Ready != nil {
		o.Ready(ln.Addr())
	}

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("http server: %w", err)
		}
	}

	// Bounded shutdown. The consumer and the HTTP server stop concurrently:
	//   - the consumer finishes the record it is on (ConsumerGrace), then that
	//     record is interrupted (Abort) and given AbortGrace to return;
	//   - the HTTP server drains in-flight requests for HTTPGrace, then drops
	//     their connections, which cancels the request contexts;
	//   - finally handlers get HandlerGrace to return.
	// Anything still running after its budget is abandoned and the pools are
	// not closed (see abandoned); the interrupted work is idempotent and is
	// redone by the next start. The worst case is
	// max(HTTPGrace, ConsumerGrace+AbortGrace) + HandlerGrace + the summary.
	stopConsumer()
	httpDone := make(chan struct{})
	go func() { //nolint:gosec // G118: the shutdown budget needs a fresh context; the serving context is already cancelled
		defer close(httpDone)
		sctx, cancel := context.WithTimeout(context.Background(), o.durations().httpGrace)
		defer cancel()
		if err := httpSrv.Shutdown(sctx); err != nil {
			_ = httpSrv.Close()
		}
	}()

	d := o.durations()
	var stuck []string
	select {
	case <-consumerDone:
	case <-time.After(d.consumerGrace):
		lg.Printf("shutdown: the consumer did not finish its record in %s; interrupting it", d.consumerGrace)
		consumer.Abort()
		select {
		case <-consumerDone:
		case <-time.After(d.abortGrace):
			stuck = append(stuck, "queue consumer")
		}
	}
	<-httpDone
	handlersDone := time.Now().Add(d.handlerGrace)
	for inflight.Load() > 0 && time.Now().Before(handlersDone) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := inflight.Load(); n > 0 {
		stuck = append(stuck, fmt.Sprintf("%d HTTP request handler(s)", n))
	}
	<-summaryDone

	if len(stuck) > 0 {
		abandoned.Store(true)
		lg.Printf("shutdown: abandoned %s; database pools are left open and interrupted work will be "+
			"reprocessed idempotently after the next start", strings.Join(stuck, ", "))
		return errors.Join(runErr, ErrShutdownIncomplete)
	}
	logSummary(lg, srv, exist, consumer, q)
	return runErr
}

func logSummary(lg *log.Logger, s *Server, e *existence.Service, c *pgqueue.Consumer, q *pgqueue.Queue) {
	sc, es, cs := s.Counters(), e.Stats(), c.Stats()
	lg.Printf("ingest summary: submit requests=%d accepted=%d rejected=%d queue_full=%d storage_errors=%d busy=%d auth_failures=%d | "+
		"exists requests=%d unknown=%d cache_hits=%d db_lookups=%d db_errors=%d | "+
		"consumer stored=%d already=%d quarantined=%d retried=%d storage_errors=%d lock_losses=%d",
		sc.SubmitRequests, sc.SubmitAccepted, sc.SubmitRejected, sc.QueueFull, sc.StorageErrors, sc.Busy, sc.AuthFailures,
		sc.ExistsRequests, sc.ExistsUnknown, es.CacheHits, es.DBLookups, es.DBErrors,
		cs.Stored, cs.Already, cs.Quarantined, cs.Retried, cs.StorageErrs, cs.LockLosses)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if st, err := q.Stats(ctx); err == nil {
		lg.Printf("ingest queue: %d pending (%d bytes), %d quarantined (%d bytes), used_bytes=%d",
			st.QueueRows, st.QueueBytes, st.QuarantineRows, st.QuarantineBytes, st.UsedBytes)
	}
}
