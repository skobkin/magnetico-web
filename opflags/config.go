package opflags

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/jessevdk/go-flags"
)

const (
	envPrefix        = "MAGNETICO_CRAWL_"
	migrateEnvPrefix = "MAGNETICO_MIGRATE_"

	// migrationEnvPrefix configures the migration source DSN outside the
	// command line (it has no CLI flag, so it can never appear in `ps`
	// output) and is validated — unknown names rejected — for every migrate
	// subcommand.
	migrationEnvPrefix = "MAGNETICO_MIGRATION_"

	// migrateDatabaseYamlKey and migrateDatabaseEnvName configure the
	// target DSN of the migrate subcommands outside the command line.
	migrateDatabaseYamlKey = "migrateDatabaseURL"
	migrateDatabaseEnvName = migrateEnvPrefix + "DATABASE"

	// migrateSourceDatabaseYamlKey and migrationSourceEnvName configure the
	// `migrate torrents` source DSN outside the command line (env over
	// file; there is no flag, so no explicit-map entry exists).
	migrateSourceDatabaseYamlKey = "migrateSourceDatabaseURL"
	migrationSourceEnvName       = migrationEnvPrefix + "SOURCE_DSN"
)

// crawlSetting describes one configurable crawl option: its CLI long name,
// its YAML key and its environment variable, plus the setters used to apply
// values from the configuration file and the environment. The value
// precedence is CLI > ENV > config file > built-in defaults.
type crawlSetting struct {
	longName string
	yamlKey  string
	envName  string
	// copyFrom applies a value decoded from the configuration file.
	copyFrom func(dst *CrawlOptions, src *CrawlOptions)
	// setFromString applies an environment-variable value.
	setFromString func(o *CrawlOptions, value string) error
}

func parseUint(value string, bitSize int) (uint, error) {
	n, err := strconv.ParseUint(value, 10, bitSize)
	if err != nil {
		return 0, err
	}
	return uint(n), nil
}

func setStringList(value string) ([]string, error) {
	return strings.Split(value, ","), nil
}

var crawlSettings = []crawlSetting{
	{
		longName: "database", yamlKey: "databaseURL", envName: "MAGNETICO_CRAWL_DATABASE",
		copyFrom:      func(dst, src *CrawlOptions) { dst.DatabaseURL = src.DatabaseURL },
		setFromString: func(o *CrawlOptions, v string) error { o.DatabaseURL = v; return nil },
	},
	{
		longName: "pyroscope", yamlKey: "pyroscopeURL", envName: "MAGNETICO_CRAWL_PYROSCOPE",
		copyFrom:      func(dst, src *CrawlOptions) { dst.PyroscopeURL = src.PyroscopeURL },
		setFromString: func(o *CrawlOptions, v string) error { o.PyroscopeURL = v; return nil },
	},
	{
		longName: "indexer-addr", yamlKey: "indexerAddrs", envName: "MAGNETICO_CRAWL_INDEXER_ADDR",
		copyFrom:      func(dst, src *CrawlOptions) { dst.IndexerAddrs = src.IndexerAddrs },
		setFromString: func(o *CrawlOptions, v string) error { l, err := setStringList(v); o.IndexerAddrs = l; return err },
	},
	{
		longName: "indexer-max-neighbors", yamlKey: "indexerMaxNeighbors", envName: "MAGNETICO_CRAWL_INDEXER_MAX_NEIGHBORS",
		copyFrom: func(dst, src *CrawlOptions) { dst.IndexerMaxNeighbors = src.IndexerMaxNeighbors },
		setFromString: func(o *CrawlOptions, v string) error {
			n, err := parseUint(v, strconv.IntSize)
			o.IndexerMaxNeighbors = n
			return err
		},
	},
	{
		longName: "leech-deadline", yamlKey: "leechDeadline", envName: "MAGNETICO_CRAWL_LEECH_DEADLINE",
		copyFrom: func(dst, src *CrawlOptions) { dst.LeechDeadline = src.LeechDeadline },
		setFromString: func(o *CrawlOptions, v string) error {
			n, err := parseUint(v, strconv.IntSize)
			o.LeechDeadline = n
			return err
		},
	},
	{
		longName: "leech-max-n", yamlKey: "leechMaxN", envName: "MAGNETICO_CRAWL_LEECH_MAX_N",
		copyFrom: func(dst, src *CrawlOptions) { dst.LeechMaxN = src.LeechMaxN },
		setFromString: func(o *CrawlOptions, v string) error {
			n, err := parseUint(v, strconv.IntSize)
			o.LeechMaxN = n
			return err
		},
	},
	{
		longName: "max-rps", yamlKey: "maxRPS", envName: "MAGNETICO_CRAWL_MAX_RPS",
		copyFrom: func(dst, src *CrawlOptions) { dst.MaxRPS = src.MaxRPS },
		setFromString: func(o *CrawlOptions, v string) error {
			n, err := parseUint(v, strconv.IntSize)
			o.MaxRPS = n
			return err
		},
	},
	{
		longName: "bootstrap-node", yamlKey: "bootstrappingNodes", envName: "MAGNETICO_CRAWL_BOOTSTRAP_NODE",
		copyFrom: func(dst, src *CrawlOptions) { dst.BootstrappingNodes = src.BootstrappingNodes },
		setFromString: func(o *CrawlOptions, v string) error {
			l, err := setStringList(v)
			o.BootstrappingNodes = l
			return err
		},
	},
	{
		longName: "filter-nodes-cidrs", yamlKey: "filterNodesCIDRs", envName: "MAGNETICO_CRAWL_FILTER_NODES_CIDRS",
		copyFrom:      func(dst, src *CrawlOptions) { dst.FilterNodesCIDRs = src.FilterNodesCIDRs },
		setFromString: func(o *CrawlOptions, v string) error { l, err := setStringList(v); o.FilterNodesCIDRs = l; return err },
	},
}

// applyConfig overlays the configuration file and the environment onto the
// parsed options of the active command, enforcing CLI > ENV > config file >
// defaults. The config file is shared across subcommands: it is decoded
// strictly against every documented key of all subcommands (so a typo in an
// unused section is still an error), and only the active command's settings
// are merged in.
// explicit maps the long names of options set on the command line.
func (o *OpFlags) applyConfig(explicit map[string]bool, command string) error {
	// The config file location itself follows CLI > ENV.
	configPath := o.ConfigFilePath
	if !explicit["config-file-path"] {
		if value, ok := os.LookupEnv("MAGNETICO_CONFIG_FILE_PATH"); ok && value != "" {
			configPath = value
		}
	}

	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return err
		}
		if err := o.applyYamlConfig(data, explicit); err != nil {
			return err
		}
	}

	switch command {
	case "crawl":
		return o.applyEnvConfig(explicit)
	case "migrate":
		if err := o.applyEnvMigrateConfig(explicit); err != nil {
			return err
		}
		return o.applyEnvMigrationConfig()
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

// applyYamlConfig merges the parsed file into the live options. The file is
// decoded twice: once into a generic map to learn which keys are present, and
// once strictly into the combined view of every subcommand's documented keys
// (unknown keys are an error — including those of inactive subcommands). Only
// present keys are merged, so a sparse file never replaces defaults with zero
// values; explicitly configured zero-ish values are real values and apply.
func (o *OpFlags) applyYamlConfig(data []byte, explicit map[string]bool) error {
	var present map[string]any
	if err := yaml.Unmarshal(data, &present); err != nil {
		return fmt.Errorf("config file: %s", redactSecrets(err.Error()))
	}

	var file struct {
		CrawlOptions             `yaml:",inline"`
		MigrateDatabaseURL       string `yaml:"migrateDatabaseURL"`
		MigrateSourceDatabaseURL string `yaml:"migrateSourceDatabaseURL"`
	}
	if err := yaml.UnmarshalWithOptions(data, &file, yaml.Strict()); err != nil {
		return fmt.Errorf("config file: %s", redactSecrets(err.Error()))
	}

	for _, s := range crawlSettings {
		if _, found := present[s.yamlKey]; !found {
			continue
		}
		if explicit[s.longName] {
			continue
		}
		s.copyFrom(&o.Crawl, &file.CrawlOptions)
	}

	// The migrate target DSN key feeds whichever migrate subcommand runs.
	if _, found := present[migrateDatabaseYamlKey]; found && !explicit["database"] {
		o.Migrate.SchemaInit.DatabaseURL = file.MigrateDatabaseURL
		o.Migrate.Torrents.DatabaseURL = file.MigrateDatabaseURL
		o.Migrate.VerifyDecode.DatabaseURL = file.MigrateDatabaseURL
		o.Migrate.VerifyContent.DatabaseURL = file.MigrateDatabaseURL
		o.Migrate.Torrents.RetryErrors.DatabaseURL = file.MigrateDatabaseURL
	}

	// The migration source DSN has no CLI flag, so there is no explicit
	// guard: the file value always applies to every source-connected command
	// (env overrides it later). verify-decode is target-only and never reads
	// it.
	if _, found := present[migrateSourceDatabaseYamlKey]; found {
		o.Migrate.Torrents.SourceDatabaseURL = file.MigrateSourceDatabaseURL
		o.Migrate.VerifyContent.SourceDatabaseURL = file.MigrateSourceDatabaseURL
		o.Migrate.Torrents.RetryErrors.SourceDatabaseURL = file.MigrateSourceDatabaseURL
	}

	return nil
}

// applyEnvConfig applies MAGNETICO_CRAWL_* variables and rejects unknown ones.
// Empty values are treated as unset so that template-based deployments can
// safely expand unset placeholders.
func (o *OpFlags) applyEnvConfig(explicit map[string]bool) error {
	for _, s := range crawlSettings {
		if explicit[s.longName] {
			continue
		}
		if value, ok := os.LookupEnv(s.envName); ok && value != "" {
			if err := s.setFromString(&o.Crawl, value); err != nil {
				return fmt.Errorf("%s: %w", s.envName, err)
			}
		}
	}

	known := make(map[string]bool, len(crawlSettings)+1)
	known[envPrefix] = true
	for _, s := range crawlSettings {
		known[s.envName] = true
	}

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, envPrefix) || known[name] {
			continue
		}
		names := make([]string, 0, len(crawlSettings))
		for _, s := range crawlSettings {
			names = append(names, s.envName)
		}
		return fmt.Errorf(
			"unknown environment variable %s; known variables: MAGNETICO_CONFIG_FILE_PATH, %s",
			name, strings.Join(names, ", "),
		)
	}

	return nil
}

// applyEnvMigrateConfig applies MAGNETICO_MIGRATE_* variables and rejects
// unknown ones, with the same empty-means-unset rule as the crawl variables.
// The target DSN feeds whichever migrate subcommand runs.
func (o *OpFlags) applyEnvMigrateConfig(explicit map[string]bool) error {
	if !explicit["database"] {
		if value, ok := os.LookupEnv(migrateDatabaseEnvName); ok && value != "" {
			o.Migrate.SchemaInit.DatabaseURL = value
			o.Migrate.Torrents.DatabaseURL = value
			o.Migrate.VerifyDecode.DatabaseURL = value
			o.Migrate.VerifyContent.DatabaseURL = value
			o.Migrate.Torrents.RetryErrors.DatabaseURL = value
		}
	}

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, migrateEnvPrefix) || name == migrateDatabaseEnvName {
			continue
		}
		return fmt.Errorf(
			"unknown environment variable %s; known variables: MAGNETICO_CONFIG_FILE_PATH, %s",
			name, migrateDatabaseEnvName,
		)
	}

	return nil
}

// applyEnvMigrationConfig applies MAGNETICO_MIGRATION_* variables for every
// migrate subcommand and rejects unknown ones, with the same
// empty-means-unset rule as the other namespaces. The source DSN has no CLI
// flag, so the env value always applies (env over file, no explicit entry)
// — to torrents, the nested retry-errors pass and verify-content;
// verify-decode is target-only and schema-init is schema-only, so there the
// value is validated as a known variable and otherwise ignored.
func (o *OpFlags) applyEnvMigrationConfig() error {
	if value, ok := os.LookupEnv(migrationSourceEnvName); ok && value != "" {
		switch o.Migrate.Subcommand {
		case "torrents":
			o.Migrate.Torrents.SourceDatabaseURL = value
		case "retry-errors":
			o.Migrate.Torrents.RetryErrors.SourceDatabaseURL = value
		case "verify-content":
			o.Migrate.VerifyContent.SourceDatabaseURL = value
		}
	}

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, migrationEnvPrefix) || name == migrationSourceEnvName {
			continue
		}
		return fmt.Errorf(
			"unknown environment variable %s; known variables: MAGNETICO_CONFIG_FILE_PATH, %s",
			name, migrationSourceEnvName,
		)
	}

	return nil
}

// explicitOptions collects the long names of all options that were set on the
// command line (as opposed to defaulted) across the whole command tree.
//
// go-flags applies tag defaults through Option.Set as well, so Option.IsSet()
// alone is true for every option after parsing. An explicitly provided option
// skips the subsequent default application, which leaves Option.IsSetDefault()
// false for it; both checks together identify CLI-provided options.
func explicitOptions(root *flags.Command) map[string]bool {
	set := map[string]bool{}
	var walkGroup func(g *flags.Group)
	walkGroup = func(g *flags.Group) {
		for _, opt := range g.Options() {
			if opt.IsSet() && !opt.IsSetDefault() && opt.LongName != "" {
				set[opt.LongName] = true
			}
		}
		for _, child := range g.Groups() {
			walkGroup(child)
		}
	}
	var walkCommand func(c *flags.Command)
	walkCommand = func(c *flags.Command) {
		walkGroup(c.Group)
		for _, sub := range c.Commands() {
			walkCommand(sub)
		}
	}
	walkCommand(root)
	return set
}

// explicitInCommand collects the long names of the options explicitly
// provided on the command line for ONE command's own option group — the
// non-recursive counterpart of explicitOptions (nested subcommands are not
// walked), so individual parse positions can be compared independently.
func explicitInCommand(c *flags.Command) map[string]bool {
	set := map[string]bool{}
	for _, opt := range c.Options() {
		if opt.IsSet() && !opt.IsSetDefault() && opt.LongName != "" {
			set[opt.LongName] = true
		}
	}
	return set
}

var (
	// urlPasswordPattern matches the userinfo password of any scheme-qualified
	// URL (`scheme://user:secret@host`, empty username included:
	// `scheme://:secret@host`) — the shape of every DSN in the configuration
	// file. Hosts without userinfo never match: the `@` after the password
	// is required.
	urlPasswordPattern = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^:/@\s]*:)[^@\s]+@`)
	// credentialKeyPattern matches a credential given as a key/value pair —
	// in PostgreSQL key-value DSNs, URL query parameters and YAML keys
	// alike (`password=secret`, `pwd: secret`). The key may be spelled in
	// either case or percent-encoded (`%70assword=secret`, as URLs encode
	// query keys). The value may be bare — with backslash-escaped characters,
	// as conninfo spells them (`password=HEAD\ SECRET_TAIL`) — or quoted
	// with single or double quotes; quoted values may contain spaces and
	// backslash escapes (`password='HEAD\' SECRET_TAIL'`), all accepted by
	// pgx (Codex review iteration 4, finding 1).
	credentialKeyPattern = regexp.MustCompile(`((?:[pP]|%70|%50)(?:[aA]|%61|%41)(?:[sS]|%73|%53)(?:[sS]|%73|%53)(?:[wW]|%77|%57)(?:[oO]|%6[fF])(?:[rR]|%72|%52)(?:[dD]|%64|%44)|(?:[pP]|%70|%50)(?:[wW]|%77|%57)(?:[dD]|%64|%44))(\s*[=:]\s*)(?:'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|(?:\\.|[^\s\\])+)`)
)

var (
	// excerptLinePattern matches a quoted configuration source line inside a
	// yaml diagnostic (gutter + line number + `|` + content; the offending
	// line is prefixed with `>`).
	excerptLinePattern = regexp.MustCompile(`^(>?)(\s*\d+\s*\|)(.*)$`)
	// credentialLinePattern is a deliberately LOOSE detector for a line that
	// may carry a credential: any password/pwd spelling (case classes and
	// percent escapes — a key itself is never escaped, so plain matching is
	// sound for detection) or a scheme-qualified URL with userinfo. Over-
	// matching costs a redacted diagnostic line; under-matching leaks a
	// secret.
	credentialLinePattern = regexp.MustCompile(`(?i:passw|pwd|%70|%50)|://[^/@\s]+@`)
)

// redactSecrets masks credential material that YAML diagnostics echo from
// the configuration file's source lines: the yaml library renders errors
// with the offending line (and its context) quoted verbatim, so a typo on
// one line would otherwise print a neighboring DSN — password included —
// to the terminal and any log capture.
//
// Two layers, because exact-value masking alone cannot be exhaustive there:
// the YAML representation re-encodes conninfo escapes on top of the DSN's
// own (a double-quoted scalar spells a conninfo escaped space as `\\ `), so
// a value parser working on the raw line inevitably misses shapes. Layer 1
// masks the recognized value shapes in place (error text, inline DSNs);
// layer 2 drops any quoted source line that may carry a credential whole —
// the line number survives, the content does not (Codex review iteration 5,
// finding 1).
func redactSecrets(msg string) string {
	msg = urlPasswordPattern.ReplaceAllString(msg, "${1}xxxxx@")
	msg = credentialKeyPattern.ReplaceAllString(msg, "${1}${2}xxxxx")
	lines := strings.Split(msg, "\n")
	for i, line := range lines {
		if m := excerptLinePattern.FindStringSubmatch(line); m != nil &&
			credentialLinePattern.MatchString(m[3]) {
			lines[i] = m[1] + m[2] + " [credential-bearing configuration line redacted]"
		}
	}
	return strings.Join(lines, "\n")
}
