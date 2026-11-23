package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Session cookie tokens are 32 random bytes in unpadded base64url (43
// characters). Only SHA-256 of the raw bytes is stored.
const (
	sessionTokenBytes = 32
	// SessionTokenLen is the length of an encoded session token.
	SessionTokenLen = 43

	purgeBatch = 1000
)

// SessionLifetimes are the validated, positive session durations
// (opflags enforces idle ≤ absolute ≤ remember ≤ 8760h).
type SessionLifetimes struct {
	Idle     time.Duration // ordinary sessions only
	Absolute time.Duration // ordinary sessions
	Remember time.Duration // remember-me sessions (absolute, never extended)
}

// Throttle is the minimum interval between last_seen_at writes:
// min(5m, idle/10) (spec §4.4.4).
func Throttle(idle time.Duration) time.Duration {
	return min(5*time.Minute, idle/10)
}

// Session is a resolved, valid session.
type Session struct {
	UserID    int64
	Username  string
	Role      string
	Remember  bool
	ExpiresAt time.Time
	// TouchDue reports that last_seen_at is older than the throttle, so the
	// caller should call TouchSession.
	TouchDue bool
}

// NewSessionToken returns a fresh cookie token and its stored hash.
func NewSessionToken() (token string, hash []byte, err error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), sum[:], nil
}

// SessionTokenHash decodes a presented cookie value and returns the stored
// hash. A value that is not exactly a well-formed 43-character token yields
// ok=false and must be treated as "no session" without any database access.
func SessionTokenHash(token string) (hash []byte, ok bool) {
	if len(token) != SessionTokenLen {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != sessionTokenBytes {
		return nil, false
	}
	sum := sha256.Sum256(raw)
	return sum[:], true
}

func secs(d time.Duration) float64 { return d.Seconds() }

// LookupSession resolves a session in one statement and without a cache:
// existence, absolute expiry, idle expiry (ordinary sessions only) and the
// owner's active flag are all evaluated at statement time on the database
// clock, and the role is read live. Any failure to resolve is
// ErrUnauthenticated; other errors are infrastructure errors.
func (s *Store) LookupSession(ctx context.Context, tokenHash []byte, idle time.Duration) (Session, error) {
	var out Session
	if len(tokenHash) != sha256.Size {
		return out, ErrUnauthenticated
	}
	err := s.pool.QueryRow(ctx, `SELECT s.user_id, u.username, u.role, s.remember, s.expires_at,
            s.last_seen_at <= statement_timestamp() - make_interval(secs => $3) AS touch_due
        FROM magnetico_web.sessions s
        JOIN magnetico_web.users u ON u.id = s.user_id
        WHERE s.token_hash = $1
          AND s.expires_at > statement_timestamp()
          AND (s.remember OR s.last_seen_at + make_interval(secs => $2) > statement_timestamp())
          AND u.active`,
		tokenHash, secs(idle), secs(Throttle(idle))).
		Scan(&out.UserID, &out.Username, &out.Role, &out.Remember, &out.ExpiresAt, &out.TouchDue)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}
	return out, nil
}

// TouchSession records activity, at most once per throttle interval. It is
// conditional: it never revives an expired or idle session and, being an
// UPDATE, never recreates a deleted one. It reports whether a row was written.
// For remember sessions last_seen_at is informational: expires_at never moves.
func (s *Store) TouchSession(ctx context.Context, tokenHash []byte, idle time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE magnetico_web.sessions SET last_seen_at = statement_timestamp()
        WHERE token_hash = $1
          AND last_seen_at <= statement_timestamp() - make_interval(secs => $3)
          AND expires_at > statement_timestamp()
          AND (remember OR last_seen_at + make_interval(secs => $2) > statement_timestamp())`,
		tokenHash, secs(idle), secs(Throttle(idle)))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteSession is logout: it removes the server-side session. Deleting a
// session that does not exist is not an error (idempotent replay).
func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM magnetico_web.sessions WHERE token_hash = $1`, tokenHash)
	return err
}

// PurgeSessions deletes absolute-expired sessions and idle-expired ordinary
// sessions in batches until a batch comes back short. It returns the number
// removed. Lookup already ignores expired rows, so this is housekeeping only.
func (s *Store) PurgeSessions(ctx context.Context, idle time.Duration) (int64, error) {
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, `DELETE FROM magnetico_web.sessions WHERE token_hash IN (
            SELECT token_hash FROM magnetico_web.sessions
             WHERE expires_at <= statement_timestamp()
                OR (NOT remember AND last_seen_at + make_interval(secs => $1) <= statement_timestamp())
             LIMIT $2)`, secs(idle), purgeBatch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < purgeBatch {
			return total, nil
		}
	}
}

// PurgeExpiredLinks deletes expired one-time links (setup, reset, email
// verification) in batches. It needs no user lock: an expired link can
// neither be consumed (consumption requires expires_at > statement_timestamp())
// nor revived, and a concurrent issuance deletes the same row itself, so
// whichever delete runs second simply affects zero rows.
func (s *Store) PurgeExpiredLinks(ctx context.Context) (int64, error) {
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, `DELETE FROM magnetico_web.one_time_tokens WHERE token_hash IN (
            SELECT token_hash FROM magnetico_web.one_time_tokens
             WHERE expires_at <= statement_timestamp() LIMIT $1)`, purgeBatch)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < purgeBatch {
			return total, nil
		}
	}
}
