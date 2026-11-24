package cli

import (
	"net"
	"regexp"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

const (
	legacyUpgradeURL = "https://github.com/skobkin/magnetico-web/blob/master/UPGRADE.md"
	legacySecret     = "HUNTER2"
	legacyDSN        = "postgres://legacyuser:" + legacySecret + "@db.invalid:5432/magnetico"
)

var logPrefix = regexp.MustCompile(`(?m)^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d `)

// firstMessage is the first non-banner log line without its timestamp.
func firstMessage(out string) string {
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(logPrefix.ReplaceAllString(l, ""))
		if l != "" && !strings.Contains(l, legacyUpgradeURL) && !strings.HasPrefix(l, "WARNING") {
			return l
		}
	}
	return ""
}

func assertNoSecret(t *testing.T, what, out string) {
	t.Helper()
	for _, leak := range []string{legacySecret, "legacyuser", "db.invalid"} {
		if strings.Contains(out, leak) {
			t.Errorf("%s leaks legacy variable content %q:\n%s", what, leak, out)
		}
	}
}

// A legacy deployment that also fails configuration validation must see the
// legacy explanation first, and the failure itself must be unchanged.
func TestLegacyWarningPrecedesConfigFailure(t *testing.T) {
	bin := testkit.Binary(t)
	base := testkit.Run(t, bin, testkit.RunOptions{Args: []string{"web"}})
	legacy := testkit.Run(t, bin, testkit.RunOptions{Args: []string{"web"}, Env: []string{"APP_DATABASE_URL=" + legacyDSN}})

	if base.ExitCode == 0 {
		t.Fatalf("`web` without configuration must fail, output %q", base.Output)
	}
	if legacy.ExitCode != base.ExitCode {
		t.Fatalf("the exit code changed with the legacy variable: %d vs %d", legacy.ExitCode, base.ExitCode)
	}
	msg := firstMessage(base.Output)
	if msg == "" {
		t.Fatalf("no configuration error found in %q", base.Output)
	}
	iBanner, iErr := strings.Index(legacy.Output, legacyUpgradeURL), strings.Index(legacy.Output, msg)
	if iBanner < 0 || iErr < 0 || iBanner > iErr {
		t.Fatalf("banner at %d must precede the unchanged error %q at %d:\n%s", iBanner, msg, iErr, legacy.Output)
	}
	assertNoSecret(t, "output", legacy.Output)
}

func TestLegacyWarningCoversServiceCommands(t *testing.T) {
	bin := testkit.Binary(t)
	for _, args := range [][]string{
		{"web"}, {"ingest"}, {"crawl"}, {"migrate", "schema-init"}, {"dev", "seed-torrents"}, {"web", "user", "list"},
	} {
		res := testkit.Run(t, bin, testkit.RunOptions{Args: args, Env: []string{"MAGNETICOD_DATABASE_URL=" + legacyDSN}})
		if !strings.Contains(res.Output, legacyUpgradeURL) || !strings.Contains(res.Output, "MAGNETICOD_DATABASE_URL") {
			t.Errorf("%v: no legacy warning in %q", args, res.Output)
		}
		assertNoSecret(t, strings.Join(args, " "), res.Output)
	}
}

func TestNoLegacyWarningForHelpVersionOrNoArguments(t *testing.T) {
	bin := testkit.Binary(t)
	env := []string{"APP_DATABASE_URL=" + legacyDSN, "MAGNETICOD_DATABASE_URL=" + legacyDSN}
	for _, args := range [][]string{
		nil, {"--help"}, {"-h"}, {"--version"}, {"web", "--help"}, {"crawl", "--help"}, {"migrate", "--help"}, {"ingest", "-h"},
	} {
		res := testkit.Run(t, bin, testkit.RunOptions{Args: args, Env: env})
		if strings.Contains(res.Output, legacyUpgradeURL) || strings.Contains(res.Output, "rewritten in Go") {
			t.Errorf("%v must stay quiet, got %q", args, res.Output)
		}
	}
	none := testkit.Run(t, bin, testkit.RunOptions{Env: env})
	if none.ExitCode != 1 || !strings.Contains(none.Output, "Please specify one command of: crawl, dev, ingest, migrate or web") {
		t.Errorf("no-argument behaviour changed with legacy variables: exit=%d %q", none.ExitCode, none.Output)
	}
}

// The warning does not replace or weaken the fail-closed schema verification.
func TestLegacyWarningDoesNotWeakenSchemaVerification(t *testing.T) {
	bin := testkit.Binary(t)
	dsn := webTarget(t) // 0.8.0 torrent schema, no magnetico_web schema
	addr := freeLoopbackAddr(t)
	args := testkit.RunOptions{Args: []string{"web"}, Env: []string{
		"MAGNETICO_WEB_DATABASE=" + dsn, "MAGNETICO_WEB_LISTEN=" + addr, "MAGNETICO_WEB_PUBLIC_URL=http://" + addr,
	}}
	base := testkit.Run(t, bin, args)
	args.Env = append(args.Env, "APP_DATABASE_URL="+legacyDSN)
	legacy := testkit.Run(t, bin, args)

	if base.ExitCode != 1 || !strings.Contains(base.Output, "magnetico_web schema is missing") {
		t.Fatalf("baseline changed: exit=%d %q", base.ExitCode, base.Output)
	}
	if legacy.ExitCode != base.ExitCode || !strings.Contains(legacy.Output, "magnetico_web schema is missing") {
		t.Fatalf("the schema refusal was lost or changed: exit=%d %q", legacy.ExitCode, legacy.Output)
	}
	if !strings.Contains(legacy.Output, legacyUpgradeURL) {
		t.Errorf("no warning: %q", legacy.Output)
	}
	assertNoSecret(t, "output", legacy.Output)
	if c, err := net.Dial("tcp", addr); err == nil {
		c.Close()
		t.Fatal("something listens although startup verification failed")
	}
}
