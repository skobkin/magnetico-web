package admincli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/password"
)

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// UserAdd: `web user add <username> [--email E] [--invites N]` (N < 0: the
// configured invite quota).
func UserAdd(ctx context.Context, e *Env, username, email string, invites int) error {
	if invites < 0 {
		invites = e.InviteQuota
	}
	res, err := e.Store.CreateUser(ctx, accounts.CreateUserParams{Username: username, Email: email, Invites: invites})
	if err != nil {
		return err
	}
	e.printf("created user %s (id %d), %d invitation(s)\n", username, res.ID, len(res.InviteCodes))
	e.printf("password setup link (single use, valid until %s):\n%s\n", ts(res.Setup.ExpiresAt), e.link("/password/setup/"+res.Setup.Token))
	return nil
}

// UserList prints one tab-separated line per account.
func UserList(ctx context.Context, e *Env) error {
	users, err := e.Store.ListUsers(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(e.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "id\tusername\trole\tactive\tpassword\temail\tcreated\tlast login")
	for _, u := range users {
		email := "none"
		switch {
		case u.EmailVerified:
			email = "verified"
		case u.Email != "":
			email = "unverified"
		}
		last := "-"
		if u.LastLoginAt != nil {
			last = ts(*u.LastLoginAt)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%t\t%s\t%s\t%s\t%s\n", u.ID, u.Username, u.Role, u.Active,
			password.Classify(u.PasswordHash), email, ts(u.CreatedAt), last)
	}
	return w.Flush()
}

func UserSetRole(ctx context.Context, e *Env, username, role string) error {
	if err := e.Store.SetRole(ctx, username, role); err != nil {
		return err
	}
	e.printf("role of %s is now %s\n", username, role)
	return nil
}

func UserDisable(ctx context.Context, e *Env, username string) error {
	n, err := e.Store.Disable(ctx, username)
	if err != nil {
		return err
	}
	e.printf("%s is disabled; %d session(s) deleted. Tokens and invitations are suspended, not revoked: they work again after `web user enable`.\n", username, n)
	return nil
}

func UserEnable(ctx context.Context, e *Env, username string) error {
	if err := e.Store.Enable(ctx, username); err != nil {
		return err
	}
	e.printf("%s is enabled; suspended tokens and invitations work again (revoked and expired ones do not). Sessions are not restored.\n", username)
	return nil
}

// UserResetPassword issues a setup (no password yet) or reset link.
func UserResetPassword(ctx context.Context, e *Env, username string) error {
	l, err := e.Store.IssuePasswordLink(ctx, username)
	if err != nil {
		return err
	}
	e.printf("password %s link (single use, valid until %s; replaces any earlier link):\n%s\n", l.Purpose, ts(l.ExpiresAt),
		e.link("/password/"+l.Purpose+"/"+l.Token))
	if !l.OwnerActive {
		e.printf("note: the account is disabled; the link cannot be used until `web user enable %s`.\n", username)
	}
	if l.Purpose == accounts.PurposeReset {
		e.printf("warning: completing the reset signs the user out everywhere and permanently revokes ALL their API tokens and ALL their RSS tokens (RSS feed URLs); clients must be updated.\n")
	}
	return nil
}

// UserSetPassword reads the new password (never from argv), hashes it
// outside any transaction and stores it with the reset revocations.
func UserSetPassword(ctx context.Context, e *Env, username string) error {
	if _, err := e.Store.GetUser(ctx, username); err != nil {
		return err
	}
	pw, err := e.Passwords.ReadNewPassword()
	if err != nil {
		return err
	}
	hash, err := e.hasher().Hash(ctx, pw)
	if err != nil {
		return err
	}
	r, err := e.Store.SetPassword(ctx, username, hash)
	if err != nil {
		return err
	}
	e.printf("password of %s changed; revoked: %d session(s), %d API token(s), %d RSS token(s); outstanding links removed\n",
		username, r.Sessions, r.APITokens, r.RSSTokens)
	return nil
}

func UserRevokeTokens(ctx context.Context, e *Env, username string) error {
	api, rss, err := e.Store.RevokeAllTokens(ctx, username)
	if err != nil {
		return err
	}
	e.printf("revoked: %d API tokens, %d RSS tokens\n", api, rss)
	return nil
}

func InviteAdd(ctx context.Context, e *Env, username string, count int, expiresIn time.Duration) error {
	var exp *time.Duration
	if expiresIn > 0 {
		exp = &expiresIn
	}
	codes, err := e.Store.CreateInvites(ctx, username, count, exp)
	if err != nil {
		return err
	}
	for _, c := range codes {
		e.printf("%s\n", e.link("/register/"+c))
	}
	return nil
}

func inviteStatus(v accounts.Invite) string {
	switch {
	case v.UsedBy != "":
		return "used by " + v.UsedBy
	case v.RevokedAt != nil:
		return "revoked"
	case v.Expired:
		return "expired"
	case !v.OwnerActive:
		return "suspended (owner disabled)"
	}
	return "unused"
}

func InviteList(ctx context.Context, e *Env, username string) error {
	invs, err := e.Store.ListInvites(ctx, username)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(e.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "id\tstatus\tlink")
	for _, v := range invs {
		st, link := inviteStatus(v), ""
		if st == "unused" {
			link = e.link("/register/" + v.Code)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\n", v.ID, st, link)
	}
	return w.Flush()
}

// InviteRevoke revokes by id, so codes never end up in shell history.
func InviteRevoke(ctx context.Context, e *Env, idArg string) error {
	id, err := strconv.ParseInt(idArg, 10, 64)
	if err != nil || id < 1 {
		return cmdErr("invitation id must be a positive number")
	}
	out, err := e.Store.RevokeInvite(ctx, id)
	switch {
	case errors.Is(err, accounts.ErrNotFound):
		return cmdErr("no invitation with id %d", id)
	case err != nil:
		return err
	}
	switch out {
	case accounts.InviteRevoked:
		e.printf("invitation %d revoked\n", id)
	case accounts.InviteAlreadyRevoked:
		e.printf("invitation %d was already revoked; nothing changed\n", id)
	case accounts.InviteAlreadyUsed:
		e.printf("invitation %d was already used and cannot be revoked; nothing changed\n", id)
	}
	return nil
}
