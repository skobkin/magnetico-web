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
	// TouchDue is true when last_used_at is unset or older than
	// TokenTouchInterval; the caller then calls TouchAPIToken/TouchRSSToken.
	TouchDue bool
}

// CreatedToken is a new token; Plaintext is shown exactly once.
type CreatedToken struct {
	ID        int64
	Plaintext string
}

// MaxActiveTokens bounds the unrevoked tokens one user may hold per type
// (expired but unrevoked ones count, so the account page stays bounded). It
// limits retained usable credentials, not row growth: revoked rows are kept and
// their creation rate is limited by the caller.
const MaxActiveTokens = 50

// TokenTouchInterval throttles last_used_at writes.
const TokenTouchInterval = 5 * time.Minute

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
	err = s.inTx(ctx, "create-token", func(tx pgx.Tx) error {
		u, err := lockUserByID(ctx, tx, userID)
		if err != nil {
			return err
		}
		if !u.active {
			return ErrNotFound
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM magnetico_web.`+table+` WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxActiveTokens {
			return ErrTokenLimit
		}
		return tx.QueryRow(ctx, `INSERT INTO magnetico_web.`+table+` (token_hash, user_id, label, expires_at) VALUES ($1,$2,$3,$4) RETURNING id`,
			hashToken(plain), userID, lbl, expiresAt).Scan(&id)
	})
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
	err := s.pool.QueryRow(ctx, `SELECT t.id, t.user_id, u.username, u.role, t.token_hash,
               t.last_used_at IS NULL OR t.last_used_at < now() - make_interval(secs => $2)
        FROM magnetico_web.`+table+` t JOIN magnetico_web.users u ON u.id = t.user_id
        WHERE t.token_hash = $1 AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > now()) AND u.active`, want, TokenTouchInterval.Seconds()).
		Scan(&id.TokenID, &id.UserID, &id.Username, &id.Role, &stored, &id.TouchDue)
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

// TokenInfo is the listable metadata of one unrevoked token. The secret and its
// hash are never returned.
type TokenInfo struct {
	ID         int64
	Label      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	Expired    bool // by the database clock
	Legacy     bool // RSS tokens migrated from the legacy application
}

func (s *Store) listTokens(ctx context.Context, table, legacyCol string, userID int64) ([]TokenInfo, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, COALESCE(label,''), created_at, last_used_at, expires_at,
            expires_at IS NOT NULL AND expires_at <= now(), `+legacyCol+`
        FROM magnetico_web.`+table+` WHERE user_id = $1 AND revoked_at IS NULL ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		var v TokenInfo
		if err := rows.Scan(&v.ID, &v.Label, &v.CreatedAt, &v.LastUsedAt, &v.ExpiresAt, &v.Expired, &v.Legacy); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListAPITokens returns the user's unrevoked API tokens, newest first.
func (s *Store) ListAPITokens(ctx context.Context, userID int64) ([]TokenInfo, error) {
	return s.listTokens(ctx, "api_tokens", "false", userID)
}

// ListRSSTokens returns the user's unrevoked RSS tokens (legacy ones flagged),
// newest first.
func (s *Store) ListRSSTokens(ctx context.Context, userID int64) ([]TokenInfo, error) {
	return s.listTokens(ctx, "rss_tokens", "legacy", userID)
}

// revokeToken permanently revokes one token of the given owner. A token that
// is unknown, belongs to another user or is already revoked is ErrNotFound, so
// the caller cannot probe other users' identifiers.
func (s *Store) revokeToken(ctx context.Context, table string, userID, id int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE magnetico_web.`+table+` SET revoked_at = now()
        WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// RevokeAPIToken permanently revokes one of the user's API tokens.
func (s *Store) RevokeAPIToken(ctx context.Context, userID, id int64) error {
	return s.revokeToken(ctx, "api_tokens", userID, id)
}

// RevokeRSSToken permanently revokes one of the user's RSS tokens.
func (s *Store) RevokeRSSToken(ctx context.Context, userID, id int64) error {
	return s.revokeToken(ctx, "rss_tokens", userID, id)
}

func (s *Store) touchToken(ctx context.Context, table string, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE magnetico_web.`+table+` SET last_used_at = now()
        WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - make_interval(secs => $2))`,
		id, TokenTouchInterval.Seconds())
	return err
}

// TouchAPIToken records a use, at most once per TokenTouchInterval.
func (s *Store) TouchAPIToken(ctx context.Context, id int64) error {
	return s.touchToken(ctx, "api_tokens", id)
}

// TouchRSSToken records a use, at most once per TokenTouchInterval.
func (s *Store) TouchRSSToken(ctx context.Context, id int64) error {
	return s.touchToken(ctx, "rss_tokens", id)
}
