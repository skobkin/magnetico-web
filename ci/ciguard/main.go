// Command ciguard holds the checks a release pipeline must make before it
// publishes anything. It is CI tooling, not part of the product, and uses only
// the standard library.
//
//	ciguard require-verification --context ci/woodpecker/tag/ci
//
// require-verification is the second line of defence behind the workflow-level
// `depends_on`: it asks the forge for the commit statuses of exactly
// CI_COMMIT_SHA and succeeds only when the newest status of the verification
// workflow's context that belongs to THIS pipeline is "success".
//
// Two things make a status count: its context equals --context exactly (the
// verification WORKFLOW's own context; the pipeline-level aggregate, which stays
// pending while the release workflow itself runs, has another context), and its
// target_url, which Woodpecker sets to the pipeline's URL, is CI_PIPELINE_URL (a
// further path segment, such as a workflow id, is allowed). A green status of an
// EARLIER pipeline for the same commit (a tag re-run, two tags on one commit)
// therefore never substitutes, and neither does an earlier pipeline's failure
// block. A verification that failed, never ran, was filtered out, is still
// pending after the timeout, or belongs to another commit or pipeline all end in
// a non-zero exit. Only status names are printed, never a token.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var (
	releaseTag = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$`)
	commitSHA  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repoName   = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
)

func main() {
	os.Exit(realMain(os.Args[1:], os.Getenv, os.Stdout, os.Stderr, &http.Client{Timeout: 30 * time.Second}))
}

type commitStatus struct {
	ID        int64  `json:"id"`
	Context   string `json:"context"`
	Status    string `json:"status"`
	TargetURL string `json:"target_url"`
}

func realMain(args []string, getenv func(string) string, stdout, stderr io.Writer, client *http.Client) int {
	if len(args) == 0 || args[0] != "require-verification" {
		fmt.Fprintln(stderr, "usage: ciguard require-verification --context <status context> [--timeout 20m] [--missing-timeout 2m] [--interval 15s]")
		return 2
	}
	fs := flag.NewFlagSet("require-verification", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ctxName := fs.String("context", "", "commit status context of the verification workflow (e.g. ci/woodpecker/tag/ci)")
	timeout := fs.Duration("timeout", 20*time.Minute, "how long a pending verification is waited for")
	missing := fs.Duration("missing-timeout", 2*time.Minute, "how long a missing status is waited for before it counts as 'never ran'")
	interval := fs.Duration("interval", 15*time.Second, "polling interval")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *ctxName == "" {
		fmt.Fprintln(stderr, "ciguard: --context is required")
		return 2
	}

	forgeURL := strings.TrimRight(getenv("CI_FORGE_URL"), "/")
	repo, sha, event, tag := getenv("CI_REPO"), getenv("CI_COMMIT_SHA"), getenv("CI_PIPELINE_EVENT"), getenv("CI_COMMIT_TAG")
	switch {
	case event != "tag":
		fmt.Fprintf(stderr, "ciguard: refusing: the pipeline event is %q, publication happens on tag events only\n", event)
		return 1
	case !releaseTag.MatchString(tag):
		fmt.Fprintf(stderr, "ciguard: refusing: tag %q does not match the release grammar (MAJOR.MINOR.PATCH or MAJOR.MINOR.PATCH-rc.N)\n", tag)
		return 1
	case !commitSHA.MatchString(sha):
		fmt.Fprintln(stderr, "ciguard: refusing: CI_COMMIT_SHA is not a full lowercase commit SHA")
		return 1
	case !repoName.MatchString(repo):
		fmt.Fprintln(stderr, "ciguard: refusing: CI_REPO is not owner/name")
		return 1
	case forgeURL == "":
		fmt.Fprintln(stderr, "ciguard: refusing: CI_FORGE_URL is empty")
		return 1
	}
	pipeline, err := parsePipelineURL(getenv("CI_PIPELINE_URL"))
	if err != nil {
		fmt.Fprintf(stderr, "ciguard: refusing: %v\n", err)
		return 1
	}
	endpoint := forgeURL + "/api/v1/repos/" + repo + "/commits/" + url.PathEscape(sha) + "/statuses"
	token := getenv("GUARD_TOKEN")

	start := time.Now()
	apiErrors := 0
	for {
		state, err := newest(client, endpoint, token, *ctxName, pipeline)
		switch {
		case err != nil:
			apiErrors++
			fmt.Fprintf(stderr, "ciguard: could not read the commit statuses (%v)\n", err)
			if apiErrors >= 3 {
				fmt.Fprintln(stderr, "ciguard: giving up: publication is refused when the verification state cannot be established")
				return 1
			}
		case state == "success":
			fmt.Fprintf(stdout, "ciguard: verification %q succeeded for commit %s\n", *ctxName, sha[:12])
			return 0
		case state == "failure" || state == "error" || state == "warning":
			fmt.Fprintf(stderr, "ciguard: verification %q ended with status %q for commit %s: publication refused\n", *ctxName, state, sha[:12])
			return 1
		case state == "":
			apiErrors = 0
			if time.Since(start) >= *missing {
				fmt.Fprintf(stderr, "ciguard: no status %q of this pipeline exists for commit %s: the verification never ran in this pipeline (a status of an earlier pipeline does not count): publication refused\n", *ctxName, sha[:12])
				return 1
			}
		default: // pending
			apiErrors = 0
		}
		if time.Since(start) >= *timeout {
			fmt.Fprintf(stderr, "ciguard: verification %q is still %q after %s: publication refused\n", *ctxName, state, *timeout)
			return 1
		}
		time.Sleep(*interval)
	}
}

// parsePipelineURL validates CI_PIPELINE_URL: an absolute http(s) URL with a host
// and a non-empty path (…/repos/<id>/pipeline/<number>).
func parsePipelineURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return nil, fmt.Errorf("CI_PIPELINE_URL is missing or not an absolute http(s) pipeline URL, so the verification cannot be tied to this pipeline")
	}
	return u, nil
}

// belongsTo reports whether a status target_url is the pipeline's URL or lies
// below it on a path-segment boundary (pipeline 1 is not pipeline 12). Scheme and
// host must match; query and fragment are ignored; dot segments never match.
func belongsTo(target string, pipeline *url.URL) bool {
	u, err := url.Parse(target)
	if target == "" || err != nil || u.Scheme != pipeline.Scheme || u.Host != pipeline.Host {
		return false
	}
	want := strings.TrimRight(pipeline.Path, "/")
	got := u.Path
	if strings.Contains(got, "/../") || strings.HasSuffix(got, "/..") || strings.Contains(got, "/./") {
		return false
	}
	return got == want || strings.HasPrefix(got, want+"/")
}

// newest returns the state of the newest status of ctxName that belongs to the
// pipeline ("" when there is none).
func newest(client *http.Client, endpoint, token, ctxName string, pipeline *url.URL) (string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint+"?limit=100", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var list []commitStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list); err != nil {
		return "", fmt.Errorf("unreadable response")
	}
	var best *commitStatus
	for i := range list {
		if list[i].Context != ctxName || !belongsTo(list[i].TargetURL, pipeline) {
			continue
		}
		if best == nil || list[i].ID > best.ID {
			best = &list[i]
		}
	}
	if best == nil {
		return "", nil
	}
	return best.Status, nil
}
