package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"tgragnato.it/magnetico/v2/dht"
	"tgragnato.it/magnetico/v2/dht/mainline"
	"tgragnato.it/magnetico/v2/metadata"
	"tgragnato.it/magnetico/v2/opflags"
	"tgragnato.it/magnetico/v2/persistence"
	"tgragnato.it/magnetico/v2/stats"
)

func main() {
	// opFlags is the "operational flags"
	opFlags := opflags.OpFlags{}
	command, err := opFlags.Parse()
	if err != nil {
		log.Fatalln(err.Error())
	}

	switch command {
	case "crawl":
		runCrawl(&opFlags.Crawl)
	case "web":
		notImplemented("web",
			"the authenticated web UI will be designed from the historical PHP application in a later phase")
	case "migrate":
		notImplemented("migrate",
			"the offline legacy database migrator follows phase 3, see docs/specs/postgresql-persistence-v2.md")
	}
}

// notImplemented reports a reserved subcommand and exits non-zero.
func notImplemented(command, detail string) {
	fmt.Fprintf(os.Stderr, "magnetico %s: not implemented yet (%s)\n", command, detail)
	os.Exit(1)
}

func runCrawl(opFlags *opflags.CrawlOptions) {
	// Handle Ctrl-C gracefully.
	interruptChan := make(chan os.Signal, 1)
	signal.Notify(interruptChan, os.Interrupt)

	// Fail closed: until phase 3 implements the new storage, MakeDatabase
	// refuses every database URL, so the crawler exits here, before any DHT
	// or profiling network activity starts. It must never open, create or
	// migrate a legacy database.
	database, err := persistence.MakeDatabase(opFlags.DatabaseURL)
	if err != nil {
		log.Fatalf("Could not open the database %s. %s\n", opFlags.DatabaseURL, err.Error())
	}
	defer func() {
		if err = database.Close(); err != nil {
			log.Printf("Could not close database! %s\n", err.Error())
		}
	}()

	if opFlags.PyroscopeURL != "" {
		profiler, err := stats.InitPyroscope(opFlags.PyroscopeURL)
		if err != nil {
			log.Fatalf("could not start pyroscope %s\n", err.Error())
		}
		defer func() {
			if err := profiler.Stop(); err != nil {
				log.Printf("Could not stop pyroscope! %s\n", err.Error())
			}
		}()
	}

	mainline.DefaultThrottleRate = int(opFlags.MaxRPS)
	trawlingManager := dht.NewManager(
		opFlags.IndexerAddrs,
		opFlags.IndexerMaxNeighbors,
		opFlags.BootstrappingNodes,
		opFlags.FilterNodesIpNets,
	)
	metadataSink := metadata.NewSink(
		time.Duration(opFlags.LeechDeadline)*time.Second,
		int(opFlags.LeechMaxN),
		opFlags.FilterNodesIpNets,
	)

	// The Event Loop
	for stopped := false; !stopped; {
		select {
		case result, ok := <-trawlingManager.Output():
			if !ok {
				continue
			}
			infoHash := result.InfoHash()

			exists, err := database.DoesTorrentExist(infoHash[:])
			if err != nil {
				go stats.GetInstance().IncDBError(false)
			} else if !exists {
				metadataSink.Sink(result)
			}

		case md := <-metadataSink.Drain():
			if err := database.AddNewTorrent(md.InfoHash, md.Name, md.Files); err != nil {
				go stats.GetInstance().IncDBError(true)
			}

		case <-interruptChan:
			trawlingManager.Terminate()
			stopped = true
		}
	}
}
