package opflags

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	ingestEnvPrefix = "MAGNETICO_INGEST_"

	// MinIngestTokenBytes is the minimum bearer-token length.
	MinIngestTokenBytes = 32

	ingestTokenEnv = ingestEnvPrefix + "TOKEN"
)

// IngestOptions carries the options of the `ingest` subcommand
// (docs/specs/crawler-ingest-delivery-v1.md §10). Precedence: CLI > ENV >
// built-in defaults. There is no config-file layer for this command. The
// bearer token is ENV-only: no flag, no YAML key, never printed.
type IngestOptions struct {
	Listen           string `long:"listen" description:"HTTP listen address (plain HTTP; terminate TLS in a reverse proxy)." default:"127.0.0.1:8080"`
	DatabaseURL      string `long:"database" description:"URL of the PostgreSQL database holding the 0.8.0 schema." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable"`
	QueueDatabaseURL string `long:"queue-database" description:"Optional URL for the magnetico_ingest queue (a role with DML on that schema only). Defaults to --database." default:""`
	QueueMaxBytes    int64  `long:"queue-max-bytes" description:"Quota of queue + quarantine payload bytes; above it admissions get 503 queue_full." default:"2147483648"`
	CacheMaxBytes    int64  `long:"cache-max-bytes" description:"Memory budget of the existence cache." default:"8388608"`
	CacheDisabled    bool   `long:"cache-disabled" description:"Disable the existence cache."`
	AllowNonDurable  bool   `long:"allow-non-durable-db" description:"Allow a PostgreSQL server running with fsync=off (development and tests only)."`

	// Token is the bearer token (MAGNETICO_INGEST_TOKEN only).
	Token string `yaml:"-"`
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
	ingestEnvPrefix + "ALLOW_NON_DURABLE_DB": "allow-non-durable-db",
}

// applyEnvIngestConfig applies MAGNETICO_INGEST_* variables (CLI wins), reads
// the ENV-only token and rejects unknown variables. Values are never echoed
// in errors.
func (o *OpFlags) applyEnvIngestConfig(explicit map[string]bool) error {
	known := map[string]bool{ingestTokenEnv: true}
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
	o.Ingest.Token = os.Getenv(ingestTokenEnv)

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, ingestEnvPrefix) && !known[name] {
			names := []string{ingestTokenEnv}
			for _, e := range ingestEnvs {
				names = append(names, e.name)
			}
			return fmt.Errorf("unknown environment variable %s; known variables: MAGNETICO_CONFIG_FILE_PATH, %s",
				name, strings.Join(names, ", "))
		}
	}
	return nil
}

// check validates the ingest options. Error text never contains the token.
func (o *IngestOptions) check() error {
	if len(o.Token) < MinIngestTokenBytes {
		return fmt.Errorf("%s is required and must be at least %d bytes long (it is read from the environment only)",
			ingestTokenEnv, MinIngestTokenBytes)
	}
	if strings.TrimSpace(o.Listen) == "" {
		return errors.New("the listen address cannot be empty")
	}
	if o.QueueMaxBytes <= 0 {
		return errors.New("queue-max-bytes must be positive")
	}
	if o.CacheMaxBytes < 0 {
		return errors.New("cache-max-bytes cannot be negative")
	}
	return nil
}
