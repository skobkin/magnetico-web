package apiv2

import (
	"sort"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/skobkin/magnetico-web/internal/web/apidocs"
)

// documentedOperations returns "METHOD /path" for every operation of the
// embedded OpenAPI v2 document.
func documentedOperations(t *testing.T) []string {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(apidocs.V2(), &doc); err != nil {
		t.Fatal(err)
	}
	var ops []string
	for path, item := range doc.Paths {
		if path == "/feeds/latest.rss" {
			continue // served by the feed package; the cross-package test in apidocs covers it
		}
		for method := range item {
			switch method {
			case "get", "put", "post", "delete", "patch", "head", "options", "trace":
				ops = append(ops, strings.ToUpper(method)+" "+path)
			}
		}
	}
	sort.Strings(ops)
	return ops
}

// Every implemented /api/v2 route is described and every described operation
// exists (AGENTS.md: API changes update the embedded OpenAPI in the same change).
func TestOpenAPIv2DescribesExactlyTheImplementedBearerRoutes(t *testing.T) {
	e := newEnv(t)
	var implemented []string
	for _, r := range e.srv.Router().Routes() {
		method, path, _ := strings.Cut(r.Pattern, " ")
		if !strings.HasPrefix(path, APIPrefix+"/") || path == APIPrefix+"/" {
			continue // the unknown-path catch-all is not an endpoint
		}
		implemented = append(implemented, method+" "+strings.TrimPrefix(path, APIPrefix))
	}
	sort.Strings(implemented)
	documented := documentedOperations(t)
	if strings.Join(implemented, "\n") != strings.Join(documented, "\n") {
		t.Fatalf("implemented:\n%s\ndocumented:\n%s", strings.Join(implemented, "\n"), strings.Join(documented, "\n"))
	}
}
