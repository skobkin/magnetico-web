package legacywarn

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

const secretDSN = "postgres://legacyuser:HUNTER2@db.internal:5432/magnetico"

func TestDetectFindsOnlyTriggerNames(t *testing.T) {
	got := Detect([]string{
		"PATH=/usr/bin",
		"MAGNETICOD_DATABASE_URL=" + secretDSN,
		"APP_DATABASE_URL=" + secretDSN,
		"MAGNETICO_WEB_DATABASE=postgres://x",
		"APP_DATABASE_URL=" + secretDSN, // duplicate entries are reported once
	})
	want := []string{"APP_DATABASE_URL", "MAGNETICOD_DATABASE_URL"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Detect = %v, want %v (sorted, deduplicated, names only)", got, want)
	}
}

func TestDetectCountsAnEmptyValueAsPresent(t *testing.T) {
	if got := Detect([]string{"APP_DATABASE_URL="}); len(got) != 1 {
		t.Fatalf("an exported but empty legacy variable still marks a legacy deployment: %v", got)
	}
}

func TestDetectIgnoresGenericSymfonyNames(t *testing.T) {
	got := Detect([]string{"APP_ENV=prod", "APP_SECRET=x", "REDIS_DSN=redis://r", "MAILER_DSN=smtp://m", "TRUSTED_PROXIES=1.2.3.4", "DEFAULT_URI=https://x", "SENTRY_DSN=https://s"})
	if len(got) != 0 {
		t.Fatalf("generic names must not trigger the warning: %v", got)
	}
}

func TestDetectIgnoresLookalikes(t *testing.T) {
	got := Detect([]string{"XAPP_DATABASE_URL=1", "APP_DATABASE_URL_X=1", "MAGNETICO_DATABASE_URL=1", "app_database_url=1"})
	if len(got) != 0 {
		t.Fatalf("only exact names count: %v", got)
	}
}

func TestWarnBannerNamesURLAndVariablesButNeverValues(t *testing.T) {
	var b bytes.Buffer
	if !Warn(&b, []string{"APP_DATABASE_URL=" + secretDSN, "MAGNETICOD_DATABASE_URL=" + secretDSN}) {
		t.Fatal("Warn reported nothing written")
	}
	out := b.String()
	for _, must := range []string{UpgradeURL, "APP_DATABASE_URL", "MAGNETICOD_DATABASE_URL", "rewritten in Go", "incompatible", "migration", "0.10.0"} {
		if !strings.Contains(out, must) {
			t.Errorf("banner lacks %q:\n%s", must, out)
		}
	}
	for _, leak := range []string{"HUNTER2", "legacyuser", "postgres://", "db.internal", "5432"} {
		if strings.Contains(out, leak) {
			t.Errorf("banner leaks %q:\n%s", leak, out)
		}
	}
}

func TestWarnIsSilentWithoutTriggers(t *testing.T) {
	var b bytes.Buffer
	if Warn(&b, []string{"PATH=/usr/bin", "MAGNETICO_WEB_DATABASE=postgres://x"}) || b.Len() != 0 {
		t.Fatalf("no trigger, no output; got %q", b.String())
	}
}
