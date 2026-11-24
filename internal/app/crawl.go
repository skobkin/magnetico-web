package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/dht"
	"github.com/skobkin/magnetico-web/dht/mainline"
	"github.com/skobkin/magnetico-web/internal/crawler"
	"github.com/skobkin/magnetico-web/internal/crawlqueue"
	"github.com/skobkin/magnetico-web/metadata"
	"github.com/skobkin/magnetico-web/opflags"
	"github.com/skobkin/magnetico-web/persistence"
	"github.com/skobkin/magnetico-web/stats"
)

// invalidMetadataWarnInterval spaces out the dropped-record warnings: a
// hostile swarm can produce invalid records far faster than they are worth
// logging.
const invalidMetadataWarnInterval = time.Hour

var invalidMetadataWarn struct {
	mu      sync.Mutex
	lastSec int64
}

// warnInvalidMetadata logs a throttled warning for a dropped record. The
// detail is controlled text only; no file paths, infohashes or error chains.
func warnInvalidMetadata(err error) {
	invalidMetadataWarn.mu.Lock()
	defer invalidMetadataWarn.mu.Unlock()
	now := time.Now().Unix()
	if invalidMetadataWarn.lastSec != 0 &&
		now-invalidMetadataWarn.lastSec < int64(invalidMetadataWarnInterval/time.Second) {
		return
	}
	invalidMetadataWarn.lastSec = now
	log.Printf("dropping an invalid metadata record: %s", controlledErrorDetail(err))
}

// dropInvalidRecord handles a rejected metadata record: the throttled
// warning plus the per-cause metric (the UTF-8/NUL bucket keeps its
// dedicated counter). Used for every ErrInvalidMetadata drop, whether the
// record was rejected at the conversion boundary or by AddTorrent; it is
// never a database failure.
func dropInvalidRecord(err error) {
	warnInvalidMetadata(err)
	if errors.Is(err, persistence.ErrInvalidTextEncoding) {
		stats.GetInstance().IncNonUTF8()
	} else {
		stats.GetInstance().IncInvalidMetadata()
	}
}

// runCrawl runs the crawler and exits non-zero when the shutdown could not
// complete safely (see crawler.ErrShutdownIncomplete) or the local queue
// failed. Deferred cleanup runs inside crawl, before the exit.
func runCrawl(opFlags *opflags.CrawlOptions) {
	if opFlags.Subcommand == "check-queue" {
		os.Exit(runCrawlCheckQueue(&opFlags.CheckQueue))
	}
	if code := crawl(opFlags); code != 0 {
		os.Exit(code)
	}
}

// runCrawlCheckQueue implements `magnetico crawl check-queue`.
func runCrawlCheckQueue(o *opflags.CheckQueueOptions) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, err := crawlqueue.Check(ctx, o.QueueDir, o.Full)
	if err != nil {
		log.Printf("check-queue: %v", err)
		return 2
	}
	fmt.Printf("%+v\n", rep)
	if !rep.OK() {
		log.Printf("check-queue: the queue database did not pass the integrity check; committed data is never repaired automatically")
		return 1
	}
	return 0
}

// crawl is the delivery pipeline: DHT → precheck → bounded metadata
// retrieval → durable SQLite queue → authenticated HTTP sender → ingest. It
// has no PostgreSQL dependency. SIGINT/SIGTERM stops the DHT, then shuts the
// pipeline down in its documented order, each stage bounded by a grace period.
func crawl(opFlags *opflags.CrawlOptions) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if opFlags.PyroscopeURL != "" {
		profiler, err := stats.InitPyroscope(opFlags.PyroscopeURL)
		if err != nil {
			log.Printf("could not start pyroscope %s\n", err.Error())
			return 1
		}
		defer func() {
			if err := profiler.Stop(); err != nil {
				log.Printf("Could not stop pyroscope! %s\n", err.Error())
			}
		}()
	}

	queue, err := crawlqueue.Open(crawlqueue.Options{
		Dir:      opFlags.QueueDir,
		MaxBytes: opFlags.QueueMaxBytes,
		Logf:     log.Printf,
	})
	if err != nil {
		log.Printf("could not open the local queue in %s: %v", opFlags.QueueDir, err)
		return 1
	}
	client, err := crawler.NewClient(opFlags.IngestURL, opFlags.IngestToken, nil)
	if err != nil {
		_ = queue.CloseWithin(crawler.QueueCloseGrace)
		log.Printf("%v", err)
		return 1
	}

	mainline.DefaultThrottleRate = int(opFlags.MaxRPS)
	trawlingManager := dht.NewManager(
		opFlags.IndexerAddrs,
		opFlags.IndexerMaxNeighbors,
		opFlags.BootstrappingNodes,
		opFlags.FilterNodesIpNets,
	)
	sink := metadata.NewSinkWithOptions(metadata.SinkOptions{
		Deadline:        time.Duration(opFlags.LeechDeadline) * time.Second,
		MaxPeersPerHash: int(opFlags.LeechMaxN),
		MaxActive:       int(opFlags.LeechMaxActive),
		FilterNodes:     opFlags.FilterNodesIpNets,
	})
	pipeline := crawler.New(crawler.Config{
		Queue: queue, Client: client, Sink: sink, Limits: codec.DefaultLimits(),
		Logf: log.Printf, OnInvalid: dropInvalidRecord,
	})
	pipeline.Start()
	log.Printf("crawler started: delivering to %s, local queue %s (limit %d bytes), %d active leeches at most",
		redactDatabaseURL(opFlags.IngestURL), opFlags.QueueDir, opFlags.QueueMaxBytes, opFlags.LeechMaxActive)

	summary := time.NewTicker(crawler.SummaryInterval)
	defer summary.Stop()

	exitCode := 0
	// The Event Loop: it only hands results over (never blocks on I/O).
	for stopped := false; !stopped; {
		select {
		case <-ctx.Done():
			stopped = true

		case <-queue.Failed():
			log.Printf("the local queue failed (%v); committed records stay on disk. Shutting down.", queue.Err())
			exitCode = 1
			stopped = true

		case <-summary.C:
			log.Print(pipeline.Summary())

		case result, ok := <-trawlingManager.Output():
			if !ok {
				continue
			}
			pipeline.Offer(result)
		}
	}

	trawlingManager.Terminate()
	log.Print("shutting down: stopping the pipeline")
	if err := pipeline.Stop(); err != nil {
		log.Printf("shutdown incomplete: %v", err)
		exitCode = 3
	}
	log.Print(pipeline.Summary())
	return exitCode
}
