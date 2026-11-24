package opflags

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

const (
	ingestEnvPrefix = "MAGNETICO_INGEST_"

	// MinIngestTokenBytes is the minimum length of the crawler's credential.
	MinIngestTokenBytes = 32

	// removedIngestTokenEnv was the shared ingest token. It has no
	// replacement variable on the server: every crawler has its own
	// credential in the database.
	removedIngestTokenEnv = ingestEnvPrefix + "TOKEN"
)

// IngestOptions carries the options of the `ingest` subcommand
// (docs/specs/crawler-ingest-delivery-v1.md §10). Precedence: CLI > ENV >
// built-in defaults. There is no config-file layer for this command. Crawler
// credentials live in PostgreSQL and are managed with `ingest crawler …`.
type IngestOptions struct {
	Listen           string `long:"listen" description:"HTTP listen address (plain HTTP; terminate TLS in a reverse proxy)." default:"127.0.0.1:8080"`
	DatabaseURL      string `long:"database" description:"URL of the PostgreSQL database holding the 0.8.0 schema." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable"`
	QueueDatabaseURL string `long:"queue-database" description:"Optional URL for the magnetico_ingest queue (a role with DML on that schema only). Defaults to --database." default:""`
	QueueMaxBytes    int64  `long:"queue-max-bytes" description:"Quota of queue + quarantine payload bytes; above it admissions get 503 queue_full." default:"2147483648"`
	CacheMaxBytes    int64  `long:"cache-max-bytes" description:"Memory budget of the existence cache." default:"8388608"`
	CacheDisabled    bool   `long:"cache-disabled" description:"Disable the existence cache."`
	TrustedProxies   string `long:"trusted-proxies" description:"Comma-separated CIDRs (or addresses) of reverse proxies whose X-Forwarded-For is trusted when recording a crawler's last address. Empty: the TCP peer is used." default:""`
	StatsDisabled    bool   `long:"stats-disabled" description:"Kill switch: collect and persist no ingestion statistics. Authentication and admission are unaffected either way."`
	AllowNonDurable  bool   `long:"allow-non-durable-db" description:"Allow a PostgreSQL server running with fsync=off (development and tests only)."`

	// TrustedProxyPrefixes is the parsed form of TrustedProxies.
	TrustedProxyPrefixes []netip.Prefix `yaml:"-"`

	Crawler IngestCrawlerCommands `command:"crawler" description:"Operator commands for the crawler registry: add, list, disable, enable, rotate-token."`

	// Subcommand is the selected operator command ("crawler add", …), empty
	// when the service itself runs.
	Subcommand string `yaml:"-"`
}

// IngestCrawlerCommands are the operator commands of `magnetico ingest crawler`.
// They connect with --database (or --queue-database when set); both must come
// before the subcommand, as with `web user …`.
type IngestCrawlerCommands struct {
	Add         IngestCrawlerName `command:"add" description:"Register a crawler and print its generated id and token once."`
	List        IngestNoArgs      `command:"list" description:"List crawlers (id, name, enabled, created, token rotated, disabled); never a token."`
	Disable     IngestCrawlerName `command:"disable" description:"Refuse a crawler's token (running ingest instances apply it within 60 seconds)."`
	Enable      IngestCrawlerName `command:"enable" description:"Accept a disabled crawler's token again."`
	RotateToken IngestCrawlerName `command:"rotate-token" description:"Replace a crawler's token (same identity) and print the new token once."`
}

// IngestNoArgs is a command without arguments.
type IngestNoArgs struct{}

// IngestCrawlerName is a command taking one crawler name.
type IngestCrawlerName struct {
	Args struct {
		Name string `positional-arg-name:"name" required:"true"`
	} `positional-args:"true"`
}

type ingestEnv struct {
	name string
	set  func(o *IngestOptions, v string) error
}

func parseBool(v string) (bool, error) { return strconv.ParseBool(strings.TrimSpace(v)) }

var ingestEnvs = []ingestEnv{
	{ingestEnvPrefix + "LISTEN", func(o *IngestOptions, v string) error { o.Listen = v; return nil }},
	{ingestEnvPrefix + "DATABASE", func(o *IngestOptions, v string) error { o.DatabaseURL = v; return nil }},
	{ingestEnvPrefix + "QUEUE_DATABASE", func(o *IngestOptions, v string) error { o.QueueDatabaseURL = v; return nil }},
	{ingestEnvPrefix + "QUEUE_MAX_BYTES", func(o *IngestOptions, v string) (err error) {
		o.QueueMaxBytes, err = strconv.ParseInt(v, 10, 64)
		return
	}},
	{ingestEnvPrefix + "CACHE_MAX_BYTES", func(o *IngestOptions, v string) (err error) {
		o.CacheMaxBytes, err = strconv.ParseInt(v, 10, 64)
		return
	}},
	{ingestEnvPrefix + "CACHE_DISABLED", func(o *IngestOptions, v string) (err error) {
		o.CacheDisabled, err = parseBool(v)
		return
	}},
	{ingestEnvPrefix + "TRUSTED_PROXIES", func(o *IngestOptions, v string) error { o.TrustedProxies = v; return nil }},
	{ingestEnvPrefix + "STATS_DISABLED", func(o *IngestOptions, v string) (err error) {
		o.StatsDisabled, err = parseBool(v)
		return
	}},
	{ingestEnvPrefix + "ALLOW_NON_DURABLE_DB", func(o *IngestOptions, v string) (err error) {
		o.AllowNonDurable, err = parseBool(v)
		return
	}},
}

// ingestEnvFlag maps an env name to the CLI long name it must not override.
var ingestEnvFlag = map[string]string{
	ingestEnvPrefix + "LISTEN":               "listen",
	ingestEnvPrefix + "DATABASE":             "database",
	ingestEnvPrefix + "QUEUE_DATABASE":       "queue-database",
	ingestEnvPrefix + "QUEUE_MAX_BYTES":      "queue-max-bytes",
	ingestEnvPrefix + "CACHE_MAX_BYTES":      "cache-max-bytes",
	ingestEnvPrefix + "CACHE_DISABLED":       "cache-disabled",
	ingestEnvPrefix + "TRUSTED_PROXIES":      "trusted-proxies",
	ingestEnvPrefix + "STATS_DISABLED":       "stats-disabled",
	ingestEnvPrefix + "ALLOW_NON_DURABLE_DB": "allow-non-durable-db",
}

// applyEnvIngestConfig applies MAGNETICO_INGEST_* variables (CLI wins) and
// rejects unknown ones, and the removed shared token with a message naming its
// replacement. Values are never echoed in errors.
func (o *OpFlags) applyEnvIngestConfig(explicit map[string]bool) error {
	if _, present := os.LookupEnv(removedIngestTokenEnv); present {
		return errors.New(removedIngestTokenEnv + " is no longer supported: the shared ingest token was replaced by " +
			"per-crawler credentials stored in PostgreSQL. Register each crawler with `magnetico ingest crawler add <name>` " +
			"and set the printed token as MAGNETICO_CRAWL_INGEST_TOKEN on that crawler (see UPGRADE.md)")
	}
	known := map[string]bool{}
	for _, e := range ingestEnvs {
		known[e.name] = true
		if explicit[ingestEnvFlag[e.name]] {
			continue
		}
		if value, ok := os.LookupEnv(e.name); ok && value != "" {
			if err := e.set(&o.Ingest, value); err != nil {
				return fmt.Errorf("%s: invalid value", e.name)
			}
		}
	}

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, ingestEnvPrefix) && !known[name] {
			var names []string
			for _, e := range ingestEnvs {
				names = append(names, e.name)
			}
			return fmt.Errorf("unknown environment variable %s; known variables: MAGNETICO_CONFIG_FILE_PATH, %s",
				name, strings.Join(names, ", "))
		}
	}
	return nil
}

// checkCrawler validates what the operator commands use: the database.
func (o *IngestOptions) checkCrawler() error {
	if strings.TrimSpace(o.DatabaseURL) == "" && strings.TrimSpace(o.QueueDatabaseURL) == "" {
		return errors.New("the database URL cannot be empty")
	}
	return nil
}

// check validates the ingest service options.
func (o *IngestOptions) check() error {
	if strings.TrimSpace(o.Listen) == "" {
		return errors.New("the listen address cannot be empty")
	}
	if o.QueueMaxBytes <= 0 {
		return errors.New("queue-max-bytes must be positive")
	}
	if o.CacheMaxBytes < 0 {
		return errors.New("cache-max-bytes cannot be negative")
	}
	prefixes, err := parseTrustedProxies(o.TrustedProxies)
	if err != nil {
		return err
	}
	o.TrustedProxyPrefixes = prefixes
	return nil
}
