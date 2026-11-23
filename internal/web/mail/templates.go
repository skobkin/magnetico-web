package mail

import (
	"fmt"
	"strings"
	"time"
)

// consequences is the statement every password-link email carries (web-v1
// §4.9.6): completing the link revokes both credential types, named
// separately, and the user must regenerate them.
const consequences = `Completing this link will:
  - sign you out of every browser and device;
  - invalidate ALL of your existing API tokens;
  - stop ALL of your existing RSS tokens and RSS feed URLs from working.

You will have to create new API tokens and new RSS tokens and update your
torrent clients, feed readers and scripts afterwards.`

func hours(d time.Duration) string {
	if d%time.Hour == 0 {
		h := int(d / time.Hour)
		if h == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", h)
	}
	return fmt.Sprintf("%d minutes", int(d/time.Minute))
}

// PasswordLinkMessage is the recovery email. setup is true when the account
// has no password yet.
func PasswordLinkMessage(to, link string, setup bool, ttl time.Duration) Message {
	what, subject := "reset your password", "Reset your Magnetico password"
	if setup {
		what, subject = "set your password", "Set your Magnetico password"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Someone asked to %s for the Magnetico account that uses this address.\n\n", what)
	fmt.Fprintf(&b, "Open this link to continue (valid for %s, usable once):\n\n%s\n\n", hours(ttl), link)
	b.WriteString(consequences)
	b.WriteString("\n\nIf you did not ask for this, ignore this message: nothing changes until the link is used.\n")
	return Message{To: to, Subject: subject, Body: b.String()}
}

// VerifyMessage asks the holder of an address to confirm it.
func VerifyMessage(to, link string, ttl time.Duration) Message {
	var b strings.Builder
	b.WriteString("Confirm this email address for your Magnetico account.\n\n")
	fmt.Fprintf(&b, "Open this link and confirm (valid for %s, usable once):\n\n%s\n\n", hours(ttl), link)
	b.WriteString("Only a confirmed address can be used to recover your password.\n")
	b.WriteString("If you did not ask for this, ignore this message.\n")
	return Message{To: to, Subject: "Confirm your Magnetico email address", Body: b.String()}
}

// EmailChangedNotice tells the previous verified address that it was replaced.
func EmailChangedNotice(to string) Message {
	return Message{To: to, Subject: "Your Magnetico email address was changed", Body: "The email address of your Magnetico account was changed to a new, confirmed address.\n" +
		"This address no longer receives password recovery messages.\n\n" +
		"If this was not you, contact the operator of the service.\n"}
}
