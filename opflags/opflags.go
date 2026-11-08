package opflags

import (
	"errors"
	"fmt"
	"net"
	"reflect"
)

// OpFlags is the root of the command line structure of the single `magnetico`
// binary: one global option plus subcommands.
type OpFlags struct {
	// ConfigFilePath must be given before the subcommand name, e.g.
	// `magnetico --config-file-path config.yml crawl`.
	ConfigFilePath string `long:"config-file-path" description:"Configuration YAML file path. If not filled in, it will default to disabled." default:""`

	Crawl   CrawlOptions   `command:"crawl" description:"Run the DHT indexer (storage is not implemented yet, it fails closed)."`
	Web     StubOptions    `command:"web" description:"Reserved for the future authenticated web UI (not implemented yet)."`
	Migrate MigrateOptions `command:"migrate" description:"Database schema maintenance."`
}

// MigrateOptions carries the subcommands of `migrate`.
type MigrateOptions struct {
	SchemaInit    SchemaInitOptions `command:"schema-init" description:"Create the 0.8.0 PostgreSQL schema on a fresh database. Strictly non-destructive: existing and legacy databases are refused, never modified."`
	Torrents      TorrentsOptions   `command:"torrents" description:"Import torrents from an original magnetico v0.12.0 PostgreSQL source into the target (offline migration; interruptible and resumable)." subcommands-optional:"true"`
	VerifyDecode  VerifyOptions     `command:"verify-decode" description:"Full target-only decode scan of an imported database: every stored torrent is fetched and decoded, metadata revalidated, orphan blobs reported. The source database is never contacted."`
	VerifyContent VerifyOptions     `command:"verify-content" description:"Full source↔target equality scan of an imported database: every source row is compared against its target counterpart. Read-only on both databases."`

	// Subcommand records the selected migrate subcommand for main's
	// dispatch (the nested repair pass records "retry-errors"); it is never
	// user-settable, and yaml:"-" keeps strict config decoding from
	// accepting a key for it.
	Subcommand string `yaml:"-"`
}

// VerifyOptions carries the shared options of `migrate verify-decode` and
// `migrate verify-content`: the target DSN (same flag/env/file precedence
// as the other migrate commands) and the scan tunables (validated by the
// runners). SourceDatabaseURL has no CLI flag — config file and
// MAGNETICO_MIGRATION_SOURCE_DSN only, so it can never appear in `ps`
// output; verify-content consumes it, verify-decode is target-only and
// never reads it.
type VerifyOptions struct {
	DatabaseURL string `long:"database" description:"URL of the PostgreSQL database to verify." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable" yaml:"migrateDatabaseURL"`

	Workers          uint  `long:"workers" description:"Number of parallel blob decoders (1-64)." default:"4"`
	BatchSize        uint  `long:"batch-size" description:"Maximum target rows per committed page (1-10000)." default:"256"`
	MaxInflightBytes int64 `long:"max-inflight-bytes" description:"Upper bound on in-flight blob bytes." default:"67108864"`

	// SourceDatabaseURL is the read-only v0.12.0 source database (verify-
	// content only). Config file and MAGNETICO_MIGRATION_SOURCE_DSN only —
	// no flag, no default, and it never falls back to the target DSN.
	SourceDatabaseURL string `yaml:"migrateSourceDatabaseURL"`
}

// TorrentsOptions carries the options of `migrate torrents`. The target DSN
// follows the same flag/env/file precedence as `migrate schema-init`. The
// SOURCE DSN deliberately has no CLI flag: it can only come from the config
// file or MAGNETICO_MIGRATION_SOURCE_DSN, so it can never appear in `ps`
// output.
type TorrentsOptions struct {
	DatabaseURL string `long:"database" description:"URL of the PostgreSQL database to import into." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable" yaml:"migrateDatabaseURL"`

	Workers               uint  `long:"workers" description:"Number of parallel record processors (1-64)." default:"4"`
	BatchSize             uint  `long:"batch-size" description:"Maximum processed records (successes and errors alike) per committed batch (1-10000)." default:"256"`
	MaxInflightBytes      int64 `long:"max-inflight-bytes" description:"Upper bound on decoded in-flight record bytes." default:"67108864"`
	DeferSecondaryIndexes bool  `long:"defer-secondary-indexes" description:"Drop the three secondary indexes for the import and rebuild them at finalization (authorized only at the start of an empty run)."`

	// SourceDatabaseURL is the read-only v0.12.0 source database.
	// Config file and MAGNETICO_MIGRATION_SOURCE_DSN only — no flag, no
	// default, and it never falls back to the target DSN.
	SourceDatabaseURL string `yaml:"migrateSourceDatabaseURL"`

	// RetryErrors is the nested `migrate torrents retry-errors` repair pass
	// (optional at the command line; bare `migrate torrents` still imports).
	RetryErrors RetryErrorsOptions `command:"retry-errors" description:"Retry the unresolved record errors of a completed import: re-process each against the source and repair the target transactionally (history-preserving; requires verify-content afterward)."`
}

// RetryErrorsOptions carries the options of `migrate torrents retry-errors`.
// AcceptSourceRepairs is deliberately CLI-only (no config-file key): an
// authorization must be given explicitly per invocation, never inherited
// from a file.
type RetryErrorsOptions struct {
	DatabaseURL string `long:"database" description:"URL of the PostgreSQL database to repair." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable" yaml:"migrateDatabaseURL"`

	Workers          uint  `long:"workers" description:"Number of parallel record processors (1-64)." default:"4"`
	BatchSize        uint  `long:"batch-size" description:"Maximum records attempted per committed batch (1-10000)." default:"256"`
	MaxInflightBytes int64 `long:"max-inflight-bytes" description:"Upper bound on decoded in-flight record bytes." default:"67108864"`

	AcceptSourceRepairs bool `long:"accept-source-repairs" description:"Authorize the repair against a source whose content differs from the recorded import fingerprint (matched by stable evidence only — database name and ID bounds; this is an operator assertion, not proof of identity)."`

	// SourceDatabaseURL is the read-only v0.12.0 source database.
	// Config file and MAGNETICO_MIGRATION_SOURCE_DSN only — no flag, no
	// default, and it never falls back to the target DSN.
	SourceDatabaseURL string `yaml:"migrateSourceDatabaseURL"`
}

// SchemaInitOptions carries the options of `migrate schema-init`. The DSN is
// configurable via CLI flag, environment and config file so that no
// password-bearing URL is forced onto the command line.
type SchemaInitOptions struct {
	DatabaseURL string `long:"database" description:"URL of the PostgreSQL database to initialize." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable" yaml:"migrateDatabaseURL"`
}

// StubOptions carries the options of reserved, not-yet-implemented commands.
type StubOptions struct{}

// CrawlOptions carries the options of the `crawl` subcommand. Flag names,
// defaults and YAML keys are inherited verbatim from upstream so existing
// crawler configuration files keep working.
type CrawlOptions struct {
	DatabaseURL  string `long:"database" description:"URL of the database." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable" yaml:"databaseURL"`
	PyroscopeURL string `long:"pyroscope" description:"URL of the Pyroscope server." default:"" yaml:"pyroscopeURL"`

	IndexerAddrs        []string `long:"indexer-addr" description:"Address(es) to be used by indexing DHT nodes." default:"0.0.0.0:0" yaml:"indexerAddrs"`
	IndexerMaxNeighbors uint     `long:"indexer-max-neighbors" description:"Maximum number of neighbors of an indexer." default:"5000" yaml:"indexerMaxNeighbors"`

	LeechDeadline uint `long:"leech-deadline" description:"Deadline for leeches in seconds." default:"600" yaml:"leechDeadline"`
	LeechMaxN     uint `long:"leech-max-n" description:"Maximum number of leeches." default:"1000" yaml:"leechMaxN"`
	MaxRPS        uint `long:"max-rps" description:"Maximum requests per second." default:"500" yaml:"maxRPS"`

	BootstrappingNodes []string `long:"bootstrap-node" description:"Host(s) to be used for bootstrapping." default:"dht.tgragnato.it:80" default:"dht.tgragnato.it:443" default:"dht.tgragnato.it:1337" default:"dht.tgragnato.it:6969" default:"dht.tgragnato.it:6881" default:"dht.tgragnato.it:25401" yaml:"bootstrappingNodes"`
	FilterNodesCIDRs   []string `long:"filter-nodes-cidrs" description:"List of CIDRs on which Magnetico can operate. Empty is open mode." default:"" yaml:"filterNodesCIDRs"`
	// FilterNodesIpNets is derived by check() from FilterNodesCIDRs and is not
	// configurable; yaml:"-" keeps strict decoding from silently accepting a
	// filterNodesIpNets key that would then be ignored.
	FilterNodesIpNets []net.IPNet `yaml:"-"`
}

// check validates the crawl options and derives FilterNodesIpNets. It is the
// former `RunDaemon` branch of the upstream check(), unchanged.
func (o *CrawlOptions) check() error {
	if err := o.checkAddrs(); err != nil {
		return err
	}

	if o.LeechMaxN > 1000 {
		fmt.Println(
			"Beware that on many systems max # of file descriptors per process is limited to 1024. " +
				"Setting maximum number of leeches greater than 1k might cause \"too many open files\" errors!",
		)
	}

	o.FilterNodesIpNets = []net.IPNet{}
	for _, cidr := range o.FilterNodesCIDRs {
		if cidr == "" {
			continue
		}
		if _, ipnet, err := net.ParseCIDR(cidr); err == nil {
			o.FilterNodesIpNets = append(o.FilterNodesIpNets, *ipnet)
		} else {
			return fmt.Errorf("error while parsing CIDR %s: %s", cidr, err.Error())
		}
	}
	if len(o.FilterNodesIpNets) != 0 && reflect.DeepEqual(o.BootstrappingNodes, []string{"dht.tgragnato.it:80", "dht.tgragnato.it:443", "dht.tgragnato.it:1337", "dht.tgragnato.it:6969", "dht.tgragnato.it:6881", "dht.tgragnato.it:25401"}) {
		return fmt.Errorf("you should specify your own internal bootstrapping nodes in filter mode")
	}

	return nil
}

func (o *CrawlOptions) checkAddrs() error {
	if len(o.IndexerAddrs) == 0 || len(o.IndexerAddrs) == 1 && o.IndexerAddrs[0] == "" {
		return errors.New("IndexerAddrs cannot be empty")
	}

	for _, addr := range o.IndexerAddrs {
		// We are using ResolveUDPAddr but it works equally well for checking TCPAddr(esses) as
		// well.
		_, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			return err
		}
	}
	return nil
}
