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
)

type status struct {
	ID      int64  `json:"id"`
	Context string `json:"context"`
	Status  string `json:"status"`
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
		_ = json.NewEncoder(w).Encode(out)
	})
}

func run(t *testing.T, f *forge, env map[string]string, extra ...string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	base := map[string]string{
		"CI_FORGE_URL": srv.URL, "CI_REPO": "skobkin/magnetico-web", "CI_COMMIT_SHA": testSHA,
		"CI_PIPELINE_EVENT": "tag", "CI_COMMIT_TAG": "0.8.0-rc.0",
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
