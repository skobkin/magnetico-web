package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
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
	shutdownGrace     = 30 * time.Second
	summaryInterval   = 5 * time.Minute
)

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
}

// Run opens the store and queue, starts the consumer and the HTTP server and
// blocks until ctx is cancelled. Shutdown order: stop accepting and finish
// in-flight requests (http.Server.Shutdown) → let the consumer finish its
// current record and release the lock → close the pools. It returns nil on a
// clean shutdown.
func Run(ctx context.Context, o Options) error {
	lg := o.Logger
	if lg == nil {
		lg = log.Default()
	}

	store, err := persistence.MakeDatabase(ctx, o.DatabaseURL)
	if err != nil {
		return fmt.Errorf("opening the database: %w", err)
	}
	defer func() { _ = store.Close() }()

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
	defer q.Close()

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
	consumer := pgqueue.NewConsumer(q, store, pgqueue.ConsumerOptions{OnStored: exist.MarkExisting})

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
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          lg,
	}

	consumerCtx, stopConsumer := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = consumer.Run(consumerCtx) }()
	go func() {
		defer wg.Done()
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

	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	if err := httpSrv.Shutdown(sctx); err != nil {
		_ = httpSrv.Close()
	}
	cancel()
	stopConsumer()
	wg.Wait()
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
