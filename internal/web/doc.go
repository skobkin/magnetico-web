// Package web is the root of the standalone `magnetico web` service packages
// (docs/specs/web-v1.md). The service is a separate dependency subgraph of the
// unified executable: nothing below internal/web may import the crawler, its
// SQLite queue, the ingest service or the DHT stack. boundary_test.go
// enforces this on the web packages themselves (the unified main package
// legitimately links everything and cannot prove isolation).
package web
