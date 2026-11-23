package web_test

import (
	"os/exec"
	"strings"
	"testing"
)

// forbidden are the import paths the web service must never depend on
// (spec §1.2), plus the DHT stack: no web code may ever speak BitTorrent.
var forbidden = []string{
	"github.com/skobkin/magnetico-web/internal/crawler",
	"github.com/skobkin/magnetico-web/internal/crawlqueue",
	"github.com/skobkin/magnetico-web/internal/ingest",
	"github.com/skobkin/magnetico-web/dht",
	"modernc.org/sqlite",
	// Test-only dependencies (OpenAPI schema validation): imported from
	// _test.go files only, never by a package that ships in the binary.
	"github.com/santhosh-tekuri/jsonschema",
	"github.com/dlclark/regexp2",
}

// TestWebPackagesDoNotImportForbiddenPackages checks the dependency graph of
// ./internal/web/... only (not of the whole executable, whose main package
// links the crawler and ingest by design).
func TestWebPackagesDoNotImportForbiddenPackages(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./...: %v\n%s", err, out)
	}
	deps := strings.Fields(string(out))
	if len(deps) < 10 {
		t.Fatalf("suspiciously small dependency list (%d): the check would be vacuous", len(deps))
	}
	for _, dep := range deps {
		for _, bad := range forbidden {
			if dep == bad || strings.HasPrefix(dep, bad+"/") {
				t.Errorf("a web package depends on forbidden package %s", dep)
			}
		}
	}
}
