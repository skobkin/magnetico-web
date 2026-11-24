// Package testkit holds the small helpers shared by the real-binary test suites:
// building the magnetico executable once per tag set, running a process with its
// output captured safely, and a goroutine-safe log sink. It is imported by test
// files only and is not linked into any production binary.
package testkit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	buildMu  sync.Mutex
	buildDir string
	built    = map[string]string{} // sorted tag list -> binary path
	rootOnce sync.Once
	rootDir  string
	rootErr  error
)

// ModuleRoot returns the directory of the main module (the repository root).
func ModuleRoot(t testing.TB) string {
	t.Helper()
	rootOnce.Do(func() {
		out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
		rootDir, rootErr = strings.TrimSpace(string(out)), err
	})
	if rootErr != nil {
		t.Fatalf("locate the module root: %v", rootErr)
	}
	return rootDir
}

// Binary builds ./cmd/magnetico with the given build tags (none for the release
// shape) and returns the path of the executable. The same tag set is built once
// per test process. Call Cleanup from TestMain to remove the build directory.
func Binary(t testing.TB, tags ...string) string {
	t.Helper()
	key := strings.Join(tags, ",")
	buildMu.Lock()
	defer buildMu.Unlock()
	if p, ok := built[key]; ok {
		return p
	}
	if buildDir == "" {
		dir, err := os.MkdirTemp("", "magnetico-testkit-")
		if err != nil {
			t.Fatalf("build directory: %v", err)
		}
		buildDir = dir
	}
	out := filepath.Join(buildDir, "magnetico-"+strings.NewReplacer(",", "_", " ", "_").Replace(key)+"-"+strconv.Itoa(len(built)))
	args := []string{"build"}
	if key != "" {
		args = append(args, "-tags", key)
	}
	args = append(args, "-o", out, "./cmd/magnetico")
	cmd := exec.Command("go", args...)
	cmd.Dir = ModuleRoot(t)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %v: %v\n%s", args, err, b)
	}
	built[key] = out
	return out
}

// Cleanup removes the directory Binary built into.
func Cleanup() {
	buildMu.Lock()
	defer buildMu.Unlock()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
		buildDir = ""
		built = map[string]string{}
	}
}

// RunOptions describes one process run.
type RunOptions struct {
	Args  []string
	Env   []string // appended to the current environment
	Stdin string
	// Timeout kills the process (default two minutes).
	Timeout time.Duration
	// SignalAfter, when positive, delivers SIGTERM after that delay (an
	// already-exited process is left alone).
	SignalAfter time.Duration
}

// RunResult is the outcome of Run. Output is stdout and stderr combined.
type RunResult struct {
	ExitCode int
	Output   string
}

// Run executes bin and waits for it. A process killed by the timeout or by a
// signal it did not handle reports a non-zero ExitCode (-1 when the OS gives no
// code). The output buffer is guarded, so it may be read while the process runs
// only through the returned result after Run returns.
func Run(t testing.TB, bin string, opts RunOptions) RunResult {
	t.Helper()
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, opts.Args...)
	// A killed process may leave children holding the output pipes open; do not
	// wait for them forever.
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(os.Environ(), opts.Env...)
	if opts.Stdin != "" {
		cmd.Stdin = strings.NewReader(opts.Stdin)
	}
	var out SyncBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	var timer *time.Timer
	if opts.SignalAfter > 0 {
		timer = time.AfterFunc(opts.SignalAfter, func() { _ = cmd.Process.Signal(syscall.SIGTERM) })
	}
	err := cmd.Wait()
	if timer != nil {
		timer.Stop()
	}
	res := RunResult{Output: out.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		t.Fatalf("run %s: %v", bin, err)
	}
	return res
}

// SyncBuffer is a goroutine-safe byte sink (a log capture that tests may read
// while the writer is still running).
type SyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *SyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *SyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
