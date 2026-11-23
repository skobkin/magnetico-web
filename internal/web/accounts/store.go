// Package accounts is the SQL-only account and credential store of the web
// service (docs/specs/web-v1.md §4, 07A §4): users, one-time password links,
// invitations, API tokens and RSS tokens. It has no HTTP types and no
// sessions logic beyond the session store (sessions.go) and the login commit
// (login.go), which belong to W3.
//
// Rules the store enforces with conditional SQL under row locks (the schema
// has no triggers):
//
//   - Lock order is invites row → users row → dependent rows. Every
//     one-time-token write happens only while the user row is held FOR UPDATE.
//   - Statements that depend on users.active read it in the locking statement
//     itself, so READ COMMITTED re-evaluates it after a lock wait.
//   - disable suspends and never revokes; revocation (revoked_at) is permanent
//     and only ever set from NULL.
//   - Every expiry comparison uses the database clock. Inside a transaction that
//     may wait for a lock it is statement_timestamp(), never now(): now() is
//     fixed at transaction start and would accept a credential that expired
//     while the transaction waited.
//
// Password hashing is done by the caller, outside any transaction.
package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors. Callers map them; none carries user-controlled text.
var (
	ErrNotFound              = errors.New("no such user")
	ErrUsernameTaken         = errors.New("username is already taken")
	ErrInvalid               = errors.New("invalid input")
	ErrInvalidRole           = errors.New("role must be user or admin")
	ErrLinkInvalid           = errors.New("link is invalid, expired or already used")
	ErrInvitationUnavailable = errors.New("invitation is not available")
	ErrConflict              = errors.New("concurrent update; retry")
	ErrUnauthenticated       = errors.New("credential rejected")
	// ErrLoginStale: the account no longer has the exact password hash that
	// was verified (changed, reset or already rehashed) or is no longer
	// active. Nothing was written.
	ErrLoginStale = errors.New("account changed during login")
	// ErrEmailUnavailable: confirming the address would violate the
	// one-verified-address-per-account rule. The transaction was rolled back, so
	// the link is NOT consumed and the previous address is unchanged.
	ErrEmailUnavailable = errors.New("email address cannot be confirmed")
	// ErrTokenLimit: the user already holds MaxActiveTokens unrevoked tokens of
	// that type.
	ErrTokenLimit = errors.New("too many active tokens")
)

// Store is the account store over a pgx pool.
type Store struct {
	pool *pgxpool.Pool

	// beforeCommit is a test hook run after a transaction's statements and
	// before COMMIT; returning an error aborts (rolls back) the transaction.
	beforeCommit func(op string) error
}

// New returns a Store. The pool's role needs the privileges of
// webschema.Runtime (the CLI uses webschema.Operator).
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) inTx(ctx context.Context, op string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err := fn(tx); err != nil {
		return err
	}
	if s.beforeCommit != nil {
		if err := s.beforeCommit(op); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// uniqueViolation reports a 23505 and its constraint name.
func uniqueViolation(err error) (constraint string, ok bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return pe.ConstraintName, true
	}
	return "", false
}

// randomToken returns prefix + base64url(nBytes random bytes).
func randomToken(prefix string, nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken is the stored form of every bearer secret: SHA-256 of the full
// presented string. Plaintext is never stored.
func hashToken(presented string) []byte {
	sum := sha256.Sum256([]byte(presented))
	return sum[:]
}

// lockedUser is the result of locking a user row.
type lockedUser struct {
	id          int64
	active      bool
	hasPassword bool
}

func lockUserByName(ctx context.Context, tx pgx.Tx, username string) (lockedUser, error) {
	var u lockedUser
	err := tx.QueryRow(ctx,
		`SELECT id, active, password_hash IS NOT NULL FROM magnetico_web.users WHERE username = $1 FOR UPDATE`,
		username).Scan(&u.id, &u.active, &u.hasPassword)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// lockUserByID locks the user row FOR UPDATE and reads active in the same
// statement.
func lockUserByID(ctx context.Context, tx pgx.Tx, id int64) (lockedUser, error) {
	u := lockedUser{id: id}
	err := tx.QueryRow(ctx,
		`SELECT active, password_hash IS NOT NULL FROM magnetico_web.users WHERE id = $1 FOR UPDATE`,
		id).Scan(&u.active, &u.hasPassword)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// Revocations counts what a credential reset removed.
type Revocations struct {
	Sessions  int64
	APITokens int64
	RSSTokens int64
}

func revokeAPITokensTx(ctx context.Context, tx pgx.Tx, userID int64) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE magnetico_web.api_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return tag.RowsAffected(), err
}

func revokeRSSTokensTx(ctx context.Context, tx pgx.Tx, userID int64) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE magnetico_web.rss_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	return tag.RowsAffected(), err
}

// revokeTokensTx revokes every not-yet-revoked API and RSS token (legacy RSS
// tokens included). An already revoked token keeps its original revoked_at.
func revokeTokensTx(ctx context.Context, tx pgx.Tx, userID int64) (api, rss int64, err error) {
	if api, err = revokeAPITokensTx(ctx, tx, userID); err != nil {
		return 0, 0, err
	}
	if rss, err = revokeRSSTokensTx(ctx, tx, userID); err != nil {
		return 0, 0, err
	}
	return api, rss, nil
}

// resetRevocationsTx is the shared tail of every password reset (§4.9.6,
// §4.10): all sessions deleted, all API and RSS tokens revoked, and every
// outstanding setup/reset link removed.
func resetRevocationsTx(ctx context.Context, tx pgx.Tx, userID int64) (Revocations, error) {
	var r Revocations
	tag, err := tx.Exec(ctx, `DELETE FROM magnetico_web.sessions WHERE user_id = $1`, userID)
	if err != nil {
		return r, err
	}
	r.Sessions = tag.RowsAffected()
	if r.APITokens, r.RSSTokens, err = revokeTokensTx(ctx, tx, userID); err != nil {
		return r, err
	}
	_, err = tx.Exec(ctx, `DELETE FROM magnetico_web.one_time_tokens WHERE user_id = $1 AND purpose IN ('setup','reset')`, userID)
	return r, err
}
