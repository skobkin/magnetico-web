package app

import "github.com/skobkin/magnetico-web/internal/web/assemble"

// diagSources are the live objects a diagnostic build may report on. The hook is
// nil in every normal build: only a binary built with the test-only `w9diag`
// tag (w9diag.go) sets it, to serve non-destructive runtime statistics on a
// loopback listener for the W9 soak. Nothing in a production build can be
// reached through it.
type diagSources = assemble.Sources

var diagHook func(diagSources)
