package opflags

import (
	"os"
	"strings"
	"testing"
)

const goodToken = "0123456789abcdef0123456789abcdef"

func TestSharedTokenVariableIsRefusedWithAMessage(t *testing.T) {
	for _, v := range []string{goodToken, "short-secret", ""} {
		t.Setenv("MAGNETICO_INGEST_TOKEN", v)
		o := &OpFlags{}
		_, err := o.parseArgs([]string{"ingest"})
		if err == nil {
			t.Fatalf("MAGNETICO_INGEST_TOKEN=%q must be refused (no fallback)", v)
		}
		for _, want := range []string{"MAGNETICO_INGEST_TOKEN", "ingest crawler add", "MAGNETICO_CRAWL_INGEST_TOKEN"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err, want)
			}
		}
		if v != "" && strings.Contains(err.Error(), v) {
			t.Errorf("the value was echoed: %v", err)
		}
	}
	// Without the variable the service starts with no token at all: there is
	// no flag, no variable and no option for one.
	t.Setenv("MAGNETICO_INGEST_TOKEN", "")
	os.Unsetenv("MAGNETICO_INGEST_TOKEN")
	if _, err := (&OpFlags{}).parseArgs([]string{"ingest"}); err != nil {
		t.Fatalf("ingest without a token: %v", err)
	}
	if _, err := (&OpFlags{}).parseArgs([]string{"ingest", "--token", goodToken}); err == nil {
		t.Fatal("--token must not exist")
	}
}

func TestIngestCrawlerSubcommandsParse(t *testing.T) {
	for args, want := range map[string]string{
		"add alpha": "crawler add", "list": "crawler list", "disable alpha": "crawler disable",
		"enable alpha": "crawler enable", "rotate-token alpha": "crawler rotate-token",
	} {
		o := &OpFlags{}
		cmd, err := o.parseArgs(append([]string{"ingest", "--database", "postgres://u@h/db", "crawler"}, strings.Fields(args)...))
		if err != nil || cmd != "ingest" || o.Ingest.Subcommand != want {
			t.Errorf("%s: cmd=%q sub=%q err=%v", args, cmd, o.Ingest.Subcommand, err)
		}
	}
	o := &OpFlags{}
	if _, err := o.parseArgs([]string{"ingest", "crawler", "add"}); err == nil {
		t.Error("add without a name must fail")
	}
	if _, err := (&OpFlags{}).parseArgs([]string{"ingest", "crawler"}); err == nil {
		t.Error("bare `ingest crawler` must fail")
	}
	// The commands need no service options and the removed variable is still refused.
	t.Setenv("MAGNETICO_INGEST_TOKEN", goodToken)
	if _, err := (&OpFlags{}).parseArgs([]string{"ingest", "crawler", "list"}); err == nil {
		t.Error("the removed variable must be refused for the crawler commands as well")
	}
}

func TestIngestEnvAndCLIPrecedence(t *testing.T) {
	t.Setenv("MAGNETICO_INGEST_LISTEN", "0.0.0.0:9000")
	t.Setenv("MAGNETICO_INGEST_QUEUE_MAX_BYTES", "12345")
	t.Setenv("MAGNETICO_INGEST_CACHE_DISABLED", "true")
	o := &OpFlags{}
	cmd, err := o.parseArgs([]string{"ingest", "--queue-max-bytes", "999"})
	if err != nil || cmd != "ingest" {
		t.Fatalf("parse: %q %v", cmd, err)
	}
	if o.Ingest.Listen != "0.0.0.0:9000" || o.Ingest.QueueMaxBytes != 999 || !o.Ingest.CacheDisabled {
		t.Fatalf("options = %+v", o.Ingest)
	}
}

func TestIngestRejectsUnknownEnvAndBadValues(t *testing.T) {
	t.Setenv("MAGNETICO_INGEST_BOGUS", "1")
	if _, err := (&OpFlags{}).parseArgs([]string{"ingest"}); err == nil || !strings.Contains(err.Error(), "MAGNETICO_INGEST_BOGUS") {
		t.Fatalf("unknown env: %v", err)
	}
}

func TestIngestTrustedProxiesAndStatsKillSwitch(t *testing.T) {
	t.Setenv("MAGNETICO_INGEST_TRUSTED_PROXIES", "10.0.0.0/8, 192.0.2.7 ,::ffff:172.16.0.0/108")
	t.Setenv("MAGNETICO_INGEST_STATS_DISABLED", "true")
	o := &OpFlags{}
	if _, err := o.parseArgs([]string{"ingest"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := map[string]bool{}
	for _, p := range o.Ingest.TrustedProxyPrefixes {
		got[p.String()] = true
	}
	if len(got) != 3 || !got["10.0.0.0/8"] || !got["192.0.2.7/32"] || !got["172.16.0.0/12"] || !o.Ingest.StatsDisabled {
		t.Fatalf("prefixes %v, disabled %v", got, o.Ingest.StatsDisabled)
	}
	// A bad entry is refused without echoing it; a bad boolean too.
	t.Setenv("MAGNETICO_INGEST_TRUSTED_PROXIES", "not-a-cidr")
	if _, err := (&OpFlags{}).parseArgs([]string{"ingest"}); err == nil || strings.Contains(err.Error(), "not-a-cidr") {
		t.Fatalf("bad proxy entry: %v", err)
	}
	t.Setenv("MAGNETICO_INGEST_TRUSTED_PROXIES", "")
	t.Setenv("MAGNETICO_INGEST_STATS_DISABLED", "maybe")
	if _, err := (&OpFlags{}).parseArgs([]string{"ingest"}); err == nil || strings.Contains(err.Error(), "maybe") {
		t.Fatalf("bad boolean: %v", err)
	}
	// The flags exist and win over the environment.
	t.Setenv("MAGNETICO_INGEST_STATS_DISABLED", "true")
	t.Setenv("MAGNETICO_INGEST_TRUSTED_PROXIES", "")
	o = &OpFlags{}
	if _, err := o.parseArgs([]string{"ingest", "--trusted-proxies", "198.51.100.0/24"}); err != nil || len(o.Ingest.TrustedProxyPrefixes) != 1 {
		t.Fatalf("flag: %v %v", err, o.Ingest.TrustedProxyPrefixes)
	}
}
