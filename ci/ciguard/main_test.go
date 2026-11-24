package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testSHA   = "0123456789abcdef0123456789abcdef01234567"
	otherSHA  = "fedcba9876543210fedcba9876543210fedcba98"
	testCtx   = "ci/woodpecker/tag/ci"
	testToken = "guard-token-must-never-be-printed"

	// The pipeline the guard runs in, and the URL Woodpecker records as the
	// target of that pipeline's commit statuses.
	pipelineURL = "https://ci.example.org/repos/42/pipeline/7"
	noTarget    = "-none-" // sentinel: the fake forge serves an empty target_url
)

type status struct {
	ID        int64  `json:"id"`
	Context   string `json:"context"`
	Status    string `json:"status"`
	TargetURL string `json:"target_url"`
}

// forge is a fake Forgejo commit-status API: statuses are served per commit SHA.
type forge struct {
	bySHA map[string][][]status // sha -> successive responses (the last one repeats)
	calls atomic.Int32
	code  int // non-zero: every request answers with this status code
	auth  atomic.Value
}

func (f *forge) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.auth.Store(r.Header.Get("Authorization"))
		if f.code != 0 {
			http.Error(w, "boom", f.code)
			return
		}
		const pre = "/api/v1/repos/skobkin/magnetico-web/commits/"
		const post = "/statuses"
		p := r.URL.Path
		if !strings.HasPrefix(p, pre) || !strings.HasSuffix(p, post) {
			http.NotFound(w, r)
			return
		}
		sha := strings.TrimSuffix(strings.TrimPrefix(p, pre), post)
		seq := f.bySHA[sha]
		n := int(f.calls.Add(1)) - 1
		var out []status
		if len(seq) > 0 {
			if n >= len(seq) {
				n = len(seq) - 1
			}
			out = seq[n]
		}
		// Statuses default to the current pipeline's URL; tests that exercise other
		// pipelines set TargetURL explicitly.
		out = append([]status(nil), out...)
		for i := range out {
			switch out[i].TargetURL {
			case "":
				out[i].TargetURL = pipelineURL
			case noTarget:
				out[i].TargetURL = ""
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
}

func run(t *testing.T, f *forge, env map[string]string, extra ...string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	base := map[string]string{
		"CI_FORGE_URL": srv.URL, "CI_REPO": "skobkin/magnetico-web", "CI_COMMIT_SHA": testSHA,
		"CI_PIPELINE_EVENT": "tag", "CI_COMMIT_TAG": "0.8.0-rc.0", "CI_PIPELINE_URL": pipelineURL,
	}
	for k, v := range env {
		base[k] = v
	}
	args := append([]string{"require-verification", "--context", testCtx, "--timeout", "600ms", "--missing-timeout", "300ms", "--interval", "20ms"}, extra...)
	var out bytes.Buffer
	code := realMain(args, func(k string) string { return base[k] }, &out, &out, srv.Client())
	return code, out.String()
}

func TestGuardPassesOnSuccessForSameSHA(t *testing.T) {
	f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: testCtx, Status: "success"}}}}}
	if code, out := run(t, f, nil); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestGuardFailsOnFailureStatus(t *testing.T) {
	f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: testCtx, Status: "failure"}}}}}
	if code, out := run(t, f, nil); code == 0 || !strings.Contains(out, "failure") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestGuardFailsOnErrorAndWarningStatus(t *testing.T) {
	for _, st := range []string{"error", "warning"} {
		f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: testCtx, Status: st}}}}}
		if code, out := run(t, f, nil); code == 0 {
			t.Fatalf("%s must not pass: %s", st, out)
		}
	}
}

// Verification never ran or was filtered out: no status of that context exists.
func TestGuardFailsWhenContextMissing(t *testing.T) {
	f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: "ci/woodpecker/tag/release", Status: "success"}}}}}
	if code, out := run(t, f, nil); code == 0 || !strings.Contains(out, "no status") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestGuardFailsOnStatusForAnotherSHA(t *testing.T) {
	f := &forge{bySHA: map[string][][]status{otherSHA: {{{ID: 1, Context: testCtx, Status: "success"}}}}}
	if code, out := run(t, f, nil); code == 0 {
		t.Fatalf("a green verification of another commit must not pass: %s", out)
	}
}

func TestGuardWaitsThroughPendingThenSucceeds(t *testing.T) {
	f := &forge{bySHA: map[string][][]status{testSHA: {
		{{ID: 1, Context: testCtx, Status: "pending"}},
		{{ID: 1, Context: testCtx, Status: "pending"}},
		{{ID: 1, Context: testCtx, Status: "success"}},
	}}}
	if code, out := run(t, f, nil); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if f.calls.Load() < 3 {
		t.Fatalf("polled %d times, want at least 3", f.calls.Load())
	}
}

func TestGuardFailsOnPendingAtTimeout(t *testing.T) {
	f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: testCtx, Status: "pending"}}}}}
	if code, out := run(t, f, nil); code == 0 || !strings.Contains(out, "pending") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

// The newest status of the context decides (a retry after a failure may pass,
// and a later failure overrides an earlier success).
func TestGuardLatestStatusOfTheContextDecides(t *testing.T) {
	f := &forge{bySHA: map[string][][]status{testSHA: {{
		{ID: 5, Context: testCtx, Status: "success"},
		{ID: 9, Context: testCtx, Status: "failure"},
		{ID: 3, Context: testCtx, Status: "success"},
	}}}}
	if code, out := run(t, f, nil); code == 0 {
		t.Fatalf("the newest status (id 9) is a failure: %s", out)
	}
	f = &forge{bySHA: map[string][][]status{testSHA: {{
		{ID: 2, Context: testCtx, Status: "failure"},
		{ID: 8, Context: testCtx, Status: "success"},
	}}}}
	if code, out := run(t, f, nil); code != 0 {
		t.Fatalf("the newest status (id 8) is a success: exit %d %s", code, out)
	}
}

func TestGuardFailsOnAPIError(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusUnauthorized, http.StatusNotFound} {
		f := &forge{code: code}
		if rc, out := run(t, f, nil); rc == 0 {
			t.Fatalf("HTTP %d must fail the guard: %s", code, out)
		}
	}
}

func TestGuardRejectsNonTagEvent(t *testing.T) {
	f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: testCtx, Status: "success"}}}}}
	for _, ev := range []string{"push", "pull_request", "manual", "cron", ""} {
		if code, out := run(t, f, map[string]string{"CI_PIPELINE_EVENT": ev}); code == 0 {
			t.Fatalf("event %q must be refused: %s", ev, out)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatalf("a refused event must not even query the forge (%d calls)", f.calls.Load())
	}
}

func TestGuardRejectsMalformedTag(t *testing.T) {
	f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: testCtx, Status: "success"}}}}}
	for _, tag := range []string{"0.8.0-rc1", "v0.8.0", "0.8", "latest", "0.8.0-rc.", "0.8.0-beta.1", "0.8.0 ", "", "0.8.0-rc.0\n"} {
		if code, out := run(t, f, map[string]string{"CI_COMMIT_TAG": tag}); code == 0 {
			t.Fatalf("tag %q must be refused: %s", tag, out)
		}
	}
	for _, tag := range []string{"0.8.0", "0.8.0-rc.0", "10.20.30-rc.12", "0.8.1"} {
		if code, out := run(t, f, map[string]string{"CI_COMMIT_TAG": tag}); code != 0 {
			t.Fatalf("tag %q must be accepted: %s", tag, out)
		}
	}
}

func TestGuardRejectsMalformedSHA(t *testing.T) {
	f := &forge{}
	for _, sha := range []string{"", "abc", strings.ToUpper(testSHA), testSHA + "0", "../" + testSHA[:37]} {
		if code, _ := run(t, f, map[string]string{"CI_COMMIT_SHA": sha}); code == 0 {
			t.Fatalf("sha %q must be refused", sha)
		}
	}
}

func TestGuardNeverPrintsTheToken(t *testing.T) {
	f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: testCtx, Status: "failure"}}}}}
	_, out := run(t, f, map[string]string{"GUARD_TOKEN": testToken})
	if strings.Contains(out, testToken) {
		t.Fatalf("token leaked into the output: %s", out)
	}
	if got, _ := f.auth.Load().(string); got != "token "+testToken {
		t.Fatalf("Authorization header = %q, want the token to be sent", got)
	}
}

func TestGuardRejectsUnknownSubcommandAndMissingContext(t *testing.T) {
	var out bytes.Buffer
	env := func(string) string { return "" }
	if code := realMain([]string{"publish"}, env, &out, &out, http.DefaultClient); code == 0 {
		t.Fatal("unknown subcommand must fail")
	}
	if code := realMain([]string{"require-verification"}, env, &out, &out, http.DefaultClient); code == 0 {
		t.Fatal("missing --context must fail")
	}
}

func TestGuardTimeoutBound(t *testing.T) {
	f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: testCtx, Status: "pending"}}}}}
	start := time.Now()
	run(t, f, nil)
	if time.Since(start) > 5*time.Second {
		t.Fatal("the guard ignored its timeout")
	}
}

// ---- the status must belong to THIS pipeline, not merely to this commit ----

// A tag re-run, or two tags on one commit, leaves a green verification of an
// EARLIER pipeline for the same SHA and context. It must not stand in for the
// verification of the pipeline that is about to publish.
func TestGuardIgnoresASuccessOfAnEarlierPipelineOfTheSameCommit(t *testing.T) {
	earlier := "https://ci.example.org/repos/42/pipeline/6"
	f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: testCtx, Status: "success", TargetURL: earlier}}}}}
	code, out := run(t, f, nil)
	if code == 0 {
		t.Fatalf("an earlier pipeline's success must not substitute for this pipeline's verification: %s", out)
	}
	if !strings.Contains(out, "this pipeline") {
		t.Fatalf("the refusal must say that this pipeline has no such status: %s", out)
	}
}

func TestGuardDoesNotPassOnAnEarlierSuccessWhileThisPipelineIsPending(t *testing.T) {
	earlier := "https://ci.example.org/repos/42/pipeline/6"
	f := &forge{bySHA: map[string][][]status{testSHA: {{
		{ID: 1, Context: testCtx, Status: "success", TargetURL: earlier},
		{ID: 2, Context: testCtx, Status: "pending"},
	}}}}
	if code, out := run(t, f, nil); code == 0 || !strings.Contains(out, "pending") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

// A newer success of the earlier pipeline (higher id) must not override this
// pipeline's failure, and an older failure of another pipeline must not block
// this pipeline's success.
func TestGuardJudgesOnlyTheStatusesOfThisPipeline(t *testing.T) {
	earlier := "https://ci.example.org/repos/42/pipeline/6"
	f := &forge{bySHA: map[string][][]status{testSHA: {{
		{ID: 5, Context: testCtx, Status: "failure"},
		{ID: 9, Context: testCtx, Status: "success", TargetURL: earlier},
	}}}}
	if code, out := run(t, f, nil); code == 0 {
		t.Fatalf("this pipeline's failure stands: %s", out)
	}
	f = &forge{bySHA: map[string][][]status{testSHA: {{
		{ID: 5, Context: testCtx, Status: "success"},
		{ID: 9, Context: testCtx, Status: "failure", TargetURL: earlier},
	}}}}
	if code, out := run(t, f, nil); code != 0 {
		t.Fatalf("an earlier pipeline's failure must not block this pipeline's success: exit %d %s", code, out)
	}
}

// Pipeline 1 must not match pipeline 12 (a prefix match without a path boundary
// would), and a workflow suffix after the pipeline number is the same pipeline.
func TestGuardMatchesThePipelineAtAPathBoundary(t *testing.T) {
	cur := "https://ci.example.org/repos/42/pipeline/1"
	for _, target := range []string{
		"https://ci.example.org/repos/42/pipeline/12",
		"https://ci.example.org/repos/42/pipeline/1x",
		"https://ci.example.org/repos/43/pipeline/1",
		"https://other.example.org/repos/42/pipeline/1",
		"http://ci.example.org/repos/42/pipeline/1",
		"https://ci.example.org/repos/42/pipeline/1/../2",
		noTarget,
		"not a url",
	} {
		f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: testCtx, Status: "success", TargetURL: target}}}}}
		if code, out := run(t, f, map[string]string{"CI_PIPELINE_URL": cur}); code == 0 {
			t.Fatalf("target %q must not count as pipeline %s: %s", target, cur, out)
		}
	}
	for _, target := range []string{
		"https://ci.example.org/repos/42/pipeline/1",
		"https://ci.example.org/repos/42/pipeline/1/",
		"https://ci.example.org/repos/42/pipeline/1/3",
		"https://ci.example.org/repos/42/pipeline/1?x=y#z",
	} {
		f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: testCtx, Status: "success", TargetURL: target}}}}}
		if code, out := run(t, f, map[string]string{"CI_PIPELINE_URL": cur}); code != 0 {
			t.Fatalf("target %q is pipeline %s: exit %d %s", target, cur, code, out)
		}
	}
}

// The pipeline-level (aggregate) status, which stays pending while the release
// workflow itself runs, has another context and never substitutes.
func TestGuardNeverAcceptsAnAggregateOrOtherWorkflowStatus(t *testing.T) {
	for _, ctx := range []string{"ci/woodpecker/tag", "ci/woodpecker/tag/", "ci/woodpecker/tag/release", "ci/woodpecker/push/ci", "ci/woodpecker/tag/ci-extra"} {
		f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: ctx, Status: "success"}}}}}
		if code, out := run(t, f, nil); code == 0 {
			t.Fatalf("context %q must not substitute for %q: %s", ctx, testCtx, out)
		}
	}
}

func TestGuardRefusesWithoutAUsablePipelineURLAndDoesNotQueryTheForge(t *testing.T) {
	f := &forge{bySHA: map[string][][]status{testSHA: {{{ID: 1, Context: testCtx, Status: "success"}}}}}
	for _, u := range []string{"", "ci.example.org/repos/42/pipeline/7", "https:///pipeline/7", "ftp://ci.example.org/repos/42/pipeline/7", "https://ci.example.org", "https://ci.example.org/"} {
		if code, out := run(t, f, map[string]string{"CI_PIPELINE_URL": u}); code == 0 {
			t.Fatalf("CI_PIPELINE_URL %q must be refused: %s", u, out)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatalf("a refused environment must not query the forge (%d calls)", f.calls.Load())
	}
}
