package repocheck

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

// localForge is a disposable fixture of the Forgejo release API that also stores
// the uploads and serves them back as downloads, so that the publisher and the
// verifier can be run against each other without a real forge or registry.
type localForge struct {
	mu      sync.Mutex
	tag     string
	release map[string]any
	assets  map[string][]byte
}

func (f *localForge) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	const api = "/api/v1/repos/skobkin/magnetico-web"
	mux.HandleFunc(api+"/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.release == nil || r.URL.Path != api+"/releases/tags/"+f.tag {
			http.NotFound(w, r)
			return
		}
		f.write(w)
	})
	mux.HandleFunc(api+"/releases", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&f.release)
		f.tag = fmt.Sprint(f.release["tag_name"])
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":7}`)
	})
	mux.HandleFunc(api+"/releases/7/assets", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		file, _, err := r.FormFile("attachment")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		data, _ := io.ReadAll(file)
		f.mu.Lock()
		f.assets[r.URL.Query().Get("name")] = data
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":1}`)
	})
	mux.HandleFunc(api+"/releases/7", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodPatch {
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			for k, v := range p {
				f.release[k] = v
			}
		}
		f.write(w)
	})
	mux.HandleFunc("/skobkin/magnetico-web/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		data, ok := f.assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	})
	return mux
}

// write answers with the release as the API shows it; the caller holds f.mu.
func (f *localForge) write(w http.ResponseWriter) {
	var as []string
	for name, data := range f.assets {
		as = append(as, fmt.Sprintf(`{"id":1,"name":%q,"size":%d}`, name, len(data)))
	}
	fmt.Fprintf(w, `{"id":7,"tag_name":%q,"draft":%v,"prerelease":%v,"assets":[%s]}`, f.tag, f.release["draft"], f.release["prerelease"], strings.Join(as, ","))
}

// TestPublishThenVerifyNativeRehearsal runs the real publisher and the real
// verifier against each other on a local fixture: what the publisher uploads is
// accepted by the verifier, and a stored archive that is changed afterwards is
// rejected by it.
func TestPublishThenVerifyNativeRehearsal(t *testing.T) {
	root := testkit.ModuleRoot(t)
	bin := filepath.Join(t.TempDir(), "publishnative")
	build := exec.Command("go", "build", "-o", bin, "./ci/publishnative")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	tag := "0.8.0-rc.0"
	dist := t.TempDir()
	var sums strings.Builder
	for _, arch := range []string{"amd64", "arm64"} {
		name := fmt.Sprintf("magnetico-web_%s_linux_%s.tar.gz", tag, arch)
		data := []byte("qualified bytes of " + name + "\n")
		if err := os.WriteFile(filepath.Join(dist, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), name)
	}
	qualified := filepath.Join(t.TempDir(), "qualified-checksums.txt")
	for _, p := range []string{filepath.Join(dist, "checksums.txt"), qualified} {
		if err := os.WriteFile(p, []byte(sums.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	forge := &localForge{assets: map[string][]byte{}}
	srv := httptest.NewServer(forge.handler(t))
	defer srv.Close()
	env := append(os.Environ(), "CI_FORGE_URL="+srv.URL, "CI_REPO=skobkin/magnetico-web", "CI_COMMIT_SHA=0123456789abcdef0123456789abcdef01234567", "GITEA_TOKEN=rehearsal-token")

	pub := exec.Command(bin, "--tag", tag, "--dist", dist, "--qualified", qualified)
	pub.Env = env
	if out, err := pub.CombinedOutput(); err != nil || strings.Contains(string(out), "rehearsal-token") {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	verify := func() (string, error) {
		v := exec.Command("sh", filepath.Join(root, "ci", "verify-native.sh"), tag, qualified)
		v.Env = env
		out, err := v.CombinedOutput()
		return string(out), err
	}
	if out, err := verify(); err != nil {
		t.Fatalf("what was published must verify: %v\n%s", err, out)
	}
	forge.mu.Lock()
	forge.assets["magnetico-web_"+tag+"_linux_amd64.tar.gz"] = []byte("replaced after publication\n")
	forge.mu.Unlock()
	if out, err := verify(); err == nil || !strings.Contains(out, "differs") {
		t.Fatalf("a replaced archive must fail verification: %v\n%s", err, out)
	}
	// A second publication of the same tag changes nothing.
	pub2 := exec.Command(bin, "--tag", tag, "--dist", dist, "--qualified", qualified)
	pub2.Env = env
	if out, err := pub2.CombinedOutput(); err == nil || !strings.Contains(string(out), "already exists") {
		t.Fatalf("a second publication must be refused: %v\n%s", err, out)
	}
}
