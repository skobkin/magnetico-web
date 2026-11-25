package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	token = "fixture-token-do-not-leak"
	sha   = "0123456789abcdef0123456789abcdef01234567"
)

// fakeForge is a disposable stand-in for the Forgejo release API: it records
// every request and keeps one release.
type fakeForge struct {
	mu              sync.Mutex
	srv             *httptest.Server
	calls           []string
	existing        bool  // a release for the tag already exists
	failUpload      int   // 1-based index of the upload that answers 500 (0: none)
	forcePrerelease *bool // the read-back reports this instead of what was sent
	release         map[string]any
	assets          map[string][]byte
	uploads         int
	badAuth         []string
}

func newForge(t *testing.T) *fakeForge {
	t.Helper()
	f := &fakeForge{assets: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/skobkin/magnetico-web/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		if f.existing {
			fmt.Fprint(w, `{"id":9,"draft":false}`)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/v1/repos/skobkin/magnetico-web/releases", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.release = body
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":7}`)
	})
	mux.HandleFunc("/api/v1/repos/skobkin/magnetico-web/releases/7/assets", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		f.uploads++
		n := f.uploads
		f.mu.Unlock()
		if f.failUpload == n {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		file, _, err := r.FormFile("attachment")
		if err != nil {
			http.Error(w, "no attachment", 400)
			return
		}
		data, _ := io.ReadAll(file)
		f.mu.Lock()
		f.assets[r.URL.Query().Get("name")] = data
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":1}`)
	})
	mux.HandleFunc("/api/v1/repos/skobkin/magnetico-web/releases/7", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodPatch {
			var patch map[string]any
			_ = json.NewDecoder(r.Body).Decode(&patch)
			for k, v := range patch {
				f.release[k] = v
			}
		}
		var as []string
		for name, data := range f.assets {
			as = append(as, fmt.Sprintf(`{"name":%q,"size":%d}`, name, len(data)))
		}
		pre := f.release["prerelease"]
		if f.forcePrerelease != nil {
			pre = *f.forcePrerelease
		}
		fmt.Fprintf(w, `{"id":7,"draft":%v,"prerelease":%v,"tag_name":%q,"assets":[%s]}`, f.release["draft"], pre, f.release["tag_name"], strings.Join(as, ","))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeForge) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if got := r.Header.Get("Authorization"); got != "token "+token {
		f.badAuth = append(f.badAuth, r.Method+" "+r.URL.Path)
	}
}

type dist struct{ dir, qualified, notes string }

func makeDist(t *testing.T, tag string) dist {
	t.Helper()
	d := dist{dir: t.TempDir()}
	var sums strings.Builder
	for _, arch := range []string{"amd64", "arm64"} {
		name := fmt.Sprintf("magnetico-web_%s_linux_%s.tar.gz", tag, arch)
		data := []byte("archive bytes of " + name + "\n")
		if err := os.WriteFile(filepath.Join(d.dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), name)
	}
	if err := os.WriteFile(filepath.Join(d.dir, "checksums.txt"), []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	d.qualified = filepath.Join(t.TempDir(), "qualified-checksums.txt")
	if err := os.WriteFile(d.qualified, []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	d.notes = filepath.Join(d.dir, "CHANGELOG.md")
	if err := os.WriteFile(d.notes, []byte("## Features\n* something\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func run(t *testing.T, f *fakeForge, d dist, tag string, env map[string]string) (string, int) {
	t.Helper()
	e := map[string]string{"CI_FORGE_URL": f.srv.URL, "CI_REPO": "skobkin/magnetico-web", "CI_COMMIT_SHA": sha, "GITEA_TOKEN": token}
	for k, v := range env {
		e[k] = v
	}
	var out bytes.Buffer
	code := realMain([]string{"--tag", tag, "--dist", d.dir, "--qualified", d.qualified, "--notes", d.notes},
		func(k string) string { return e[k] }, &out, &out, f.srv.Client())
	return out.String(), code
}

func TestPublishesTheQualifiedArchivesAsADraftThenPublishesWithPrereleaseForRC(t *testing.T) {
	f := newForge(t)
	tag := "0.8.0-rc.0"
	d := makeDist(t, tag)
	out, code := run(t, f, d, tag, nil)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	want := []string{
		"GET /api/v1/repos/skobkin/magnetico-web/releases/tags/" + tag,
		"POST /api/v1/repos/skobkin/magnetico-web/releases",
		"POST /api/v1/repos/skobkin/magnetico-web/releases/7/assets",
		"POST /api/v1/repos/skobkin/magnetico-web/releases/7/assets",
		"POST /api/v1/repos/skobkin/magnetico-web/releases/7/assets",
		"PATCH /api/v1/repos/skobkin/magnetico-web/releases/7",
		"GET /api/v1/repos/skobkin/magnetico-web/releases/7",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if len(f.badAuth) != 0 {
		t.Fatalf("requests without the token: %v", f.badAuth)
	}
	if f.release["draft"] != false || f.release["prerelease"] != true || f.release["name"] != tag || f.release["tag_name"] != tag || f.release["target_commitish"] != sha {
		t.Fatalf("release metadata: %v", f.release)
	}
	if !strings.Contains(fmt.Sprint(f.release["body"]), "something") {
		t.Fatalf("the release notes were not published: %v", f.release["body"])
	}
	for name := range f.assets {
		onDisk, _ := os.ReadFile(filepath.Join(d.dir, name))
		if !bytes.Equal(f.assets[name], onDisk) {
			t.Errorf("asset %s is not the qualified file", name)
		}
	}
	if len(f.assets) != 3 {
		t.Fatalf("assets: %d, want both archives and checksums.txt", len(f.assets))
	}
	if strings.Contains(out, token) {
		t.Fatal("the token appears in the output")
	}
}

func TestAStableTagIsNotAPrerelease(t *testing.T) {
	f := newForge(t)
	d := makeDist(t, "0.8.0")
	if out, code := run(t, f, d, "0.8.0", nil); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if f.release["prerelease"] != false {
		t.Fatalf("a stable release was created with prerelease=%v", f.release["prerelease"])
	}
}

func TestRefusesBeforeAnyRequestWhenTheFilesAreNotTheQualifiedBuild(t *testing.T) {
	tag := "0.8.0-rc.0"
	for name, mutate := range map[string]func(d dist){
		"archive differs": func(d dist) {
			_ = os.WriteFile(filepath.Join(d.dir, "magnetico-web_"+tag+"_linux_amd64.tar.gz"), []byte("rebuilt\n"), 0o644)
		},
		"checksums.txt differs": func(d dist) { _ = os.WriteFile(filepath.Join(d.dir, "checksums.txt"), []byte("other\n"), 0o644) },
		"arm64 archive missing": func(d dist) { _ = os.Remove(filepath.Join(d.dir, "magnetico-web_"+tag+"_linux_arm64.tar.gz")) },
		"qualified file lacks an archive": func(d dist) {
			b, _ := os.ReadFile(d.qualified)
			_ = os.WriteFile(d.qualified, []byte(strings.SplitN(string(b), "\n", 2)[0]+"\n"), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newForge(t)
			d := makeDist(t, tag)
			mutate(d)
			out, code := run(t, f, d, tag, nil)
			if code == 0 {
				t.Fatalf("published something that is not the qualified build:\n%s", out)
			}
			if len(f.calls) != 0 {
				t.Fatalf("requests were made before the files were verified: %v", f.calls)
			}
		})
	}
}

func TestNeverOverwritesAnExistingRelease(t *testing.T) {
	f := newForge(t)
	f.existing = true
	tag := "0.8.0-rc.0"
	out, code := run(t, f, makeDist(t, tag), tag, nil)
	if code == 0 || !strings.Contains(out, "already exists") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Fatalf("a write was attempted against an existing release: %v", f.calls)
		}
	}
}

func TestAFailedUploadLeavesAnInvisibleDraftAndSaysHowToRecover(t *testing.T) {
	f := newForge(t)
	f.failUpload = 2
	tag := "0.8.0-rc.0"
	out, code := run(t, f, makeDist(t, tag), tag, nil)
	if code == 0 {
		t.Fatalf("a failed upload must fail the step:\n%s", out)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "PATCH ") {
			t.Fatalf("the release was published although an asset is missing: %v", f.calls)
		}
	}
	if f.release["draft"] != true {
		t.Fatalf("the half-uploaded release must remain a draft: %v", f.release)
	}
	if !strings.Contains(out, "draft") || !strings.Contains(out, "delete") || strings.Contains(out, token) {
		t.Fatalf("recovery advice missing or token leaked:\n%s", out)
	}
}

func TestFailsWhenTheReadBackDoesNotShowTheIntendedPrereleaseState(t *testing.T) {
	f := newForge(t)
	no := false
	f.forcePrerelease = &no
	tag := "0.8.0-rc.0"
	if out, code := run(t, f, makeDist(t, tag), tag, nil); code == 0 || !strings.Contains(out, "prerelease") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestRefusesBadInputsWithoutAnyRequest(t *testing.T) {
	tag := "0.8.0-rc.0"
	cases := map[string]struct {
		tag string
		env map[string]string
	}{
		"tag outside the grammar": {tag: "latest"},
		"rc without number":       {tag: "0.8.0-rc"},
		"no token":                {tag: tag, env: map[string]string{"GITEA_TOKEN": ""}},
		"plain http to a remote":  {tag: tag, env: map[string]string{"CI_FORGE_URL": "http://git.example.org"}},
		"bad repository":          {tag: tag, env: map[string]string{"CI_REPO": "../../etc"}},
		"bad commit":              {tag: tag, env: map[string]string{"CI_COMMIT_SHA": "HEAD"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newForge(t)
			d := makeDist(t, tag)
			out, code := run(t, f, d, c.tag, c.env)
			if code == 0 || len(f.calls) != 0 {
				t.Fatalf("exit %d, calls %v:\n%s", code, f.calls, out)
			}
		})
	}
}
