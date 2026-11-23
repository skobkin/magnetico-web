package accounts

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ChangePasswordParams describes a self-service password change (web-v1 §4.7).
type ChangePasswordParams struct {
	UserID int64
	// VerifiedHash is the exact stored hash the current password was verified
	// against; the change applies only while the account still has it.
	VerifiedHash string
	// NewHash was computed outside any transaction.
	NewHash string
	// CurrentSession is the token hash of the session making the request; it is
	// replaced by a rotated one, every other session is deleted.
	CurrentSession []byte
	// RevokeAPI and RevokeRSS are the two independent, explicit options.
	RevokeAPI, RevokeRSS bool
}

// ChangePasswordResult is a committed change.
type ChangePasswordResult struct {
	Token     string // the rotated session's cookie value; shown once
	Remember  bool
	ExpiresAt time.Time
	// Now is the database statement time of the session insert.
	Now time.Time
	// Revocations counts what the change removed: Sessions are the OTHER
	// sessions; token counts are zero unless the matching option was set.
	Revocations
}

// ChangePassword is the optimistic-concurrency commit of a verified current
// password: it sets the new hash, deletes every session and inserts a rotated
// replacement for the current one with the SAME absolute expiry and remember
// flag (rotation never extends a lifetime), deletes outstanding setup/reset
// links, and optionally revokes all API and/or all RSS tokens. Tokens are kept
// by default. A concurrent password change, reset or disable makes the
// conditional UPDATE match nothing: ErrLoginStale. A current session that no
// longer exists or has expired is ErrUnauthenticated. Nothing is written in
// either case.
func (s *Store) ChangePassword(ctx context.Context, p ChangePasswordParams) (ChangePasswordResult, error) {
	var res ChangePasswordResult
	if p.VerifiedHash == "" || len(p.CurrentSession) == 0 {
		return res, invalid("password change parameters")
	}
	if err := checkNewHash(p.NewHash); err != nil {
		return res, err
	}
	token, th, err := NewSessionToken()
	if err != nil {
		return res, err
	}
	err = s.inTx(ctx, "change-password", func(tx pgx.Tx) error {
		res = ChangePasswordResult{}
		tag, err := tx.Exec(ctx, `UPDATE magnetico_web.users SET password_hash = $3, password_changed_at = now()
            WHERE id = $1 AND active AND password_hash = $2`, p.UserID, p.VerifiedHash, p.NewHash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrLoginStale
		}
		var expires time.Time
		err = tx.QueryRow(ctx, `SELECT expires_at, remember FROM magnetico_web.sessions
            WHERE token_hash = $1 AND user_id = $2 AND expires_at > statement_timestamp()`,
			p.CurrentSession, p.UserID).Scan(&expires, &res.Remember)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		del, err := tx.Exec(ctx, `DELETE FROM magnetico_web.sessions WHERE user_id = $1`, p.UserID)
		if err != nil {
			return err
		}
		res.Sessions = del.RowsAffected() - 1
		if err := tx.QueryRow(ctx, `INSERT INTO magnetico_web.sessions (token_hash, user_id, created_at, last_seen_at, expires_at, remember)
            VALUES ($1, $2, statement_timestamp(), statement_timestamp(), $3, $4)
            RETURNING expires_at, statement_timestamp()`,
			th, p.UserID, expires, res.Remember).Scan(&res.ExpiresAt, &res.Now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM magnetico_web.one_time_tokens WHERE user_id = $1 AND purpose IN ('setup','reset')`, p.UserID); err != nil {
			return err
		}
		if p.RevokeAPI {
			if res.APITokens, err = revokeAPITokensTx(ctx, tx, p.UserID); err != nil {
				return err
			}
		}
		if p.RevokeRSS {
			if res.RSSTokens, err = revokeRSSTokensTx(ctx, tx, p.UserID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ChangePasswordResult{}, err
	}
	res.Token = token
	return res, nil
}

// IssueEmailVerifyLink replaces the user's outstanding email link with a new
// one (24 h) that confirms email. Nothing is written to users: the address is
// pending inside the link and the current verified address stays in force
// until CompleteEmailVerify. An inactive account is ErrNotFound. Confirming an
// address another account already holds verified is not checked here (it would
// reveal who holds it); CompleteEmailVerify refuses it generically.
func (s *Store) IssueEmailVerifyLink(ctx context.Context, userID int64, email string) (Link, error) {
	var l Link
	if err := ValidateEmail(email); err != nil {
		return l, err
	}
	err := s.inTx(ctx, "issue-email-link", func(tx pgx.Tx) error {
		u, err := lockUserByID(ctx, tx, userID)
		if err != nil {
			return err
		}
		if !u.active {
			return ErrNotFound
		}
		l, err = issueLinkForTx(ctx, tx, userID, PurposeEmailVerify, EmailLinkTTL, &email)
		return err
	})
	return l, err
}

// PendingEmail returns the address awaiting confirmation (an unexpired
// email_verify link), if any.
func (s *Store) PendingEmail(ctx context.Context, userID int64) (email string, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT email FROM magnetico_web.one_time_tokens
        WHERE user_id = $1 AND purpose = 'email_verify' AND expires_at > now()`, userID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return email, true, nil
}

// EmailVerifyResult says what confirming an address changed.
type EmailVerifyResult struct {
	UserID int64
	Email  string
	// PreviousEmail is the verified address that was replaced, or "" if there
	// was none (or it is the same address).
	PreviousEmail string
}

// CompleteEmailVerify confirms the address held by the link: in one
// transaction it locks the user, consumes the link by deleting it (expiry on
// the current database clock) and sets email and email_verified_at. It never
// touches the password or active. Unknown, replaced, used, expired and
// inactive-owner links are all ErrLinkInvalid, and an inactive owner's link is
// NOT consumed. If another account already holds the address verified, the
// unique index aborts the transaction: everything rolls back, so the link is
// NOT consumed and the previous address is unchanged, and the result is
// ErrEmailUnavailable.
func (s *Store) CompleteEmailVerify(ctx context.Context, token string) (EmailVerifyResult, error) {
	var res EmailVerifyResult
	if len(token) == 0 || len(token) > 256 {
		return res, ErrLinkInvalid
	}
	th := hashToken(token)
	err := s.inTx(ctx, "complete-email-verify", func(tx pgx.Tx) error {
		res = EmailVerifyResult{}
		var userID int64
		var target string
		err := tx.QueryRow(ctx, `SELECT user_id, email FROM magnetico_web.one_time_tokens
            WHERE token_hash = $1 AND purpose = 'email_verify'`, th).Scan(&userID, &target)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLinkInvalid
		}
		if err != nil {
			return err
		}
		var active, verified bool
		var current string
		if err := tx.QueryRow(ctx, `SELECT active, COALESCE(email,''), email_verified_at IS NOT NULL
            FROM magnetico_web.users WHERE id = $1 FOR UPDATE`, userID).Scan(&active, &current, &verified); err != nil {
			return err
		}
		if !active {
			return ErrLinkInvalid
		}
		tag, err := tx.Exec(ctx, `DELETE FROM magnetico_web.one_time_tokens
            WHERE token_hash = $1 AND user_id = $2 AND purpose = 'email_verify' AND expires_at > statement_timestamp()`, th, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrLinkInvalid
		}
		if _, err := tx.Exec(ctx, `UPDATE magnetico_web.users SET email = $2, email_verified_at = statement_timestamp() WHERE id = $1`, userID, target); err != nil {
			if c, ok := uniqueViolation(err); ok && c == "users_verified_email_key" {
				return ErrEmailUnavailable
			}
			return err
		}
		res.UserID, res.Email = userID, target
		if verified && !strings.EqualFold(current, target) {
			res.PreviousEmail = current
		}
		return nil
	})
	if err != nil {
		return EmailVerifyResult{}, err
	}
	return res, nil
}

// RecoveryLink is a password link issued for a verified address.
type RecoveryLink struct {
	Link
	Email string
}

// IssueRecoveryLink issues the self-service recovery link for the account
// whose VERIFIED address is email: purpose reset (1 h) if it has a password,
// setup (72 h) otherwise; it replaces any earlier password link. An unknown or
// unverified address, or an inactive account, is ErrNotFound (the caller must
// not distinguish these to the requester). The unique verified-email index
// guarantees at most one account matches. The submitted address only locates
// the account: RecoveryLink.Email is the stored verified spelling, the only
// address a recovery message may be sent to.
func (s *Store) IssueRecoveryLink(ctx context.Context, email string) (RecoveryLink, error) {
	var out RecoveryLink
	if err := ValidateEmail(email); err != nil {
		return out, ErrNotFound
	}
	err := s.inTx(ctx, "issue-recovery-link", func(tx pgx.Tx) error {
		out = RecoveryLink{}
		var id int64
		var hasPassword bool
		var stored string
		err := tx.QueryRow(ctx, `SELECT id, password_hash IS NOT NULL, email FROM magnetico_web.users
            WHERE lower(email) = lower($1) AND email_verified_at IS NOT NULL AND active FOR UPDATE`, email).Scan(&id, &hasPassword, &stored)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		purpose, ttl := PurposeReset, ResetLinkTTL
		if !hasPassword {
			purpose, ttl = PurposeSetup, SetupLinkTTL
		}
		if out.Link, err = issueLinkTx(ctx, tx, id, purpose, ttl); err != nil {
			return err
		}
		out.Email = stored // the verified spelling, never the submitted one
		return nil
	})
	return out, err
}
