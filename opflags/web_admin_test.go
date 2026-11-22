package opflags

import (
	"strings"
	"testing"
)

func TestWebAdminCommandParsing(t *testing.T) {
	webEnvClean(t)
	cases := []struct {
		name string
		args []string
		sub  string
		chk  func(t *testing.T, w *WebOptions)
	}{
		{"user add", []string{"user", "add", "--public-url", webPublic, "--email", "a@example.org", "--invites", "3", "alice"}, "user add",
			func(t *testing.T, w *WebOptions) {
				if w.User.Add.Args.Username != "alice" || w.User.Add.Email != "a@example.org" || w.User.Add.Invites != 3 {
					t.Fatalf("%+v", w.User.Add)
				}
			}},
		{"user add defaults to the quota", []string{"--public-url", webPublic, "--invite-quota", "5", "user", "add", "alice"}, "user add",
			func(t *testing.T, w *WebOptions) {
				if w.User.Add.Invites != -1 || w.InviteQuota != 5 {
					t.Fatalf("invites=%d quota=%d", w.User.Add.Invites, w.InviteQuota)
				}
			}},
		{"user list", []string{"user", "list"}, "user list", nil},
		{"set-role", []string{"user", "set-role", "alice", "admin"}, "user set-role",
			func(t *testing.T, w *WebOptions) {
				if w.User.SetRole.Args.Username != "alice" || w.User.SetRole.Args.Role != "admin" {
					t.Fatal("args")
				}
			}},
		{"disable", []string{"user", "disable", "alice"}, "user disable", nil},
		{"enable", []string{"user", "enable", "alice"}, "user enable", nil},
		{"reset-password", []string{"--public-url", webPublic, "user", "reset-password", "alice"}, "user reset-password", nil},
		{"set-password", []string{"user", "set-password", "alice"}, "user set-password", nil},
		{"revoke-tokens", []string{"user", "revoke-tokens", "alice"}, "user revoke-tokens", nil},
		{"invite add", []string{"--public-url", webPublic, "user", "invite", "add", "--count", "4", "--expires-in", "48h", "alice"}, "user invite add",
			func(t *testing.T, w *WebOptions) {
				if w.User.Invite.Add.Count != 4 || w.User.Invite.Add.ExpiresIn.Hours() != 48 {
					t.Fatalf("%+v", w.User.Invite.Add)
				}
			}},
		{"invite list", []string{"--public-url", webPublic, "user", "invite", "list", "alice"}, "user invite list", nil},
		{"invite revoke", []string{"user", "invite", "revoke", "17"}, "user invite revoke",
			func(t *testing.T, w *WebOptions) {
				if w.User.Invite.Revoke.Args.ID != "17" {
					t.Fatal("id")
				}
			}},
		{"tracker list", []string{"tracker", "list"}, "tracker list", nil},
		{"tracker add", []string{"tracker", "add", "--position", "2", "udp://t.example.org:1337/announce"}, "tracker add",
			func(t *testing.T, w *WebOptions) {
				if w.Tracker.Add.Position != 2 || w.Tracker.Add.Args.URL != "udp://t.example.org:1337/announce" {
					t.Fatalf("%+v", w.Tracker.Add)
				}
			}},
		{"tracker remove", []string{"tracker", "remove", "7"}, "tracker remove", nil},
		{"tracker move", []string{"tracker", "move", "7", "1"}, "tracker move",
			func(t *testing.T, w *WebOptions) {
				if w.Tracker.Move.Args.Ref != "7" || w.Tracker.Move.Args.Position != 1 {
					t.Fatal("args")
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, err := parseWeb(t, c.args...)
			if err != nil {
				t.Fatal(err)
			}
			if o.Web.Subcommand != c.sub {
				t.Fatalf("Subcommand = %q, want %q", o.Web.Subcommand, c.sub)
			}
			if c.chk != nil {
				c.chk(t, &o.Web)
			}
		})
	}
}

func TestWebAdminUsageErrors(t *testing.T) {
	webEnvClean(t)
	for name, args := range map[string][]string{
		"missing username":      {"user", "disable"},
		"missing role":          {"user", "set-role", "alice"},
		"extra argument":        {"user", "disable", "alice", "bob"},
		"unknown subcommand":    {"user", "frobnicate"},
		"bare user":             {"user"},
		"bare invite":           {"user", "invite"},
		"bare tracker":          {"tracker"},
		"missing tracker url":   {"tracker", "add"},
		"move needs a position": {"tracker", "move", "7"},
		"non-numeric position":  {"tracker", "move", "7", "first"},
		"password on argv":      {"user", "set-password", "alice", "--password", "x"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseWeb(t, args...); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// Each command validates only what it uses.
func TestWebAdminValidatesOnlyWhatItUses(t *testing.T) {
	webEnvClean(t)
	// No public URL, and service-only options at invalid values: fine for commands that print no link.
	t.Setenv("MAGNETICO_WEB_POOL_MAX_CONNS", "1")
	t.Setenv("MAGNETICO_WEB_SMTP_PORT", "70000")
	for _, args := range [][]string{
		{"user", "list"}, {"user", "disable", "a"}, {"user", "set-password", "a"}, {"user", "revoke-tokens", "a"},
		{"user", "invite", "revoke", "1"}, {"tracker", "list"}, {"tracker", "add", "udp://t.example.org:1/a"},
	} {
		if _, err := parseWeb(t, args...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	// The public URL is required exactly where links are printed.
	for _, args := range [][]string{
		{"user", "add", "a"}, {"user", "reset-password", "a"}, {"user", "invite", "add", "a"}, {"user", "invite", "list", "a"},
	} {
		_, err := parseWeb(t, args...)
		if err == nil || !strings.Contains(err.Error(), "public URL") {
			t.Errorf("%v: err = %v, want the public-URL requirement", args, err)
		}
	}
	// invite-quota is checked for `user add` only.
	if _, err := parseWeb(t, "--public-url", webPublic, "--invite-quota", "20000", "user", "add", "a"); err == nil {
		t.Error("invalid invite-quota accepted by user add")
	}
	if _, err := parseWeb(t, "--invite-quota", "20000", "user", "list"); err != nil {
		t.Errorf("invite-quota checked for an unrelated command: %v", err)
	}
	// Bare `web` keeps its W1 validation (public URL required).
	if _, err := parseWeb(t); err == nil {
		t.Error("bare web without a public URL accepted")
	}
}

func TestWebAdminDatabaseRules(t *testing.T) {
	webEnvClean(t)
	if _, err := parseWeb(t, "--database", "postgres://u:secret@localhost/db", "user", "list"); err == nil ||
		strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "MAGNETICO_WEB_DATABASE") {
		t.Fatalf("password on the command line: %v", err)
	}
	t.Setenv("MAGNETICO_WEB_DATABASE", "postgres://u:secret@localhost/db")
	o, err := parseWeb(t, "user", "list")
	if err != nil || o.Web.DatabaseURL != "postgres://u:secret@localhost/db" {
		t.Fatalf("env database: %v %q", err, o.Web.DatabaseURL)
	}
	t.Setenv("MAGNETICO_WEB_DATABASE", "mysql://x")
	if _, err := parseWeb(t, "user", "list"); err == nil {
		t.Fatal("non-postgres URL accepted")
	}
	t.Setenv("MAGNETICO_WEB_DATABASE", "")
	t.Setenv("MAGNETICO_WEB_BOGUS", "1")
	if _, err := parseWeb(t, "user", "list"); err == nil || !strings.Contains(err.Error(), "unknown environment variable") {
		t.Fatalf("unknown variable: %v", err)
	}
}
