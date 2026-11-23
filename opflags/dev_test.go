package opflags

import (
	"strings"
	"testing"
)

func TestParseArgs_DevSeedTorrents(t *testing.T) {
	t.Setenv(DevDatabaseEnvName, "")
	t.Setenv(migrateDatabaseEnvName, "postgres://migrate-env@localhost/prod")

	o := &OpFlags{}
	command, err := o.parseArgs([]string{"dev", "seed-torrents", "--database", "postgres://dev@localhost/dev", "--confirm-development-database"})
	if err != nil {
		t.Fatal(err)
	}
	s := o.Dev.SeedTorrents
	if command != "dev" || s.DatabaseURL != "postgres://dev@localhost/dev" || !s.Confirm || s.DryRun {
		t.Fatalf("command=%q opts=%+v", command, s)
	}
}

// The target is explicit: no default, and never inherited from the migrate
// settings (flag, environment or config file).
func TestParseArgs_DevSeedTorrentsRequiresExplicitTarget(t *testing.T) {
	t.Setenv(DevDatabaseEnvName, "")
	t.Setenv(migrateDatabaseEnvName, "postgres://migrate-env@localhost/prod")
	o := &OpFlags{}
	_, err := o.parseArgs([]string{"dev", "seed-torrents", "--confirm-development-database"})
	if err == nil || !strings.Contains(err.Error(), "target database is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseArgs_DevSeedTorrentsEnvironmentTarget(t *testing.T) {
	t.Setenv(DevDatabaseEnvName, "postgres://env@localhost/dev")
	o := &OpFlags{}
	if _, err := o.parseArgs([]string{"dev", "seed-torrents", "--confirm-development-database"}); err != nil {
		t.Fatal(err)
	}
	if got := o.Dev.SeedTorrents.DatabaseURL; got != "postgres://env@localhost/dev" {
		t.Fatalf("DatabaseURL = %q", got)
	}
	o = &OpFlags{}
	if _, err := o.parseArgs([]string{"dev", "seed-torrents", "--database", "postgres://cli@localhost/dev", "--confirm-development-database"}); err != nil {
		t.Fatal(err)
	}
	if got := o.Dev.SeedTorrents.DatabaseURL; got != "postgres://cli@localhost/dev" {
		t.Fatalf("the flag must win over the environment, got %q", got)
	}
}

func TestParseArgs_DevSeedTorrentsRequiresConfirmationUnlessDryRun(t *testing.T) {
	t.Setenv(DevDatabaseEnvName, "")
	o := &OpFlags{}
	_, err := o.parseArgs([]string{"dev", "seed-torrents", "--database", "postgres://dev@localhost/dev"})
	if err == nil || !strings.Contains(err.Error(), "--confirm-development-database") {
		t.Fatalf("err = %v", err)
	}
	o = &OpFlags{}
	if _, err := o.parseArgs([]string{"dev", "seed-torrents", "--database", "postgres://dev@localhost/dev", "--dry-run"}); err != nil {
		t.Fatalf("dry run needs no confirmation: %v", err)
	}
}

func TestParseArgs_DevBareAndExtraArgs(t *testing.T) {
	for _, args := range [][]string{
		{"dev"},
		{"dev", "seed-torrents", "--database", "postgres://dev@localhost/dev", "--dry-run", "extra"},
		{"dev", "unknown"},
	} {
		o := &OpFlags{}
		if _, err := o.parseArgs(args); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}
