// Command magnetico is the single executable of Magnetico Web: the DHT
// crawler, the ingest service, the web service and the migration tooling,
// selected by subcommand. The commands themselves live in internal/app.
package main

import (
	// Embeds the IANA time zone database (about 450 KB) so that resolving a
	// zone by name (migrate users --source-timezone) works in the minimal
	// container image and on hosts without /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/skobkin/magnetico-web/internal/app"
)

func main() { app.Main() }
