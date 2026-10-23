package opflags

import (
	"testing"

	"github.com/jessevdk/go-flags"
)

func TestExplicitOptions(t *testing.T) {
	t.Parallel()

	o := OpFlags{}
	parser := flags.NewParser(&o, flags.None)
	if _, err := parser.ParseArgs([]string{
		"crawl", "--max-rps", "42", "--database", "postgres://cli@localhost/db",
	}); err != nil {
		t.Fatalf("ParseArgs() error = %v", err)
	}

	set := explicitOptions(parser.Command)
	if !set["max-rps"] {
		t.Errorf("--max-rps not detected as explicit: %v", set)
	}
	if !set["database"] {
		t.Errorf("--database not detected as explicit: %v", set)
	}
	if set["config-file-path"] {
		t.Errorf("--config-file-path wrongly detected as explicit: %v", set)
	}
	if set["indexer-addr"] {
		t.Errorf("defaulted --indexer-addr wrongly detected as explicit: %v", set)
	}
}
