package testkit

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	code := m.Run()
	Cleanup()
	os.Exit(code)
}

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	res := Run(t, "/bin/sh", RunOptions{Args: []string{"-c", "echo out; echo err >&2; exit 3"}})
	if res.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Output, "out") || !strings.Contains(res.Output, "err") {
		t.Fatalf("combined output = %q, want both streams", res.Output)
	}
}

func TestRunSeparatesStandardStreams(t *testing.T) {
	res := Run(t, "/bin/sh", RunOptions{Args: []string{"-c", "echo to-out; echo to-err >&2"}})
	if strings.TrimSpace(res.Stdout) != "to-out" || strings.TrimSpace(res.Stderr) != "to-err" {
		t.Fatalf("stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Output, "to-out") || !strings.Contains(res.Output, "to-err") {
		t.Fatalf("combined output lost a stream: %q", res.Output)
	}
}

func TestRunPassesEnvAndStdin(t *testing.T) {
	res := Run(t, "/bin/sh", RunOptions{
		Args:  []string{"-c", `read x; echo "$GREETING $x"`},
		Env:   []string{"GREETING=hello"},
		Stdin: "world\n",
	})
	if res.ExitCode != 0 || strings.TrimSpace(res.Output) != "hello world" {
		t.Fatalf("got exit=%d output=%q", res.ExitCode, res.Output)
	}
}

func TestRunSignalAfterDeliversSIGTERM(t *testing.T) {
	res := Run(t, "/bin/sh", RunOptions{
		Args:        []string{"-c", `trap 'echo got-term; exit 7' TERM; echo ready; while :; do sleep 0.05; done`},
		SignalAfter: 400 * time.Millisecond,
		Timeout:     20 * time.Second,
	})
	if res.ExitCode != 7 || !strings.Contains(res.Output, "got-term") {
		t.Fatalf("got exit=%d output=%q, want 7 and got-term", res.ExitCode, res.Output)
	}
}

func TestRunKillsAfterTimeout(t *testing.T) {
	start := time.Now()
	res := Run(t, "/bin/sh", RunOptions{Args: []string{"-c", "sleep 30"}, Timeout: 300 * time.Millisecond})
	if res.ExitCode == 0 {
		t.Fatalf("a timed-out process must not report success")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("timeout was not enforced")
	}
}

func TestBinaryBuildsOncePerTagSet(t *testing.T) {
	a := Binary(t)
	b := Binary(t)
	if a != b {
		t.Fatalf("same tag set built twice: %s vs %s", a, b)
	}
	c := Binary(t, "testkitprobe")
	if c == a {
		t.Fatalf("a different tag set must build a different binary")
	}
	res := Run(t, a, RunOptions{Args: []string{"--help"}})
	if !strings.Contains(res.Output, "Available commands") {
		t.Fatalf("built binary does not look like magnetico: %q", res.Output)
	}
}

func TestSyncBufferIsSafeForConcurrentWriters(t *testing.T) {
	var b SyncBuffer
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = b.Write([]byte("x"))
				_ = b.String()
			}
		}()
	}
	wg.Wait()
	if got := len(b.String()); got != 800 {
		t.Fatalf("len = %d, want 800", got)
	}
}
