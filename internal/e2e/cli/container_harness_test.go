//go:build loadtest

package cli

// Container mode of the load harness (RC0 automated acceptance): the web and ingest
// services under test are containers of a published image instead of locally built
// binaries in systemd scopes. Everything else (the workload, the generators, the
// monitors, the gates) is unchanged. Opt-in, off by default:
//
//	MAGNETICO_BENCH_IMAGE=docker.io/skobkin/magnetico-web@sha256:…   the image under test
//	MAGNETICO_BENCH_BIN=…/magnetico-image-cli    the operator CLI (a wrapper running the same image)
//	MAGNETICO_BENCH_CONTAINER_NETWORK=host       "host" (what the soak uses, see startContainer), or a
//	                                             bridge network that also holds PostgreSQL, with:
//	MAGNETICO_BENCH_CONTAINER_DSN=postgres://…@pg-container:5432/db   MAGNETICO_BENCH_DSN as seen from that network
//	MAGNETICO_BENCH_CONTAINER_TRUSTED_PROXIES=172.20.0.1/32           where published-port traffic comes from
//	MAGNETICO_BENCH_CONTAINER_PREFIX=rc0soak     container names
//
// Each service runs as the documented deployment does: the image's user, a read-only
// root file system, every capability dropped, no-new-privileges, a memory limit with
// no swap, a CPU quota (cpuPercent/100 CPUs) and the service's GOMEMLIMIT. GOMAXPROCS is
// deliberately not set: the Go runtime of the image derives it from the CPU quota. On a
// bridge network the listener is published on the loopback address the harness picked. Secrets (the DSN)
// reach the docker CLI through its environment (`-e NAME`), never its arguments.

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/internal/testkit"
)

func benchImage() string { return os.Getenv("MAGNETICO_BENCH_IMAGE") }

// containerEnvAllowed names the variables passed into a service container: the
// service's own configuration and the Go runtime settings the harness measures with.
func containerEnvAllowed(key string) bool {
	return strings.HasPrefix(key, "MAGNETICO_") || key == "GOMEMLIMIT" || key == "GODEBUG"
}

// startContainer runs the service in a container; see the file comment.
func startContainer(t *testing.T, name, mem string, cpuPercent int, env []string, args ...string) *harnessScoped {
	t.Helper()
	image := benchImage()
	network := os.Getenv("MAGNETICO_BENCH_CONTAINER_NETWORK")
	cdsn := os.Getenv("MAGNETICO_BENCH_CONTAINER_DSN")
	hostDSN := os.Getenv("MAGNETICO_BENCH_DSN")
	// With the host network the service keeps the harness's loopback listener, DSN and
	// trusted proxies unchanged. That is required for the harness's loopback http://
	// public URL (the service refuses development mode on any other listen address)
	// and is the configuration of every earlier harness run.
	hostNet := network == "host"
	if network == "" || (cdsn == "" && !hostNet) {
		t.Fatal("container mode needs MAGNETICO_BENCH_CONTAINER_NETWORK (and MAGNETICO_BENCH_CONTAINER_DSN unless it is host)")
	}
	cname := fmt.Sprintf("%s-%s-%d", envOr("MAGNETICO_BENCH_CONTAINER_PREFIX", "magnetico-bench"), name, harnessScopeSeq.Add(1))
	run := []string{"run", "-d", "--name", cname, "--network", network,
		"--memory", strings.ToLower(mem), "--memory-swap", strings.ToLower(mem), "--cpus", strconv.FormatFloat(float64(cpuPercent)/100, 'f', 2, 64),
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--log-driver", "local", "--log-opt", "max-size=20m", "--log-opt", "max-file=5", "--stop-timeout", "90"}
	var cenv []string
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !containerEnvAllowed(k) {
			continue
		}
		switch {
		case hostNet:
			// unchanged (see above)
		case strings.HasSuffix(k, "_LISTEN"):
			_, port, err := net.SplitHostPort(v)
			if err != nil {
				t.Fatalf("%s=%q: %v", k, v, err)
			}
			v = "0.0.0.0:" + port
			run = append(run, "-p", "127.0.0.1:"+port+":"+port)
		case k == "MAGNETICO_WEB_TRUSTED_PROXIES" || k == "MAGNETICO_INGEST_TRUSTED_PROXIES":
			if p := os.Getenv("MAGNETICO_BENCH_CONTAINER_TRUSTED_PROXIES"); p != "" {
				v = p
			}
		case v == hostDSN:
			v = cdsn
		}
		if k == "MAGNETICO_DIAG_ADDR" {
			continue // the published image has no diagnostic listener
		}
		run = append(run, "-e", k)
		cenv = append(cenv, k+"="+v)
	}
	run = append(append(run, image), args...)
	cmd := exec.Command("docker", run...)
	cmd.Env = append(os.Environ(), cenv...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker run %s: %v\n%s", cname, err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cname).Run() })
	b, err := exec.Command("docker", "inspect", "-f", "{{.State.Pid}}", cname).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", cname, err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	s := &harnessScoped{name: name, container: cname, out: &testkit.SyncBuffer{}, exited: make(chan error, 1), pid: pid, start: time.Now(),
		codeCh: make(chan int, 1), logsDone: make(chan struct{})}
	if cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid)); err == nil {
		s.cg = "/sys/fs/cgroup" + strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(cg)), "0::"))
	}
	if s.cg == "" || pid == 0 {
		t.Fatalf("no cgroup for container %s (pid %d)", cname, pid)
	}
	// The service's output, as the scoped process's pipe would have carried it.
	logs := exec.Command("docker", "logs", "-f", cname)
	logs.Stdout, logs.Stderr = s.out, s.out
	if err := logs.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = logs.Wait(); close(s.logsDone) }()
	go func() {
		out, err := exec.Command("docker", "wait", cname).Output()
		code := -2
		if err == nil {
			code, _ = strconv.Atoi(strings.TrimSpace(string(out)))
		}
		s.codeCh <- code
	}()
	return s
}

// stopContainer is harnessScoped.stop for a container: SIGTERM (docker stop with the
// given grace period), the exit code, then the complete output and removal.
func (s *harnessScoped) stopContainer(within time.Duration) int {
	secs := int(within.Seconds())
	_ = exec.Command("docker", "stop", "-t", strconv.Itoa(secs), s.container).Run()
	code := -1
	select {
	case code = <-s.codeCh:
	case <-time.After(within + 30*time.Second):
		_ = exec.Command("docker", "kill", s.container).Run()
	}
	select {
	case <-s.logsDone:
	case <-time.After(30 * time.Second):
	}
	_ = exec.Command("docker", "rm", "-f", s.container).Run()
	// docker stop ends in SIGKILL after the grace period: report that as the timeout
	// the scoped variant reports.
	if code == 137 {
		return -1
	}
	return code
}

var containerFDCache sync.Map // container name -> fdReading

type fdReading struct {
	at time.Time
	n  int
}

// containerFDs counts the descriptors of the container's PID 1 (the service). The host
// cannot read /proc/PID/fd of another user's process, so it asks inside the container,
// at most once a minute per container.
func containerFDs(name string) int {
	if v, ok := containerFDCache.Load(name); ok && time.Since(v.(fdReading).at) < time.Minute {
		return v.(fdReading).n
	}
	n := -1
	if b, err := exec.Command("docker", "exec", name, "sh", "-c", "ls /proc/1/fd | wc -l").Output(); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	containerFDCache.Store(name, fdReading{time.Now(), n})
	return n
}
