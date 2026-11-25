package repocheck

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/opstats"
)

// tableNames returns the first-column backticked names of the markdown table
// whose header line starts with header, within text.
func tableNames(t *testing.T, text, header string) []string {
	t.Helper()
	i := strings.Index(text, "\n"+header)
	if i < 0 {
		t.Fatalf("no table headed %q", header)
	}
	var out []string
	re := regexp.MustCompile("^\\| `([a-z0-9_]+)` \\|")
	for _, line := range strings.Split(text[i+1:], "\n")[2:] { // skip the header and the separator
		if !strings.HasPrefix(line, "|") {
			break
		}
		if m := re.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func sameSet(t *testing.T, what string, doc, schema []string) {
	t.Helper()
	sort.Strings(schema)
	have := map[string]bool{}
	for _, n := range doc {
		have[n] = true
	}
	want := map[string]bool{}
	for _, n := range schema {
		want[n] = true
		if !have[n] {
			t.Errorf("%s: %q exists in the schema but not in docs/specs/statistics-v1.md", what, n)
		}
	}
	for _, n := range doc {
		if !want[n] {
			t.Errorf("%s: %q is documented in docs/specs/statistics-v1.md but does not exist", what, n)
		}
	}
}

func TestStatisticsSpecMetricNamesEqualTheSchema(t *testing.T) {
	spec := readRepo(t, "docs/specs/statistics-v1.md")
	var counters, gauges, measures []string
	for c := opstats.Counter(0); c < opstats.NumCounters; c++ {
		counters = append(counters, c.Name())
	}
	for g := opstats.Gauge(0); g < opstats.NumGauges; g++ {
		gauges = append(gauges, g.Name())
	}
	for m := opstats.Measure(0); m < opstats.NumMeasures; m++ {
		measures = append(measures, m.Name())
	}
	sameSet(t, "counters", tableNames(t, spec, "| Counter |"), counters)
	sameSet(t, "gauges", tableNames(t, spec, "| Gauge |"), gauges)
	sameSet(t, "storage measures", tableNames(t, spec, "| Storage measure |"), measures)

	// And they are columns, not only names: a documented counter is a column of every aggregate table.
	cols := map[string]bool{}
	for _, c := range opstats.ColumnNames() {
		cols[c] = true
	}
	for _, c := range counters {
		for _, tbl := range []string{"agg_hour", "agg_day", "agg_month"} {
			if !cols[tbl+"."+c] {
				t.Errorf("counter %s is not a column of %s", c, tbl)
			}
		}
	}
	for _, g := range gauges {
		if !cols["agg_hour."+g+"_sum"] || !cols["agg_hour."+g+"_last"] || !cols["shared_state."+g] {
			t.Errorf("gauge %s lacks its aggregate or shared-state columns", g)
		}
	}
	for _, m := range measures {
		if !cols["storage_hour."+m+"_last"] || !cols["shared_state.storage_"+m] {
			t.Errorf("storage measure %s lacks its columns", m)
		}
	}
	for _, tbl := range opstats.TableNames() {
		if !strings.Contains(spec, "`"+tbl+"`") {
			t.Errorf("table %s is not described in the spec", tbl)
		}
	}
}

func TestStatisticsSpecStatesTheInFlightIdentityAndTheQuiescenceRule(t *testing.T) {
	spec := readRepo(t, "docs/specs/statistics-v1.md")
	for _, want := range []string{
		"`records_received − records_acknowledged − records_refused = in_flight`",
		"At **quiescence**",
		"`records_received = records_acknowledged + records_refused`",
		"`records_acknowledged = records_enqueued + admission_duplicates`",
		"sealed immutable batches", "tombstone", "single arbiter",
	} {
		if !strings.Contains(strings.ToLower(spec), strings.ToLower(want)) {
			t.Errorf("the statistics spec does not state %q", want)
		}
	}
}

// The schema holds nothing that could carry content or a credential.
func TestStatisticsSchemaHasNoColumnThatCouldHoldContentOrACredential(t *testing.T) {
	banned := regexp.MustCompile(`(?i)(payload|info_?hash|token|digest|password|secret|name$|path|detail|code$)`)
	for _, c := range opstats.ColumnNames() {
		col := c[strings.IndexByte(c, '.')+1:]
		if banned.MatchString(col) {
			t.Errorf("column %s looks like content or a credential", c)
		}
	}
	spec := readRepo(t, "docs/specs/statistics-v1.md")
	if !strings.Contains(spec, "no `bytea`") && !strings.Contains(spec, "No `bytea`") {
		t.Error("the spec does not state the no-bytea/no-text rule")
	}
}

// The administrator pages select explicit safe columns only.
func TestAdministratorQueriesNeverNameACredentialOrContentColumn(t *testing.T) {
	root := filepath.Join("..", "..", "internal", "web", "admin")
	forbidden := regexp.MustCompile(`(?i)\b(password_hash|token_hash|info_hash|payload|code)\b`)
	sqlLiteral := regexp.MustCompile("(?s)`([^`]*)`")
	n := 0
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, _ := os.ReadFile(path)
		for _, m := range sqlLiteral.FindAllStringSubmatch(string(b), -1) {
			q := m[1]
			if !strings.Contains(strings.ToUpper(q), "SELECT") {
				continue
			}
			n++
			if strings.Contains(strings.ReplaceAll(strings.ToUpper(q), " ", ""), "SELECT*") {
				t.Errorf("%s: SELECT * in an administrator query", path)
			}
			if loc := forbidden.FindString(q); loc != "" {
				t.Errorf("%s: an administrator query names %q", path, loc)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 8 {
		t.Fatalf("only %d SQL literals found: the check lost its target", n)
	}
}

// Every administrator route is documented in the web specification.
func TestEveryAdministratorRouteIsInTheWebSpec(t *testing.T) {
	spec := readRepo(t, "docs/specs/web-v1.md")
	route := regexp.MustCompile(`"GET (/admin[^"]*)"`)
	found := 0
	for _, f := range []string{"internal/web/admin/admin.go", "internal/web/admin/pages.go"} {
		for _, m := range route.FindAllStringSubmatch(readRepo(t, f), -1) {
			found++
			if !strings.Contains(spec, m[1]) {
				t.Errorf("route %s is not documented in docs/specs/web-v1.md", m[1])
			}
		}
	}
	if found < 7 {
		t.Fatalf("found %d admin routes, want at least 7", found)
	}
}

// The removed shared ingest token appears in no example, compose file or
// workflow: only where it is said to be removed.
func TestTheRemovedSharedIngestTokenAppearsInNoExample(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if fi.IsDir() {
			switch rel {
			case ".git", ".superpowers", ".claude", "third_party", filepath.Join("docs", "wip") + "/", "docs/wip":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(rel, "_test.go") || fi.Size() > 2<<20 {
			return nil
		}
		b, _ := os.ReadFile(path)
		for i, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "MAGNETICO_INGEST_TOKEN") {
				continue
			}
			l := strings.ToLower(line)
			if strings.Contains(l, "remov") || strings.Contains(l, "refus") || strings.Contains(l, "former") || strings.Contains(l, "no longer") {
				continue
			}
			// The production source names it only in the error that refuses it.
			if strings.HasPrefix(rel, "opflags"+string(filepath.Separator)) || strings.HasPrefix(rel, "internal"+string(filepath.Separator)) {
				continue
			}
			t.Errorf("%s:%d mentions MAGNETICO_INGEST_TOKEN as if it were valid: %s", rel, i+1, strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The documented reinitialisation names only the ingest schema.
func TestDocumentedReinitialisationDropsOnlyTheIngestSchema(t *testing.T) {
	up := readRepo(t, "UPGRADE.md")
	i := strings.Index(up, "**Reinitialising an older development ingest schema.**")
	if i < 0 {
		t.Fatal("UPGRADE.md has no reinitialisation section")
	}
	section := up[i:]
	if j := strings.Index(section, "Only `magnetico_ingest` is touched"); j > 0 {
		section = section[:j]
	} else {
		t.Fatal("the reinitialisation section does not state what it leaves alone")
	}
	drops := regexp.MustCompile("DROP SCHEMA ([a-z_]+)").FindAllStringSubmatch(section, -1)
	if len(drops) != 1 || drops[0][1] != "magnetico_ingest" {
		t.Errorf("the documented steps drop %v, want exactly magnetico_ingest", drops)
	}
	for _, f := range []string{"docs/container.md", "docs/crawler.md", "docs/web-deployment.md", "README.md"} {
		for _, m := range regexp.MustCompile("DROP SCHEMA ([a-z_]+)").FindAllStringSubmatch(readRepo(t, f), -1) {
			if m[1] != "magnetico_ingest" && m[1] != "magnetico_stats" {
				t.Errorf("%s documents DROP SCHEMA %s", f, m[1])
			}
		}
	}
}

// Relative links of the statistics documents resolve.
func TestStatisticsDocumentLinksResolve(t *testing.T) {
	link := regexp.MustCompile(`\]\(([^)#\s]+)(#[^)]*)?\)`)
	for _, f := range []string{
		"docs/specs/statistics-v1.md", "docs/operator-statistics.md", "docs/specs/crawler-ingest-delivery-v1.md", "docs/specs/web-v1.md",
		"docs/web-deployment.md", "docs/crawler.md", "docs/container.md", "README.md", "AGENTS.md", "docs/wip/README.md", "docs/projects_differences.md",
	} {
		dir := filepath.Dir(f)
		for _, m := range link.FindAllStringSubmatch(readRepo(t, f), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if _, err := os.Stat(filepath.Join("..", "..", dir, filepath.FromSlash(target))); err != nil {
				t.Errorf("%s links to %s, which does not exist", f, target)
			}
		}
	}
}
