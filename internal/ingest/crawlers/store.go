package crawlers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrNameTaken: a crawler with this name (compared case-insensitively) exists.
	ErrNameTaken = errors.New("a crawler with this name already exists")
	// ErrNotFound: no crawler has this name or credential.
	ErrNotFound = errors.New("crawler not found")
)

// DB is the subset of *pgxpool.Pool the store needs.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store reads and writes magnetico_ingest.crawlers.
type Store struct{ db DB }

// New returns a Store over db.
func New(db DB) *Store { return &Store{db: db} }

// Created is the result of Add and RotateToken. Token is shown exactly once
// and cannot be recovered afterwards: only its digest is stored.
type Created struct {
	ID    string
	Name  string
	Token string
}

// Crawler is a registry entry for listings. It deliberately has no token or
// digest field.
type Crawler struct {
	ID             string
	Name           string
	Enabled        bool
	CreatedAt      time.Time
	TokenRotatedAt *time.Time
	DisabledAt     *time.Time
}

// Record is the authentication view of a crawler.
type Record struct {
	ID      string
	Name    string
	Enabled bool
}

// Add registers a crawler with a server-generated UUIDv4 and a fresh token.
func (s *Store) Add(ctx context.Context, name string) (Created, error) {
	if err := ValidateName(name); err != nil {
		return Created{}, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		id, err := newUUIDv4()
		if err != nil {
			return Created{}, err
		}
		token, err := newToken()
		if err != nil {
			return Created{}, err
		}
		digest := Digest(token)
		_, err = s.db.Exec(ctx,
			`INSERT INTO magnetico_ingest.crawlers (crawler_id, name, token_hash) VALUES ($1, $2, $3)`,
			id, name, digest[:])
		if err == nil {
			return Created{ID: id, Name: name, Token: token}, nil
		}
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			switch pe.ConstraintName {
			case "crawlers_name_lower":
				return Created{}, ErrNameTaken
			case "crawlers_pkey", "crawlers_token_hash_key":
				continue // a 2^-122 / 2^-256 collision: draw again
			}
		}
		return Created{}, fmt.Errorf("crawlers: add: %w", err)
	}
	return Created{}, errors.New("crawlers: could not generate a unique identity")
}

// List returns every crawler ordered by name (case-insensitively).
func (s *Store) List(ctx context.Context) ([]Crawler, error) {
	rows, err := s.db.Query(ctx, `SELECT crawler_id::text, name, enabled, created_at, token_rotated_at, disabled_at
FROM magnetico_ingest.crawlers ORDER BY lower(name), crawler_id`)
	if err != nil {
		return nil, fmt.Errorf("crawlers: list: %w", err)
	}
	defer rows.Close()
	var out []Crawler
	for rows.Next() {
		var c Crawler
		if err := rows.Scan(&c.ID, &c.Name, &c.Enabled, &c.CreatedAt, &c.TokenRotatedAt, &c.DisabledAt); err != nil {
			return nil, fmt.Errorf("crawlers: list: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("crawlers: list: %w", err)
	}
	return out, nil
}

// SetEnabled disables or enables a crawler by name (case-insensitive). The
// identity and the token are untouched.
func (s *Store) SetEnabled(ctx context.Context, name string, enabled bool) error {
	tag, err := s.db.Exec(ctx, `UPDATE magnetico_ingest.crawlers
SET enabled = $2,
    disabled_at = CASE WHEN $2 THEN NULL WHEN enabled THEN now() ELSE disabled_at END
WHERE lower(name) = lower($1)`, name, enabled)
	if err != nil {
		return fmt.Errorf("crawlers: set enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RotateToken replaces the crawler's credential. The identity is unchanged and
// the enabled state is not touched (a disabled crawler stays disabled).
func (s *Store) RotateToken(ctx context.Context, name string) (Created, error) {
	for attempt := 0; attempt < 3; attempt++ {
		token, err := newToken()
		if err != nil {
			return Created{}, err
		}
		digest := Digest(token)
		var c Created
		err = s.db.QueryRow(ctx, `UPDATE magnetico_ingest.crawlers
SET token_hash = $2, token_rotated_at = now()
WHERE lower(name) = lower($1) RETURNING crawler_id::text, name`, name, digest[:]).Scan(&c.ID, &c.Name)
		switch {
		case err == nil:
			c.Token = token
			return c, nil
		case errors.Is(err, pgx.ErrNoRows):
			return Created{}, ErrNotFound
		}
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			continue
		}
		return Created{}, fmt.Errorf("crawlers: rotate: %w", err)
	}
	return Created{}, errors.New("crawlers: could not generate a unique token")
}

// LookupDigest resolves a credential digest through the unique index. A
// disabled crawler is returned with Enabled=false; the caller decides.
func (s *Store) LookupDigest(ctx context.Context, digest [32]byte) (Record, error) {
	var r Record
	err := s.db.QueryRow(ctx, `SELECT crawler_id::text, name, enabled
FROM magnetico_ingest.crawlers WHERE token_hash = $1`, digest[:]).Scan(&r.ID, &r.Name, &r.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("crawlers: lookup: %w", err)
	}
	return r, nil
}
