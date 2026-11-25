package repocheck

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

// The forced rebuild (rebuild.yaml) and the scheduled security check (security.yaml):
// owner decisions after the first Woodpecker runs. The rebuild publishes, so it carries
// the same guarantees as release-docker: the agreed repositories and platforms, no
// registry cache, no latest, credentials in the publishing step only.

func TestWorkflowFilesAreTheKnownSet(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(testkit.ModuleRoot(t), ".woodpecker"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := append([]string(nil), allWorkflows...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf(".woodpecker holds %v, want %v: a new workflow needs its rules here (and in allWorkflows)", got, want)
	}
}

// A manual run with FORCE_REBUILD=true is a rebuild and nothing else; every other manual
// run is a verification run and never a rebuild.
func TestRebuildStartsOnlyOnAForcedManualRun(t *testing.T) {
	when, _ := load(t, "rebuild.yaml").doc["when"].([]any)
	if len(when) != 1 {
		t.Fatalf("rebuild.yaml when = %v, want exactly one condition", when)
	}
	m := when[0].(map[string]any)
	if m["event"] != "manual" || m["evaluate"] != `FORCE_REBUILD == "true"` || len(m) != 2 {
		t.Errorf("rebuild.yaml must start only on a manual run with FORCE_REBUILD=true: %v", m)
	}
	found := false
	for _, e := range load(t, "ci.yaml").doc["when"].([]any) {
		m := e.(map[string]any)
		if events(m)["manual"] {
			found = true
			if m["evaluate"] != `FORCE_REBUILD != "true"` {
				t.Errorf("ci.yaml must leave a forced-rebuild manual run alone: %v", m)
			}
		}
	}
	if !found {
		t.Error("ci.yaml does not run on manual")
	}
	if strings.Contains(commentFree(load(t, "ci.yaml").raw), "cron") {
		t.Error("ci.yaml must not run on cron: scheduled checks are security.yaml")
	}
	if !strings.Contains(readCI(t, "prepare-rebuild.sh"), `case "${FORCE_REBUILD:-}" in`) {
		t.Error("ci/prepare-rebuild.sh must refuse to run without FORCE_REBUILD itself as well")
	}
}

func TestRebuildPublishesLikeTheReleaseWithComputedTagsOnly(t *testing.T) {
	w := load(t, "rebuild.yaml")
	var names []string
	for _, s := range w.steps() {
		names = append(names, s["name"].(string))
	}
	if strings.Join(names, ",") != "prepare-rebuild,rebuild-docker" {
		t.Fatalf("rebuild.yaml steps = %v, want prepare-rebuild and rebuild-docker", names)
	}
	if len(w.services()) != 0 {
		t.Errorf("rebuild.yaml needs no services: %v", w.services())
	}
	if cmd := strings.Join(toStrings(w.step(t, "prepare-rebuild")["commands"]), "\n"); !strings.Contains(cmd, "sh ci/prepare-rebuild.sh") {
		t.Errorf("prepare-rebuild must run ci/prepare-rebuild.sh: %s", cmd)
	}
	s := w.step(t, "rebuild-docker")
	if deps, _ := dependsOn(s["depends_on"]); strings.Join(deps, ",") != "prepare-rebuild" {
		t.Errorf("rebuild-docker must wait for prepare-rebuild, depends_on = %v", deps)
	}
	rel := load(t, "release.yaml").step(t, "release-docker")
	if s["image"] != rel["image"] {
		t.Errorf("rebuild-docker image %v, release-docker %v: the same plugin builds both", s["image"], rel["image"])
	}
	set, _ := s["settings"].(map[string]any)
	relSet, _ := rel["settings"].(map[string]any)
	for _, k := range []string{"repo", "platforms", "dockerfile", "default_labels", "sbom", "provenance", "pull_image", "logins"} {
		if !reflect.DeepEqual(set[k], relSet[k]) {
			t.Errorf("rebuild-docker %s = %v, release-docker %v: a rebuild publishes like the release", k, set[k], relSet[k])
		}
	}
	if set["dry_run"] != nil || set["tags"] != nil {
		t.Error("rebuild-docker must push, with the tags of prepare-rebuild only (no tags setting, no dry_run)")
	}
	if set["no_cache"] != true || set["pull_image"] != true {
		t.Error("rebuild-docker must build without cache and pull the base images: that is the point of a rebuild")
	}
	if set["tags_file"] != ".tags" || set["env_file"] != ".rebuild.env" {
		t.Errorf("rebuild-docker tags_file/env_file = %v/%v, want .tags and .rebuild.env (written by ci/prepare-rebuild.sh)", set["tags_file"], set["env_file"])
	}
	if got := toStrings(set["build_args_from_env"]); strings.Join(got, ",") != "VERSION,COMMIT" {
		t.Errorf("rebuild-docker build_args_from_env = %v, want VERSION and COMMIT", got)
	}
	if regexp.MustCompile(`\blatest\b`).MatchString(commentFree(w.raw)) {
		t.Error("rebuild.yaml applies latest")
	}
}

func TestRebuildAndSecurityCredentials(t *testing.T) {
	if strings.Contains(commentFree(load(t, "security.yaml").raw), "from_secret") {
		t.Error("security.yaml must not reference any secret")
	}
	w := load(t, "rebuild.yaml")
	for _, s := range w.steps() {
		if env, _ := s["environment"].(map[string]any); env != nil {
			t.Errorf("rebuild.yaml step %v sets an environment; the credentials belong to the plugin logins only", s["name"])
		}
	}
	if strings.Contains(text(w.step(t, "prepare-rebuild")), "from_secret") {
		t.Error("prepare-rebuild must not see a secret (the registries are read anonymously)")
	}
	set, _ := w.step(t, "rebuild-docker")["settings"].(map[string]any)
	used := map[string]bool{}
	for _, l := range set["logins"].([]any) {
		for _, f := range []string{"username", "password"} {
			if m, _ := l.(map[string]any)[f].(map[string]any); m != nil {
				used[m["from_secret"].(string)] = true
			}
		}
	}
	want := map[string]bool{"DOCKER_LOGIN": true, "DOCKER_TOKEN": true, "GHCR_LOGIN": true, "GHCR_TOKEN": true}
	if !reflect.DeepEqual(used, want) {
		t.Errorf("rebuild-docker logins use %v, want exactly %v", used, want)
	}
}

// The scheduled check is cheap: no build, no suite, no secret, a pinned scanner, and the same
// govulncheck as the verification workflow.
func TestSecurityCheckIsScheduledCheapAndPinned(t *testing.T) {
	w := load(t, "security.yaml")
	when, _ := w.doc["when"].([]any)
	if len(when) != 1 {
		t.Fatalf("security.yaml when = %v, want exactly one condition", when)
	}
	if m := when[0].(map[string]any); m["event"] != "cron" || m["cron"] != "security-check" {
		t.Errorf("security.yaml must start only on the cron job security-check: %v", m)
	}
	var names []string
	for _, s := range w.steps() {
		names = append(names, s["name"].(string))
	}
	if strings.Join(names, ",") != "govulncheck,release-tag,image-scan" {
		t.Errorf("security.yaml steps = %v", names)
	}
	if len(w.services()) != 0 {
		t.Errorf("security.yaml needs no services: %v", w.services())
	}
	ci := load(t, "ci.yaml").step(t, "govulncheck")
	gv := w.step(t, "govulncheck")
	if !reflect.DeepEqual(gv["commands"], ci["commands"]) || gv["image"] != ci["image"] || !reflect.DeepEqual(gv["environment"], ci["environment"]) {
		t.Error("security.yaml/govulncheck must be the same check as ci.yaml/govulncheck (image, environment, commands)")
	}
	scan := w.step(t, "image-scan")
	if img, _ := scan["image"].(string); !regexp.MustCompile(`^aquasec/trivy:[0-9]+\.[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}$`).MatchString(img) {
		t.Errorf("image-scan image %q must be Trivy pinned by version and digest", img)
	}
	if cmd := strings.Join(toStrings(scan["commands"]), "\n"); !strings.Contains(cmd, "sh ci/scan-release-image.sh .scan-tag") {
		t.Errorf("image-scan must run ci/scan-release-image.sh: %s", cmd)
	}
}

// The script tests skip where git or jq is missing (the test step); workflow-invariants must
// run them, or nothing would.
func TestScriptTestsRunSomewhere(t *testing.T) {
	s := load(t, "ci.yaml").step(t, "workflow-invariants")
	env, _ := s["environment"].(map[string]any)
	cmd := strings.Join(toStrings(s["commands"]), "\n")
	if env["REPOCHECK_REQUIRE_TOOLS"] != "1" || !strings.Contains(cmd, "apk add --no-cache git jq") || !strings.Contains(cmd, "./internal/repocheck") {
		t.Errorf("workflow-invariants must install git and jq and run ./internal/repocheck with REPOCHECK_REQUIRE_TOOLS=1:\n%s", text(s))
	}
}

func readCI(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testkit.ModuleRoot(t), "ci", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
