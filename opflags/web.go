package opflags

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	webEnvPrefix = "MAGNETICO_WEB_"
	webSMTPPass  = webEnvPrefix + "SMTP_PASSWORD"

	// Bounds shared by validation and its messages.
	webMinPoolConns = 4
	webMaxPoolConns = 200
)

// WebOptions carries the options of the `web` subcommand
// (docs/specs/web-v1.md §2.3). Precedence: CLI > ENV > built-in defaults.
//
// An optional configuration file (--config-file-path /
// MAGNETICO_CONFIG_FILE_PATH, the repository-wide mechanism) may carry a
// `web:` section; precedence is CLI > ENV > file > defaults. The container
// deployment remains ENV-based.
//
// Secrets are environment-only: the SMTP password has no flag and no file
// key, and a password-bearing database URL is refused on the command line
// and in the file.
type WebOptions struct {
	Listen         string `long:"listen" description:"HTTP listen address (plain HTTP; terminate TLS in a reverse proxy)." default:"127.0.0.1:8081"`
	PublicURL      string `long:"public-url" description:"Public base URL of the service: https://, or http:// to a loopback host (development mode). All absolute URLs are built from it, never from the Host header." default:""`
	DatabaseURL    string `long:"database" description:"URL of the PostgreSQL database. A URL containing a password must be given through MAGNETICO_WEB_DATABASE, not here." default:"postgres://magnetico:magnetico@localhost:5432/magnetico?sslmode=disable"`
	PoolMaxConns   int    `long:"pool-max-conns" description:"Maximum PostgreSQL connections of the web service (4-200)." default:"10"`
	TrustedProxies string `long:"trusted-proxies" description:"Comma-separated CIDRs (or addresses) of reverse proxies whose X-Forwarded-For is trusted. Empty: forwarding headers are ignored." default:""`

	SessionIdleTimeout      time.Duration `long:"session-idle-timeout" description:"Session idle timeout (5m or more; not above the absolute lifetime)." default:"12h"`
	SessionAbsoluteLifetime time.Duration `long:"session-absolute-lifetime" description:"Absolute session lifetime (not below the idle timeout; not above the remember lifetime)." default:"168h"`
	SessionRememberLifetime time.Duration `long:"session-remember-lifetime" description:"Absolute lifetime of 'remember me' sessions (not above 8760h)." default:"2160h"`

	SearchTimeout     time.Duration `long:"search-timeout" description:"SQL deadline of a search (1s-15m). Reverse-proxy timeouts must exceed it." default:"60s"`
	SearchConcurrency int           `long:"search-concurrency" description:"Maximum simultaneous searches (1 to pool-max-conns minus 2)." default:"4"`
	RequestTimeout    time.Duration `long:"request-timeout" description:"Deadline of non-search requests (5s-10m)." default:"30s"`
	ShutdownTimeout   time.Duration `long:"shutdown-timeout" description:"Grace period for in-flight requests on SIGINT/SIGTERM (1s-10m)." default:"30s"`

	InviteQuota     int           `long:"invite-quota" description:"Invitations given to a new user (0-10000)." default:"0"`
	TrackerCacheTTL time.Duration `long:"tracker-cache-ttl" description:"Tracker list cache lifetime (5s-1h)." default:"60s"`

	SMTPHost     string `long:"smtp-host" description:"SMTP server host (email features are disabled when unset)." default:""`
	SMTPPort     int    `long:"smtp-port" description:"SMTP server port." default:"0"`
	SMTPSecurity string `long:"smtp-security" description:"SMTP transport security: starttls or tls (no plaintext mode exists)." default:""`
	SMTPUsername string `long:"smtp-username" description:"SMTP username." default:""`
	SMTPFrom     string `long:"smtp-from" description:"Sender address of outgoing email." default:""`

	LogLevel  string `long:"log-level" description:"Log level: debug, info, warn or error." default:"info"`
	LogFormat string `long:"log-format" description:"Log format: text or json." default:"text"`

	// SMTPPassword is MAGNETICO_WEB_SMTP_PASSWORD only: no flag, never printed.
	SMTPPassword string `yaml:"-"`

	// Derived by check(); not configurable.
	PublicBaseURL        *url.URL       `yaml:"-"`
	TrustedProxyPrefixes []netip.Prefix `yaml:"-"`
	// DevMode is true for a loopback http:// public URL (§4.4.3).
	DevMode bool `yaml:"-"`
}

// SMTPConfigured reports whether the email features are configured.
func (o *WebOptions) SMTPConfigured() bool { return o.SMTPHost != "" }

type webEnv struct {
	name string
	flag string // the CLI long name an explicit flag of which wins
	yaml string // key inside the `web:` section of the configuration file
	set  func(o *WebOptions, v string) error
}

func envString(dst func(o *WebOptions) *string) func(*WebOptions, string) error {
	return func(o *WebOptions, v string) error { *dst(o) = v; return nil }
}

func envInt(dst func(o *WebOptions) *int) func(*WebOptions, string) error {
	return func(o *WebOptions, v string) (err error) {
		*dst(o), err = strconv.Atoi(strings.TrimSpace(v))
		return
	}
}

func envDuration(dst func(o *WebOptions) *time.Duration) func(*WebOptions, string) error {
	return func(o *WebOptions, v string) (err error) {
		*dst(o), err = time.ParseDuration(strings.TrimSpace(v))
		return
	}
}

var webEnvs = []webEnv{
	{webEnvPrefix + "LISTEN", "listen", "listen", envString(func(o *WebOptions) *string { return &o.Listen })},
	{webEnvPrefix + "PUBLIC_URL", "public-url", "publicURL", envString(func(o *WebOptions) *string { return &o.PublicURL })},
	{webEnvPrefix + "DATABASE", "database", "databaseURL", envString(func(o *WebOptions) *string { return &o.DatabaseURL })},
	{webEnvPrefix + "POOL_MAX_CONNS", "pool-max-conns", "poolMaxConns", envInt(func(o *WebOptions) *int { return &o.PoolMaxConns })},
	{webEnvPrefix + "TRUSTED_PROXIES", "trusted-proxies", "trustedProxies", envString(func(o *WebOptions) *string { return &o.TrustedProxies })},
	{webEnvPrefix + "SESSION_IDLE_TIMEOUT", "session-idle-timeout", "sessionIdleTimeout", envDuration(func(o *WebOptions) *time.Duration { return &o.SessionIdleTimeout })},
	{webEnvPrefix + "SESSION_ABSOLUTE_LIFETIME", "session-absolute-lifetime", "sessionAbsoluteLifetime", envDuration(func(o *WebOptions) *time.Duration { return &o.SessionAbsoluteLifetime })},
	{webEnvPrefix + "SESSION_REMEMBER_LIFETIME", "session-remember-lifetime", "sessionRememberLifetime", envDuration(func(o *WebOptions) *time.Duration { return &o.SessionRememberLifetime })},
	{webEnvPrefix + "SEARCH_TIMEOUT", "search-timeout", "searchTimeout", envDuration(func(o *WebOptions) *time.Duration { return &o.SearchTimeout })},
	{webEnvPrefix + "SEARCH_CONCURRENCY", "search-concurrency", "searchConcurrency", envInt(func(o *WebOptions) *int { return &o.SearchConcurrency })},
	{webEnvPrefix + "REQUEST_TIMEOUT", "request-timeout", "requestTimeout", envDuration(func(o *WebOptions) *time.Duration { return &o.RequestTimeout })},
	{webEnvPrefix + "SHUTDOWN_TIMEOUT", "shutdown-timeout", "shutdownTimeout", envDuration(func(o *WebOptions) *time.Duration { return &o.ShutdownTimeout })},
	{webEnvPrefix + "INVITE_QUOTA", "invite-quota", "inviteQuota", envInt(func(o *WebOptions) *int { return &o.InviteQuota })},
	{webEnvPrefix + "TRACKER_CACHE_TTL", "tracker-cache-ttl", "trackerCacheTTL", envDuration(func(o *WebOptions) *time.Duration { return &o.TrackerCacheTTL })},
	{webEnvPrefix + "SMTP_HOST", "smtp-host", "smtpHost", envString(func(o *WebOptions) *string { return &o.SMTPHost })},
	{webEnvPrefix + "SMTP_PORT", "smtp-port", "smtpPort", envInt(func(o *WebOptions) *int { return &o.SMTPPort })},
	{webEnvPrefix + "SMTP_SECURITY", "smtp-security", "smtpSecurity", envString(func(o *WebOptions) *string { return &o.SMTPSecurity })},
	{webEnvPrefix + "SMTP_USERNAME", "smtp-username", "smtpUsername", envString(func(o *WebOptions) *string { return &o.SMTPUsername })},
	{webEnvPrefix + "SMTP_FROM", "smtp-from", "smtpFrom", envString(func(o *WebOptions) *string { return &o.SMTPFrom })},
	{webEnvPrefix + "LOG_LEVEL", "log-level", "logLevel", envString(func(o *WebOptions) *string { return &o.LogLevel })},
	{webEnvPrefix + "LOG_FORMAT", "log-format", "logFormat", envString(func(o *WebOptions) *string { return &o.LogFormat })},
}

// applyEnvWebConfig applies MAGNETICO_WEB_* variables (an explicit flag wins;
// empty means unset), reads the ENV-only SMTP password and rejects unknown
// variables. Values are never echoed in errors.
func (o *OpFlags) applyEnvWebConfig(explicit map[string]bool) error {
	known := map[string]bool{webSMTPPass: true}
	names := []string{webSMTPPass}
	for _, e := range webEnvs {
		known[e.name] = true
		names = append(names, e.name)
		if explicit[e.flag] {
			continue
		}
		if value, ok := os.LookupEnv(e.name); ok && value != "" {
			if err := e.set(&o.Web, value); err != nil {
				return fmt.Errorf("%s: invalid value", e.name)
			}
		}
	}
	o.Web.SMTPPassword = os.Getenv(webSMTPPass)

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, webEnvPrefix) && !known[name] {
			return fmt.Errorf("unknown environment variable %s; known variables: MAGNETICO_CONFIG_FILE_PATH (refused for web), %s",
				name, strings.Join(names, ", "))
		}
	}
	return nil
}

// webYamlSection is the strict key set of the `web:` section of the
// configuration file. There is deliberately no key for the SMTP password
// (environment only), and a password-bearing databaseURL is refused.
//
// mergeWebYaml applies the present keys with CLI-explicit flags winning;
// environment variables are applied afterwards and win over the file
// (CLI > ENV > YAML > defaults).
func (o *OpFlags) mergeWebYaml(section any, explicit map[string]bool) error {
	if section == nil {
		return nil
	}
	m, ok := section.(map[string]any)
	if !ok {
		return errors.New("config file: web must be a mapping")
	}
	byKey := make(map[string]webEnv, len(webEnvs))
	for _, e := range webEnvs {
		byKey[e.yaml] = e
	}
	for key, raw := range m {
		e, known := byKey[key]
		if !known {
			return fmt.Errorf("config file: unknown key web.%s", key)
		}
		var v string
		switch t := raw.(type) {
		case string:
			v = t
		case int, int64, uint64:
			v = fmt.Sprint(t)
		case []any:
			if key != "trustedProxies" {
				return fmt.Errorf("config file: web.%s must be a scalar", key)
			}
			parts := make([]string, 0, len(t))
			for _, it := range t {
				str, ok := it.(string)
				if !ok {
					return fmt.Errorf("config file: web.%s must be a list of strings", key)
				}
				parts = append(parts, str)
			}
			v = strings.Join(parts, ",")
		default:
			return fmt.Errorf("config file: web.%s has an unsupported value type", key)
		}
		if key == "databaseURL" && urlCarriesPassword(v) {
			return errors.New("config file: web.databaseURL must not contain a password; use MAGNETICO_WEB_DATABASE")
		}
		if explicit[e.flag] || v == "" {
			continue
		}
		if err := e.set(&o.Web, v); err != nil {
			return fmt.Errorf("config file: web.%s: invalid value", key)
		}
	}
	return nil
}

func urlCarriesPassword(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			return true
		}
	}
	return u.Query().Has("password")
}

// check validates the web options and derives PublicBaseURL, DevMode and
// TrustedProxyPrefixes. explicitDatabase tells whether --database was given
// on the command line. Error text names the option, never its value.
func (o *WebOptions) check(explicitDatabase bool) error {
	if err := o.checkPublicAndListen(); err != nil {
		return err
	}
	if err := o.checkDatabase(explicitDatabase); err != nil {
		return err
	}
	if err := o.checkLimits(); err != nil {
		return err
	}
	if err := o.checkSMTP(); err != nil {
		return err
	}
	switch o.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return errors.New("log-level must be one of: debug, info, warn, error")
	}
	switch o.LogFormat {
	case "text", "json":
	default:
		return errors.New("log-format must be text or json")
	}
	prefixes, err := parseTrustedProxies(o.TrustedProxies)
	if err != nil {
		return err
	}
	o.TrustedProxyPrefixes = prefixes
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func (o *WebOptions) checkPublicAndListen() error {
	const publicHint = "the public URL (--public-url / MAGNETICO_WEB_PUBLIC_URL) is required and must be an absolute https:// URL (http:// only for a loopback host, development mode), without credentials, query or fragment"
	if strings.TrimSpace(o.PublicURL) == "" {
		return errors.New(publicHint)
	}
	u, err := url.Parse(o.PublicURL)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New(publicHint)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return errors.New(publicHint)
		}
		o.DevMode = true
	default:
		return errors.New(publicHint)
	}
	o.PublicBaseURL = &url.URL{Scheme: u.Scheme, Host: u.Host}

	host, port, err := net.SplitHostPort(o.Listen)
	if err != nil {
		return errors.New("listen must be a host:port address (--listen / MAGNETICO_WEB_LISTEN)")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return errors.New("listen must carry a numeric port from 0 to 65535")
	}
	if o.DevMode && !isLoopbackHost(host) {
		return errors.New("development mode (http:// public URL) requires a loopback listen address: its relaxed cookie rules must never be exposed to a network")
	}
	return nil
}

func (o *WebOptions) checkDatabase(explicit bool) error {
	u, err := url.Parse(o.DatabaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return errors.New("the database must be a postgres:// or postgresql:// URL (--database / MAGNETICO_WEB_DATABASE)")
	}
	if explicit && urlCarriesPassword(o.DatabaseURL) {
		return errors.New("a database URL containing a password must not be given with --database (it would be visible in process listings): use MAGNETICO_WEB_DATABASE")
	}
	return nil
}

func within(d, lo, hi time.Duration) bool { return d >= lo && d <= hi }

func (o *WebOptions) checkLimits() error {
	if o.PoolMaxConns < webMinPoolConns || o.PoolMaxConns > webMaxPoolConns {
		return fmt.Errorf("pool-max-conns must be between %d and %d", webMinPoolConns, webMaxPoolConns)
	}
	if o.SearchConcurrency < 1 || o.SearchConcurrency > o.PoolMaxConns-2 {
		return errors.New("search-concurrency must be between 1 and pool-max-conns minus 2")
	}
	if !within(o.SessionIdleTimeout, 5*time.Minute, o.SessionAbsoluteLifetime) {
		return errors.New("session-idle-timeout must be at least 5m and not above session-absolute-lifetime")
	}
	if !within(o.SessionAbsoluteLifetime, o.SessionIdleTimeout, o.SessionRememberLifetime) {
		return errors.New("session-absolute-lifetime must be at least session-idle-timeout and not above session-remember-lifetime")
	}
	if !within(o.SessionRememberLifetime, o.SessionAbsoluteLifetime, 8760*time.Hour) {
		return errors.New("session-remember-lifetime must be at least session-absolute-lifetime and not above 8760h")
	}
	if !within(o.SearchTimeout, time.Second, 15*time.Minute) {
		return errors.New("search-timeout must be between 1s and 15m")
	}
	if !within(o.RequestTimeout, 5*time.Second, 10*time.Minute) {
		return errors.New("request-timeout must be between 5s and 10m")
	}
	if !within(o.ShutdownTimeout, time.Second, 10*time.Minute) {
		return errors.New("shutdown-timeout must be between 1s and 10m")
	}
	if o.InviteQuota < 0 || o.InviteQuota > 10000 {
		return errors.New("invite-quota must be between 0 and 10000")
	}
	if !within(o.TrackerCacheTTL, 5*time.Second, time.Hour) {
		return errors.New("tracker-cache-ttl must be between 5s and 1h")
	}
	return nil
}

// checkSMTP validates the SMTP settings only (no delivery in W1): email is
// either wholly disabled or fully configured, with no plaintext transport.
func (o *WebOptions) checkSMTP() error {
	any := o.SMTPHost != "" || o.SMTPPort != 0 || o.SMTPSecurity != "" ||
		o.SMTPUsername != "" || o.SMTPFrom != "" || o.SMTPPassword != ""
	if !any {
		return nil
	}
	if o.SMTPHost == "" || o.SMTPPort == 0 || o.SMTPSecurity == "" || o.SMTPFrom == "" {
		return errors.New("SMTP is partially configured: smtp-host, smtp-port, smtp-security and smtp-from are all required together (the password is MAGNETICO_WEB_SMTP_PASSWORD)")
	}
	if o.SMTPPort < 1 || o.SMTPPort > 65535 {
		return errors.New("smtp-port must be between 1 and 65535")
	}
	if o.SMTPSecurity != "starttls" && o.SMTPSecurity != "tls" {
		return errors.New("smtp-security must be starttls or tls (there is no plaintext mode)")
	}
	if (o.SMTPUsername == "") != (o.SMTPPassword == "") {
		return errors.New("smtp-username and MAGNETICO_WEB_SMTP_PASSWORD must be given together")
	}
	if a, err := mail.ParseAddress(o.SMTPFrom); err != nil || a.Address == "" {
		return errors.New("smtp-from must be a valid email address")
	}
	return nil
}

// parseTrustedProxies parses a comma-separated list of CIDRs or bare
// addresses. Prefixes are masked; IPv4-mapped IPv6 forms are normalized.
func parseTrustedProxies(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(item, "/") {
			var err error
			if p, err = netip.ParsePrefix(item); err != nil {
				return nil, errors.New("trusted-proxies: an entry is not a valid CIDR")
			}
		} else {
			a, err := netip.ParseAddr(item)
			if err != nil {
				return nil, errors.New("trusted-proxies: an entry is not a valid address or CIDR")
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Addr().Is4In6() {
			// ::ffff:a.b.c.d/N maps onto IPv4 N-96.
			if p.Bits() < 96 {
				return nil, errors.New("trusted-proxies: an IPv4-mapped entry must have a prefix length of at least 96")
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
