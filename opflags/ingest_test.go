package opflags

import (
	"strings"
	"testing"
)

const goodToken = "0123456789abcdef0123456789abcdef"

func TestIngestTokenEnvOnlyAndRequired(t *testing.T) {
	t.Setenv("MAGNETICO_INGEST_TOKEN", "")
	o := &OpFlags{}
	if _, err := o.parseArgs([]string{"ingest"}); err == nil || !strings.Contains(err.Error(), "MAGNETICO_INGEST_TOKEN") {
		t.Fatalf("missing token: %v", err)
	}
	t.Setenv("MAGNETICO_INGEST_TOKEN", "short-secret")
	o = &OpFlags{}
	if _, err := o.parseArgs([]string{"ingest"}); err == nil || strings.Contains(err.Error(), "short-secret") {
		t.Fatalf("short token must fail without echoing it: %v", err)
	}
	// There is no token flag.
	t.Setenv("MAGNETICO_INGEST_TOKEN", goodToken)
	o = &OpFlags{}
	if _, err := o.parseArgs([]string{"ingest", "--token", goodToken}); err == nil {
		t.Fatal("--token must not exist")
	}
}

func TestIngestEnvAndCLIPrecedence(t *testing.T) {
	t.Setenv("MAGNETICO_INGEST_TOKEN", goodToken)
	t.Setenv("MAGNETICO_INGEST_LISTEN", "0.0.0.0:9000")
	t.Setenv("MAGNETICO_INGEST_QUEUE_MAX_BYTES", "12345")
	t.Setenv("MAGNETICO_INGEST_CACHE_DISABLED", "true")
	o := &OpFlags{}
	cmd, err := o.parseArgs([]string{"ingest", "--queue-max-bytes", "999"})
	if err != nil || cmd != "ingest" {
		t.Fatalf("parse: %q %v", cmd, err)
	}
	if o.Ingest.Listen != "0.0.0.0:9000" || o.Ingest.QueueMaxBytes != 999 || !o.Ingest.CacheDisabled || o.Ingest.Token != goodToken {
		t.Fatalf("options = %+v", o.Ingest)
	}
}

func TestIngestRejectsUnknownEnvAndBadValues(t *testing.T) {
	t.Setenv("MAGNETICO_INGEST_TOKEN", goodToken)
	t.Setenv("MAGNETICO_INGEST_BOGUS", "1")
	if _, err := (&OpFlags{}).parseArgs([]string{"ingest"}); err == nil || !strings.Contains(err.Error(), "MAGNETICO_INGEST_BOGUS") {
		t.Fatalf("unknown env: %v", err)
	}
}
