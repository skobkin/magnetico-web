package repocheck

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/skobkin/magnetico-web/internal/testkit"
)

// The Woodpecker workflows carry the release's security properties. These tests
// parse them and fail the build when an edit would, for example, give pull
// request code a registry credential, publish without the verification workflow,
// apply an alias tag, or build the toolchain below the security floor. They check the
// shape of two ordinary workflows (ci.yaml for every pull request, release.yaml for
// release tags); they are not a release system.

const (
	tagGrammar = `CI_COMMIT_TAG matches "^[0-9]+[.][0-9]+[.][0-9]+"`

	// migrationE2E selects the migration end-to-end tests: the test step skips them and the
	// migration-e2e step runs them, with the same pattern.
	migrationE2E = `'^(TestFault|TestImportWithReadOnlySourceRole|TestMigrateTorrentsBinarySmoke|TestMigrateVerifyRetryBinarySmoke|TestMigrateExportCleanupBinarySmoke)'`
)

// allWorkflows are the workflow files of .woodpecker: verification (ci), publication of a
// release tag (release), forced rebuild of a published image (rebuild) and the scheduled
// security check (security). TestWorkflowFilesAreTheKnownSet keeps the list complete, so a
// new workflow cannot escape the rules that apply to every file.
var allWorkflows = []string{"ci.yaml", "release.yaml", "rebuild.yaml", "security.yaml"}

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
	// Pushes only on master: every other branch is verified by its pull request alone, so
	// no branch gets two pipelines (owner decision).
	var branches []string
	for _, e := range w.doc["when"].([]any) {
		m := e.(map[string]any)
		if m["event"] == "push" {
			switch b := m["branch"].(type) {
			case string:
				branches = append(branches, b)
			case []any:
				branches = append(branches, toStrings(b)...)
			}
		}
	}
	sort.Strings(branches)
	if strings.Join(branches, ",") != "master" {
		t.Errorf("push branches = %v, want master only", branches)
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
	for _, name := range []string{"fmt", "lint", "vet", "build", "test", "migration-e2e", "race", "govulncheck", "openapi", "native-snapshot", "native-smoke"} {
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

// TestHistoryIsThereWhereItIsNeeded: the clone plugin's partial setting defaults to true
// and then overrides depth with 1, a single commit; the first pull-request pipeline failed
// with "refusing to merge unrelated histories" on such a clone. Only two places need
// history: release.yaml (GoReleaser's changelog), which clones it whole, and the
// pull-request merge in ci.yaml, which fetches it (the other ci.yaml steps keep the fast
// single-commit clone, owner decision).
func TestHistoryIsThereWhereItIsNeeded(t *testing.T) {
	w := load(t, "release.yaml")
	clone, _ := w.doc["clone"].(map[string]any)
	git, _ := clone["git"].(map[string]any)
	settings, _ := git["settings"].(map[string]any)
	if d, ok := settings["depth"].(uint64); !ok || d != 0 {
		t.Errorf("release.yaml: clone depth = %v, want 0 (no shallow clone)", settings["depth"])
	}
	if p, ok := settings["partial"].(bool); !ok || p {
		t.Errorf("release.yaml: clone partial = %v, want false (true overrides depth with 1)", settings["partial"])
	}
	cmd := strings.Join(toStrings(load(t, "ci.yaml").step(t, "merge-target")["commands"]), "\n")
	unshallow := strings.Index(cmd, "--unshallow")
	if !strings.Contains(cmd, "--is-shallow-repository") || unshallow < 0 || unshallow > strings.Index(cmd, "git merge") {
		t.Error("ci.yaml/merge-target must fetch the history of a shallow clone before it merges")
	}
}

// TestWorkflowsContainNoBackslash: Woodpecker substitutes variables over the whole file
// before parsing it, and that pass consumes one level of backslashes. A `\\.` in the tag
// expression reached the expression engine as `\.`, an invalid string escape, so neither
// workflow compiled ("invalid char escape (1:32)"). Use a character class such as `[.]`.
func TestWorkflowsContainNoBackslash(t *testing.T) {
	for _, name := range allWorkflows {
		for i, line := range strings.Split(load(t, name).raw, "\n") {
			if strings.Contains(line, `\`) {
				t.Errorf("%s:%d contains a backslash, which Woodpecker's substitution alters: %s", name, i+1, line)
			}
		}
	}
}

// TestTagGrammarAcceptsExactlyReleaseTags runs the pattern of the tag expression through Go's
// regexp, which the expression engine's `matches` uses. A release tag starts with
// MAJOR.MINOR.PATCH; the suffix is deliberately not restricted (owner decision).
func TestTagGrammarAcceptsExactlyReleaseTags(t *testing.T) {
	m := regexp.MustCompile(`^CI_COMMIT_TAG matches "(.*)"$`).FindStringSubmatch(tagGrammar)
	if m == nil {
		t.Fatalf("unexpected tag expression %q", tagGrammar)
	}
	re := regexp.MustCompile(m[1])
	for _, tag := range []string{"0.8.0", "0.8.0-rc1", "10.20.30-rc12", "0.8.0-beta", "0.8.0-rc.1"} {
		if !re.MatchString(tag) {
			t.Errorf("release tag %q is not accepted", tag)
		}
	}
	for _, tag := range []string{"", "0.8", "v0.8.0", "0x8x0", "latest", "release-0.8.0", "0.8-rc1"} {
		if re.MatchString(tag) {
			t.Errorf("tag %q is accepted", tag)
		}
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

func TestNoRegistryCacheIsConfigured(t *testing.T) {
	for _, name := range allWorkflows {
		body := commentFree(load(t, name).raw)
		for _, k := range []string{"cache_from", "cache_to", "cache_images", "--cache-to", "--cache-from"} {
			if strings.Contains(body, k) {
				t.Errorf("%s configures a registry cache (%s): none is configured so no unreviewed workflow can poison one", name, k)
			}
		}
	}
}

var goImage = regexp.MustCompile(`^golang:(\d+)\.(\d+)\.(\d+)`)

func TestEveryStepThatCompilesHonoursTheGoSecurityFloor(t *testing.T) {
	for _, name := range allWorkflows {
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

func TestNoAutomaticRerunsAndNoIgnoredFailures(t *testing.T) {
	for _, name := range allWorkflows {
		body := commentFree(load(t, name).raw)
		for _, k := range []string{"retry", "failure: ignore", "|| true", "continue-on-error"} {
			if strings.Contains(body, k) {
				t.Errorf("%s must not contain %q: failing tests are never hidden", name, k)
			}
		}
	}
}

// Woodpecker substitutes variables into settings and filters only at compile
// time; runtime-only variables (CI_WORKSPACE, CI_PIPELINE_STATUS, ...) become
// empty strings there. Commands run in a shell and may use them as $VAR.
func TestNoRuntimeOnlyVariableIsSubstitutedIntoAWorkflow(t *testing.T) {
	for _, name := range allWorkflows {
		raw := commentFree(load(t, name).raw)
		for _, v := range []string{"CI_WORKSPACE", "CI_PIPELINE_STATUS"} {
			if strings.Contains(raw, "${"+v) {
				t.Errorf("%s uses ${%s}: it is runtime-only and would be substituted as an empty string", name, v)
			}
		}
	}
}

func TestReleaseWorkflowRunsOnReleaseTagsOnly(t *testing.T) {
	w := load(t, "release.yaml")
	ev := events(w.doc["when"])
	if len(ev) != 1 || !ev["tag"] {
		t.Errorf("release.yaml must run on tag events only, runs on %v", ev)
	}
	for _, s := range w.steps() {
		if e := events(s["when"]); len(e) > 0 && !(len(e) == 1 && e["tag"]) {
			t.Errorf("step %v of release.yaml filters on %v", s["name"], e)
		}
	}
}

// The release workflow is the conventional pair of the project's other repositories:
// GoReleaser creates the Forgejo release, the buildx plugin pushes the image.
func TestReleaseWorkflowIsGoReleaserAndTheBuildxPluginOnly(t *testing.T) {
	w := load(t, "release.yaml")
	names := []string{}
	for _, s := range w.steps() {
		names = append(names, s["name"].(string))
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "release-docker,release-forgejo" {
		t.Fatalf("release.yaml steps = %v, want release-docker and release-forgejo", names)
	}
	if len(w.services()) != 0 {
		t.Errorf("release.yaml needs no services: %v", w.services())
	}
	rel := w.step(t, "release-forgejo")
	if img, _ := rel["image"].(string); !regexp.MustCompile(`^goreleaser/goreleaser:v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(img) {
		t.Errorf("release-forgejo image %q must be the pinned GoReleaser image", img)
	}
	if cmd := strings.Join(toStrings(rel["commands"]), "\n"); !strings.Contains(cmd, "goreleaser release --clean") || strings.Contains(cmd, "--skip") || strings.Contains(cmd, "--snapshot") {
		t.Errorf("release-forgejo must run a plain `goreleaser release --clean`: %s", cmd)
	}
}

func TestDockerReleaseKeepsTheAgreedRepositoriesPlatformsAndTag(t *testing.T) {
	s := load(t, "release.yaml").step(t, "release-docker")
	if img, _ := s["image"].(string); !strings.HasPrefix(img, "woodpeckerci/plugin-docker-buildx:") {
		t.Fatalf("release-docker must use the Woodpecker buildx plugin, got %q", img)
	}
	set, _ := s["settings"].(map[string]any)
	if set["dry_run"] != nil {
		t.Error("release-docker must push (no dry_run)")
	}
	repos := toStrings(set["repo"])
	sort.Strings(repos)
	if strings.Join(repos, ",") != "ghcr.io/skobkin/magnetico-web,skobkin/magnetico-web" {
		t.Errorf("repositories = %v, want docker.io (skobkin/magnetico-web) and ghcr.io/skobkin/magnetico-web", repos)
	}
	if got := toStrings(set["platforms"]); strings.Join(got, ",") != "linux/amd64,linux/arm64" {
		t.Errorf("platforms = %v, want linux/amd64 and linux/arm64", got)
	}
	// The release tag and latest (the owner's decision for the first stable release, 0.8.0).
	if got := toStrings(set["tags"]); strings.Join(got, ",") != "${CI_COMMIT_TAG},latest" {
		t.Errorf("tags = %v, want exactly ${CI_COMMIT_TAG} and latest", got)
	}
	if set["default_labels"] != false {
		t.Error("the plugin's automatic labels must stay off (they override the Dockerfile's source label with the Forgejo URL)")
	}
	if set["dockerfile"] != "Dockerfile" {
		t.Errorf("dockerfile = %v", set["dockerfile"])
	}
}

// The buildx plugin (v6) reads build_args as a JSON string map. Woodpecker encodes a
// mapping setting as JSON, but a list as one comma-joined string, which the plugin keeps
// under the key "*": the build then falls back to the Dockerfile defaults (VERSION=dev, an
// empty COMMIT), which is how the 0.8.0-rc0 image was published. The check substitutes the
// variables first, as Woodpecker does over the whole file. Both values must be quoted:
// Woodpecker's parser reads an unquoted SHA such as 1234…567e12 as a number and encodes it
// as a JSON number, which the plugin's string map rejects in the same way (this module's
// YAML library keeps it a string, so the quotes are checked in the text).
func TestDockerReleasePassesVersionAndCommitAsABuildArgumentMap(t *testing.T) {
	raw := load(t, "release.yaml").raw
	for _, l := range []string{`VERSION: "${CI_COMMIT_TAG}"`, `COMMIT: "${CI_COMMIT_SHA}"`} {
		if !strings.Contains(commentFree(raw), l) {
			t.Errorf("release-docker build_args must contain the quoted entry %s", l)
		}
	}
	const tag, sha = "0.8.0-rc1", "1234567890123456789012345678901234567e12"
	sub := strings.NewReplacer("${CI_COMMIT_TAG}", tag, "${CI_COMMIT_SHA}", sha).Replace(raw)
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(sub), &doc); err != nil {
		t.Fatalf("release.yaml after substitution is not valid YAML: %v", err)
	}
	s := workflowFile{name: "release.yaml", raw: sub, doc: doc}.step(t, "release-docker")
	set, _ := s["settings"].(map[string]any)
	args, ok := set["build_args"].(map[string]any)
	if !ok {
		t.Fatalf("release-docker build_args is %T, want a mapping (a list reaches the plugin as the single key \"*\")", set["build_args"])
	}
	want := map[string]any{"VERSION": tag, "COMMIT": sha}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("release-docker build_args = %#v, want %#v (string values: quote them)", args, want)
	}
}

func TestCredentialsAreOnlyInTheirPublicationSteps(t *testing.T) {
	allowed := map[string]string{
		"GITEA_TOKEN": "release-forgejo",
	}
	login := map[string]bool{"DOCKER_LOGIN": true, "DOCKER_TOKEN": true, "GHCR_LOGIN": true, "GHCR_TOKEN": true}
	ci := load(t, "ci.yaml")
	if strings.Contains(commentFree(ci.raw), "from_secret") {
		t.Error("ci.yaml (pull-request code) must not reference any secret")
	}
	rel := load(t, "release.yaml")
	for _, s := range rel.steps() {
		env, _ := s["environment"].(map[string]any)
		for k, v := range env {
			if m, ok := v.(map[string]any); ok && m["from_secret"] != nil {
				if allowed[k] != s["name"] {
					t.Errorf("release.yaml step %v uses the secret %s", s["name"], k)
				}
			}
		}
	}
	set, _ := rel.step(t, "release-docker")["settings"].(map[string]any)
	used := map[string]bool{}
	for _, l := range set["logins"].([]any) {
		for _, f := range []string{"username", "password"} {
			if m, _ := l.(map[string]any)[f].(map[string]any); m != nil {
				used[m["from_secret"].(string)] = true
			}
		}
	}
	if len(used) != len(login) {
		t.Errorf("release-docker logins use %v, want exactly %v", used, login)
	}
	for k := range used {
		if !login[k] {
			t.Errorf("release-docker uses an unexpected secret %s", k)
		}
	}
}

// Privileged execution is a server-side decision for the buildx plugin (it must be listed in
// WOODPECKER_PLUGINS_PRIVILEGED); no workflow asks for it.
func TestNoWorkflowRequestsPrivilegedExecution(t *testing.T) {
	for _, name := range allWorkflows {
		w := load(t, name)
		if strings.Contains(commentFree(w.raw), "privileged") {
			t.Errorf("%s must not request privileged execution", name)
		}
	}
}

// Every gate of the project is in the verification workflow, and all of them run for every
// event except two (owner decision): race never runs for a pull request (it does for
// master, tags, so that a release waits for it, and manual runs), and a pull request
// skips the migration end-to-end tests in test and runs them in migration-e2e only when it
// changes migration paths (every other event runs them inside test).
func TestEveryGateRunsInTheVerificationWorkflow(t *testing.T) {
	w := load(t, "ci.yaml")
	conditional := map[string]bool{"migration-e2e": true, "race": true}
	for _, name := range []string{"fmt", "lint", "vet", "build", "test", "migration-e2e", "race", "govulncheck", "openapi",
		"native-snapshot", "native-smoke", "frontend-selftest", "dockerfile-lint", "workflow-invariants"} {
		s := w.step(t, name)
		if s["when"] != nil && !conditional[name] {
			t.Errorf("ci.yaml/%s has a when filter: it must run for every event", name)
		}
	}
	ev := events(w.step(t, "race")["when"])
	for _, want := range []string{"push", "tag", "manual"} {
		if !ev[want] {
			t.Errorf("ci.yaml/race must run on %s", want)
		}
	}
	if ev := events(w.step(t, "migration-e2e")["when"]); len(ev) != 1 || !ev["pull_request"] {
		t.Errorf("ci.yaml/migration-e2e must run for pull requests only (other events run the tests in test): %v", ev)
	}
	// race never runs for a pull request; migration-e2e only behind its path filter, and
	// then for the migration code at least.
	for _, e := range w.step(t, "race")["when"].([]any) {
		if events(e.(map[string]any))["pull_request"] {
			t.Error("ci.yaml/race must not run for a pull request")
		}
	}
	for _, e := range w.step(t, "migration-e2e")["when"].([]any) {
		m := e.(map[string]any)
		if !events(m)["pull_request"] {
			continue
		}
		if m["path"] == nil {
			t.Error("ci.yaml/migration-e2e runs for every pull request: it must have a path filter")
		} else if !strings.Contains(text(m), "internal/migration/**") {
			t.Errorf("ci.yaml/migration-e2e's path filter must include internal/migration/**: %v", m["path"])
		}
	}
	// Nothing waits on a step that a when filter can leave out: Woodpecker does not document
	// what a required dependency on a filtered-out step does.
	for _, s := range w.steps() {
		deps, _ := dependsOn(s["depends_on"])
		for _, d := range deps {
			if conditional[d] {
				t.Errorf("ci.yaml/%v depends on %s, which does not run for every pull request", s["name"], d)
			}
		}
	}
	// A pull request skips the migration end-to-end tests in test and runs them in their step,
	// with the same pattern, so no test falls between them; every other event runs the whole
	// suite in test.
	cmd := strings.Join(toStrings(w.step(t, "test")["commands"]), "\n")
	pr := regexp.MustCompile(`if \[ "\$CI_PIPELINE_EVENT" = "pull_request" \]; then\s+sh ci/go-test\.sh test [^\n]*-skip ` + regexp.QuoteMeta(migrationE2E) + ` \./\.\.\.\s*\n\s*else\s+sh ci/go-test\.sh test -p 3 -count=1 -timeout 20m \./\.\.\.\s*\n`)
	if !pr.MatchString(cmd) {
		t.Errorf("ci.yaml/test must skip %s for a pull request only and run the whole suite otherwise:\n%s", migrationE2E, cmd)
	}
	if cmd := strings.Join(toStrings(w.step(t, "migration-e2e")["commands"]), "\n"); !strings.Contains(cmd, "-run "+migrationE2E) {
		t.Errorf("ci.yaml/migration-e2e must run %s", migrationE2E)
	}
	if cmd := strings.Join(toStrings(w.step(t, "race")["commands"]), "\n"); !strings.Contains(cmd, "-race") {
		t.Error("the race step must run with -race")
	}
	vc := strings.Join(toStrings(w.step(t, "govulncheck")["commands"]), "\n")
	if !regexp.MustCompile(`govulncheck@v[0-9]+\.[0-9]+\.[0-9]+`).MatchString(vc) || !strings.Contains(vc, "-tags loadtest,diag") {
		t.Errorf("govulncheck must be pinned and cover both tag sets: %s", vc)
	}
}

// -p 3 is the verified configuration because the suite has timing-sensitive tests
// (issues #52, #53) and failures are never rerun: the suites run with nothing heavy beside
// them on the agent, one after the other.
func TestTimingSensitiveSuitesRunAlone(t *testing.T) {
	ci := load(t, "ci.yaml")
	for _, light := range []string{"fmt", "lint", "vet", "build", "dockerfile-lint"} {
		if !transitivelyDependsOn(ci, "test", light) {
			t.Errorf("ci.yaml: the test step must wait for %s (it must not share the agent with it)", light)
		}
	}
	for _, after := range []string{"govulncheck", "openapi", "native-snapshot", "workflow-invariants"} {
		if !transitivelyDependsOn(ci, after, "test") {
			t.Errorf("ci.yaml: %s must run after the test suite, not beside it", after)
		}
	}
	// The migration end-to-end tests (pull requests) and the race suite (everything else)
	// run after everything else: each heavy suite has the agent to itself. The two never run
	// in the same pipeline.
	for _, heavy := range []string{"migration-e2e", "race"} {
		for _, before := range []string{"test", "govulncheck", "openapi", "native-smoke", "workflow-invariants", "frontend-selftest"} {
			if !transitivelyDependsOn(ci, heavy, before) {
				t.Errorf("ci.yaml: %s must run after %s, not beside it", heavy, before)
			}
		}
	}
}

func TestTestStepsRequirePostgresRunTheCensusAndUseVerifiedParallelism(t *testing.T) {
	w := load(t, "ci.yaml")
	for _, step := range []string{"test", "migration-e2e", "race"} {
		s := w.step(t, step)
		env, _ := s["environment"].(map[string]any)
		if env["MAGNETICO_TEST_REQUIRE_POSTGRES"] != "1" {
			t.Errorf("ci.yaml/%s must set MAGNETICO_TEST_REQUIRE_POSTGRES=1", step)
		}
		if dsn, _ := env["MAGNETICO_TEST_POSTGRES_DSN"].(string); dsn != "postgres://postgres@postgres:5432/postgres?sslmode=disable" {
			t.Errorf("ci.yaml/%s DSN = %q: the harness derives role DSNs without a password, so the service uses trust authentication", step, dsn)
		}
		cmds := strings.Join(toStrings(s["commands"]), "\n")
		// -timeout 20m: internal/e2e/cli alone takes 6 to 10 minutes, close to go test's default.
		for _, want := range []string{"-p 3", "-count=1", "-timeout 20m", "skipcensus"} {
			if !strings.Contains(cmds, want) {
				t.Errorf("ci.yaml/%s lacks %q", step, want)
			}
		}
		// ci/go-test.sh shows live progress, keeps the JSON stream and keeps go test's exit
		// status through its pipe; the census then reads the stream that very run kept.
		m := regexp.MustCompile(`sh ci/go-test\.sh (\w+) `).FindAllStringSubmatch(cmds, -1)
		if len(m) == 0 {
			t.Errorf("ci.yaml/%s must run its suite through ci/go-test.sh", step)
		}
		for _, run := range m {
			if !strings.Contains(cmds, "skipcensus --allow ci/skip-allowlist.txt < /tmp/"+run[1]+".json") {
				t.Errorf("ci.yaml/%s: the census must read /tmp/%s.json, the stream ci/go-test.sh kept", step, run[1])
			}
		}
		if strings.Contains(cmds, "go test ") {
			t.Errorf("ci.yaml/%s runs go test directly: its log would be silent until the end", step)
		}
		if deps, _ := dependsOn(s["depends_on"]); !contains(deps, "wait-postgres") && !transitivelyDependsOn(w, step, "wait-postgres") {
			t.Errorf("ci.yaml/%s must (transitively) wait for wait-postgres", step)
		}
	}
	for _, svc := range w.services() {
		if svc["name"] == "postgres" {
			env, _ := svc["environment"].(map[string]any)
			if env["POSTGRES_HOST_AUTH_METHOD"] != "trust" {
				t.Error("the disposable postgres service must use trust authentication")
			}
			if img, _ := svc["image"].(string); !strings.HasPrefix(img, "postgres:17") {
				t.Errorf("postgres service image %q must be PostgreSQL 17", img)
			}
		}
	}
}

// TestFrontendSelfTestsRunInCIWithAPinnedNodeImage (Task 09A): the Node
// self-tests of the administrator charts are part of the regular verification
// workflow, in an image pinned by version and digest, under several time zones;
// Node is never part of a build of the product.
func TestFrontendSelfTestsRunInCIWithAPinnedNodeImage(t *testing.T) {
	w := load(t, "ci.yaml")
	s := w.step(t, "frontend-selftest")
	img, _ := s["image"].(string)
	if !regexp.MustCompile(`^node:[0-9]+(\.[0-9]+)*-alpine@sha256:[0-9a-f]{64}$`).MatchString(img) {
		t.Errorf("the Node image must be pinned by version and digest: %q", img)
	}
	cmd := strings.Join(toStrings(s["commands"]), "\n")
	for _, want := range []string{"internal/web/ui/jstest/admin_test.js", "TZ=America/Los_Angeles", "TZ=Pacific/Kiritimati", "EXPECT_TZ_OFFSET"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("frontend-selftest does not run %q: %s", want, cmd)
		}
	}
	root := testkit.ModuleRoot(t)
	for _, f := range []string{"Dockerfile", ".goreleaser.yaml", ".woodpecker/release.yaml"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`(?i)\bnode(js)?\b|\bnpm\b`).Match(b) {
			t.Errorf("%s mentions Node: Node must not be part of a build or a release", f)
		}
	}
}
