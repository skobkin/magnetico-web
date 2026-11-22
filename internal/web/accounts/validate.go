package accounts

import (
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ValidateUsername applies the rules for NEW usernames: 2–25 code points
// (the legacy form rule), valid UTF-8, no control or format characters, no
// leading or trailing whitespace. Case is kept as typed and matching is
// case-sensitive.
func ValidateUsername(s string) error {
	n := utf8.RuneCountInString(s)
	switch {
	case !utf8.ValidString(s):
		return invalid("username is not valid UTF-8")
	case n < 2 || n > 25:
		return invalid("username must be 2 to 25 characters")
	case strings.TrimSpace(s) != s:
		return invalid("username must not start or end with whitespace")
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return invalid("username contains a control or format character")
		}
	}
	return nil
}

// ValidateEmail accepts a bare address only (no display name), 3–254 chars.
func ValidateEmail(s string) error {
	if len(s) < 3 || len(s) > 254 || strings.ContainsAny(s, "\r\n") {
		return invalid("email must be 3 to 254 characters on one line")
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s {
		return invalid("email is not a plain address")
	}
	return nil
}

// ValidateLabel checks an API/RSS token label (1–100 characters, no control characters).
func ValidateLabel(s string) error {
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n < 1 || n > 100 {
		return invalid("label must be 1 to 100 characters")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return invalid("label contains a control character")
		}
	}
	return nil
}

// ValidateTrackerURL checks a tracker announce URL. The URL is stored
// verbatim: scheme udp/http/https/ws/wss (any case), a host, no userinfo, no
// fragment, no whitespace, at most 2,048 characters.
func ValidateTrackerURL(s string) error {
	if len(s) == 0 || len(s) > 2048 {
		return invalid("tracker URL must be 1 to 2048 characters")
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return invalid("tracker URL contains whitespace or a control character")
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return invalid("tracker URL does not parse")
	}
	switch strings.ToLower(u.Scheme) {
	case "udp", "http", "https", "ws", "wss":
	default:
		return invalid("tracker URL scheme must be udp, http, https, ws or wss")
	}
	if u.Host == "" || u.Hostname() == "" {
		return invalid("tracker URL needs a host")
	}
	if u.User != nil {
		return invalid("tracker URL must not contain credentials")
	}
	if u.Fragment != "" || strings.Contains(s, "#") {
		return invalid("tracker URL must not contain a fragment")
	}
	return nil
}

func validRole(role string) bool { return role == "user" || role == "admin" }
