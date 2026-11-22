// Command crawlerd is a TEST-ONLY crawler harness for the end-to-end suite
// (internal/e2e). It runs the production delivery components — the enqueue
// writer, the durable SQLite queue and the HTTP sender — over a synthetic
// metadata source, so crash and fault tests can kill and restart a real
// process without a live DHT. It is never part of the shipped binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/crawler"
	"github.com/skobkin/magnetico-web/internal/crawlqueue"
	"github.com/skobkin/magnetico-web/internal/ingest/record"
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

// bigMeta builds a valid record whose serialized JSON is exactly target bytes
// (record.Encode output), made of many files so the size comes from the file
// list as in a real large torrent. The name stays short.
func bigMeta(id, target int) metadata.Metadata {
	mk := func(n, pad int) metadata.Metadata {
		fs := make([]persistence.File, n)
		var total int64
		for i := range fs {
			p := fmt.Sprintf("media/set-%06d/season-%02d/episode-%06d.mkv", i/50, i%50, i)
			if i == n-1 {
				p += strings.Repeat("x", pad)
			}
			fs[i] = persistence.File{Path: p, Size: 1000 + int64(i)}
			total += fs[i].Size
		}
		return metadata.Metadata{InfoHash: hashOf(id), Name: fmt.Sprintf("e2e big %d", id),
			TotalSize: uint64(total), DiscoveredOn: 1700000000 + int64(id), Files: fs}
	}
	encLen := func(md metadata.Metadata) int {
		r, err := record.FromMetadata(&md)
		if err != nil {
			log.Fatalf("bigMeta: %v", err)
		}
		data, err := record.Encode(r, codec.DefaultLimits())
		if err != nil && !errors.Is(err, record.ErrRecordTooLarge) {
			log.Fatalf("bigMeta: %v", err)
		}
		return len(data)
	}
	n := target / 90
	if n < 1 {
		n = 1
	}
	for iter := 0; iter < 40; iter++ {
		md := mk(n, 0)
		l := encLen(md)
		switch {
		case l > target:
			n -= (l-target)/90 + 1
		case target-l >= 120:
			n += (target - l) / 100
		default:
			md = mk(n, target-l)
			if got := encLen(md); got != target {
				log.Fatalf("bigMeta: wanted %d bytes, built %d", target, got)
			}
			return md
		}
	}
	log.Fatalf("bigMeta: did not converge on %d bytes", target)
	return metadata.Metadata{}
}

type phase struct {
	rate  float64
	count int
}

// parsePhases parses "rate:count,rate:count" (rate 0 = unthrottled).
func parsePhases(s string) ([]phase, error) {
	var out []phase
	for _, part := range strings.Split(s, ",") {
		rc := strings.Split(strings.TrimSpace(part), ":")
		if len(rc) != 2 {
			return nil, fmt.Errorf("bad phase %q (want rate:count)", part)
		}
		r, err1 := strconv.ParseFloat(rc[0], 64)
		c, err2 := strconv.Atoi(rc[1])
		if err1 != nil || err2 != nil || r < 0 || c < 0 {
			return nil, fmt.Errorf("bad phase %q", part)
		}
		out = append(out, phase{r, c})
	}
	return out, nil
}

// parseBig parses "bytes:count,bytes:count" into a list of target JSON sizes,
// interleaved round-robin across the kinds.
func parseBig(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	var kinds [][2]int
	for _, part := range strings.Split(s, ",") {
		sc := strings.Split(strings.TrimSpace(part), ":")
		if len(sc) != 2 {
			return nil, fmt.Errorf("bad big spec %q (want bytes:count)", part)
		}
		sz, err1 := strconv.Atoi(sc[0])
		c, err2 := strconv.Atoi(sc[1])
		if err1 != nil || err2 != nil || sz <= 0 || c < 0 {
			return nil, fmt.Errorf("bad big spec %q", part)
		}
		kinds = append(kinds, [2]int{sz, c})
	}
	var out []int
	for more := true; more; {
		more = false
		for i := range kinds {
			if kinds[i][1] > 0 {
				out = append(out, kinds[i][0])
				kinds[i][1]--
				more = true
			}
		}
	}
	return out, nil
}

func main() {
	dir := flag.String("queue-dir", "", "queue directory")
	url := flag.String("url", "", "ingest URL")
	from := flag.Int("from", 1, "first synthetic id")
	count := flag.Int("count", 100, "number of synthetic records")
	rate := flag.Float64("rate", 0, "records per second (0 = as fast as possible)")
	poison := flag.Int("poison", 0, "append N strictly invalid payloads directly to the queue first")
	phasesFlag := flag.String("phases", "", "rate:count[,rate:count...]; overrides -rate/-count (ordinary records only)")
	bigFlag := flag.String("big", "", "bytes:count[,bytes:count...] large records (target JSON size) interleaved with the ordinary ones")
	bigFrom := flag.Int("big-from", 7_000_000, "first synthetic id of the large records")
	poisonLarge := flag.Int("poison-large", 0, "append N over-limit (16 MiB + 4 KiB) records directly to the queue first")
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

	for i := 0; i < *poisonLarge; i++ {
		md := bigMeta(9_500_000+i, record.MaxRecordBytes+4096)
		r, _ := record.FromMetadata(&md)
		data, err := record.Encode(r, codec.DefaultLimits())
		if !errors.Is(err, record.ErrRecordTooLarge) {
			log.Fatalf("poison-large: expected ErrRecordTooLarge, got %v", err)
		}
		if _, err := q.Append(context.Background(), r.InfoHash, 1, data); err != nil {
			log.Fatalf("poison-large: %v", err)
		}
	}

	phases := []phase{{*rate, *count}}
	if *phasesFlag != "" {
		var err error
		if phases, err = parsePhases(*phasesFlag); err != nil {
			log.Fatalf("phases: %v", err)
		}
	}
	bigs, err := parseBig(*bigFlag)
	if err != nil {
		log.Fatalf("big: %v", err)
	}
	ordinary := 0
	for _, ph := range phases {
		ordinary += ph.count
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); sender.Run(ctx, 5*time.Second) }()

	drain := make(chan metadata.Metadata, 16)
	producedAll := make(chan struct{})
	go func() {
		defer close(drain)
		defer close(producedAll)
		bigEvery := 0
		if len(bigs) > 0 {
			bigEvery = ordinary / len(bigs)
			if bigEvery < 1 {
				bigEvery = 1
			}
		}
		emit := func(md metadata.Metadata) bool {
			select {
			case drain <- md:
				return true
			case <-ctx.Done():
				return false
			}
		}
		next, bi := *from, 0
		for _, ph := range phases {
			var tick <-chan time.Time
			if ph.rate > 0 {
				t := time.NewTicker(time.Duration(float64(time.Second) / ph.rate))
				defer t.Stop()
				tick = t.C
			}
			for i := 0; i < ph.count; i++ {
				if tick != nil {
					select {
					case <-tick:
					case <-ctx.Done():
						return
					}
				}
				if bigEvery > 0 && bi < len(bigs) && i%bigEvery == bigEvery/2 {
					if !emit(bigMeta(*bigFrom+bi, bigs[bi])) {
						return
					}
					bi++
				}
				if !emit(synth(next)) {
					return
				}
				next++
			}
		}
		for ; bi < len(bigs); bi++ { // any not yet placed
			if !emit(bigMeta(*bigFrom+bi, bigs[bi])) {
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
	fmt.Printf("DONE queue_records=%d quarantine_records=%d appended=%d duplicate=%d acked=%d quarantined=%d too_large=%d invalid=%d dropped_shutdown=%d\n",
		s.QueueRecords, s.QuarantineRecords, counters.EnqueueAppended.Load(), counters.EnqueueDuplicate.Load(),
		counters.SendAcked.Load(), counters.SendQuarantined.Load(), counters.EnqueueTooLarge.Load(),
		counters.EnqueueInvalid.Load(), counters.DroppedShutdown.Load())
	if err := q.CloseWithin(5 * time.Second); err != nil {
		log.Printf("close: %v", err)
		exit = 3
	}
	os.Exit(exit)
}
