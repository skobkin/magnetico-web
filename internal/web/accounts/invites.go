package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// createInvitesTx inserts n invitations (16 random bytes, base64url, 22 chars).
func createInvitesTx(ctx context.Context, tx pgx.Tx, ownerID int64, n int, expiresIn *time.Duration) ([]string, error) {
	codes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		code, err := randomToken("", 16)
		if err != nil {
			return nil, err
		}
		if expiresIn != nil {
			_, err = tx.Exec(ctx, `INSERT INTO magnetico_web.invites (owner_id, code, expires_at) VALUES ($1, $2, statement_timestamp() + make_interval(secs => $3))`,
				ownerID, code, expiresIn.Seconds())
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO magnetico_web.invites (owner_id, code) VALUES ($1, $2)`, ownerID, code)
		}
		if err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// CreateInvites adds n invitations for the user.
func (s *Store) CreateInvites(ctx context.Context, username string, n int, expiresIn *time.Duration) ([]string, error) {
	if n < 1 || n > 1000 || (expiresIn != nil && *expiresIn <= 0) {
		return nil, invalid("invitation count or lifetime out of range")
	}
	var codes []string
	err := s.inTx(ctx, "create-invites", func(tx pgx.Tx) error {
		u, err := lockUserByName(ctx, tx, username)
		if err != nil {
			return err
		}
		codes, err = createInvitesTx(ctx, tx, u.id, n, expiresIn)
		return err
	})
	return codes, err
}

// Invite is one invitation with the state needed to describe it.
type Invite struct {
	ID          int64
	Code        string
	UsedBy      string // username, "" when unused
	UsedAt      *time.Time
	CreatedAt   time.Time
	ExpiresAt   *time.Time
	RevokedAt   *time.Time
	Expired     bool // by the database clock
	OwnerActive bool
}

// ListInvites returns the user's invitations, newest first.
func (s *Store) ListInvites(ctx context.Context, username string) ([]Invite, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM magnetico_web.users WHERE username = $1)`, username).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT i.id, i.code, COALESCE(ub.username,''), i.used_at, i.created_at, i.expires_at, i.revoked_at,
            i.expires_at IS NOT NULL AND i.expires_at <= now(), o.active
        FROM magnetico_web.invites i JOIN magnetico_web.users o ON o.id = i.owner_id
        LEFT JOIN magnetico_web.users ub ON ub.id = i.used_by
        WHERE o.username = $1 ORDER BY i.id DESC`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var v Invite
		if err := rows.Scan(&v.ID, &v.Code, &v.UsedBy, &v.UsedAt, &v.CreatedAt, &v.ExpiresAt, &v.RevokedAt, &v.Expired, &v.OwnerActive); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RevokeOutcome says what RevokeInvite did.
type RevokeOutcome int

const (
	InviteRevoked RevokeOutcome = iota
	InviteAlreadyRevoked
	InviteAlreadyUsed
)

// RevokeInvite revokes an unused invitation by id. A used or already revoked
// invitation is reported, not changed; an unknown id is ErrNotFound.
func (s *Store) RevokeInvite(ctx context.Context, id int64) (RevokeOutcome, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE magnetico_web.invites SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL AND used_by IS NULL`, id)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 1 {
		return InviteRevoked, nil
	}
	var usedBy *int64
	var revoked *time.Time
	err = s.pool.QueryRow(ctx, `SELECT used_by, revoked_at FROM magnetico_web.invites WHERE id = $1`, id).Scan(&usedBy, &revoked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrNotFound
	case err != nil:
		return 0, err
	case usedBy != nil:
		return InviteAlreadyUsed, nil
	}
	return InviteAlreadyRevoked, nil
}

// RegisterParams describes a registration with an invitation code.
type RegisterParams struct {
	Code     string
	Username string
	Email    string
	Hash     string // argon2id, computed outside any transaction
	Quota    int    // invitations the new user receives
}

// RegisterWithInvite creates a user from an invitation. Lock order: invite
// row, then the owner row FOR SHARE (so a concurrent disable either finishes
// first and refuses the registration, or waits for it). Unknown, used,
// revoked, expired and suspended-owner invitations all return
// ErrInvitationUnavailable and leave the invitation untouched; a duplicate
// username returns ErrUsernameTaken and does not consume it.
func (s *Store) RegisterWithInvite(ctx context.Context, p RegisterParams) (int64, error) {
	if err := ValidateUsername(p.Username); err != nil {
		return 0, err
	}
	var email *string
	if p.Email != "" {
		if err := ValidateEmail(p.Email); err != nil {
			return 0, err
		}
		email = &p.Email
	}
	if err := checkNewHash(p.Hash); err != nil {
		return 0, err
	}
	if p.Quota < 0 || p.Quota > 1000 || len(p.Code) == 0 || len(p.Code) > 64 {
		return 0, ErrInvitationUnavailable
	}
	var newID int64
	err := s.inTx(ctx, "register", func(tx pgx.Tx) error {
		newID = 0
		var inviteID, ownerID int64
		var usable bool
		err := tx.QueryRow(ctx, `SELECT id, owner_id, used_by IS NULL AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())
            FROM magnetico_web.invites WHERE code = $1 FOR UPDATE`, p.Code).Scan(&inviteID, &ownerID, &usable)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvitationUnavailable
		}
		if err != nil {
			return err
		}
		var ownerActive bool
		if err := tx.QueryRow(ctx, `SELECT active FROM magnetico_web.users WHERE id = $1 FOR SHARE`, ownerID).Scan(&ownerActive); err != nil {
			return err
		}
		if !usable || !ownerActive {
			return ErrInvitationUnavailable
		}
		// The first query's now() is the transaction start; the owner lock may
		// have been waited for since. Re-check expiry on the current database
		// clock now that every lock is held.
		var unexpired bool
		if err := tx.QueryRow(ctx, `SELECT expires_at IS NULL OR expires_at > statement_timestamp()
            FROM magnetico_web.invites WHERE id = $1`, inviteID).Scan(&unexpired); err != nil {
			return err
		}
		if !unexpired {
			return ErrInvitationUnavailable
		}
		err = tx.QueryRow(ctx, `INSERT INTO magnetico_web.users (username, email, password_hash, password_changed_at)
            VALUES ($1, $2, $3, now()) RETURNING id`, p.Username, email, p.Hash).Scan(&newID)
		if err != nil {
			if c, ok := uniqueViolation(err); ok && c == "users_username_key" {
				return ErrUsernameTaken
			}
			return err
		}
		if _, err := createInvitesTx(ctx, tx, newID, p.Quota, nil); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE magnetico_web.invites SET used_by = $2, used_at = now()
            WHERE id = $1 AND used_by IS NULL AND revoked_at IS NULL
              AND (expires_at IS NULL OR expires_at > statement_timestamp())`, inviteID, newID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrInvitationUnavailable
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newID, nil
}
