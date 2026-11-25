package repocheck

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

// Task 09B: a real browser at 360 px showed that an administrator's page was wider than the
// screen (the extra "Admin" link of the header navigation did not fit; the administrator tables
// had no scroll container). No layout can be judged without a browser, so these rules are pinned
// statically; the browser check itself is docs/wip/09B-measurements/browser/driver.js.
func TestPhoneLayoutRulesThatKeepTheAdministratorPagesInsideTheScreen(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(testkit.ModuleRoot(t), "internal", "web", "ui", "static", "app.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	if !regexp.MustCompile(`\.table-wrap\s*\{[^}]*overflow-x:\s*auto`).MatchString(css) {
		t.Error("app.css must make .table-wrap scroll horizontally: wide tables must not widen the page")
	}
	if !regexp.MustCompile(`(?s)\.main-nav\s*\{[^}]*min-width:\s*0[^}]*overflow-x:\s*auto`).MatchString(css) {
		t.Error("app.css must let .main-nav shrink and scroll: the administrator's header must fit 360 px")
	}
}
