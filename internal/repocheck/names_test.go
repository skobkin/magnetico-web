// Package repocheck holds repository-level consistency tests: that historical
// reports stay reproducible after renames, and that the CI workflow files keep
// their security invariants. It has no non-test code.
package repocheck

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

const mappingSection = "## Reproducing historical reports"

// mappingRow is one table row of the mapping section of docs/testing.md:
// | kind | old | new | notes |   (kind: env, test, tag, command).
type mappingRow struct {
	kind string
	old  string
	news []string
}

var codeSpan = regexp.MustCompile("`([^`]+)`")

func loadMapping(t *testing.T) map[string]mappingRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testkit.ModuleRoot(t), "docs", "testing.md"))
	if err != nil {
		t.Fatalf("docs/testing.md must hold the old-to-new mapping: %v", err)
	}
	text := string(raw)
	i := strings.Index(text, mappingSection)
	if i < 0 {
		t.Fatalf("docs/testing.md has no %q section", mappingSection)
	}
	section := text[i+len(mappingSection):]
	if j := strings.Index(section, "\n## "); j >= 0 {
		section = section[:j]
	}
	rows := map[string]mappingRow{}
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 3 {
			continue
		}
		kind := strings.TrimSpace(cells[0])
		if kind != "env" && kind != "test" && kind != "tag" && kind != "command" {
			continue
		}
		olds := codeSpan.FindAllStringSubmatch(cells[1], -1)
		if len(olds) == 0 {
			t.Fatalf("mapping row without an old name: %q", line)
		}
		row := mappingRow{kind: kind, old: olds[0][1]}
		for _, m := range codeSpan.FindAllStringSubmatch(cells[2], -1) {
			row.news = append(row.news, m[1])
		}
		rows[row.old] = row
	}
	if len(rows) == 0 {
		t.Fatal("the mapping section has no rows")
	}
	return rows
}

var (
	envToken   = regexp.MustCompile(`MAGNETICO_W[0-9]_[A-Z0-9_]+`)
	testToken  = regexp.MustCompile(`\bTestW9[A-Za-z0-9]*`)
	tagToken   = regexp.MustCompile(`\bw9diag\b`)
	rootRun    = regexp.MustCompile(`go test\b[^\n` + "`" + `]*\s\.(\s|$)[^\n` + "`" + `]*`)
	runArg     = regexp.MustCompile(`-run[= ]+('[^']*'|"[^"]*"|\S+)`)
	testIdent  = regexp.MustCompile(`Test[A-Za-z0-9_]*`)
	rootCmdKey = "go test ."
)

// historicalTokens collects every old name the historical documents use.
func historicalTokens(t *testing.T) map[string]string {
	t.Helper()
	root := testkit.ModuleRoot(t)
	found := map[string]string{} // token -> first file
	add := func(tok, file string) {
		if _, ok := found[tok]; !ok {
			found[tok] = file
		}
	}
	err := filepath.WalkDir(filepath.Join(root, "docs", "wip"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		base := d.Name()
		if strings.HasPrefix(base, "08-ci-oci-") {
			return nil // this task's own documents describe the mapping
		}
		switch filepath.Ext(base) {
		case ".md", ".sh", ".py", ".txt":
		default:
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		text := string(raw)
		rel, _ := filepath.Rel(root, p)
		for _, m := range envToken.FindAllString(text, -1) {
			add(m, rel)
		}
		for _, m := range testToken.FindAllString(text, -1) {
			add(m, rel)
		}
		for _, m := range tagToken.FindAllString(text, -1) {
			add(m, rel)
		}
		// `go test … .` (the former root package): the command itself and every
		// -run target on the line.
		for _, line := range rootRun.FindAllString(text, -1) {
			add(rootCmdKey, rel)
			for _, a := range runArg.FindAllStringSubmatch(line, -1) {
				for _, id := range testIdent.FindAllString(strings.Trim(a[1], `'"`), -1) {
					add(id, rel)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// Every historical command, environment variable, test name and build tag that
// a report uses must have a mapping to its current name, so the reports stay
// reproducible.
func TestHistoricalNamesAreMapped(t *testing.T) {
	rows := loadMapping(t)
	var missing []string
	for tok, file := range historicalTokens(t) {
		if _, ok := rows[tok]; !ok {
			missing = append(missing, tok+"  (first seen in "+file+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("historical names without a mapping row in docs/testing.md:\n  %s", strings.Join(missing, "\n  "))
	}
}

// The new names a mapping row promises must exist in the code, so the mapping
// cannot rot.
func TestMappedNewNamesExist(t *testing.T) {
	rows := loadMapping(t)
	root := testkit.ModuleRoot(t)
	var goSrc, goBuild strings.Builder
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".go" || strings.HasSuffix(p, "internal/repocheck/names_test.go") {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		goSrc.Write(raw)
		goSrc.WriteByte('\n')
		for _, l := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(l, "//go:build") {
				goBuild.WriteString(l + "\n")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		for _, n := range row.news {
			switch row.kind {
			case "env":
				if !strings.Contains(goSrc.String(), n) {
					t.Errorf("mapped variable %s (for %s) appears in no Go source", n, row.old)
				}
			case "test":
				if strings.HasPrefix(n, "Test") && !regexp.MustCompile(`func `+regexp.QuoteMeta(n)+`\(`).MatchString(goSrc.String()) &&
					!strings.Contains(goSrc.String(), "func "+n) {
					t.Errorf("mapped test %s (for %s) is declared nowhere", n, row.old)
				}
			case "tag":
				if !regexp.MustCompile(`\b` + regexp.QuoteMeta(n) + `\b`).MatchString(goBuild.String()) {
					t.Errorf("mapped build tag %s (for %s) is used in no //go:build line", n, row.old)
				}
			}
		}
	}
}

// Persisted benchmark-dataset identifiers must not be renamed: databases that
// were seeded long ago carry them and the guards recognise a database by them.
func TestFrozenDatasetIdentifiersAreUnchanged(t *testing.T) {
	root := testkit.ModuleRoot(t)
	var src strings.Builder
	_ = filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".go" && !strings.HasSuffix(p, "internal/repocheck/names_test.go") {
			raw, _ := os.ReadFile(p)
			src.Write(raw)
		}
		return nil
	})
	for _, frozen := range []string{
		`"magnetico-w9-benchmark:w9-large-v1"`, // database comment marker
		`"w9u%02d"`,                            // generated account names
		`" w9ing "`,                            // seeded torrent-name pattern (SQL filter)
		`"w9ing/"`,                             // info-hash derivation seed
		`"w9"`,                                 // label of every minted credential
	} {
		if !strings.Contains(src.String(), strings.Trim(frozen, `"`)) {
			t.Errorf("frozen dataset identifier %s is gone from the harness", frozen)
		}
	}
}
