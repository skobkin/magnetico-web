// Package webdb is the production database backend of the web service: its
// own bounded pgx pool, the read-only startup verification and the
// readiness ping. It creates, alters and migrates nothing.
package webdb

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/internal/web/webschema"
	"github.com/skobkin/magnetico-web/persistence"
)

// Re-exported so callers need only this package: the failure classes of the
// startup verification.
var (
	// ErrWebSchemaMissing: the magnetico_web schema (created by
	// `magnetico migrate web-schema-init`) or its version table is absent.
	ErrWebSchemaMissing = webschema.ErrWebSchemaMissing
	// ErrWebSchemaUnsupported: the schema is not exactly the supported
	// version-1 structure.
	ErrWebSchemaUnsupported = webschema.ErrWebSchemaUnsupported
	// ErrWebPrivileges: the database role lacks a required privilege.
	ErrWebPrivileges = webschema.ErrWebPrivileges
)

// Backend is a pgx pool plus the verification logic.
type Backend struct {
	pool *pgxpool.Pool
}

// Open connects with an explicit pool limit and pings. Errors are wrapped
// with persistence.ErrStorageIO and never echo the URL.
func Open(ctx context.Context, rawURL string, maxConns int) (*Backend, error) {
	cfg, err := pgxpool.ParseConfig(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid database URL", persistence.ErrStorageIO)
	}
	cfg.MaxConns = int32(maxConns)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	return &Backend{pool: pool}, nil
}

// Verify is the read-only startup verification. In order:
//
//  1. the role can read the torrent schema (ErrWebPrivileges names what is
//     missing, so a missing grant is a controlled failure, not a raw driver
//     error);
//  2. the torrent schema is exactly the supported one (the shared
//     persistence.VerifySchema: schema objects and version, pg_trgm, no
//     legacy shape);
//  3. the magnetico_web schema is exactly the supported version-1 structure
//     and the role holds the runtime privileges (webschema.Verify).
//
// It never creates, alters or migrates anything.
func (b *Backend) Verify(ctx context.Context) error {
	if err := webschema.VerifyTorrentPrivileges(ctx, b.pool); err != nil {
		return err
	}
	if err := persistence.VerifySchema(ctx, b.pool); err != nil {
		return err
	}
	return webschema.Verify(ctx, b.pool, webschema.Runtime)
}

// Pool exposes the pool to the stores built on it (accounts). The backend
// still owns it: Close closes it.
func (b *Backend) Pool() *pgxpool.Pool { return b.pool }

// Ping implements the readiness check.
func (b *Backend) Ping(ctx context.Context) error { return b.pool.Ping(ctx) }

// Close closes the pool (it waits for acquired connections).
func (b *Backend) Close() { b.pool.Close() }

// OpenOperator opens a small pool (two connections) for the administrator
// CLI and verifies, read-only, that the magnetico_web schema is exactly the
// supported version and that the role holds the operator privileges
// (webschema.Operator). The caller closes the pool. Errors never echo the URL.
func OpenOperator(ctx context.Context, rawURL string) (*pgxpool.Pool, error) {
	b, err := Open(ctx, rawURL, 2)
	if err != nil {
		return nil, err
	}
	if err := webschema.Verify(ctx, b.pool, webschema.Operator); err != nil {
		b.Close()
		return nil, err
	}
	return b.pool, nil
}
