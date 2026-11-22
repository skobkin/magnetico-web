// Command crawlerd is a TEST-ONLY crawler harness for the end-to-end suite
// (internal/e2e). It runs the production delivery components — the enqueue
// writer, the durable SQLite queue and the HTTP sender — over a synthetic
// metadata source, so crash and fault tests can kill and restart a real
// process without a live DHT. It is never part of the shipped binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/crawler"
	"github.com/skobkin/magnetico-web/internal/crawlqueue"
	"github.com/skobkin/magnetico-web/metadata"
	"github.com/skobkin/magnetico-web/persistence"
)

func hashOf(n int) []byte {
	h := make([]byte, 20)
	h[0], h[1], h[2], h[3], h[19] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n), 0xE2
	return h
}

// synth builds a record with a realistic size mix: ~70% single-file, ~25%
// tens of files, ~5% hundreds of files.
func synth(n int) metadata.Metadata {
	files := 1
	switch {
	case n%20 == 0:
		files = 400
	case n%4 == 0:
		files = 30
	}
	var fs []persistence.File
	for i := 0; i < files; i++ {
		fs = append(fs, persistence.File{Path: fmt.Sprintf("season/episode-%04d/file-%04d.mkv", i, i), Size: 1000 + int64(i)})
	}
	var total int64
	for _, f := range fs {
		total += f.Size
	}
	return metadata.Metadata{InfoHash: hashOf(n), Name: fmt.Sprintf("e2e torrent %d", n),
		TotalSize: uint64(total), DiscoveredOn: 1700000000 + int64(n), Files: fs}
}

func main() {
	dir := flag.String("queue-dir", "", "queue directory")
	url := flag.String("url", "", "ingest URL")
	from := flag.Int("from", 1, "first synthetic id")
	count := flag.Int("count", 100, "number of synthetic records")
	rate := flag.Float64("rate", 0, "records per second (0 = as fast as possible)")
	poison := flag.Int("poison", 0, "append N strictly invalid payloads directly to the queue first")
	hold := flag.Bool("hold", false, "keep running after everything is delivered")
	maxBytes := flag.Int64("queue-max-bytes", 1<<30, "queue limit")
	flag.Parse()
	token := os.Getenv("MAGNETICO_CRAWL_INGEST_TOKEN")

	q, err := crawlqueue.Open(crawlqueue.Options{Dir: *dir, MaxBytes: *maxBytes, Logf: log.Printf})
	if err != nil {
		log.Fatalf("open queue: %v", err)
	}
	client, err := crawler.NewClient(*url, token, nil)
	if err != nil {
		log.Fatalf("client: %v", err)
	}
	counters := &crawler.Counters{}
	sender := crawler.NewSender(q, client, counters, log.Printf)
	enq := &crawler.Enqueuer{Queue: q, Limits: codec.DefaultLimits(), Counters: counters, Logf: log.Printf, OnAppended: sender.Wake}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for i := 0; i < *poison; i++ {
		bad := []byte(fmt.Sprintf(`{"v":1,"info_hash":"%x"}`, hashOf(9_000_000+i)))
		if _, err := q.Append(context.Background(), [20]byte(hashOf(9_000_000+i)), 1, bad); err != nil {
			log.Fatalf("poison: %v", err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); sender.Run(ctx, 5*time.Second) }()

	drain := make(chan metadata.Metadata, 16)
	producedAll := make(chan struct{})
	go func() {
		defer close(drain)
		defer close(producedAll)
		var tick <-chan time.Time
		if *rate > 0 {
			t := time.NewTicker(time.Duration(float64(time.Second) / *rate))
			defer t.Stop()
			tick = t.C
		}
		for i := 0; i < *count; i++ {
			if tick != nil {
				select {
				case <-tick:
				case <-ctx.Done():
					return
				}
			}
			select {
			case drain <- synth(*from + i):
			case <-ctx.Done():
				return
			}
		}
	}()
	enqDone := make(chan struct{})
	go func() { enq.Run(drain, context.Background()); close(enqDone) }()

	stats := time.NewTicker(500 * time.Millisecond)
	defer stats.Stop()
	exit := 0
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-q.Failed():
			log.Printf("queue failed: %v", q.Err())
			exit = 1
			break loop
		case <-stats.C:
			s := q.Stats()
			fmt.Printf("STATS queue_records=%d queue_bytes=%d quarantine_records=%d appended=%d acked=%d unrecognised=%d\n",
				s.QueueRecords, s.QueueBytes, s.QuarantineRecords, counters.EnqueueAppended.Load(), counters.SendAcked.Load(),
				counters.SendUnrecognised.Load())
			select {
			case <-enqDone:
				if !*hold && s.QueueRecords == 0 {
					break loop
				}
			default:
			}
		}
	}
	stop()
	wg.Wait()
	s := q.Stats()
	fmt.Printf("DONE queue_records=%d quarantine_records=%d appended=%d duplicate=%d acked=%d quarantined=%d\n",
		s.QueueRecords, s.QuarantineRecords, counters.EnqueueAppended.Load(), counters.EnqueueDuplicate.Load(),
		counters.SendAcked.Load(), counters.SendQuarantined.Load())
	if err := q.CloseWithin(5 * time.Second); err != nil {
		log.Printf("close: %v", err)
		exit = 3
	}
	os.Exit(exit)
}
