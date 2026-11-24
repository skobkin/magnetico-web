package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skobkin/magnetico-web/internal/web/password"
)

// Lifetimes of one-time password links (owner decision, spec §4.9).
const (
	SetupLinkTTL = 72 * time.Hour
	ResetLinkTTL = time.Hour
)

// Link purposes.
const (
	PurposeSetup = "setup"
	PurposeReset = "reset"
	// PurposeEmailVerify links confirm an address (web-v1 §4.9.7).
	PurposeEmailVerify = "email_verify"
	// EmailLinkTTL is the lifetime of an email verification link (§4.9.4).
	EmailLinkTTL = 24 * time.Hour
)

// User is one account row as the CLI and later the web layer need it.
type User struct {
	ID            int64
	Username      string
	Email         string // "" when none
	EmailVerified bool
	PasswordHash  string // "" when none; never printed, only classified
	Active        bool
	Role          string
	CreatedAt     time.Time
	LastLoginAt   *time.Time
}

// ListUsers returns all accounts ordered by id.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, username, COALESCE(email,''), email_verified_at IS NOT NULL,
        COALESCE(password_hash,''), active, role, created_at, last_login_at FROM magnetico_web.users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &u.EmailVerified, &u.PasswordHash, &u.Active, &u.Role, &u.CreatedAt, &u.LastLoginAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUser returns one account by exact (case-sensitive) username.
func (s *Store) GetUser(ctx context.Context, username string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `SELECT id, username, COALESCE(email,''), email_verified_at IS NOT NULL,
        COALESCE(password_hash,''), active, role, created_at, last_login_at FROM magnetico_web.users WHERE username = $1`, username).
		Scan(&u.ID, &u.Username, &u.Email, &u.EmailVerified, &u.PasswordHash, &u.Active, &u.Role, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// GetUserByID is GetUser by primary key.
func (s *Store) GetUserByID(ctx context.Context, id int64) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `SELECT id, username, COALESCE(email,''), email_verified_at IS NOT NULL,
        COALESCE(password_hash,''), active, role, created_at, last_login_at FROM magnetico_web.users WHERE id = $1`, id).
		Scan(&u.ID, &u.Username, &u.Email, &u.EmailVerified, &u.PasswordHash, &u.Active, &u.Role, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// Link is a freshly issued one-time link; Token is shown exactly once.
type Link struct {
	Token     string
	Purpose   string
	ExpiresAt time.Time
	// OwnerActive is false when the account is disabled (the link then
	// cannot be completed until the account is enabled).
	OwnerActive bool
}

// issueLinkTx replaces the user's outstanding password link. The caller
// holds the user row FOR UPDATE. The DELETE removes an expired link too, so
// the unique index cannot conflict, and it is undone if the transaction fails.
func issueLinkTx(ctx context.Context, tx pgx.Tx, userID int64, purpose string, ttl time.Duration) (Link, error) {
	return issueLinkForTx(ctx, tx, userID, purpose, ttl, nil)
}

// issueLinkForTx is issueLinkTx for any purpose: email is the target address
// of an email_verify link and nil for setup/reset. Each kind (password link,
// email link) has at most one outstanding row per user; issuing replaces it.
func issueLinkForTx(ctx context.Context, tx pgx.Tx, userID int64, purpose string, ttl time.Duration, email *string) (Link, error) {
	kind := `purpose IN ('setup','reset')`
	if purpose == PurposeEmailVerify {
		kind = `purpose = 'email_verify'`
	}
	if _, err := tx.Exec(ctx, `DELETE FROM magnetico_web.one_time_tokens WHERE user_id = $1 AND `+kind, userID); err != nil {
		return Link{}, err
	}
	token, err := randomToken("", 32)
	if err != nil {
		return Link{}, err
	}
	var expires time.Time
	err = tx.QueryRow(ctx, `INSERT INTO magnetico_web.one_time_tokens (token_hash, user_id, purpose, email, expires_at)
        VALUES ($1, $2, $3, $4, statement_timestamp() + make_interval(secs => $5)) RETURNING expires_at`,
		hashToken(token), userID, purpose, email, ttl.Seconds()).Scan(&expires)
	if err != nil {
		if c, ok := uniqueViolation(err); ok && c == "one_time_tokens_one_per_kind" {
			return Link{}, ErrConflict
		}
		return Link{}, err
	}
	return Link{Token: token, Purpose: purpose, ExpiresAt: expires, OwnerActive: true}, nil
}

// CreateUserParams describes an operator-created account.
type CreateUserParams struct {
	Username string
	Email    string // optional, stored unverified
	Invites  int    // invitation quota to create now
}

// CreatedUser is the result of CreateUser.
type CreatedUser struct {
	ID          int64
	Setup       Link
	InviteCodes []string
}

// CreateUser creates an active, passwordless user with role user, N invitations
// and a setup link (72 h), in one transaction.
func (s *Store) CreateUser(ctx context.Context, p CreateUserParams) (CreatedUser, error) {
	var res CreatedUser
	if err := ValidateUsername(p.Username); err != nil {
		return res, err
	}
	var email *string
	if p.Email != "" {
		if err := ValidateEmail(p.Email); err != nil {
			return res, err
		}
		email = &p.Email
	}
	if p.Invites < 0 || p.Invites > 1000 {
		return res, invalid("invitation count out of range")
	}
	err := s.inTx(ctx, "create-user", func(tx pgx.Tx) error {
		res = CreatedUser{}
		err := tx.QueryRow(ctx, `INSERT INTO magnetico_web.users (username, email) VALUES ($1, $2) RETURNING id`,
			p.Username, email).Scan(&res.ID)
		if err != nil {
			if c, ok := uniqueViolation(err); ok && c == "users_username_key" {
				return ErrUsernameTaken
			}
			return err
		}
		if res.InviteCodes, err = createInvitesTx(ctx, tx, res.ID, p.Invites, nil); err != nil {
			return err
		}
		res.Setup, err = issueLinkTx(ctx, tx, res.ID, PurposeSetup, SetupLinkTTL)
		return err
	})
	return res, err
}

// Disable suspends the account: active=false and its sessions are deleted.
// Tokens, invitations, links, password, email and last login are untouched,
// so enabling restores them (never revokes). Idempotent. Returns the number
// of sessions deleted.
func (s *Store) Disable(ctx context.Context, username string) (sessions int64, err error) {
	err = s.inTx(ctx, "disable", func(tx pgx.Tx) error {
		u, err := lockUserByName(ctx, tx, username)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE magnetico_web.users SET active = false WHERE id = $1`, u.id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM magnetico_web.sessions WHERE user_id = $1`, u.id)
		sessions = tag.RowsAffected()
		return err
	})
	return sessions, err
}

// Enable sets active=true and nothing else.
func (s *Store) Enable(ctx context.Context, username string) error {
	return s.inTx(ctx, "enable", func(tx pgx.Tx) error {
		u, err := lockUserByName(ctx, tx, username)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE magnetico_web.users SET active = true WHERE id = $1`, u.id)
		return err
	})
}

// SetRole changes a role to "user" or "admin".
func (s *Store) SetRole(ctx context.Context, username, role string) error {
	if !validRole(role) {
		return ErrInvalidRole
	}
	return s.inTx(ctx, "set-role", func(tx pgx.Tx) error {
		u, err := lockUserByName(ctx, tx, username)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE magnetico_web.users SET role = $2 WHERE id = $1`, u.id, role)
		return err
	})
}

// IssuePasswordLink replaces the user's outstanding setup/reset link with a
// new one: purpose setup (72 h) while the account has no password, otherwise
// reset (1 h). It works for a disabled account too (the link cannot be
// completed until enable). Revocations happen at completion, not here. If the
// transaction fails the previous link survives.
func (s *Store) IssuePasswordLink(ctx context.Context, username string) (Link, error) {
	var l Link
	err := s.inTx(ctx, "issue-link", func(tx pgx.Tx) error {
		u, err := lockUserByName(ctx, tx, username)
		if err != nil {
			return err
		}
		purpose, ttl := PurposeReset, ResetLinkTTL
		if !u.hasPassword {
			purpose, ttl = PurposeSetup, SetupLinkTTL
		}
		if l, err = issueLinkTx(ctx, tx, u.id, purpose, ttl); err != nil {
			return err
		}
		l.OwnerActive = u.active
		return nil
	})
	return l, err
}

func checkNewHash(hash string) error {
	switch password.Classify(hash) {
	case password.ClassNone, password.ClassUnsupported:
		return invalid("password hash is not in a supported format")
	}
	return nil
}

// CompleteResult says what completing a link changed.
type CompleteResult struct {
	UserID int64
	Revocations
}

// CompletePasswordLink consumes the link by deleting it, sets the new
// password and applies the reset revocations, atomically. The hash must have
// been computed beforehand (outside any transaction). Unknown, replaced,
// used, expired, wrong-purpose and disabled-owner links all return
// ErrLinkInvalid; a disabled owner's link is NOT consumed. active and email
// fields are never written.
func (s *Store) CompletePasswordLink(ctx context.Context, token, purpose, newHash string) (CompleteResult, error) {
	var res CompleteResult
	if purpose != PurposeSetup && purpose != PurposeReset {
		return res, invalid("unknown link purpose")
	}
	if err := checkNewHash(newHash); err != nil {
		return res, err
	}
	if len(token) == 0 || len(token) > 256 {
		return res, ErrLinkInvalid
	}
	th := hashToken(token)
	err := s.inTx(ctx, "complete-link", func(tx pgx.Tx) error {
		res = CompleteResult{}
		var userID int64
		err := tx.QueryRow(ctx, `SELECT user_id FROM magnetico_web.one_time_tokens WHERE token_hash = $1 AND purpose = $2`, th, purpose).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLinkInvalid
		}
		if err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT active FROM magnetico_web.users WHERE id = $1 FOR UPDATE`, userID).Scan(&active); err != nil {
			return err
		}
		if !active {
			return ErrLinkInvalid
		}
		// Consume by deleting. Under the user lock this re-checks the committed
		// state, so a link replaced or completed meanwhile finds no row.
		// Expiry uses statement_timestamp(), not now(): now() is fixed at
		// transaction start and would accept a link that expired while this
		// transaction waited for the user row lock.
		tag, err := tx.Exec(ctx, `DELETE FROM magnetico_web.one_time_tokens
            WHERE token_hash = $1 AND user_id = $2 AND purpose = $3 AND expires_at > statement_timestamp()`, th, userID, purpose)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrLinkInvalid
		}
		if _, err := tx.Exec(ctx, `UPDATE magnetico_web.users SET password_hash = $2, password_changed_at = now() WHERE id = $1`, userID, newHash); err != nil {
			return err
		}
		res.UserID = userID
		res.Revocations, err = resetRevocationsTx(ctx, tx, userID)
		return err
	})
	if err != nil {
		return CompleteResult{}, err
	}
	return res, nil
}

// SetPassword is the operator's set-password: the hash replaces the old one,
// outstanding links are deleted and the reset revocations apply.
func (s *Store) SetPassword(ctx context.Context, username, newHash string) (Revocations, error) {
	var r Revocations
	if err := checkNewHash(newHash); err != nil {
		return r, err
	}
	err := s.inTx(ctx, "set-password", func(tx pgx.Tx) error {
		u, err := lockUserByName(ctx, tx, username)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE magnetico_web.users SET password_hash = $2, password_changed_at = now() WHERE id = $1`, u.id, newHash); err != nil {
			return err
		}
		r, err = resetRevocationsTx(ctx, tx, u.id)
		return err
	})
	if err != nil {
		return Revocations{}, err
	}
	return r, nil
}

// RevokeAllTokens permanently revokes every API and RSS token of the user.
// Sessions, password and active are untouched.
func (s *Store) RevokeAllTokens(ctx context.Context, username string) (api, rss int64, err error) {
	err = s.inTx(ctx, "revoke-tokens", func(tx pgx.Tx) error {
		u, err := lockUserByName(ctx, tx, username)
		if err != nil {
			return err
		}
		api, rss, err = revokeTokensTx(ctx, tx, u.id)
		return err
	})
	return api, rss, err
}
