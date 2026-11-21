package metadata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/bencode"
)

// Hostile bencode nesting (Task 06 M0).
//
// Peer-supplied metadata of up to MAX_METADATA_SIZE goes through
// bencode.Unmarshal. A Go stack overflow is fatal and cannot be recovered, so
// each case runs in a child process (this test binary re-executed) and the
// parent records the outcome: exit status, wall time and the child's peak
// memory as reported by the OS.

// hostileTimeout kills a case that is still running; a case that needs that
// long is itself a finding (CPU exhaustion).
const hostileTimeout = 20 * time.Second

const (
	hostileEnvCase  = "MAGNETICO_TEST_HOSTILE_CASE"
	hostileEnvDepth = "MAGNETICO_TEST_HOSTILE_DEPTH"
)

const infoPrefix = "d6:lengthi1e4:name1:x12:piece lengthi16384e6:pieces20:" +
	"01234567890123456789"

// hostilePayload builds a syntactically plausible info dictionary hiding deep
// nesting in a place the decoder must walk.
func hostilePayload(kind string, depth int) []byte {
	var b bytes.Buffer
	switch kind {
	case "unknown-list":
		// deep list under an unknown info key
		b.WriteString(infoPrefix)
		b.WriteString("3:zzz")
		b.WriteString(strings.Repeat("l", depth))
		b.WriteString(strings.Repeat("e", depth))
		b.WriteString("e")
	case "unknown-dict":
		b.WriteString(infoPrefix)
		b.WriteString("3:zzz")
		b.WriteString(strings.Repeat("d1:a", depth))
		b.WriteString("i0e")
		b.WriteString(strings.Repeat("e", depth))
		b.WriteString("e")
	case "file-tree":
		// deep dictionaries inside "file tree"
		b.WriteString("d9:file tree")
		b.WriteString(strings.Repeat("d1:a", depth))
		b.WriteString("de")
		b.WriteString(strings.Repeat("e", depth))
		b.WriteString("4:name1:x12:piece lengthi16384e6:pieces20:01234567890123456789e")
	}
	return b.Bytes()
}

// TestHostileNestingChild is the child half; it only runs when re-executed by
// TestHostileNesting.
func TestHostileNestingChild(t *testing.T) {
	kind := os.Getenv(hostileEnvCase)
	if kind == "" {
		t.Skip("child of TestHostileNesting")
	}
	depth, _ := strconv.Atoi(os.Getenv(hostileEnvDepth))
	payload := hostilePayload(kind, depth)
	if len(payload) > MAX_METADATA_SIZE {
		fmt.Printf("RESULT skipped payload=%d exceeds limit\n", len(payload))
		return
	}
	_, err := unmarshalMetainfo(payload)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("RESULT payload=%d err=%v deep=%v sys=%dMiB\n", len(payload), err != nil,
		errors.Is(err, bencode.ErrNestingTooDeep), ms.Sys>>20)
}

type hostileOutcome struct {
	crashed  bool
	timedOut bool
	wall     time.Duration
	peakKiB  int64
	out      string
}

func runHostile(t *testing.T, kind string, depth int) hostileOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), hostileTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHostileNestingChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		hostileEnvCase+"="+kind, hostileEnvDepth+"="+strconv.Itoa(depth))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	start := time.Now()
	err := cmd.Run()
	o := hostileOutcome{wall: time.Since(start), crashed: err != nil, timedOut: ctx.Err() != nil}
	if cmd.ProcessState != nil {
		o.peakKiB = maxRSSKiB(cmd.ProcessState)
	}
	o.out = out.String()
	return o
}

func TestHostileNesting(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess hostile-input experiment")
	}
	// Without bencode.CheckDepth (M0 measurements, this machine): unknown-list
	// 4,000,000 → "fatal error: stack overflow" (1 GiB RSS, process dies);
	// file-tree 10,000 and 100,000 → still running after the 20 s cap (CPU
	// exhaustion: every subtree is re-decoded); file-tree 1,000 → 0.47 s for
	// a 5 KB input.
	cases := []struct {
		kind  string
		depth int
		deep  bool // must be rejected as over-deep
	}{
		{"unknown-list", 200, false},
		{"unknown-list", 257, true},
		{"unknown-list", 100_000, true},
		{"unknown-list", 4_000_000, true},
		{"unknown-dict", 1_000_000, true},
		{"file-tree", 200, false},
		{"file-tree", 1_000, true},
		{"file-tree", 10_000, true},
		{"file-tree", 100_000, true},
	}
	for _, c := range cases {
		o := runHostile(t, c.kind, c.depth)
		first := ""
		if i := strings.Index(o.out, "RESULT"); i >= 0 {
			first = strings.SplitN(o.out[i:], "\n", 2)[0]
		} else if i := strings.Index(o.out, "fatal error"); i >= 0 {
			first = strings.SplitN(o.out[i:], "\n", 2)[0]
		}
		t.Logf("%-12s depth=%-8d crashed=%-5v timeout=%-5v wall=%-10v peakRSS=%dMiB %s",
			c.kind, c.depth, o.crashed, o.timedOut, o.wall.Round(time.Millisecond), o.peakKiB>>10, first)

		if o.crashed || o.timedOut {
			t.Errorf("%s depth=%d: process crashed or exceeded %v\n%s", c.kind, c.depth, hostileTimeout, first)
			continue
		}
		if o.wall > 5*time.Second {
			t.Errorf("%s depth=%d: took %v", c.kind, c.depth, o.wall)
		}
		if got := strings.Contains(o.out, "deep=true"); got != c.deep {
			t.Errorf("%s depth=%d: rejected-as-too-deep = %v, want %v", c.kind, c.depth, got, c.deep)
		}
	}
}
