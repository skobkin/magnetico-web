package accounts

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Token prefixes. They are cosmetic (so a leaked token is recognisable); no
// code routes on them: each function below queries only its own table, so a
// legacy un-prefixed RSS token can never reach api_tokens and vice versa.
const (
	APITokenPrefix = "mgt_"
	RSSTokenPrefix = "mgf_"
)

// Identity is the authenticated owner of a token. The owner's active flag is
// joined on every lookup, never cached on the credential.
type Identity struct {
	UserID   int64
	Username string
	Role     string
	TokenID  int64
}

// CreatedToken is a new token; Plaintext is shown exactly once.
type CreatedToken struct {
	ID        int64
	Plaintext string
}

func (s *Store) createToken(ctx context.Context, table, prefix string, userID int64, label string, expiresAt *time.Time) (CreatedToken, error) {
	var lbl *string
	if label != "" {
		if err := ValidateLabel(label); err != nil {
			return CreatedToken{}, err
		}
		lbl = &label
	}
	plain, err := randomToken(prefix, 32)
	if err != nil {
		return CreatedToken{}, err
	}
	var id int64
	err = s.pool.QueryRow(ctx, `INSERT INTO magnetico_web.`+table+` (token_hash, user_id, label, expires_at) VALUES ($1,$2,$3,$4) RETURNING id`,
		hashToken(plain), userID, lbl, expiresAt).Scan(&id)
	if err != nil {
		return CreatedToken{}, err
	}
	return CreatedToken{ID: id, Plaintext: plain}, nil
}

// CreateAPIToken stores a JSON-API credential (only its SHA-256).
func (s *Store) CreateAPIToken(ctx context.Context, userID int64, label string, expiresAt *time.Time) (CreatedToken, error) {
	return s.createToken(ctx, "api_tokens", APITokenPrefix, userID, label, expiresAt)
}

// CreateRSSToken stores an RSS-feed credential (only its SHA-256).
func (s *Store) CreateRSSToken(ctx context.Context, userID int64, label string, expiresAt *time.Time) (CreatedToken, error) {
	return s.createToken(ctx, "rss_tokens", RSSTokenPrefix, userID, label, expiresAt)
}

func (s *Store) authenticate(ctx context.Context, table, presented string) (Identity, error) {
	var id Identity
	if len(presented) == 0 || len(presented) > 256 {
		return id, ErrUnauthenticated
	}
	want := hashToken(presented)
	var stored []byte
	err := s.pool.QueryRow(ctx, `SELECT t.id, t.user_id, u.username, u.role, t.token_hash
        FROM magnetico_web.`+table+` t JOIN magnetico_web.users u ON u.id = t.user_id
        WHERE t.token_hash = $1 AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > now()) AND u.active`, want).
		Scan(&id.TokenID, &id.UserID, &id.Username, &id.Role, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return Identity{}, err
	}
	if subtle.ConstantTimeCompare(stored, want) != 1 {
		return Identity{}, ErrUnauthenticated
	}
	return id, nil
}

// AuthenticateAPIToken resolves an API token of an active, unrevoked,
// unexpired credential. It never consults rss_tokens.
func (s *Store) AuthenticateAPIToken(ctx context.Context, presented string) (Identity, error) {
	return s.authenticate(ctx, "api_tokens", presented)
}

// AuthenticateRSSToken resolves an RSS token (including migrated legacy
// ones). It never consults api_tokens.
func (s *Store) AuthenticateRSSToken(ctx context.Context, presented string) (Identity, error) {
	return s.authenticate(ctx, "rss_tokens", presented)
}
