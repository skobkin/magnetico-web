// Package webdb is the production database backend of the web service: its
// own bounded pgx pool, the read-only startup verification and the
// readiness ping. It creates, alters and migrates nothing.
package webdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/skobkin/magnetico-web/persistence"
)

// ErrWebSchemaMissing is returned while the dedicated magnetico_web schema
// (created by the explicit initialization command of milestone W2) is absent
// or lacks its version table. The web service fails closed on it.
var ErrWebSchemaMissing = errors.New(
	"the magnetico_web schema is missing or incomplete: initialize it with the explicit schema command before starting the web service")

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

// Verify checks the torrent schema (the shared read-only verification: exact
// schema objects and version, pg_trgm, no legacy shape) and then that the
// magnetico_web schema and its schema_version table exist.
//
// W1 limitation: the web schema's version row and DDL are defined by W2, so
// only presence is verified here; W2 extends this with the exact version
// check. Until W2 provides the schema, the real binary cannot start.
func (b *Backend) Verify(ctx context.Context) error {
	if err := persistence.VerifySchema(ctx, b.pool); err != nil {
		return err
	}
	var schemaOK, versionOK bool
	if err := b.pool.QueryRow(ctx,
		`SELECT to_regnamespace('magnetico_web') IS NOT NULL, to_regclass('magnetico_web.schema_version') IS NOT NULL`,
	).Scan(&schemaOK, &versionOK); err != nil {
		return fmt.Errorf("%w: %w", persistence.ErrStorageIO, err)
	}
	if !schemaOK || !versionOK {
		return ErrWebSchemaMissing
	}
	return nil
}

// Ping implements the readiness check.
func (b *Backend) Ping(ctx context.Context) error { return b.pool.Ping(ctx) }

// Close closes the pool (it waits for acquired connections).
func (b *Backend) Close() { b.pool.Close() }
