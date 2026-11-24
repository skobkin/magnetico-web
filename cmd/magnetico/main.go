// Command magnetico is the single executable of Magnetico Web: the DHT
// crawler, the ingest service, the web service and the migration tooling,
// selected by subcommand. The commands themselves live in internal/app.
package main

import "github.com/skobkin/magnetico-web/internal/app"

func main() { app.Main() }
