package accounts

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Credential is what the login path needs about an account. Hash is nil for
// an account without a password.
type Credential struct {
	UserID int64
	Hash   *string
	Active bool
}

// maxLoginUsernameBytes is 25 code points × 4 bytes (the validated username
// bound); anything longer cannot exist.
const maxLoginUsernameBytes = 100

// LoginCredential loads the account for a login attempt: exact,
// case-sensitive username, no lock. A username that cannot exist (invalid
// UTF-8, NUL, too long) or is unknown returns ErrNotFound without being sent
// to the database (PostgreSQL would reject such text with an error, which
// would otherwise be distinguishable from "unknown").
func (s *Store) LoginCredential(ctx context.Context, username string) (Credential, error) {
	var c Credential
	if username == "" || len(username) > maxLoginUsernameBytes || !utf8.ValidString(username) {
		return c, ErrNotFound
	}
	for i := 0; i < len(username); i++ {
		if username[i] == 0 {
			return c, ErrNotFound
		}
	}
	err := s.pool.QueryRow(ctx,
		`SELECT id, password_hash, active FROM magnetico_web.users WHERE username = $1`, username).
		Scan(&c.UserID, &c.Hash, &c.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	return c, err
}

// LoginParams describes one login commit.
type LoginParams struct {
	UserID int64
	// VerifiedHash is the exact stored hash string the password was verified
	// against. The commit applies only while the account still has it.
	VerifiedHash string
	// NewHash, if not empty, replaces VerifiedHash (rehash-on-login). It was
	// computed outside any transaction.
	NewHash   string
	Remember  bool
	Lifetimes SessionLifetimes
	// ReplaceSession, if not nil, is the hash of the session cookie the login
	// request presented; that row is deleted in the same transaction
	// (fixation defence). It is not an error if the row does not exist.
	ReplaceSession []byte
}

// LoginResult is a committed login.
type LoginResult struct {
	Token     string // the cookie value; shown once, never stored
	Username  string
	Role      string
	Remember  bool
	ExpiresAt time.Time
	// Now is the database statement time of the session insert, so callers
	// derive cookie lifetimes from the same clock as expires_at.
	Now time.Time
}

// CompleteLogin is the optimistic-concurrency commit of a successful password
// verification: in one transaction it records last_login_at, optionally
// replaces the verified hash with its rehash, deletes the presented old
// session and creates the new one. The conditional UPDATE takes the user row
// lock and, after any lock wait, re-evaluates active and the exact hash
// against the committed row, so a concurrent password change, reset or
// disable is never overwritten or outlived: the result is ErrLoginStale and
// nothing is written. last_login_at is therefore written only for a login
// whose session was committed. A rehash does not touch password_changed_at
// (the password did not change). Lock order is users → sessions, as in
// Disable and the reset paths.
func (s *Store) CompleteLogin(ctx context.Context, p LoginParams) (LoginResult, error) {
	var res LoginResult
	if p.VerifiedHash == "" || p.Lifetimes.Absolute <= 0 || p.Lifetimes.Remember <= 0 {
		return res, invalid("login parameters")
	}
	if p.NewHash != "" {
		if err := checkNewHash(p.NewHash); err != nil {
			return res, err
		}
	}
	var newHash *string
	if p.NewHash != "" {
		newHash = &p.NewHash
	}
	token, th, err := NewSessionToken()
	if err != nil {
		return res, err
	}
	lifetime := p.Lifetimes.Absolute
	if p.Remember {
		lifetime = p.Lifetimes.Remember
	}

	err = s.inTx(ctx, "login", func(tx pgx.Tx) error {
		res = LoginResult{}
		err := tx.QueryRow(ctx, `UPDATE magnetico_web.users
               SET last_login_at = statement_timestamp(),
                   password_hash = COALESCE($3::text, password_hash)
             WHERE id = $1 AND active AND password_hash = $2
         RETURNING username, role`, p.UserID, p.VerifiedHash, newHash).Scan(&res.Username, &res.Role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLoginStale
		}
		if err != nil {
			return err
		}
		if len(p.ReplaceSession) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM magnetico_web.sessions WHERE token_hash = $1`, p.ReplaceSession); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, `INSERT INTO magnetico_web.sessions (token_hash, user_id, created_at, last_seen_at, expires_at, remember)
            VALUES ($1, $2, statement_timestamp(), statement_timestamp(), statement_timestamp() + make_interval(secs => $3), $4)
            RETURNING expires_at, statement_timestamp()`,
			th, p.UserID, secs(lifetime), p.Remember).Scan(&res.ExpiresAt, &res.Now)
	})
	if err != nil {
		return LoginResult{}, err
	}
	res.Token, res.Remember = token, p.Remember
	return res, nil
}
