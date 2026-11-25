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
// links and outstanding email_verify links (the stored address and its
// verification status are kept, only the pending confirmation goes), and
// optionally revokes all API and/or all RSS tokens. Tokens are kept
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
		if res.EmailLinks, err = deleteEmailVerifyLinksTx(ctx, tx, p.UserID); err != nil {
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
//
// It carries no credential check: use it only where no earlier credential can
// exist (the account was just registered) or where the caller is the operator.
// A request authenticated by a session must use IssueEmailVerifyLinkForSession.
func (s *Store) IssueEmailVerifyLink(ctx context.Context, userID int64, email string) (Link, error) {
	return s.issueEmailVerifyLink(ctx, userID, email, nil)
}

// IssueEmailVerifyLinkForSession is IssueEmailVerifyLink for a request that
// was authorized by the session whose token hash is given. After locking the
// user row it requires that session to still exist and be unexpired (the same
// test as ChangePassword's), else ErrUnauthenticated and nothing is written.
// Every password write deletes the account's sessions (a self-service change
// replaces the current one with a rotated session) in the transaction that
// holds the same row lock, so a request authorized by the credentials that a
// committed password change replaced cannot create a pending link after it
// (§4.9.11); a request that locks first has its link deleted by the change.
func (s *Store) IssueEmailVerifyLinkForSession(ctx context.Context, userID int64, email string, session []byte) (Link, error) {
	if len(session) == 0 {
		return Link{}, ErrUnauthenticated
	}
	return s.issueEmailVerifyLink(ctx, userID, email, session)
}

func (s *Store) issueEmailVerifyLink(ctx context.Context, userID int64, email string, session []byte) (Link, error) {
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
		if session != nil {
			// A new statement after the lock wait: READ COMMITTED sees the
			// sessions a committed password change deleted.
			var live bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM magnetico_web.sessions
                WHERE token_hash = $1 AND user_id = $2 AND expires_at > statement_timestamp())`,
				session, userID).Scan(&live); err != nil {
				return err
			}
			if !live {
				return ErrUnauthenticated
			}
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
	// RecoveryLinksCancelled counts the outstanding setup/reset links removed
	// because a verified address was replaced (see CompleteEmailVerify).
	RecoveryLinksCancelled int64
}

// CompleteEmailVerify confirms the address held by the link: in one
// transaction it locks the user, consumes the link by deleting it (expiry on
// the current database clock) and sets email and email_verified_at. When it
// replaces a different verified address it also deletes the user's outstanding
// setup/reset links (the former address must not stay a recovery channel). It
// never touches the password or active. Unknown, replaced, used, expired and
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
			// Replacing a verified address ends the former address as a
			// recovery channel: a reset link mailed to it must not outlive it.
			// Password links carry no origin, so an administrator-issued link
			// is cancelled too (the operator re-issues it).
			tag, err := tx.Exec(ctx, `DELETE FROM magnetico_web.one_time_tokens WHERE user_id = $1 AND purpose IN ('setup','reset')`, userID)
			if err != nil {
				return err
			}
			res.RecoveryLinksCancelled = tag.RowsAffected()
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

// EmailRemoveResult says what RemoveEmail changed.
type EmailRemoveResult struct {
	// Email is the address that was removed ("" if the account held none).
	Email string
	// WasVerified reports that the removed address was a verified one, so
	// password recovery through it is gone.
	WasVerified bool
	// PendingCancelled reports that an outstanding email_verify link was
	// deleted.
	PendingCancelled bool
	// RecoveryLinksCancelled counts the setup/reset links deleted because the
	// removed address was verified.
	RecoveryLinksCancelled int64
}

// RemoveEmail removes the account's address, verified or not, and cancels the
// pending verification link, in one transaction that locks the user row first
// (the lock every email and recovery writer takes, so it is ordered against
// IssueRecoveryLink, CompleteEmailVerify and IssueEmailVerifyLink).
//
// If the removed address was verified, every outstanding setup/reset link of
// the account is deleted as well: password links carry neither an origin nor
// an address, so an email-issued link cannot be told from an administrator-
// issued one, and the former address must not remain an indirect recovery
// channel. The operator re-issues an administrator link with reset-password.
// An unverified address was never a recovery channel, so its removal leaves
// password links alone. Sessions, the password, tokens and invitations are
// untouched. Nothing to remove is Removed == false (no error); an unknown or
// inactive account is ErrNotFound.
func (s *Store) RemoveEmail(ctx context.Context, userID int64) (EmailRemoveResult, error) {
	var res EmailRemoveResult
	err := s.inTx(ctx, "remove-email", func(tx pgx.Tx) error {
		res = EmailRemoveResult{}
		var active, verified bool
		var email *string
		err := tx.QueryRow(ctx, `SELECT active, email, email_verified_at IS NOT NULL
            FROM magnetico_web.users WHERE id = $1 FOR UPDATE`, userID).Scan(&active, &email, &verified)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !active {
			return ErrNotFound
		}
		tag, err := tx.Exec(ctx, `DELETE FROM magnetico_web.one_time_tokens WHERE user_id = $1 AND purpose = 'email_verify'`, userID)
		if err != nil {
			return err
		}
		res.PendingCancelled = tag.RowsAffected() > 0
		if email == nil {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE magnetico_web.users SET email = NULL, email_verified_at = NULL WHERE id = $1`, userID); err != nil {
			return err
		}
		res.Email, res.WasVerified = *email, verified
		if verified {
			tag, err := tx.Exec(ctx, `DELETE FROM magnetico_web.one_time_tokens WHERE user_id = $1 AND purpose IN ('setup','reset')`, userID)
			if err != nil {
				return err
			}
			res.RecoveryLinksCancelled = tag.RowsAffected()
		}
		return nil
	})
	if err != nil {
		return EmailRemoveResult{}, err
	}
	return res, nil
}
