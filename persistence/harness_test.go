package persistence

// Integration harness: a disposable PostgreSQL cluster, bootstrapped lazily
// on the first integration test. The bootstrap itself (environment DSN →
// cached Docker postgres:17 → initdb/postgres on PATH → skip) is shared with
// the other integration-test packages via internal/pgtest; see its package
// comment for the full source policy. This file only keeps the package-local
// spellings the persistence suites use.

import (
	"testing"

	"github.com/skobkin/magnetico-web/internal/pgtest"
)

// testCluster is the shared disposable cluster type, aliased so the
// persistence suites keep reading as they did before the extraction.
type testCluster = pgtest.Cluster

// integrationCluster returns the shared disposable cluster or skips the test.
func integrationCluster(t *testing.T) *testCluster {
	return pgtest.IntegrationCluster(t)
}

// Aliases for the helpers the persistence suites use directly.
const dbOpTimeout = pgtest.DBOpTimeout

func randomSuffix() string { return pgtest.RandomSuffix() }

func quoteIdent(name string) string { return pgtest.QuoteIdent(name) }
