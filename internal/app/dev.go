package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/skobkin/magnetico-web/internal/devseed"
	"github.com/skobkin/magnetico-web/opflags"
)

// runDevSeedTorrents implements `magnetico dev seed-torrents`: the
// deterministic synthetic dataset, written through the production persistence
// API into a verified, empty-or-fixture-only database. Output carries counts
// and the redacted URL only; refusals use fixed messages.
func runDevSeedTorrents(o *opflags.SeedTorrentsOptions) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := devseed.Seed(ctx, o.DatabaseURL, o.DryRun)
	if err != nil {
		detail := controlledErrorDetail(err)
		switch {
		case errors.Is(err, devseed.ErrForeignTorrents), errors.Is(err, devseed.ErrFixtureConflict), errors.Is(err, devseed.ErrBusy):
			detail = err.Error()
		}
		log.Fatalln(appendSchemaStateDetails(fmt.Sprintf(
			"dev seed-torrents: database %s: %s", redactDatabaseURL(o.DatabaseURL), detail), err))
	}
	verb, n := "added", res.Created
	if res.DryRun {
		verb, n = "would add", res.Torrents-res.Existing
	}
	log.Printf("dev seed-torrents on %s: database was %s; dataset %s: %d torrents, %d files, %d bytes; %d already stored; %s %d torrents",
		redactDatabaseURL(o.DatabaseURL), res.Before, devseed.Version, res.Torrents, res.Files, res.Bytes,
		res.Existing, verb, n)
}
