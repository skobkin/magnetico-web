package opflags

import (
	"strings"
	"testing"
)

// migrateDefaults returns SchemaInitOptions equivalent to the built-in flag
// defaults of `migrate schema-init`.
func migrateDefaults() SchemaInitOptions {
	return SchemaInitOptions{
		DatabaseURL: "postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable",
	}
}

func TestApplyYamlConfig_MigrateKeyApplied(t *testing.T) {
	t.Parallel()

	data := []byte("databaseURL: postgres://file-only@localhost/db\nmigrateDatabaseURL: postgres://migrate-file@localhost/db\n")

	o := &OpFlags{Crawl: defaults(), Migrate: MigrateOptions{SchemaInit: migrateDefaults()}}
	if err := o.applyYamlConfig(data, map[string]bool{}); err != nil {
		t.Fatalf("applyYamlConfig() error = %v", err)
	}

	if o.Crawl.DatabaseURL != "postgres://file-only@localhost/db" {
		t.Errorf("Crawl.DatabaseURL = %s, want the file value", o.Crawl.DatabaseURL)
	}
	if o.Migrate.SchemaInit.DatabaseURL != "postgres://migrate-file@localhost/db" {
		t.Errorf("Migrate.SchemaInit.DatabaseURL = %s, want the file value", o.Migrate.SchemaInit.DatabaseURL)
	}
}

func TestApplyYamlConfig_MigrateKeyUnknownTypoRejected(t *testing.T) {
	t.Parallel()

	data := []byte("migrateDatabaseurl: postgres://typo@localhost/db\n")

	o := &OpFlags{Crawl: defaults(), Migrate: MigrateOptions{SchemaInit: migrateDefaults()}}
	err := o.applyYamlConfig(data, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "migrateDatabaseurl") {
		t.Errorf("expected unknown-key error mentioning migrateDatabaseurl, got: %v", err)
	}
}

func TestApplyYamlConfig_MigrateExplicitCLIBeatsFile(t *testing.T) {
	t.Parallel()

	data := []byte("migrateDatabaseURL: postgres://migrate-file@localhost/db\n")

	o := &OpFlags{Crawl: defaults(), Migrate: MigrateOptions{SchemaInit: migrateDefaults()}}
	explicit := map[string]bool{"database": true}
	if err := o.applyYamlConfig(data, explicit); err != nil {
		t.Fatalf("applyYamlConfig() error = %v", err)
	}

	if o.Migrate.SchemaInit.DatabaseURL != migrateDefaults().DatabaseURL {
		t.Errorf("explicit CLI flag was overwritten by the file: %s", o.Migrate.SchemaInit.DatabaseURL)
	}
}

func TestApplyEnvMigrateConfig_Precedence(t *testing.T) {
	fileValue := "postgres://migrate-file@localhost/db"
	envValue := "postgres://migrate-env@localhost/db"

	t.Run("EnvBeatsFile", func(t *testing.T) {
		t.Setenv(migrateDatabaseEnvName, envValue)

		o := &OpFlags{Crawl: defaults(), Migrate: MigrateOptions{SchemaInit: migrateDefaults()}}
		if err := o.applyYamlConfig([]byte("migrateDatabaseURL: "+fileValue+"\n"), map[string]bool{}); err != nil {
			t.Fatalf("applyYamlConfig() error = %v", err)
		}
		if err := o.applyEnvMigrateConfig(map[string]bool{}); err != nil {
			t.Fatalf("applyEnvMigrateConfig() error = %v", err)
		}

		if o.Migrate.SchemaInit.DatabaseURL != envValue {
			t.Errorf("DatabaseURL = %s, want the env value", o.Migrate.SchemaInit.DatabaseURL)
		}
	})

	t.Run("EnvIgnoredWhenFlagExplicit", func(t *testing.T) {
		t.Setenv(migrateDatabaseEnvName, envValue)

		o := &OpFlags{Crawl: defaults(), Migrate: MigrateOptions{SchemaInit: migrateDefaults()}}
		if err := o.applyEnvMigrateConfig(map[string]bool{"database": true}); err != nil {
			t.Fatalf("applyEnvMigrateConfig() error = %v", err)
		}

		if o.Migrate.SchemaInit.DatabaseURL != migrateDefaults().DatabaseURL {
			t.Errorf("DatabaseURL = %s, want the CLI/default value", o.Migrate.SchemaInit.DatabaseURL)
		}
	})

	t.Run("EmptyEnvTreatedAsUnset", func(t *testing.T) {
		t.Setenv(migrateDatabaseEnvName, "")

		o := &OpFlags{Crawl: defaults(), Migrate: MigrateOptions{SchemaInit: migrateDefaults()}}
		if err := o.applyEnvMigrateConfig(map[string]bool{}); err != nil {
			t.Fatalf("applyEnvMigrateConfig() error = %v", err)
		}

		if o.Migrate.SchemaInit.DatabaseURL != migrateDefaults().DatabaseURL {
			t.Errorf("DatabaseURL = %s, want the default (empty env is unset)", o.Migrate.SchemaInit.DatabaseURL)
		}
	})

	t.Run("UnknownMigrateEnvRejected", func(t *testing.T) {
		t.Setenv("MAGNETICO_MIGRATE_BOGUS", "1")

		o := &OpFlags{Crawl: defaults(), Migrate: MigrateOptions{SchemaInit: migrateDefaults()}}
		err := o.applyEnvMigrateConfig(map[string]bool{})
		if err == nil || !strings.Contains(err.Error(), "MAGNETICO_MIGRATE_BOGUS") {
			t.Errorf("expected unknown-variable error, got: %v", err)
		}
	})
}

func TestApplyConfig_MigrateCommandUsesItsOwnEnvNamespace(t *testing.T) {
	// A crawl-run variable must not leak into migrate and vice versa.
	t.Setenv("MAGNETICO_MIGRATE_DATABASE", "postgres://migrate-env@localhost/db")
	t.Setenv("MAGNETICO_CRAWL_DATABASE", "postgres://crawl-env@localhost/db")

	o := &OpFlags{Crawl: defaults(), Migrate: MigrateOptions{SchemaInit: migrateDefaults()}}
	if err := o.applyConfig(map[string]bool{}, "migrate"); err != nil {
		t.Fatalf("applyConfig(migrate) error = %v", err)
	}
	if o.Migrate.SchemaInit.DatabaseURL != "postgres://migrate-env@localhost/db" {
		t.Errorf("migrate DatabaseURL = %s, want the migrate env value", o.Migrate.SchemaInit.DatabaseURL)
	}
	if o.Crawl.DatabaseURL != defaults().DatabaseURL {
		t.Errorf("crawl DatabaseURL = %s, want the default (crawl env is not applied in migrate runs)", o.Crawl.DatabaseURL)
	}

	o = &OpFlags{Crawl: defaults(), Migrate: MigrateOptions{SchemaInit: migrateDefaults()}}
	if err := o.applyConfig(map[string]bool{}, "crawl"); err != nil {
		t.Fatalf("applyConfig(crawl) error = %v", err)
	}
	if o.Crawl.DatabaseURL != "postgres://crawl-env@localhost/db" {
		t.Errorf("crawl DatabaseURL = %s, want the crawl env value", o.Crawl.DatabaseURL)
	}
	if o.Migrate.SchemaInit.DatabaseURL != migrateDefaults().DatabaseURL {
		t.Errorf("migrate DatabaseURL = %s, want the default (migrate env is not applied in crawl runs)", o.Migrate.SchemaInit.DatabaseURL)
	}
}

func TestParseArgs_MigrateSubcommands(t *testing.T) {
	t.Run("BareMigrateIsAUsageError", func(t *testing.T) {
		o := &OpFlags{}
		_, err := o.parseArgs([]string{"migrate"})
		if err == nil || !strings.Contains(err.Error(), "schema-init") || !strings.Contains(err.Error(), "torrents") {
			t.Errorf("parseArgs([migrate]) error = %v, want a usage error naming both subcommands", err)
		}
	})

	t.Run("SchemaInitDefaults", func(t *testing.T) {
		o := &OpFlags{}
		command, err := o.parseArgs([]string{"migrate", "schema-init"})
		if err != nil {
			t.Fatalf("parseArgs([migrate schema-init]) error = %v", err)
		}
		if command != "migrate" {
			t.Errorf("command = %q, want migrate", command)
		}
		if o.Migrate.SchemaInit.DatabaseURL != migrateDefaults().DatabaseURL {
			t.Errorf("DatabaseURL = %s, want the built-in default", o.Migrate.SchemaInit.DatabaseURL)
		}
	})

	t.Run("CLIFlagWinsOverEnv", func(t *testing.T) {
		t.Setenv(migrateDatabaseEnvName, "postgres://migrate-env@localhost/db")

		o := &OpFlags{}
		if _, err := o.parseArgs([]string{"migrate", "schema-init", "--database", "postgres://cli@localhost/db"}); err != nil {
			t.Fatalf("parseArgs() error = %v", err)
		}
		if o.Migrate.SchemaInit.DatabaseURL != "postgres://cli@localhost/db" {
			t.Errorf("DatabaseURL = %s, want the CLI value", o.Migrate.SchemaInit.DatabaseURL)
		}
	})

	t.Run("EnvWinsWithoutFlag", func(t *testing.T) {
		t.Setenv(migrateDatabaseEnvName, "postgres://migrate-env@localhost/db")

		o := &OpFlags{}
		if _, err := o.parseArgs([]string{"migrate", "schema-init"}); err != nil {
			t.Fatalf("parseArgs() error = %v", err)
		}
		if o.Migrate.SchemaInit.DatabaseURL != "postgres://migrate-env@localhost/db" {
			t.Errorf("DatabaseURL = %s, want the env value", o.Migrate.SchemaInit.DatabaseURL)
		}
	})
}

// torrentsDefaults returns TorrentsOptions equivalent to the built-in flag
// defaults of `migrate torrents`.
func torrentsDefaults() TorrentsOptions {
	return TorrentsOptions{
		DatabaseURL:      "postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable",
		Workers:          4,
		BatchSize:        256,
		MaxInflightBytes: 67108864,
	}
}

func TestParseArgs_TorrentsSubcommand(t *testing.T) {
	t.Run("DefaultsAndSubcommandRecording", func(t *testing.T) {
		o := &OpFlags{}
		command, err := o.parseArgs([]string{"migrate", "torrents"})
		if err != nil {
			t.Fatalf("parseArgs([migrate torrents]) error = %v", err)
		}
		if command != "migrate" {
			t.Errorf("command = %q, want migrate", command)
		}
		if o.Migrate.Subcommand != "torrents" {
			t.Errorf("Migrate.Subcommand = %q, want torrents", o.Migrate.Subcommand)
		}
		if o.Migrate.Torrents != torrentsDefaults() {
			t.Errorf("Torrents = %+v, want the built-in defaults", o.Migrate.Torrents)
		}
	})

	t.Run("CLIFlags", func(t *testing.T) {
		o := &OpFlags{}
		if _, err := o.parseArgs([]string{"migrate", "torrents",
			"--database", "postgres://cli@localhost/db",
			"--workers", "8",
			"--batch-size", "100",
			"--max-inflight-bytes", "33554432",
			"--defer-secondary-indexes",
		}); err != nil {
			t.Fatalf("parseArgs() error = %v", err)
		}
		got := o.Migrate.Torrents
		if got.DatabaseURL != "postgres://cli@localhost/db" {
			t.Errorf("DatabaseURL = %s, want the CLI value", got.DatabaseURL)
		}
		if got.Workers != 8 || got.BatchSize != 100 || got.MaxInflightBytes != 33554432 || !got.DeferSecondaryIndexes {
			t.Errorf("Torrents = %+v, want the CLI values", got)
		}
	})

	t.Run("TargetEnvAppliesWithoutFlag", func(t *testing.T) {
		t.Setenv(migrateDatabaseEnvName, "postgres://migrate-env@localhost/db")

		o := &OpFlags{}
		if _, err := o.parseArgs([]string{"migrate", "torrents"}); err != nil {
			t.Fatalf("parseArgs() error = %v", err)
		}
		if o.Migrate.Torrents.DatabaseURL != "postgres://migrate-env@localhost/db" {
			t.Errorf("DatabaseURL = %s, want the env value", o.Migrate.Torrents.DatabaseURL)
		}
	})

	t.Run("ExplicitTargetFlagBeatsEnv", func(t *testing.T) {
		t.Setenv(migrateDatabaseEnvName, "postgres://migrate-env@localhost/db")

		o := &OpFlags{}
		if _, err := o.parseArgs([]string{"migrate", "torrents", "--database", "postgres://cli@localhost/db"}); err != nil {
			t.Fatalf("parseArgs() error = %v", err)
		}
		if o.Migrate.Torrents.DatabaseURL != "postgres://cli@localhost/db" {
			t.Errorf("DatabaseURL = %s, want the CLI value", o.Migrate.Torrents.DatabaseURL)
		}
	})

	t.Run("SourceEnvApplies", func(t *testing.T) {
		t.Setenv(migrationSourceEnvName, "postgres://source-env@localhost/legacy")

		o := &OpFlags{}
		if _, err := o.parseArgs([]string{"migrate", "torrents"}); err != nil {
			t.Fatalf("parseArgs() error = %v", err)
		}
		if o.Migrate.Torrents.SourceDatabaseURL != "postgres://source-env@localhost/legacy" {
			t.Errorf("SourceDatabaseURL = %s, want the env value", o.Migrate.Torrents.SourceDatabaseURL)
		}
	})

	t.Run("SourceEnvBeatsFile", func(t *testing.T) {
		t.Setenv(migrationSourceEnvName, "postgres://source-env@localhost/legacy")

		o := &OpFlags{}
		data := []byte("migrateSourceDatabaseURL: postgres://source-file@localhost/legacy\n")
		// parseArgs with a config file would need a real file on disk; apply
		// the layers directly (the parseArgs-level wiring is covered above).
		if err := o.applyYamlConfig(data, map[string]bool{}); err != nil {
			t.Fatalf("applyYamlConfig() error = %v", err)
		}
		if err := o.applyEnvMigrateConfig(map[string]bool{}); err != nil {
			t.Fatalf("applyEnvMigrateConfig() error = %v", err)
		}
		if err := o.applyEnvTorrentsConfig(); err != nil {
			t.Fatalf("applyEnvTorrentsConfig() error = %v", err)
		}
		if o.Migrate.Torrents.SourceDatabaseURL != "postgres://source-env@localhost/legacy" {
			t.Errorf("SourceDatabaseURL = %s, want the env value to beat the file", o.Migrate.Torrents.SourceDatabaseURL)
		}
	})

	t.Run("EmptySourceEnvTreatedAsUnset", func(t *testing.T) {
		t.Setenv(migrationSourceEnvName, "")

		o := &OpFlags{}
		if err := o.applyYamlConfig([]byte("migrateSourceDatabaseURL: postgres://source-file@localhost/legacy\n"), map[string]bool{}); err != nil {
			t.Fatalf("applyYamlConfig() error = %v", err)
		}
		if err := o.applyEnvTorrentsConfig(); err != nil {
			t.Fatalf("applyEnvTorrentsConfig() error = %v", err)
		}
		if o.Migrate.Torrents.SourceDatabaseURL != "postgres://source-file@localhost/legacy" {
			t.Errorf("SourceDatabaseURL = %s, want the file value (empty env is unset)", o.Migrate.Torrents.SourceDatabaseURL)
		}
	})

	t.Run("UnknownMigrationEnvRejected", func(t *testing.T) {
		t.Setenv("MAGNETICO_MIGRATION_BOGUS", "1")

		o := &OpFlags{}
		err := o.applyEnvTorrentsConfig()
		if err == nil || !strings.Contains(err.Error(), "MAGNETICO_MIGRATION_BOGUS") {
			t.Errorf("expected unknown-variable error, got: %v", err)
		}
	})
}

func TestApplyYamlConfig_TorrentsKeys(t *testing.T) {
	t.Run("TargetAndSourceKeysApplied", func(t *testing.T) {
		data := []byte("migrateDatabaseURL: postgres://target-file@localhost/db\n" +
			"migrateSourceDatabaseURL: postgres://source-file@localhost/legacy\n")

		o := &OpFlags{Migrate: MigrateOptions{SchemaInit: migrateDefaults(), Torrents: torrentsDefaults()}}
		if err := o.applyYamlConfig(data, map[string]bool{}); err != nil {
			t.Fatalf("applyYamlConfig() error = %v", err)
		}
		if o.Migrate.Torrents.DatabaseURL != "postgres://target-file@localhost/db" {
			t.Errorf("Torrents.DatabaseURL = %s, want the file value", o.Migrate.Torrents.DatabaseURL)
		}
		if o.Migrate.Torrents.SourceDatabaseURL != "postgres://source-file@localhost/legacy" {
			t.Errorf("Torrents.SourceDatabaseURL = %s, want the file value", o.Migrate.Torrents.SourceDatabaseURL)
		}
		if o.Migrate.SchemaInit.DatabaseURL != "postgres://target-file@localhost/db" {
			t.Errorf("SchemaInit.DatabaseURL = %s, want the file value", o.Migrate.SchemaInit.DatabaseURL)
		}
	})

	t.Run("SourceKeyTypoRejected", func(t *testing.T) {
		data := []byte("migrateSourceDatabaseurl: postgres://typo@localhost/legacy\n")

		o := &OpFlags{Migrate: MigrateOptions{SchemaInit: migrateDefaults(), Torrents: torrentsDefaults()}}
		err := o.applyYamlConfig(data, map[string]bool{})
		if err == nil || !strings.Contains(err.Error(), "migrateSourceDatabaseurl") {
			t.Errorf("expected unknown-key error mentioning migrateSourceDatabaseurl, got: %v", err)
		}
	})

	t.Run("ExplicitTargetFlagBeatsFile", func(t *testing.T) {
		data := []byte("migrateDatabaseURL: postgres://target-file@localhost/db\n")

		o := &OpFlags{Migrate: MigrateOptions{SchemaInit: migrateDefaults(), Torrents: torrentsDefaults()}}
		if err := o.applyYamlConfig(data, map[string]bool{"database": true}); err != nil {
			t.Fatalf("applyYamlConfig() error = %v", err)
		}
		if o.Migrate.Torrents.DatabaseURL != torrentsDefaults().DatabaseURL {
			t.Errorf("Torrents.DatabaseURL = %s, want the CLI/default value", o.Migrate.Torrents.DatabaseURL)
		}
	})
}

// The migration env namespace is scoped to `migrate torrents` runs: a
// schema-init run neither applies nor rejects MAGNETICO_MIGRATION_* (the
// same way crawl variables are not validated during migrate runs).
func TestApplyEnvMigrateConfig_IgnoresMigrationNamespace(t *testing.T) {
	t.Setenv("MAGNETICO_MIGRATION_BOGUS", "1")
	t.Setenv(migrationSourceEnvName, "postgres://source-env@localhost/legacy")

	o := &OpFlags{Crawl: defaults(), Migrate: MigrateOptions{SchemaInit: migrateDefaults(), Torrents: torrentsDefaults()}}
	if err := o.applyConfig(map[string]bool{}, "migrate"); err != nil {
		t.Fatalf("applyConfig(migrate) error = %v", err)
	}
	if o.Migrate.SchemaInit.DatabaseURL != migrateDefaults().DatabaseURL {
		t.Errorf("SchemaInit.DatabaseURL = %s, want the default", o.Migrate.SchemaInit.DatabaseURL)
	}
}
