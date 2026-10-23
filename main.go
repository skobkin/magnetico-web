package main

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/skobkin/magnetico-web/dht"
	"github.com/skobkin/magnetico-web/dht/mainline"
	"github.com/skobkin/magnetico-web/metadata"
	"github.com/skobkin/magnetico-web/opflags"
	"github.com/skobkin/magnetico-web/persistence"
	"github.com/skobkin/magnetico-web/stats"
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

// unparsableDatabaseURL is printed instead of any database URL that cannot be
// safely reduced to scheme://host/path. Raw URLs are never echoed: they
// routinely carry credentials in the userinfo or the query.
const unparsableDatabaseURL = "<unparsable database URL>"

// redactDatabaseURL renders a database URL without any credential material:
// only the scheme, host and path survive. Userinfo, query, fragment and opaque
// parts are dropped by construction (never url.URL.String, which re-emits
// them). Input that cannot be parsed into a scheme and a host maps to a fixed
// placeholder string.
func redactDatabaseURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return unparsableDatabaseURL
	}
	return fmt.Sprintf("%s://%s%s", parsed.Scheme, parsed.Host, parsed.Path)
}

// controlledErrorDetail is the only error text allowed in database log lines.
// It never returns err.Error(), not even for a recognized sentinel: driver
// errors and wrapped sentinels can carry credentials anywhere in their text.
// A recognized sentinel contributes its own fixed message; anything else
// contributes only the error's Go type, which carries no runtime content.
func controlledErrorDetail(err error) string {
	if errors.Is(err, persistence.ErrStorageNotImplemented) {
		return persistence.ErrStorageNotImplemented.Error()
	}
	return fmt.Sprintf("unexpected error (%T)", err)
}

// databaseOpenFailureMessage composes the database-open failure log line from
// controlled content only: the redacted URL and the controlled error detail.
func databaseOpenFailureMessage(rawURL string, err error) string {
	return fmt.Sprintf(
		"Could not open the database %s. %s",
		redactDatabaseURL(rawURL), controlledErrorDetail(err),
	)
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
		// The raw URL and the error text may both carry credentials; log the
		// redacted URL and a controlled error detail instead.
		log.Fatalln(databaseOpenFailureMessage(opFlags.DatabaseURL, err))
	}
	defer func() {
		if err = database.Close(); err != nil {
			log.Printf("Could not close database! %s\n", controlledErrorDetail(err))
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
