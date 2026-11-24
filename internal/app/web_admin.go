package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/skobkin/magnetico-web/internal/web/accounts"
	"github.com/skobkin/magnetico-web/internal/web/admincli"
	"github.com/skobkin/magnetico-web/internal/web/password"
	"github.com/skobkin/magnetico-web/internal/web/webdb"
	"github.com/skobkin/magnetico-web/opflags"
)

// adminErrorDetail maps a command failure to controlled text: the sentinels
// of the stores carry fixed messages, everything else goes through the
// database-error reduction (no URLs, no driver text).
func adminErrorDetail(err error) string {
	for _, known := range []error{
		accounts.ErrNotFound, accounts.ErrUsernameTaken, accounts.ErrInvalid, accounts.ErrInvalidRole,
		accounts.ErrLinkInvalid, accounts.ErrInvitationUnavailable, accounts.ErrConflict,
		password.ErrTooShort, password.ErrTooLong, password.ErrBusy,
	} {
		if errors.Is(err, known) {
			return err.Error()
		}
	}
	var ce *admincli.CommandError
	if errors.As(err, &ce) {
		return ce.Error()
	}
	if errors.Is(err, context.Canceled) {
		return "interrupted"
	}
	return webErrorDetail(err)
}

// runWebAdmin implements `magnetico web user …` and `magnetico web tracker …`.
// Results and requested links go to stdout; failures go to stderr with
// controlled text, and nothing secret is ever logged.
func runWebAdmin(o *opflags.WebOptions) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := webdb.OpenOperator(ctx, o.DatabaseURL)
	if err != nil {
		log.Fatalln(appendSchemaStateDetails(fmt.Sprintf("Could not run the administrator command: database %s. %s",
			redactDatabaseURL(o.DatabaseURL), webErrorDetail(err)), err))
	}
	defer pool.Close()

	env := &admincli.Env{
		Store:       accounts.New(pool),
		Trackers:    admincli.NewTrackerStore(pool),
		Out:         os.Stdout,
		PublicBase:  o.PublicBaseURL,
		InviteQuota: o.InviteQuota,
		Passwords:   admincli.StdinPasswords{In: os.Stdin, Prompt: os.Stderr},
	}
	if err := dispatchWebAdmin(ctx, env, o); err != nil {
		pool.Close()
		log.Fatalln("The administrator command failed: " + adminErrorDetail(err))
	}
}

func dispatchWebAdmin(ctx context.Context, e *admincli.Env, o *opflags.WebOptions) error {
	u, t := &o.User, &o.Tracker
	switch o.Subcommand {
	case "user add":
		return admincli.UserAdd(ctx, e, u.Add.Args.Username, u.Add.Email, u.Add.Invites)
	case "user list":
		return admincli.UserList(ctx, e)
	case "user set-role":
		return admincli.UserSetRole(ctx, e, u.SetRole.Args.Username, u.SetRole.Args.Role)
	case "user disable":
		return admincli.UserDisable(ctx, e, u.Disable.Args.Username)
	case "user enable":
		return admincli.UserEnable(ctx, e, u.Enable.Args.Username)
	case "user reset-password":
		return admincli.UserResetPassword(ctx, e, u.ResetPassword.Args.Username)
	case "user set-password":
		return admincli.UserSetPassword(ctx, e, u.SetPassword.Args.Username)
	case "user revoke-tokens":
		return admincli.UserRevokeTokens(ctx, e, u.RevokeTokens.Args.Username)
	case "user invite add":
		return admincli.InviteAdd(ctx, e, u.Invite.Add.Args.Username, u.Invite.Add.Count, u.Invite.Add.ExpiresIn)
	case "user invite list":
		return admincli.InviteList(ctx, e, u.Invite.List.Args.Username)
	case "user invite revoke":
		return admincli.InviteRevoke(ctx, e, u.Invite.Revoke.Args.ID)
	case "tracker list":
		return admincli.TrackerList(ctx, e)
	case "tracker add":
		return admincli.TrackerAdd(ctx, e, t.Add.Args.URL, t.Add.Position)
	case "tracker remove":
		return admincli.TrackerRemove(ctx, e, t.Remove.Args.Ref)
	case "tracker move":
		return admincli.TrackerMove(ctx, e, t.Move.Args.Ref, t.Move.Args.Position)
	}
	return fmt.Errorf("unknown administrator command %q", o.Subcommand)
}
