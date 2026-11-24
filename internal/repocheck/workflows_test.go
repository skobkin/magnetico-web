package repocheck

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/skobkin/magnetico-web/internal/testkit"
)

// The Woodpecker workflows carry the release's security properties. These tests
// parse them and fail the build when an edit would, for example, give pull
// request code a registry credential, publish without the verification gates,
// apply an alias tag, or build the toolchain below the security floor.

const (
	tagGrammar = `CI_COMMIT_TAG matches "^[0-9]+\\.[0-9]+\\.[0-9]+(-rc\\.[0-9]+)?$"`

	branch080 = "refactor/0.8.0-unify-and-new-storage"
)

var gates = []string{"race", "govulncheck", "image-smoke", "image-scan", "native-smoke"}

var publication = []string{"promote-image", "release-forgejo"}

type workflowFile struct {
	name string
	raw  string
	doc  map[string]any
}

func load(t *testing.T, name string) workflowFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testkit.ModuleRoot(t), ".woodpecker", name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v", name, err)
	}
	return workflowFile{name: name, raw: string(b), doc: doc}
}

func (w workflowFile) steps() []map[string]any {
	var out []map[string]any
	list, _ := w.doc["steps"].([]any)
	for _, s := range list {
		if m, ok := s.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (w workflowFile) step(t *testing.T, name string) map[string]any {
	t.Helper()
	for _, s := range w.steps() {
		if s["name"] == name {
			return s
		}
	}
	t.Fatalf("%s has no step %q", w.name, name)
	return nil
}

func (w workflowFile) services() []map[string]any {
	var out []map[string]any
	list, _ := w.doc["services"].([]any)
	for _, s := range list {
		if m, ok := s.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// events returns the set of events of a `when` value (a workflow's or a step's).
func events(when any) map[string]bool {
	out := map[string]bool{}
	var list []any
	switch v := when.(type) {
	case []any:
		list = v
	case map[string]any:
		list = []any{v}
	}
	for _, e := range list {
		m, _ := e.(map[string]any)
		switch ev := m["event"].(type) {
		case string:
			out[ev] = true
		case []any:
			for _, x := range ev {
				out[x.(string)] = true
			}
		}
	}
	return out
}

// dependsOn returns the names a step or workflow depends on, and whether any of
// them is marked optional.
func dependsOn(v any) (names []string, optional bool) {
	list, _ := v.([]any)
	for _, d := range list {
		switch x := d.(type) {
		case string:
			names = append(names, x)
		case map[string]any:
			names = append(names, x["name"].(string))
			if o, _ := x["optional"].(bool); o {
				optional = true
			}
		}
	}
	return names, optional
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func text(s map[string]any) string {
	b, _ := yaml.Marshal(s)
	return string(b)
}

// commentFree is the file text without comment lines (a `latest` inside an
// explanatory comment is fine, an active one is not).
func commentFree(raw string) string {
	var b strings.Builder
	for _, l := range strings.Split(raw, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if i := strings.Index(l, " #"); i >= 0 {
			l = l[:i]
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}

func TestCIWorkflowHasNoSecretsOrPrivilege(t *testing.T) {
	w := load(t, "ci.yaml")
	body := commentFree(w.raw)
	for _, forbidden := range []string{
		"from_secret", "logins", "cache_images", "cache_to", "cache_from", "privileged", "docker.sock", "dind",
		"plugin-docker-buildx", "retry", "failure: ignore", "volumes", "GITEA_TOKEN", "DOCKER_TOKEN", "GHCR_TOKEN",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("ci.yaml (which runs for pull requests) must not contain %q", forbidden)
		}
	}
	svcs := w.services()
	if len(svcs) != 1 || svcs[0]["name"] != "postgres" {
		t.Errorf("ci.yaml services must be exactly one postgres service, got %v", svcs)
	}
}

func TestCIWorkflowRunsOnEveryRequiredEvent(t *testing.T) {
	w := load(t, "ci.yaml")
	ev := events(w.doc["when"])
	for _, want := range []string{"push", "pull_request", "tag", "manual"} {
		if !ev[want] {
			t.Errorf("ci.yaml does not run on %s", want)
		}
	}
	// Pushes only on the default branch and the active 0.8.0 branch: every other
	// branch is verified by its pull request alone (no duplicate pipelines).
	var branches []string
	for _, e := range w.doc["when"].([]any) {
		m := e.(map[string]any)
		if m["event"] == "push" {
			for _, b := range m["branch"].([]any) {
				branches = append(branches, b.(string))
			}
		}
	}
	sort.Strings(branches)
	if strings.Join(branches, ",") != "master,"+branch080 {
		t.Errorf("push branches = %v, want master and %s", branches, branch080)
	}
	// A pull request is never excluded: merged-state verification is preserved.
	for _, e := range w.doc["when"].([]any) {
		m := e.(map[string]any)
		if events(m)["pull_request"] && (m["evaluate"] != nil || m["branch"] != nil) {
			t.Errorf("the pull_request trigger must not be restricted: %v", m)
		}
	}
}

func TestMergedStateIsVerifiedForPullRequests(t *testing.T) {
	w := load(t, "ci.yaml")
	m := w.step(t, "merge-target")
	body := text(m)
	for _, want := range []string{"pull_request", "CI_COMMIT_TARGET_BRANCH", "git merge", "--no-edit"} {
		if !strings.Contains(strings.Join(toStrings(m["commands"]), "\n"), want) && !strings.Contains(body, want) {
			t.Errorf("merge-target lacks %q", want)
		}
	}
	for _, name := range []string{"fmt", "lint", "vet", "build", "test", "openapi", "release-dry-run"} {
		deps, _ := dependsOn(w.step(t, name)["depends_on"])
		if !contains(deps, "merge-target") && !transitivelyDependsOn(w, name, "merge-target") {
			t.Errorf("step %s does not (transitively) wait for merge-target", name)
		}
	}
}

func transitivelyDependsOn(w workflowFile, from, to string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		if seen[n] {
			return false
		}
		seen[n] = true
		for _, s := range w.steps() {
			if s["name"] != n {
				continue
			}
			deps, _ := dependsOn(s["depends_on"])
			for _, d := range deps {
				if d == to || walk(d) {
					return true
				}
			}
		}
		return false
	}
	return walk(from)
}

func toStrings(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestReleaseWorkflowIsNeverTriggeredByPullRequestsOrPushes(t *testing.T) {
	w := load(t, "release.yaml")
	for ev := range events(w.doc["when"]) {
		if ev != "tag" && ev != "manual" {
			t.Errorf("release.yaml must run on tag and manual only, it also runs on %q", ev)
		}
	}
	if !events(w.doc["when"])["tag"] || !events(w.doc["when"])["manual"] {
		t.Error("release.yaml must run on tag events and manual pre-flight runs")
	}
}

func TestReleaseWorkflowDependsOnCIAndNeverOptionally(t *testing.T) {
	w := load(t, "release.yaml")
	deps, optional := dependsOn(w.doc["depends_on"])
	if !contains(deps, "ci") {
		t.Fatalf("release.yaml must depend on the ci workflow, depends_on = %v", deps)
	}
	if optional {
		t.Error("the dependency on ci must not be optional: a verification that was not built must block the release")
	}
}

func TestTagGrammarIsIdenticalInBothWorkflows(t *testing.T) {
	for _, name := range []string{"ci.yaml", "release.yaml"} {
		w := load(t, name)
		found := false
		for _, e := range w.doc["when"].([]any) {
			m := e.(map[string]any)
			if events(m)["tag"] {
				ev, _ := m["evaluate"].(string)
				if ev != tagGrammar {
					t.Errorf("%s: tag trigger evaluate = %q, want %q", name, ev, tagGrammar)
				}
				found = true
			}
		}
		if !found {
			t.Errorf("%s has no tag trigger", name)
		}
	}
}

func TestEveryPublicationStepIsTagOnlyAndDependsOnTheGuardAndAllGates(t *testing.T) {
	w := load(t, "release.yaml")
	for _, name := range publication {
		s := w.step(t, name)
		ev := events(s["when"])
		if len(ev) != 1 || !ev["tag"] {
			t.Errorf("publication step %s must run on tag events only, runs on %v", name, ev)
		}
		deps, optional := dependsOn(s["depends_on"])
		if optional {
			t.Errorf("%s: no dependency of a publication step may be optional", name)
		}
		for _, need := range append([]string{"require-verification"}, gates...) {
			if !contains(deps, need) {
				t.Errorf("publication step %s does not depend on %s", name, need)
			}
		}
	}
	// And every step that holds a credential is one of them (see the secrets test).
}

func TestRequireVerificationIsTagOnlyAndGatesDoNotWaitForIt(t *testing.T) {
	w := load(t, "release.yaml")
	g := w.step(t, "require-verification")
	if ev := events(g["when"]); len(ev) != 1 || !ev["tag"] {
		t.Errorf("require-verification runs on tag events only, got %v", ev)
	}
	if !strings.Contains(strings.Join(toStrings(g["commands"]), "\n"), "ci/ciguard") {
		t.Error("require-verification must run ci/ciguard")
	}
	// A manual pre-flight has no tag: the gates must not wait for a step that is
	// filtered out there.
	for _, name := range gates {
		deps, _ := dependsOn(w.step(t, name)["depends_on"])
		if contains(deps, "require-verification") {
			t.Errorf("gate %s must not depend on require-verification (it would block manual pre-flight runs)", name)
		}
	}
}

func TestOnlyTheReleaseTagIsEverApplied(t *testing.T) {
	for _, name := range []string{"ci.yaml", "release.yaml"} {
		w := load(t, name)
		for _, l := range strings.Split(commentFree(w.raw), "\n") {
			if regexp.MustCompile(`\blatest\b`).MatchString(l) && !strings.Contains(l, "image:") {
				t.Errorf("%s: an active line mentions latest: %q", name, strings.TrimSpace(l))
			}
		}
	}
	w := load(t, "release.yaml")
	// The commented-out latest entry must exist as the owner's switch.
	if !strings.Contains(w.raw, "# - latest") {
		t.Error("release.yaml must keep the commented-out `# - latest` entry (the owner enables it explicitly)")
	}
	p := w.step(t, "promote-image")
	if !strings.Contains(strings.Join(toStrings(p["commands"]), "\n"), `ci/promote-image.sh .ci/image "$CI_COMMIT_TAG"`) {
		t.Errorf("promote-image must promote the qualified layout with exactly the release tag: %v", p["commands"])
	}
	script, err := os.ReadFile(filepath.Join(testkit.ModuleRoot(t), "ci", "promote-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(script), "\n") {
		if strings.Contains(l, "latest") && !strings.HasPrefix(strings.TrimSpace(l), "#") && !strings.Contains(l, "refusing") && !strings.Contains(l, "never") {
			t.Errorf("promote-image.sh mentions latest outside a refusal: %q", l)
		}
	}
}

func TestImageIsBuiltOnceByTheBuildxPluginWithoutPushingOrCaching(t *testing.T) {
	w := load(t, "release.yaml")
	builds := 0
	for _, s := range w.steps() {
		img, _ := s["image"].(string)
		cmds := strings.Join(toStrings(s["commands"]), "\n")
		if strings.Contains(img, "plugin-docker-buildx") {
			builds++
			if s["name"] != "image-build" {
				t.Errorf("only image-build may use the buildx plugin, %v does", s["name"])
			}
		}
		if regexp.MustCompile(`docker (buildx )?build`).MatchString(cmds) {
			t.Errorf("step %v builds an image itself: the published image must be the one image-build produced", s["name"])
		}
	}
	if builds != 1 {
		t.Fatalf("%d steps use the buildx plugin, want exactly one", builds)
	}
	s := w.step(t, "image-build")
	set, _ := s["settings"].(map[string]any)
	if set["dry_run"] != true {
		t.Error("image-build must run with dry_run: true (it must never push)")
	}
	if !strings.HasSuffix(strings.SplitN(set["repo"].(string), "/", 2)[0], ".invalid") {
		t.Errorf("image-build's repo %q must be on a non-routable .invalid host: the build is staged, never published", set["repo"])
	}
	out, _ := set["output"].(string)
	// Relative to the workspace (the step's working directory). CI_WORKSPACE is a
	// runtime-only variable: Woodpecker cannot substitute it into a setting, so an
	// absolute ${CI_WORKSPACE}/... path would silently become /.ci/image.
	if !strings.HasPrefix(out, "type=oci,tar=false,dest=.ci/image,") {
		t.Errorf("image-build must write the OCI layout directory .ci/image (relative), output = %q", out)
	}
	if set["pull_image"] != true {
		t.Error("image-build must resolve fresh base images (pull_image: true)")
	}
	if set["default_labels"] != false {
		t.Error("image-build must disable the plugin's automatic labels (they override the Dockerfile's source label with the Forgejo URL)")
	}
	if set["sbom"] == nil || set["provenance"] == nil {
		t.Error("image-build must request SBOM and provenance attestations")
	}
	pl, _ := set["platforms"].([]any)
	if len(pl) != 2 || pl[0] != "linux/amd64" || pl[1] != "linux/arm64" {
		t.Errorf("platforms = %v, want exactly linux/amd64 and linux/arm64", pl)
	}
}

func TestSmokeScanAndPromotionUseTheQualifiedLayoutOnly(t *testing.T) {
	w := load(t, "release.yaml")
	for _, name := range []string{"image-smoke", "image-scan", "promote-image"} {
		s := w.step(t, name)
		if !strings.Contains(text(s), ".ci/image") {
			t.Errorf("%s must read the layout image-build wrote (.ci/image)", name)
		}
		deps, _ := dependsOn(s["depends_on"])
		if name != "promote-image" && !contains(deps, "image-build") {
			t.Errorf("%s must depend on image-build", name)
		}
	}
	scan := text(w.step(t, "image-scan"))
	for _, want := range []string{"linux/amd64", "linux/arm64", "HIGH,CRITICAL", "--exit-code 1"} {
		if !strings.Contains(scan, want) {
			t.Errorf("image-scan lacks %q", want)
		}
	}
	// Scanner pinned by version and digest.
	if !regexp.MustCompile(`aquasec/trivy:[0-9]+\.[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}`).MatchString(scan) {
		t.Error("the image scanner must be pinned by version and digest")
	}
}

func TestCredentialsAreOnlyInTheirPublicationStepsAndTagEvents(t *testing.T) {
	allowed := map[string]string{
		"DOCKER_LOGIN": "promote-image", "DOCKER_TOKEN": "promote-image",
		"GHCR_LOGIN": "promote-image", "GHCR_TOKEN": "promote-image",
		"GITEA_TOKEN": "release-forgejo",
	}
	for _, name := range []string{"ci.yaml", "release.yaml"} {
		w := load(t, name)
		for _, s := range w.steps() {
			env, _ := s["environment"].(map[string]any)
			for k, v := range env {
				m, ok := v.(map[string]any)
				if !ok || m["from_secret"] == nil {
					continue
				}
				step := allowed[k]
				if step == "" {
					t.Errorf("%s step %v uses an unexpected secret %s", name, s["name"], k)
				} else if name != "release.yaml" || s["name"] != step {
					t.Errorf("secret %s may only be used by release.yaml step %s, found in %s step %v", k, step, name, s["name"])
				}
				if ev := events(s["when"]); len(ev) != 1 || !ev["tag"] {
					t.Errorf("step %v holds a secret but is not tag-only", s["name"])
				}
			}
		}
	}
}

func TestNoRegistryCacheIsConfigured(t *testing.T) {
	for _, name := range []string{"ci.yaml", "release.yaml"} {
		body := commentFree(load(t, name).raw)
		for _, k := range []string{"cache_from", "cache_to", "cache_images", "--cache-to", "--cache-from"} {
			if strings.Contains(body, k) {
				t.Errorf("%s configures a registry cache (%s): none is configured so no unreviewed workflow can poison one", name, k)
			}
		}
	}
}

func TestPrivilegedExecutionIsLimitedToTheBuildAndTheDaemon(t *testing.T) {
	ci := load(t, "ci.yaml")
	if strings.Contains(commentFree(ci.raw), "privileged") {
		t.Error("ci.yaml must not use privileged execution")
	}
	rel := load(t, "release.yaml")
	for _, s := range rel.steps() {
		if s["privileged"] == true && s["name"] != "image-build" {
			t.Errorf("step %v is privileged: only image-build (the buildx plugin) may be", s["name"])
		}
	}
	for _, svc := range rel.services() {
		if svc["privileged"] == true && svc["name"] != "dind" {
			t.Errorf("service %v is privileged: only dind may be", svc["name"])
		}
	}
}

var goImage = regexp.MustCompile(`^golang:(\d+)\.(\d+)\.(\d+)`)

func TestEveryStepThatCompilesHonoursTheGoSecurityFloor(t *testing.T) {
	for _, name := range []string{"ci.yaml", "release.yaml"} {
		w := load(t, name)
		for _, s := range w.steps() {
			img, _ := s["image"].(string)
			if m := goImage.FindStringSubmatch(img); m != nil {
				if !(m[1] == "1" && (atoi(m[2]) > 27 || (atoi(m[2]) == 27 && atoi(m[3]) >= 4))) && m[1] == "1" {
					t.Errorf("%s step %v uses %s: below the Go 1.27.4 floor", name, s["name"], img)
				}
			}
			if strings.HasPrefix(img, "goreleaser/goreleaser:") {
				env, _ := s["environment"].(map[string]any)
				if env["GOTOOLCHAIN"] != "go1.27.4+auto" {
					t.Errorf("%s step %v runs GoReleaser (whose image ships Go 1.27.3 with GOTOOLCHAIN=local) without GOTOOLCHAIN=go1.27.4+auto", name, s["name"])
				}
			}
		}
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func TestTestStepsRequirePostgresRunTheCensusAndUseVerifiedParallelism(t *testing.T) {
	for _, tc := range []struct{ file, step string }{{"ci.yaml", "test"}, {"release.yaml", "race"}} {
		w := load(t, tc.file)
		s := w.step(t, tc.step)
		env, _ := s["environment"].(map[string]any)
		if env["MAGNETICO_TEST_REQUIRE_POSTGRES"] != "1" {
			t.Errorf("%s/%s must set MAGNETICO_TEST_REQUIRE_POSTGRES=1", tc.file, tc.step)
		}
		dsn, _ := env["MAGNETICO_TEST_POSTGRES_DSN"].(string)
		if dsn != "postgres://postgres@postgres:5432/postgres?sslmode=disable" {
			t.Errorf("%s/%s DSN = %q: the harness derives role DSNs without a password, so the service uses trust authentication", tc.file, tc.step, dsn)
		}
		cmds := strings.Join(toStrings(s["commands"]), "\n")
		for _, want := range []string{"-p 3", "-count=1", "skipcensus"} {
			if !strings.Contains(cmds, want) {
				t.Errorf("%s/%s lacks %q", tc.file, tc.step, want)
			}
		}
		if tc.step == "race" && !strings.Contains(cmds, "-race") {
			t.Error("the release test step must run with -race")
		}
		deps, _ := dependsOn(s["depends_on"])
		if !contains(deps, "wait-postgres") {
			t.Errorf("%s/%s must wait for wait-postgres", tc.file, tc.step)
		}
		for _, svc := range w.services() {
			if svc["name"] == "postgres" {
				env, _ := svc["environment"].(map[string]any)
				if env["POSTGRES_HOST_AUTH_METHOD"] != "trust" {
					t.Errorf("%s: the disposable postgres service must use trust authentication", tc.file)
				}
				if img, _ := svc["image"].(string); !strings.HasPrefix(img, "postgres:17") {
					t.Errorf("%s: postgres service image %q must be PostgreSQL 17", tc.file, img)
				}
			}
		}
	}
}

func TestNoAutomaticRerunsAndNoIgnoredFailures(t *testing.T) {
	for _, name := range []string{"ci.yaml", "release.yaml"} {
		body := commentFree(load(t, name).raw)
		for _, k := range []string{"retry", "failure: ignore", "|| true", "continue-on-error"} {
			if strings.Contains(body, k) {
				t.Errorf("%s must not contain %q: failing tests are never hidden", name, k)
			}
		}
	}
}

func TestRehearsalOverridesAreNeverSetByAWorkflow(t *testing.T) {
	for _, name := range []string{"ci.yaml", "release.yaml"} {
		body := load(t, name).raw
		for _, k := range []string{"MAGNETICO_PROMOTE_REHEARSAL", "MAGNETICO_PROMOTE_REPOS"} {
			if strings.Contains(body, k) {
				t.Errorf("%s sets %s: that override exists for local rehearsals only", name, k)
			}
		}
	}
}

func TestReleaseWorkflowPinsCraneAndLimitsItsToolchain(t *testing.T) {
	rel := load(t, "release.yaml")
	p := rel.step(t, "promote-image")
	img, _ := p["image"].(string)
	if !goImage.MatchString(img) {
		t.Errorf("promote-image runs crane through go run and needs a Go image, got %q", img)
	}
}

// Woodpecker substitutes variables into settings and filters only at compile
// time; runtime-only variables (CI_WORKSPACE, CI_PIPELINE_STATUS, ...) become
// empty strings there. Commands run in a shell and may use them as $VAR.
func TestNoRuntimeOnlyVariableIsSubstitutedIntoAWorkflow(t *testing.T) {
	for _, name := range []string{"ci.yaml", "release.yaml"} {
		raw := commentFree(load(t, name).raw)
		for _, v := range []string{"CI_WORKSPACE", "CI_PIPELINE_STATUS"} {
			if strings.Contains(raw, "${"+v) {
				t.Errorf("%s uses ${%s}: it is runtime-only and would be substituted as an empty string", name, v)
			}
		}
	}
}

// -p 3 is the verified configuration because the suite has timing-sensitive
// tests (issues #52, #53) and failures are never rerun: the suites run with
// nothing heavy beside them on the agent.
func TestTimingSensitiveSuitesRunAlone(t *testing.T) {
	ci := load(t, "ci.yaml")
	for _, light := range []string{"fmt", "lint", "vet", "build", "dockerfile-lint"} {
		if !transitivelyDependsOn(ci, "test", light) {
			t.Errorf("ci.yaml: the test step must wait for %s (it must not share the agent with it)", light)
		}
	}
	for _, after := range []string{"openapi", "release-dry-run", "workflow-invariants"} {
		if !transitivelyDependsOn(ci, after, "test") {
			t.Errorf("ci.yaml: %s must run after the test step, not beside it", after)
		}
	}
	rel := load(t, "release.yaml")
	for _, after := range []string{"image-build", "native-build", "govulncheck"} {
		if !transitivelyDependsOn(rel, after, "race") {
			t.Errorf("release.yaml: %s must run after the race suite, not beside it", after)
		}
	}
}
