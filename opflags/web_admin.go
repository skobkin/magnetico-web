package opflags

import (
	"errors"
	"strings"
	"time"
)

// WebUserCommands and WebTrackerCommands are the administrator subcommands
// of `magnetico web` (docs/specs/web-v1.md §4.10). They share the web
// database option and the ENV/YAML overlay with the service; each command
// validates only what it uses. Passwords never come from argv.
type WebUserCommands struct {
	Add           WebUserAdd       `command:"add" description:"Create an enabled account without a password and print a single-use password-setup link."`
	List          WebNoArgs        `command:"list" description:"List accounts (id, username, role, active, password state, email state, created, last login)."`
	SetRole       WebUserSetRole   `command:"set-role" description:"Set a role: user or admin."`
	Disable       WebUserName      `command:"disable" description:"Suspend an account: delete its sessions; tokens and invitations are suspended, not revoked."`
	Enable        WebUserName      `command:"enable" description:"Re-enable an account; suspended tokens and invitations work again."`
	ResetPassword WebUserName      `command:"reset-password" description:"Issue a password setup/reset link (completion revokes sessions and ALL API and RSS tokens)."`
	SetPassword   WebUserName      `command:"set-password" description:"Set a password read from a no-echo prompt (twice) or from one line of piped standard input; never from arguments. Revokes sessions and all API and RSS tokens."`
	RevokeTokens  WebUserName      `command:"revoke-tokens" description:"Permanently revoke all API and RSS tokens of an account."`
	Invite        WebInviteCommand `command:"invite" description:"Manage invitations of an account."`
}

type WebNoArgs struct{}

type WebUserName struct {
	Args struct {
		Username string `positional-arg-name:"username" required:"true"`
	} `positional-args:"true"`
}

type WebUserAdd struct {
	Email   string `long:"email" description:"Email address (stored unverified)." default:""`
	Invites int    `long:"invites" description:"Invitations to create now (default: the configured invite-quota)." default:"-1"`
	Args    struct {
		Username string `positional-arg-name:"username" required:"true"`
	} `positional-args:"true"`
}

type WebUserSetRole struct {
	Args struct {
		Username string `positional-arg-name:"username" required:"true"`
		Role     string `positional-arg-name:"role" required:"true"`
	} `positional-args:"true"`
}

type WebInviteCommand struct {
	Add    WebInviteAdd    `command:"add" description:"Create invitations and print their links."`
	List   WebInviteList   `command:"list" description:"List an account's invitations with status and links of unused ones."`
	Revoke WebInviteRevoke `command:"revoke" description:"Permanently revoke an unused invitation by id."`
}

type WebInviteAdd struct {
	Count     int           `long:"count" description:"Number of invitations." default:"1"`
	ExpiresIn time.Duration `long:"expires-in" description:"Lifetime of the new invitations (default: no expiry)." default:"0s"`
	Args      struct {
		Username string `positional-arg-name:"username" required:"true"`
	} `positional-args:"true"`
}

type WebInviteList struct {
	Args struct {
		Username string `positional-arg-name:"username" required:"true"`
	} `positional-args:"true"`
}

type WebInviteRevoke struct {
	Args struct {
		ID string `positional-arg-name:"invite-id" required:"true"`
	} `positional-args:"true"`
}

type WebTrackerCommands struct {
	List   WebNoArgs      `command:"list" description:"List the public tracker list (position, id, url)."`
	Add    WebTrackerAdd  `command:"add" description:"Add a tracker URL (default: append) and renumber."`
	Remove WebTrackerRef  `command:"remove" description:"Remove a tracker by id (digits) or exact URL."`
	Move   WebTrackerMove `command:"move" description:"Move a tracker (id or exact URL) to a position."`
}

type WebTrackerAdd struct {
	Position int `long:"position" description:"Insert position (default: append)." default:"0"`
	Args     struct {
		URL string `positional-arg-name:"url" required:"true"`
	} `positional-args:"true"`
}

type WebTrackerRef struct {
	Args struct {
		Ref string `positional-arg-name:"url-or-id" required:"true"`
	} `positional-args:"true"`
}

type WebTrackerMove struct {
	Args struct {
		Ref      string `positional-arg-name:"url-or-id" required:"true"`
		Position int    `positional-arg-name:"position" required:"true"`
	} `positional-args:"true"`
}

// adminNeeds says which validations an administrator command requires.
type adminNeeds struct {
	publicURL   bool // the command prints absolute links
	inviteQuota bool
}

// adminCommands maps the dispatch name ("user add", …) to what it needs.
var adminCommands = map[string]adminNeeds{
	"user add":            {publicURL: true, inviteQuota: true},
	"user list":           {},
	"user set-role":       {},
	"user disable":        {},
	"user enable":         {},
	"user reset-password": {publicURL: true},
	"user set-password":   {},
	"user revoke-tokens":  {},
	"user invite add":     {publicURL: true},
	"user invite list":    {publicURL: true},
	"user invite revoke":  {},
	"tracker list":        {},
	"tracker add":         {},
	"tracker remove":      {},
	"tracker move":        {},
}

// checkAdmin validates only what the selected administrator command uses:
// always the database (including the password-on-the-command-line refusal),
// plus the public URL for commands that print links and invite-quota for
// `user add`. Service-only options (listen, pool, timeouts, SMTP …) are not
// validated.
func (o *WebOptions) checkAdmin(explicitDatabase bool) error {
	needs, ok := adminCommands[o.Subcommand]
	if !ok {
		return errors.New("web " + o.Subcommand + ": expected a complete administrator command (see --help)")
	}
	if err := o.checkDatabase(explicitDatabase); err != nil {
		return err
	}
	if needs.publicURL {
		if err := o.checkPublicURL(); err != nil {
			return err
		}
	}
	if needs.inviteQuota && (o.InviteQuota < 0 || o.InviteQuota > 10000) {
		return errors.New("invite-quota must be between 0 and 10000")
	}
	return nil
}

func joinCommandPath(names ...string) string { return strings.Join(names, " ") }
