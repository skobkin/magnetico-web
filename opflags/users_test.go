package opflags

import (
	"strings"
	"testing"
)

const usersDefaultDSN = "postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable"

func TestParseArgs_UsersDefaultsAndFlags(t *testing.T) {
	o := &OpFlags{}
	command, err := o.parseArgs([]string{"migrate", "users", "--source-timezone", "Europe/Moscow"})
	if err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	if command != "migrate" || o.Migrate.Subcommand != "users" {
		t.Fatalf("command=%q subcommand=%q", command, o.Migrate.Subcommand)
	}
	u := o.Migrate.Users
	if u.DatabaseURL != usersDefaultDSN || u.SourceDatabaseURL != "" || u.DryRun || u.ReplaceUsers ||
		u.SourceWritesStopped || u.AcceptNeedsReset != 0 || u.ConfirmPlan != "" || u.ReportJSON != "" {
		t.Errorf("defaults: %+v", u)
	}

	o = &OpFlags{}
	if _, err := o.parseArgs([]string{"migrate", "users",
		"--database", "postgres://cli@localhost/db", "--source-timezone", "UTC", "--dry-run",
		"--accept-needs-reset", "3", "--replace-users", "--confirm-plan", "abc",
		"--delete-unmatched-user-ids", "5,7", "--source-writes-stopped", "--report-json", "/tmp/r.json",
	}); err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	u = o.Migrate.Users
	if u.DatabaseURL != "postgres://cli@localhost/db" || u.SourceTimezone != "UTC" || !u.DryRun || u.AcceptNeedsReset != 3 ||
		!u.ReplaceUsers || u.ConfirmPlan != "abc" || u.DeleteUnmatchedUserIDs != "5,7" || !u.SourceWritesStopped || u.ReportJSON != "/tmp/r.json" {
		t.Errorf("flags: %+v", u)
	}
}

// The source DSN has no command-line flag: it must never reach `ps` output.
func TestParseArgs_UsersSourceDSNHasNoFlag(t *testing.T) {
	for _, flag := range []string{"--source", "--source-database", "--source-database-url", "--source-dsn"} {
		o := &OpFlags{}
		if _, err := o.parseArgs([]string{"migrate", "users", flag, "postgres://secret@legacy/db"}); err == nil {
			t.Errorf("%s was accepted", flag)
		}
	}
}

func TestParseArgs_UsersSourceFromEnvironmentOnly(t *testing.T) {
	t.Setenv(migrationUsersSourceEnvName, "postgres://legacy-env@localhost/php")
	t.Setenv(migrateDatabaseEnvName, "postgres://target-env@localhost/web")
	for _, args := range [][]string{{"migrate", "users"}, {"migrate", "users", "verify"}} {
		o := &OpFlags{}
		if _, err := o.parseArgs(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		src, tgt := o.Migrate.Users.SourceDatabaseURL, o.Migrate.Users.DatabaseURL
		if len(args) == 3 {
			src, tgt = o.Migrate.Users.Verify.SourceDatabaseURL, o.Migrate.Users.Verify.DatabaseURL
		}
		if src != "postgres://legacy-env@localhost/php" || tgt != "postgres://target-env@localhost/web" {
			t.Errorf("%v: source=%s target=%s", args, src, tgt)
		}
	}
}

// The legacy PHP database is a different database from the torrent source:
// the torrent-source variable and key must never feed `migrate users`.
func TestUsersSourceNeverFallsBackToTheTorrentSourceOrTarget(t *testing.T) {
	t.Setenv(migrationSourceEnvName, "postgres://torrent-source@localhost/old")
	t.Setenv(migrateDatabaseEnvName, "postgres://target-env@localhost/web")
	o := &OpFlags{}
	if _, err := o.parseArgs([]string{"migrate", "users"}); err != nil {
		t.Fatal(err)
	}
	if got := o.Migrate.Users.SourceDatabaseURL; got != "" {
		t.Errorf("the users source fell back to %q", got)
	}

	data := []byte("migrateSourceDatabaseURL: postgres://torrent-file@localhost/old\nmigrateDatabaseURL: postgres://target-file@localhost/web\n")
	o = &OpFlags{Crawl: defaults()}
	if err := o.applyYamlConfig(data, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if o.Migrate.Users.SourceDatabaseURL != "" || o.Migrate.Users.Verify.SourceDatabaseURL != "" {
		t.Error("the torrent source key fed the users source")
	}
	if o.Migrate.Users.DatabaseURL != "postgres://target-file@localhost/web" || o.Migrate.Users.Verify.DatabaseURL != "postgres://target-file@localhost/web" {
		t.Error("the target key did not reach migrate users")
	}
}

func TestApplyYamlConfig_UsersSourceKey(t *testing.T) {
	data := []byte("migrateUsersSourceDatabaseURL: postgres://legacy-file@localhost/php\n")
	o := &OpFlags{Crawl: defaults()}
	if err := o.applyYamlConfig(data, map[string]bool{}); err != nil {
		t.Fatalf("applyYamlConfig() error = %v", err)
	}
	if o.Migrate.Users.SourceDatabaseURL != "postgres://legacy-file@localhost/php" || o.Migrate.Users.Verify.SourceDatabaseURL != "postgres://legacy-file@localhost/php" {
		t.Errorf("file key not applied: %+v", o.Migrate.Users)
	}
	if o.Migrate.Torrents.SourceDatabaseURL != "" {
		t.Error("the users key fed the torrent source")
	}
	// Environment beats the file.
	t.Setenv(migrationUsersSourceEnvName, "postgres://legacy-env@localhost/php")
	o2 := &OpFlags{}
	o2.Migrate.Subcommand = "users"
	o2.Migrate.Users.SourceDatabaseURL = "postgres://legacy-file@localhost/php"
	if err := o2.applyEnvMigrationConfig(); err != nil {
		t.Fatal(err)
	}
	if o2.Migrate.Users.SourceDatabaseURL != "postgres://legacy-env@localhost/php" {
		t.Errorf("env did not override the file: %s", o2.Migrate.Users.SourceDatabaseURL)
	}
}

func TestParseArgs_UsersVerifyNested(t *testing.T) {
	o := &OpFlags{}
	if _, err := o.parseArgs([]string{"migrate", "users", "--database", "postgres://parent@localhost/db",
		"--source-timezone", "UTC", "--report-json", "/tmp/p.json", "verify"}); err != nil {
		t.Fatal(err)
	}
	if o.Migrate.Subcommand != "users-verify" {
		t.Fatalf("subcommand %q", o.Migrate.Subcommand)
	}
	v := o.Migrate.Users.Verify
	if v.DatabaseURL != "postgres://parent@localhost/db" || v.SourceTimezone != "UTC" || v.ReportJSON != "/tmp/p.json" {
		t.Errorf("shared flags before the nested command were dropped: %+v", v)
	}
	// The nested position wins.
	o = &OpFlags{}
	if _, err := o.parseArgs([]string{"migrate", "users", "--source-timezone", "UTC", "verify", "--source-timezone", "Asia/Tokyo"}); err != nil {
		t.Fatal(err)
	}
	if got := o.Migrate.Users.Verify.SourceTimezone; got != "Asia/Tokyo" {
		t.Errorf("nested value lost: %s", got)
	}
}

func TestMigrationEnvNamespaceStillRejectsUnknownVariables(t *testing.T) {
	t.Setenv("MAGNETICO_MIGRATION_USER_SOURCE_DSN", "typo")
	o := &OpFlags{}
	_, err := o.parseArgs([]string{"migrate", "users"})
	if err == nil || !strings.Contains(err.Error(), "MAGNETICO_MIGRATION_USER_SOURCE_DSN") || !strings.Contains(err.Error(), migrationUsersSourceEnvName) {
		t.Errorf("unknown variable error: %v", err)
	}
}
