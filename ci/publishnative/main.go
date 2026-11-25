// Command publishnative publishes the native release archives that the release
// gate qualified, byte for byte, as a Forgejo release. It is CI tooling, not part
// of the product, and uses only the standard library.
//
//	publishnative --tag 0.8.0-rc.0 --dist dist --qualified .ci/qualified-checksums.txt --notes dist/CHANGELOG.md
//
// GoReleaser OSS cannot publish a build it has already made, so the workflow used
// to rebuild and rely on reproducibility. This tool uploads the very files that
// were qualified instead:
//
//  1. Before any request, the files are verified: checksums.txt in the dist
//     directory equals the qualified checksums byte for byte and both Linux
//     archives hash to the qualified values. Nothing else is ever uploaded.
//  2. A release that already exists for the tag is never touched.
//  3. The release is created as a DRAFT (invisible), the three assets are
//     uploaded, and only then is it published, so a failure never leaves a
//     visible release with missing files. The release metadata matches what
//     GoReleaser produced: name and tag are the release tag, the body is the
//     generated changelog, and a release candidate (MAJOR.MINOR.PATCH-rc.N) is a
//     prerelease, a stable tag is not.
//  4. The release is read back and must be published, with the intended
//     prerelease state and every asset at its local size. ci/verify-native.sh
//     later downloads the archives and compares their hashes.
//
// A failed run leaves a draft and says how to remove it; the tool never deletes
// anything. The token goes only to the forge's API, in the Authorization header,
// and is never printed. Reads CI_FORGE_URL, CI_REPO, CI_COMMIT_SHA, GITEA_TOKEN.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	releaseTag = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$`)
	commitSHA  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repoName   = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	hexSum     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func main() {
	os.Exit(realMain(os.Args[1:], os.Getenv, os.Stdout, os.Stderr, &http.Client{Timeout: 5 * time.Minute}))
}

// asset is one file to upload. Its bytes are read ONCE, when the files are verified, and
// those very bytes are uploaded: nothing that changes on disk afterwards can reach the forge.
type asset struct {
	name string
	data []byte
	size int64
}

func realMain(args []string, getenv func(string) string, stdout, stderr io.Writer, client *http.Client) int {
	fs := flag.NewFlagSet("publishnative", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tag := fs.String("tag", "", "release tag (MAJOR.MINOR.PATCH or MAJOR.MINOR.PATCH-rc.N)")
	dist := fs.String("dist", "dist", "directory holding the qualified archives and checksums.txt")
	qualified := fs.String("qualified", "", "the checksums file the release gate qualified")
	notes := fs.String("notes", "", "release notes (the generated changelog); may be empty")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "publishnative: "+format+"\n", a...)
		return 1
	}

	// ---- inputs -----------------------------------------------------------------
	if !releaseTag.MatchString(*tag) {
		return fail("refusing tag %q: only MAJOR.MINOR.PATCH or MAJOR.MINOR.PATCH-rc.N is published", *tag)
	}
	repo, sha, tok := getenv("CI_REPO"), getenv("CI_COMMIT_SHA"), getenv("GITEA_TOKEN")
	if !repoName.MatchString(repo) {
		return fail("CI_REPO is not owner/name")
	}
	if !commitSHA.MatchString(sha) {
		return fail("CI_COMMIT_SHA is not a full commit hash")
	}
	if tok == "" {
		return fail("GITEA_TOKEN is required")
	}
	base, err := forgeBase(getenv("CI_FORGE_URL"))
	if err != nil {
		return fail("%v", err)
	}
	prerelease := strings.Contains(*tag, "-rc.")

	// ---- the files, before any request -----------------------------------------
	assets, err := verifyFiles(*tag, *dist, *qualified)
	if err != nil {
		return fail("%v", err)
	}
	body := ""
	if *notes != "" { // asked for: it must be readable, a release without its body is not what was promised
		b, err := os.ReadFile(*notes)
		if err != nil {
			return fail("the release notes %s cannot be read: %v", *notes, err)
		}
		body = string(b)
	}

	api := &forge{base: base + "/api/v1/repos/" + repo, token: tok, client: client}

	// ---- never overwrite --------------------------------------------------------
	status, _, err := api.do(http.MethodGet, "/releases/tags/"+url.PathEscape(*tag), "", nil)
	if err != nil {
		return fail("cannot ask the forge about release %s: %v", *tag, err)
	}
	switch status {
	case http.StatusOK:
		return fail("a release for %s already exists on the forge: nothing was changed. If it is a draft left by an earlier failed run, delete that draft in the forge and run again; a published release is never replaced", *tag)
	case http.StatusNotFound:
	default:
		return fail("the forge answered %d when asked about release %s", status, *tag)
	}

	// ---- draft, assets, publish -------------------------------------------------
	create, _ := json.Marshal(map[string]any{
		"tag_name": *tag, "target_commitish": sha, "name": *tag, "body": body, "draft": true, "prerelease": prerelease,
	})
	status, resp, err := api.do(http.MethodPost, "/releases", "application/json", create)
	if err != nil || status != http.StatusCreated {
		return fail("creating the draft release failed (%s)", describe(status, err))
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(resp, &created) != nil || created.ID == 0 {
		return fail("the forge did not return the id of the new release")
	}
	rel := fmt.Sprintf("/releases/%d", created.ID)
	recovery := fmt.Sprintf("the release %s is left as a DRAFT (invisible to users) and nothing was published: delete that draft in the forge, then run the step again", *tag)

	for _, a := range assets {
		if err := upload(api, rel, a); err != nil {
			return fail("uploading %s failed (%v); %s", a.name, err, recovery)
		}
		fmt.Fprintf(stdout, "uploaded %s (%d bytes)\n", a.name, a.size)
	}
	patch, _ := json.Marshal(map[string]any{"draft": false, "prerelease": prerelease})
	if status, _, err = api.do(http.MethodPatch, rel, "application/json", patch); err != nil || status != http.StatusOK {
		// The reply may have been lost after the release was published: look before advising.
		if !publishedAlready(api, rel) {
			return fail("publishing the release failed (%s); %s", describe(status, err), recovery)
		}
		fmt.Fprintf(stdout, "the reply to the publishing request was lost (%s) but the release is published; checking it\n", describe(status, err))
	}

	// ---- read back --------------------------------------------------------------
	status, resp, err = api.do(http.MethodGet, rel, "", nil)
	if err != nil || status != http.StatusOK {
		return fail("reading the release back failed (%s)", describe(status, err))
	}
	if err := checkReadBack(resp, prerelease, assets); err != nil {
		return fail("the published release is not as intended: %v", err)
	}
	kind := "stable release"
	if prerelease {
		kind = "prerelease"
	}
	fmt.Fprintf(stdout, "published release %s (%s) with the qualified archives and checksums.txt\n", *tag, kind)
	return 0
}

// forgeBase validates the forge URL: https, or http only for a loopback host
// (the local rehearsal), with no credentials, query or fragment.
func forgeBase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("CI_FORGE_URL is not a plain URL")
	}
	loopback := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && loopback:
	default:
		return "", errors.New("CI_FORGE_URL must be https (plain http is accepted only for a loopback address)")
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// verifyFiles checks that dist holds exactly the qualified build and returns the
// three assets to upload.
func verifyFiles(tag, dist, qualifiedPath string) ([]asset, error) {
	if qualifiedPath == "" {
		return nil, errors.New("--qualified is required")
	}
	qualified, err := os.ReadFile(qualifiedPath)
	if err != nil {
		return nil, fmt.Errorf("no qualified checksums file: %v", err)
	}
	listed := map[string]string{}
	for _, line := range strings.Split(string(qualified), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			listed[f[1]] = f[0]
		}
	}
	published, err := os.ReadFile(filepath.Join(dist, "checksums.txt"))
	if err != nil {
		return nil, fmt.Errorf("no checksums.txt in %s: %v", dist, err)
	}
	if !bytes.Equal(published, qualified) {
		return nil, errors.New("checksums.txt in the dist directory is not the qualified one: this is not the qualified build")
	}
	var out []asset
	for _, arch := range []string{"amd64", "arm64"} {
		name := fmt.Sprintf("magnetico-web_%s_linux_%s.tar.gz", tag, arch)
		want := listed[name]
		if !hexSum.MatchString(want) {
			return nil, fmt.Errorf("the qualified checksums do not list %s", name)
		}
		p := filepath.Join(dist, name)
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("the qualified archive %s is missing: %v", name, err)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != want {
			return nil, fmt.Errorf("%s differs from the qualified one (sha256 %s, qualified %s): this is not the qualified build", name, got, want)
		}
		out = append(out, asset{name: name, data: data, size: int64(len(data))})
	}
	out = append(out, asset{name: "checksums.txt", data: published, size: int64(len(published))})
	return out, nil
}

type forge struct {
	base, token string
	client      *http.Client
}

// do sends one API request; the token is only ever put in the header.
func (f *forge) do(method, path, contentType string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(method, f.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "token "+f.token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return f.send(req)
}

func (f *forge) send(req *http.Request) (int, []byte, error) {
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, nil, errors.New(redact(err.Error(), f.token))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

func upload(api *forge, rel string, a asset) error {
	data := a.data
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("attachment", a.name)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	status, _, err := api.do(http.MethodPost, rel+"/assets?name="+url.QueryEscape(a.name), mw.FormDataContentType(), buf.Bytes())
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("HTTP %d", status)
	}
	return nil
}

func checkReadBack(resp []byte, prerelease bool, assets []asset) error {
	var got struct {
		Draft      bool `json:"draft"`
		Prerelease bool `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		return errors.New("the answer is not readable")
	}
	if got.Draft {
		return errors.New("it is still a draft")
	}
	if got.Prerelease != prerelease {
		return fmt.Errorf("prerelease is %v, want %v", got.Prerelease, prerelease)
	}
	sizes := map[string]int64{}
	for _, a := range got.Assets {
		sizes[a.Name] = a.Size
	}
	for _, a := range assets {
		size, ok := sizes[a.name]
		switch {
		case !ok:
			return fmt.Errorf("asset %s is missing", a.name)
		case size != a.size:
			return fmt.Errorf("asset %s has %d bytes, the qualified file has %d", a.name, size, a.size)
		}
	}
	return nil
}

func describe(status int, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("HTTP %d", status)
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

// publishedAlready reports whether the release is public (not a draft).
func publishedAlready(api *forge, rel string) bool {
	status, resp, err := api.do(http.MethodGet, rel, "", nil)
	if err != nil || status != http.StatusOK {
		return false
	}
	var got struct {
		Draft bool `json:"draft"`
	}
	return json.Unmarshal(resp, &got) == nil && !got.Draft
}
