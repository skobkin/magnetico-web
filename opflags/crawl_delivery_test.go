package opflags

import (
	"strings"
	"testing"
)

func TestCrawlStaleDatabaseConfigIsRejectedLoudly(t *testing.T) {
	t.Setenv("MAGNETICO_CRAWL_DATABASE", "postgres://u:pw@localhost/db")
	o := &OpFlags{Crawl: defaults()}
	err := o.applyConfig(map[string]bool{}, "crawl")
	if err == nil || !strings.Contains(err.Error(), "UPGRADE.md") || strings.Contains(err.Error(), "pw@") {
		t.Fatalf("stale env: %v", err)
	}

	o = &OpFlags{Crawl: defaults()}
	err = o.applyYamlConfig([]byte("databaseURL: postgres://u:pw@localhost/db\n"), map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "UPGRADE.md") || strings.Contains(err.Error(), "pw@") {
		t.Fatalf("stale yaml key: %v", err)
	}

	if _, err := (&OpFlags{}).parseArgs([]string{"crawl", "--database", "postgres://x"}); err == nil {
		t.Fatal("--database must no longer exist on crawl")
	}
}

func TestCrawlIngestTokenIsEnvOnly(t *testing.T) {
	t.Setenv("MAGNETICO_CRAWL_INGEST_TOKEN", "")
	t.Setenv("MAGNETICO_CRAWL_INGEST_URL", "https://ingest.example.org")
	if _, err := (&OpFlags{}).parseArgs([]string{"crawl"}); err == nil || !strings.Contains(err.Error(), "MAGNETICO_CRAWL_INGEST_TOKEN") {
		t.Fatalf("missing token: %v", err)
	}
	t.Setenv("MAGNETICO_CRAWL_INGEST_TOKEN", "short-secret")
	if _, err := (&OpFlags{}).parseArgs([]string{"crawl"}); err == nil || strings.Contains(err.Error(), "short-secret") {
		t.Fatalf("short token: %v", err)
	}
	t.Setenv("MAGNETICO_CRAWL_INGEST_TOKEN", goodToken)
	if _, err := (&OpFlags{}).parseArgs([]string{"crawl", "--ingest-token", goodToken}); err == nil {
		t.Fatal("no token flag may exist")
	}
	o := &OpFlags{}
	if cmd, err := o.parseArgs([]string{"crawl", "--leech-max-active", "7"}); err != nil || cmd != "crawl" ||
		o.Crawl.IngestToken != goodToken || o.Crawl.LeechMaxActive != 7 || o.Crawl.IngestURL != "https://ingest.example.org" {
		t.Fatalf("parse: %v %+v", err, o.Crawl)
	}
	// A yaml token key is rejected (strict decode).
	o = &OpFlags{Crawl: defaults()}
	if err := o.applyYamlConfig([]byte("ingestToken: "+goodToken+"\n"), map[string]bool{}); err == nil {
		t.Fatal("a token in the config file must be rejected")
	}
}

func TestCrawlIngestURLRule(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://ingest.example.org": true, "http://127.0.0.1:8080": true, "http://localhost": true,
		"http://ingest.example.org": false, "": false, "ftp://x": false,
	} {
		c := defaults()
		c.IngestURL = raw
		if err := c.checkDelivery(); (err == nil) != ok {
			t.Errorf("%q: %v", raw, err)
		}
	}
}

func TestCrawlCheckQueueSubcommand(t *testing.T) {
	t.Setenv("MAGNETICO_CRAWL_INGEST_TOKEN", "")
	t.Setenv("MAGNETICO_CRAWL_QUEUE_DIR", "/env/queue")
	o := &OpFlags{}
	cmd, err := o.parseArgs([]string{"crawl", "check-queue", "--full"})
	if err != nil || cmd != "crawl" || o.Crawl.Subcommand != "check-queue" || !o.Crawl.CheckQueue.Full ||
		o.Crawl.CheckQueue.QueueDir != "/env/queue" {
		t.Fatalf("%q %v %+v", cmd, err, o.Crawl)
	}
	o = &OpFlags{}
	if _, err := o.parseArgs([]string{"crawl", "check-queue", "--queue-dir", "/cli"}); err != nil || o.Crawl.CheckQueue.QueueDir != "/cli" {
		t.Fatalf("%v %+v", err, o.Crawl.CheckQueue)
	}
}
