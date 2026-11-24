package app

import (
	"github.com/skobkin/magnetico-web/internal/web/filecache"
	webdb "github.com/skobkin/magnetico-web/internal/web/webdb"
)

// runWeb implements `magnetico web` (the HTTP foundation). It
// verifies the database read-only, serves until SIGINT/SIGTERM and then shuts
// down within the configured grace period. Error text is reduced to
// controlled content: database URLs and driver messages never reach the log.
// diagSources are the live objects a diagnostic build may report on. The hook is
// nil in every normal build: only a binary built with the test-only `w9diag`
// tag (main_w9diag.go) sets it, to serve non-destructive runtime statistics on a
// loopback listener for the W9 soak. Nothing in a production build can be
// reached through it.
type diagSources struct {
	FileCache *filecache.Cache
	Backend   *webdb.Backend
}

var diagHook func(diagSources)
