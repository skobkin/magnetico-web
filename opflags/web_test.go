package opflags

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const webPublic = "https://magnet.example.org"

// webEnvClean removes every MAGNETICO_WEB_* variable of the test process.
func webEnvClean(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "MAGNETICO_WEB_") || name == "MAGNETICO_CONFIG_FILE_PATH" {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
}

func parseWeb(t *testing.T, args ...string) (*OpFlags, error) {
	t.Helper()
	o := &OpFlags{}
	cmd, err := o.parseArgs(append([]string{"web"}, args...))
	if err == nil && cmd != "web" {
		t.Fatalf("command = %q", cmd)
	}
	return o, err
}

func TestWebDefaults(t *testing.T) {
	webEnvClean(t)
	o, err := parseWeb(t, "--public-url", webPublic)
	if err != nil {
		t.Fatal(err)
	}
	w := o.Web
	if w.Listen != "127.0.0.1:8081" || w.PoolMaxConns != 10 || w.SearchConcurrency != 4 ||
		w.SearchTimeout != 60*time.Second || w.RequestTimeout != 30*time.Second ||
		w.SessionIdleTimeout != 12*time.Hour || w.SessionAbsoluteLifetime != 168*time.Hour ||
		w.SessionRememberLifetime != 2160*time.Hour || w.TrackerCacheTTL != 60*time.Second ||
		w.InviteQuota != 0 || w.LogLevel != "info" || w.LogFormat != "text" || w.ShutdownTimeout != 30*time.Second {
		t.Fatalf("defaults = %+v", w)
	}
	if w.DevMode || w.PublicBaseURL.String() != webPublic || len(w.TrustedProxyPrefixes) != 0 || w.SMTPConfigured() {
		t.Fatalf("derived = %+v", w)
	}
}

func TestWebPrecedenceCLIOverEnvOverDefault(t *testing.T) {
	webEnvClean(t)
	t.Setenv("MAGNETICO_WEB_PUBLIC_URL", webPublic)
	t.Setenv("MAGNETICO_WEB_LISTEN", "0.0.0.0:9000")
	t.Setenv("MAGNETICO_WEB_SEARCH_TIMEOUT", "90s")
	t.Setenv("MAGNETICO_WEB_REQUEST_TIMEOUT", "45s")
	t.Setenv("MAGNETICO_WEB_POOL_MAX_CONNS", "20")
	o, err := parseWeb(t, "--request-timeout", "20s")
	if err != nil {
		t.Fatal(err)
	}
	w := o.Web
	if w.Listen != "0.0.0.0:9000" || w.SearchTimeout != 90*time.Second || w.PoolMaxConns != 20 {
		t.Fatalf("env not applied: %+v", w)
	}
	if w.RequestTimeout != 20*time.Second {
		t.Fatalf("CLI must beat ENV, got %v", w.RequestTimeout)
	}
}

func TestWebEmptyEnvMeansUnset(t *testing.T) {
	webEnvClean(t)
	t.Setenv("MAGNETICO_WEB_PUBLIC_URL", webPublic)
	t.Setenv("MAGNETICO_WEB_LISTEN", "")
	o, err := parseWeb(t)
	if err != nil || o.Web.Listen != "127.0.0.1:8081" {
		t.Fatalf("%v %+v", err, o.Web)
	}
}

func TestWebRejectsUnknownEnvAndBadValues(t *testing.T) {
	webEnvClean(t)
	t.Setenv("MAGNETICO_WEB_PUBLIC_URL", webPublic)
	t.Setenv("MAGNETICO_WEB_BOGUS", "secret-ish-value")
	_, err := parseWeb(t)
	if err == nil || !strings.Contains(err.Error(), "MAGNETICO_WEB_BOGUS") || strings.Contains(err.Error(), "secret-ish-value") {
		t.Fatalf("unknown env: %v", err)
	}
	os.Unsetenv("MAGNETICO_WEB_BOGUS")

	t.Setenv("MAGNETICO_WEB_SEARCH_TIMEOUT", "90 parsecs")
	_, err = parseWeb(t)
	if err == nil || !strings.Contains(err.Error(), "MAGNETICO_WEB_SEARCH_TIMEOUT") || strings.Contains(err.Error(), "parsecs") {
		t.Fatalf("bad duration must name the variable, not echo the value: %v", err)
	}
}

func TestWebConfigFileIsRefusedNotIgnored(t *testing.T) {
	webEnvClean(t)
	file := filepath.Join(t.TempDir(), "c.yml")
	if err := os.WriteFile(file, []byte("queueDir: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// CLI form (global option before the subcommand).
	o := &OpFlags{}
	_, err := o.parseArgs([]string{"--config-file-path", file, "web", "--public-url", webPublic})
	if err == nil || !strings.Contains(err.Error(), "does not read a configuration file") {
		t.Fatalf("CLI config file: %v", err)
	}
	// Environment form.
	t.Setenv("MAGNETICO_CONFIG_FILE_PATH", file)
	if _, err = parseWeb(t, "--public-url", webPublic); err == nil || !strings.Contains(err.Error(), "does not read a configuration file") {
		t.Fatalf("ENV config file: %v", err)
	}
}

func TestWebPublicURLRules(t *testing.T) {
	webEnvClean(t)
	bad := []string{
		"", "magnet.example.org", "ftp://magnet.example.org", "http://magnet.example.org",
		"https://user:pw@magnet.example.org", "https://magnet.example.org/sub",
		"https://magnet.example.org/?q=1", "https://magnet.example.org/#f", "https://",
	}
	for _, u := range bad {
		if _, err := parseWeb(t, "--public-url", u); err == nil {
			t.Errorf("public url %q must be rejected", u)
		} else if strings.Contains(err.Error(), "pw@") {
			t.Errorf("error echoes URL credentials: %v", err)
		}
	}
	for _, u := range []string{"https://magnet.example.org", "https://magnet.example.org/", "https://magnet.example.org:8443"} {
		o, err := parseWeb(t, "--public-url", u)
		if err != nil || o.Web.DevMode {
			t.Errorf("%q: %v dev=%v", u, err, o.Web.DevMode)
		}
	}
}

func TestWebDevModeIsStrict(t *testing.T) {
	webEnvClean(t)
	for _, u := range []string{"http://localhost:8081", "http://127.0.0.1:8081", "http://[::1]:8081"} {
		o, err := parseWeb(t, "--public-url", u)
		if err != nil || !o.Web.DevMode {
			t.Errorf("%q: err=%v dev=%v", u, err, o.Web.DevMode)
		}
	}
	// Dev mode must never be exposed on a non-loopback listener.
	for _, l := range []string{"0.0.0.0:8081", ":8081", "192.168.1.5:8081"} {
		if _, err := parseWeb(t, "--public-url", "http://localhost:8081", "--listen", l); err == nil ||
			!strings.Contains(err.Error(), "loopback") {
			t.Errorf("dev mode with listen %q: %v", l, err)
		}
	}
	// A production URL may listen anywhere (container behind a proxy).
	if _, err := parseWeb(t, "--public-url", webPublic, "--listen", "0.0.0.0:8081"); err != nil {
		t.Fatal(err)
	}
}

func TestWebDatabasePasswordNotAcceptedOnCommandLine(t *testing.T) {
	webEnvClean(t)
	_, err := parseWeb(t, "--public-url", webPublic, "--database", "postgres://u:hunter2@db/m")
	if err == nil || !strings.Contains(err.Error(), "MAGNETICO_WEB_DATABASE") || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("CLI password: %v", err)
	}
	// Without a password a flag is fine, and the environment may carry one.
	if _, err := parseWeb(t, "--public-url", webPublic, "--database", "postgres://u@db/m"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGNETICO_WEB_DATABASE", "postgres://u:hunter2@db/m")
	o, err := parseWeb(t, "--public-url", webPublic)
	if err != nil || o.Web.DatabaseURL != "postgres://u:hunter2@db/m" {
		t.Fatalf("env password: %v", err)
	}
	t.Setenv("MAGNETICO_WEB_DATABASE", "mysql://u@db/m")
	if _, err := parseWeb(t, "--public-url", webPublic); err == nil {
		t.Fatal("non-postgres scheme must be rejected")
	}
}

func TestWebLimitValidation(t *testing.T) {
	webEnvClean(t)
	cases := map[string][]string{
		"pool too small":          {"--pool-max-conns", "3"},
		"concurrency above pool":  {"--search-concurrency", "9"},
		"concurrency zero":        {"--search-concurrency", "0"},
		"idle too short":          {"--session-idle-timeout", "1m"},
		"idle above absolute":     {"--session-idle-timeout", "200h"},
		"absolute above remember": {"--session-absolute-lifetime", "3000h"},
		"remember too long":       {"--session-remember-lifetime", "9000h"},
		"search too short":        {"--search-timeout", "500ms"},
		"search too long":         {"--search-timeout", "16m"},
		"request too short":       {"--request-timeout", "4s"},
		"shutdown zero":           {"--shutdown-timeout", "0s"},
		"negative quota":          {"--invite-quota", "-1"},
		"tracker ttl low":         {"--tracker-cache-ttl", "1s"},
		"tracker ttl high":        {"--tracker-cache-ttl", "2h"},
		"bad log level":           {"--log-level", "trace"},
		"bad log format":          {"--log-format", "xml"},
		"bad listen":              {"--listen", "nonsense"},
		"bad listen port":         {"--listen", "127.0.0.1:99999"},
	}
	for name, args := range cases {
		if _, err := parseWeb(t, append([]string{"--public-url", webPublic}, args...)...); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	// Boundaries that must pass.
	if _, err := parseWeb(t, "--public-url", webPublic, "--pool-max-conns", "4", "--search-concurrency", "2"); err != nil {
		t.Fatal(err)
	}
}

func TestWebTrustedProxies(t *testing.T) {
	webEnvClean(t)
	o, err := parseWeb(t, "--public-url", webPublic, "--trusted-proxies", "10.0.0.0/8, 192.0.2.7 ,2001:db8::/32")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, p := range o.Web.TrustedProxyPrefixes {
		got = append(got, p.String())
	}
	if strings.Join(got, " ") != "10.0.0.0/8 192.0.2.7/32 2001:db8::/32" {
		t.Fatalf("prefixes = %v", got)
	}
	for _, bad := range []string{"10.0.0.0/33", "not-an-ip", "10.0.0.1/x", "::ffff:10.0.0.0/64"} {
		if _, err := parseWeb(t, "--public-url", webPublic, "--trusted-proxies", bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestWebSMTPValidation(t *testing.T) {
	webEnvClean(t)
	base := []string{"--public-url", webPublic}
	full := []string{"--smtp-host", "smtp.example.org", "--smtp-port", "587", "--smtp-security", "starttls", "--smtp-from", "magnetico@example.org"}
	if o, err := parseWeb(t, append(base, full...)...); err != nil || !o.Web.SMTPConfigured() {
		t.Fatalf("complete SMTP: %v", err)
	}
	for name, args := range map[string][]string{
		"host only":          {"--smtp-host", "smtp.example.org"},
		"plaintext security": {"--smtp-host", "h", "--smtp-port", "25", "--smtp-security", "none", "--smtp-from", "a@b.c"},
		"bad from":           {"--smtp-host", "h", "--smtp-port", "25", "--smtp-security", "tls", "--smtp-from", "nope"},
		"bad port":           {"--smtp-host", "h", "--smtp-port", "70000", "--smtp-security", "tls", "--smtp-from", "a@b.c"},
		"username alone":     append(append([]string{}, full...), "--smtp-username", "u"),
	} {
		if _, err := parseWeb(t, append(base, args...)...); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	// The password is environment-only and never echoed.
	t.Setenv("MAGNETICO_WEB_SMTP_PASSWORD", "smtp-secret")
	if _, err := parseWeb(t, base...); err == nil || strings.Contains(err.Error(), "smtp-secret") {
		t.Fatalf("password without SMTP config: %v", err)
	}
	o, err := parseWeb(t, append(append(base, full...), "--smtp-username", "u")...)
	if err != nil || o.Web.SMTPPassword != "smtp-secret" {
		t.Fatalf("full SMTP with password: %v", err)
	}
	if _, err := parseWeb(t, append(base, "--smtp-password", "x")...); err == nil {
		t.Fatal("--smtp-password must not exist")
	}
}
