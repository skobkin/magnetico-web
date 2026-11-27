package opflags

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"strings"

	"github.com/skobkin/magnetico-web/internal/netpolicy"
)

// OpFlags is the root of the command line structure of the single `magnetico`
// binary: one global option plus subcommands.
type OpFlags struct {
	// ConfigFilePath must be given before the subcommand name, e.g.
	// `magnetico --config-file-path config.yml crawl`.
	ConfigFilePath string `long:"config-file-path" description:"Configuration YAML file path. If not filled in, it will default to disabled." default:""`

	Crawl   CrawlOptions   `command:"crawl" description:"Run the DHT crawler: precheck, fetch metadata, queue it durably on local disk and deliver it to the ingest service over authenticated HTTP." subcommands-optional:"true"`
	Ingest  IngestOptions  `command:"ingest" description:"Run the authenticated HTTP ingest service (existence lookup and durable record admission into the PostgreSQL queue, plus the single queue consumer). Subcommand crawler is the operator CLI for crawler credentials." subcommands-optional:"true"`
	Web     WebOptions     `command:"web" description:"Run the authenticated web UI service: torrent search and details, the file browser, accounts and invitations, the v1 legacy RSS feed and the v2 API. Subcommands user and tracker are the operator CLI." subcommands-optional:"true"`
	Migrate MigrateOptions `command:"migrate" description:"Database schema maintenance."`
	Dev     DevOptions     `command:"dev" description:"Development helpers. NOT for production databases: seed-torrents writes synthetic torrents into an initialized 0.8.0 database." subcommands-optional:"true"`
}

// MigrateOptions carries the subcommands of `migrate`.
type MigrateOptions struct {
	SchemaInit       SchemaInitOptions `command:"schema-init" description:"Create the 0.8.0 PostgreSQL schema on a fresh database. Strictly non-destructive: existing and legacy databases are refused, never modified."`
	StatsSchemaInit  SchemaInitOptions `command:"stats-schema-init" description:"Create the optional magnetico_stats schema (ingestion statistics: hourly, daily and monthly aggregates, instance snapshots, queue and storage state; requires a verified 0.8.0 schema). Non-destructive and idempotent; an existing schema that is not exactly the supported version and structure is refused and left untouched. ingest and web start without it."`
	WebSchemaInit    SchemaInitOptions `command:"web-schema-init" description:"Create the magnetico_web schema (accounts, sessions, invitations, API and RSS tokens, one-time links, trackers, settings) and seed the approved tracker list once (requires a verified 0.8.0 schema; run before starting the web service). Non-destructive and idempotent; an existing schema that is not exactly the supported version is refused and left untouched."`
	IngestSchemaInit SchemaInitOptions `command:"ingest-schema-init" description:"Create the magnetico_ingest queue schema (requires a verified 0.8.0 schema; run after the import and before starting ingest). Non-destructive and idempotent."`
	Torrents         TorrentsOptions   `command:"torrents" description:"Import torrents from an original magnetico v0.12.0 PostgreSQL source into the target (offline migration; interruptible and resumable)." subcommands-optional:"true"`
	VerifyDecode     VerifyOptions     `command:"verify-decode" description:"Full target-only decode scan of an imported database: every stored torrent is fetched and decoded, metadata revalidated, orphan blobs reported. The source database is never contacted."`
	VerifyContent    VerifyOptions     `command:"verify-content" description:"Full source↔target equality scan of an imported database: every source row is compared against its target counterpart. Read-only on both databases."`
	Users            UsersOptions      `command:"users" description:"Import the legacy PHP accounts (users, invitations, API tokens → RSS tokens) into the magnetico_web schema in ONE transaction, identity-preserving. --dry-run is read-only on both databases. A final run is an offline operation (see docs/web-deployment.md §10); nested: verify." subcommands-optional:"true"`
	Errors           ErrorsOptions     `command:"errors" description:"Forensic access to the migration error/verification registry."`
	Cleanup          CleanupOptions    `command:"cleanup" description:"Gated, fail-closed teardown of the magnetico_migration operational schema. The default is a non-destructive gate-by-gate preview; --execute drops the schema in one transaction after every gate passes."`

	// Subcommand records the selected migrate subcommand for main's
	// dispatch (the nested repair pass records "retry-errors"; the nested
	// forensic export records "export"); it is never user-settable, and
	// yaml:"-" keeps strict config decoding from accepting a key for it.
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

	Workers          uint  `long:"workers" description:"verify-decode: number of parallel blob decoders (1-64). verify-content: accepted but has no effect — the content scan decodes strictly sequentially to hold its decoded-content memory bound." default:"4"`
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

	Workers          uint  `long:"workers" description:"Accepted for symmetry with the import; repair attempts settle strictly sequentially (one transaction per record), so this value provides no concurrency (1-64)." default:"4"`
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

// ErrorsOptions carries the `migrate errors` command group. Export is its
// only (required) subcommand: bare `migrate errors` is a usage error.
// DatabaseURL exists so an operator-positioned shared flag
// (`migrate errors --database X export`) parses and transfers, exactly like
// the retry-errors precedent.
type ErrorsOptions struct {
	DatabaseURL string `long:"database" description:"URL of the PostgreSQL database whose migration state is exported." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable" yaml:"migrateDatabaseURL"`

	Export ErrorsExportOptions `command:"export" description:"Forensic, fail-open JSONL export of the migration operational state (run history, errors, verification and retry collections; full record blocks with --include-records). Read-only unless records are requested; the source database is contacted only then."`
}

// ErrorsExportOptions carries the options of `migrate errors export`. The
// four Max* caps are safety limits: a breach is a hard error naming the cap,
// never a truncation. SourceDatabaseURL has no CLI flag — config file and
// MAGNETICO_MIGRATION_SOURCE_DSN only, consumed only with --include-records.
type ErrorsExportOptions struct {
	DatabaseURL string `long:"database" description:"URL of the PostgreSQL database whose migration state is exported." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable" yaml:"migrateDatabaseURL"`

	Output         string `long:"output" description:"Output path of the JSONL export ('-' writes the metadata-only export to stdout; record expansion requires a file)." default:"-"`
	Manifest       bool   `long:"manifest" description:"Write <output>.manifest.json (SHA-256, event counts, run binding, registry-state digests) after a successful publish — the artifact cleanup validates as evidence."`
	OnlyUnresolved bool   `long:"only-unresolved" description:"Export only unresolved import errors in the error collection (makes the export partial — never usable as cleanup evidence)."`
	VerificationID string `long:"verification-id" description:"Filter the verification collections to this verification's ID (makes the export partial — never usable as cleanup evidence)."`
	IncludeRecords bool   `long:"include-records" description:"Expand every selected source ID with its full source and target record blocks (contacts the source database read-only)."`
	IncludeBlobs   bool   `long:"include-blobs" description:"Include each stored blob's exact bytes as ordered Base64 chunks (requires --include-records)."`

	MaxEvents      int64 `long:"max-events" description:"Safety cap: total emitted JSONL events, blob chunks included." default:"10000000"`
	MaxRecords     int64 `long:"max-records" description:"Safety cap: expanded source-ID record snapshots." default:"1000000"`
	MaxOutputBytes int64 `long:"max-output-bytes" description:"Safety cap: total JSONL output bytes." default:"8589934592"`
	MaxBlobBytes   int64 `long:"max-blob-bytes" description:"Safety cap: raw bytes per exported blob; a larger blob fails the export (raise it or drop --include-blobs)." default:"67108864"`

	// SourceDatabaseURL is the read-only v0.12.0 source database, consumed
	// only with --include-records. Config file and MAGNETICO_MIGRATION_SOURCE_DSN
	// only — no flag, no default, and it never falls back to the target DSN.
	SourceDatabaseURL string `yaml:"migrateSourceDatabaseURL"`
}

// CleanupOptions carries the options of `migrate cleanup`. The authorization
// flags (ConfirmBackup, ConfirmExport, ForceUnverified, Reason,
// AllowMissingExportEvidence, ConfirmHistoryLoss) are deliberately CLI-only
// (no config-file keys): authorizations must be given explicitly per
// invocation, never inherited from a file.
type CleanupOptions struct {
	DatabaseURL string `long:"database" description:"URL of the PostgreSQL database to clean up." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable" yaml:"migrateDatabaseURL"`

	DryRun  bool `long:"dry-run" description:"Explicit non-destructive preview (the default without --execute): every gate is evaluated and reported, nothing is dropped."`
	Execute bool `long:"execute" description:"Run the gated teardown (the single confirmation): the magnetico_migration schema is dropped in one transaction after every gate passes on the transaction's own snapshot."`

	ConfirmBackup    bool   `long:"confirm-backup" description:"Attest that an independent backup of the target database exists (the drop irreversibly destroys the migration history)."`
	ConfirmExport    bool   `long:"confirm-export" description:"Attest that a COMPLETE metadata export of this run, with a validated SHA-256 manifest, is independently retained (the normal evidence path; paired with --evidence-manifest)."`
	EvidenceManifest string `long:"evidence-manifest" description:"Path of the retained export's manifest file, validated (structure, artifact hash, run binding, registry-state digests) before anything is dropped."`
	ForceUnverified  bool   `long:"force-unverified" description:"Waive ORDINARY gate failures (stale or failed verification verdicts, unresolved findings). It never waives corrupt state, reconciliation failures or missing attestations."`
	Reason           string `long:"reason" description:"Mandatory audit reason when --force-unverified is used (1-512 bytes, recorded on the result)."`
	// AllowMissingExportEvidence + ConfirmHistoryLoss form the exceptional,
	// path-exclusive evidence attestation: NO complete export exists and all
	// migration history will be irrecoverably destroyed. Requires
	// --force-unverified --reason; mutually exclusive with --confirm-export /
	// --evidence-manifest.
	AllowMissingExportEvidence bool `long:"allow-missing-export-evidence" description:"Exceptional evidence path: no complete export evidence exists (must be paired with --confirm-history-loss and --force-unverified --reason)."`
	ConfirmHistoryLoss         bool `long:"confirm-history-loss" description:"Attest that NO complete export evidence exists and ALL migration history will be irrecoverably destroyed (paired with --allow-missing-export-evidence)."`
}

// StubOptions carries the options of reserved, not-yet-implemented commands.
type StubOptions struct{}

// CrawlOptions carries the options of the `crawl` subcommand. Flag names,
// defaults and YAML keys are inherited verbatim from upstream so existing
// crawler configuration files keep working.
type CrawlOptions struct {
	// IngestURL is the base URL of the ingest service (https://, or http://
	// to a loopback host for local runs). IngestToken is ENV-only
	// (MAGNETICO_CRAWL_INGEST_TOKEN): no flag, no YAML key, never logged.
	IngestURL      string `long:"ingest-url" description:"Base URL of the ingest service (https://; plain http:// only for a loopback host)." default:"" yaml:"ingestURL"`
	QueueDir       string `long:"queue-dir" description:"Directory of the durable local queue (SQLite WAL; must be on a local filesystem that honours fsync)." default:"magnetico-crawl-queue" yaml:"queueDir"`
	QueueMaxBytes  int64  `long:"queue-max-bytes" description:"Limit of queue + quarantine payload bytes on local disk." default:"1073741824" yaml:"queueMaxBytes"`
	LeechMaxActive uint   `long:"leech-max-active" description:"Maximum number of simultaneously active leeches." default:"32" yaml:"leechMaxActive"`
	IngestToken    string `yaml:"-"`

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

	// DenyCIDRs lists extra destinations the crawler never contacts (DHT or
	// BitTorrent); AllowNonGlobalAddrs disables the built-in list of
	// non-globally-reachable ranges (internal/netpolicy). DenyPrefixes is
	// derived by check() from DenyCIDRs.
	DenyCIDRs           []string       `long:"deny-cidrs" description:"Extra CIDRs the crawler never sends DHT or BitTorrent traffic to (always refused, also in filter mode)." default:"" yaml:"denyCIDRs"`
	AllowNonGlobalAddrs bool           `long:"allow-non-global-addrs" description:"Also contact private, reserved and other non-globally-reachable addresses (lab use only: on a public host this traffic is reported as port scanning)." yaml:"allowNonGlobalAddrs"`
	DenyPrefixes        []netip.Prefix `yaml:"-"`

	// CheckQueue is the diagnostic `crawl check-queue`; Subcommand records
	// which subcommand (if any) was selected.
	CheckQueue CheckQueueOptions `command:"check-queue" description:"Run SQLite integrity diagnostics on the local queue directory and print its counts (read-only; run it while the crawler is stopped)." yaml:"-"`
	Subcommand string            `yaml:"-"`
}

// CheckQueueOptions carries the options of `crawl check-queue`.
type CheckQueueOptions struct {
	QueueDir string `long:"queue-dir" description:"Directory of the durable local queue." default:"magnetico-crawl-queue"`
	Full     bool   `long:"full" description:"Run the full integrity_check instead of quick_check."`
}

// check validates the crawl options and derives FilterNodesIpNets. It is the
// former `RunDaemon` branch of the upstream check(), unchanged.
func (o *CrawlOptions) check() error {
	if err := o.checkDHT(); err != nil {
		return err
	}
	return o.checkDelivery()
}

// checkDelivery validates the ingest delivery options.
func (o *CrawlOptions) checkDelivery() error {
	if _, err := validateIngestURL(o.IngestURL); err != nil {
		return err
	}
	if len(o.IngestToken) < MinIngestTokenBytes {
		return fmt.Errorf("MAGNETICO_CRAWL_INGEST_TOKEN is required and must be at least %d bytes long (it is read from the environment only)",
			MinIngestTokenBytes)
	}
	if strings.TrimSpace(o.QueueDir) == "" {
		return errors.New("the queue directory cannot be empty")
	}
	if o.QueueMaxBytes <= 0 {
		return errors.New("queue-max-bytes must be positive")
	}
	if o.LeechMaxActive == 0 || o.LeechMaxActive > 4096 {
		return errors.New("leech-max-active must be between 1 and 4096")
	}

	return nil
}

// checkDHT validates the DHT options and derives FilterNodesIpNets.
func (o *CrawlOptions) checkDHT() error {
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
	deny, err := netpolicy.ParsePrefixes(o.DenyCIDRs)
	if err != nil {
		return fmt.Errorf("deny-cidrs: %w", err)
	}
	o.DenyPrefixes = deny
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

// validateIngestURL mirrors the crawler client's rule without importing it:
// https, or plain http to a loopback host.
func validateIngestURL(raw string) (*url.URL, error) {
	bad := errors.New("the ingest URL (--ingest-url / MAGNETICO_CRAWL_INGEST_URL) must be an https:// URL (plain http:// is allowed only for a loopback host)")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return nil, bad
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return u, nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return u, nil
		}
	}
	return nil, bad
}

// UsersOptions carries the options of `migrate users` (docs/wip/07H-user-
// migration-plan.md §2). The target DSN follows the same flag/env/file
// precedence as the other migrate commands. The SOURCE DSN deliberately has
// no CLI flag: it can only come from the config file key
// migrateUsersSourceDatabaseURL or MAGNETICO_MIGRATION_USERS_SOURCE_DSN, so it
// can never appear in `ps` output, and it never falls back to the target (or
// to the torrent source) DSN.
type UsersOptions struct {
	DatabaseURL string `long:"database" description:"URL of the PostgreSQL database holding the magnetico_web schema to import into." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable" yaml:"migrateDatabaseURL"`

	SourceTimezone         string `long:"source-timezone" description:"IANA time zone of the legacy PHP application (the legacy timestamps carry no zone; required, no default; confirm it with php -r 'echo date_default_timezone_get();')."`
	DryRun                 bool   `long:"dry-run" description:"Read-only preflight on both databases: validates everything, prints the plan digest, writes nothing (sequence state included). Allowed during normal operation."`
	AcceptNeedsReset       int    `long:"accept-needs-reset" description:"Acknowledge, by exact number, the accounts whose legacy password hash format is unsupported (they cannot log in until an administrator resets them)." default:"0"`
	ReplaceUsers           bool   `long:"replace-users" description:"DESTRUCTIVE: delete every user account and all user-owned data in the target (sessions, one-time links, API and RSS tokens, invitations, users) and import again, in the same transaction. Never touches torrents, trackers, settings or schema versions. A real run requires --confirm-plan."`
	ConfirmPlan            string `long:"confirm-plan" description:"The plan digest printed by a previous --dry-run with identical options (required for a real --replace-users run; any source, target, timezone or option change invalidates it)."`
	DeleteUnmatchedUserIDs string `long:"delete-unmatched-user-ids" description:"With --replace-users: comma-separated ids of target accounts that are not in the source and may be deleted; must equal the unmatched set exactly."`
	SourceWritesStopped    bool   `long:"source-writes-stopped" description:"Operator assertion that the legacy application is stopped and legacy writes are prevented. Required for a final run (not for --dry-run)."`
	ReportJSON             string `long:"report-json" description:"Write the secret-free JSON report here (mode 0600, created exclusively; an existing file is never overwritten)."`

	// SourceDatabaseURL is the read-only legacy PHP database (schema "users").
	SourceDatabaseURL string `yaml:"migrateUsersSourceDatabaseURL"`

	Verify UsersVerifyOptions `command:"verify" description:"Read-only source↔target reconciliation of a completed import (exit status 0 only on an exact match)."`
}

// UsersVerifyOptions carries the options of `migrate users verify`.
type UsersVerifyOptions struct {
	DatabaseURL string `long:"database" description:"URL of the PostgreSQL database holding the magnetico_web schema to verify." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable" yaml:"migrateDatabaseURL"`

	SourceTimezone string `long:"source-timezone" description:"IANA time zone of the legacy PHP application (the same value the import used)."`
	ReportJSON     string `long:"report-json" description:"Write the secret-free JSON report here (mode 0600, created exclusively)."`

	// SourceDatabaseURL is the read-only legacy PHP database (schema "users").
	SourceDatabaseURL string `yaml:"migrateUsersSourceDatabaseURL"`
}

// DevOptions carries the `dev` command group. SeedTorrents is its only
// subcommand; bare `dev` is a usage error.
type DevOptions struct {
	SeedTorrents SeedTorrentsOptions `command:"seed-torrents" description:"Add the deterministic synthetic torrent dataset (101 torrents, one with 10,000 files) to an initialized 0.8.0 database, through the production persistence API. Refuses any database that holds other torrents; never updates or deletes anything; PostgreSQL access only: no DHT, BitTorrent, tracker or other unrelated external network activity (it connects only to the PostgreSQL server you name). Development only (docs/development-fixtures.md)."`
}

// DevDatabaseEnvName is the environment variable of the dev target DSN.
const DevDatabaseEnvName = "MAGNETICO_DEV_DATABASE"

// SeedTorrentsOptions carries the options of `dev seed-torrents`. The target
// is deliberately NOT inherited from the migrate settings and has no default:
// it must be given explicitly (flag or MAGNETICO_DEV_DATABASE), and the
// confirmation flag is CLI-only, so a configuration file or an environment
// can never turn a production target into a seed target by accident.
type SeedTorrentsOptions struct {
	DatabaseURL string `long:"database" description:"URL of the DISPOSABLE development PostgreSQL database (required, no default; or MAGNETICO_DEV_DATABASE)."`
	DryRun      bool   `long:"dry-run" description:"Run every check and report what would be written; write nothing."`
	Confirm     bool   `long:"confirm-development-database" description:"Attest that the target is a disposable development database (required unless --dry-run)."`
}

// check validates the seed options after the DSN layers were applied.
func (o *SeedTorrentsOptions) check() error {
	if strings.TrimSpace(o.DatabaseURL) == "" {
		return errors.New("dev seed-torrents: a target database is required (--database or " + DevDatabaseEnvName + "); there is no default")
	}
	if !o.DryRun && !o.Confirm {
		return errors.New("dev seed-torrents: refusing to write without --confirm-development-database (use --dry-run to preview)")
	}
	return nil
}
