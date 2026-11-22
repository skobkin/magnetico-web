package metadata

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Hostile peer input (Task 06 review correction 1).
//
// The extension handshake and the ut_metadata header are decoded by the
// recursive bencode decoder straight from peer bytes. A Go stack overflow is
// fatal and unrecoverable, so each case runs in a child process: the parent
// asserts the child exits normally and the hostile input is rejected.

const (
	peerEnvCase  = "MAGNETICO_TEST_PEER_CASE"
	peerEnvDepth = "MAGNETICO_TEST_PEER_DEPTH"
)

// deepList is "d" + a valid prefix + an unknown key holding `depth` nested lists.
func deepValue(depth int) string {
	return strings.Repeat("l", depth) + strings.Repeat("e", depth)
}

func frame(extID byte, payload []byte) []byte {
	b := make([]byte, 4, 4+2+len(payload))
	binary.BigEndian.PutUint32(b, uint32(2+len(payload)))
	return append(b, append([]byte{20, extID}, payload...)...)
}

// TestHostilePeerChild is the child half.
func TestHostilePeerChild(t *testing.T) {
	kind := os.Getenv(peerEnvCase)
	if kind == "" {
		t.Skip("child of TestHostilePeerInput")
	}
	depth, _ := strconv.Atoi(os.Getenv(peerEnvDepth))

	switch kind {
	case "handshake":
		hs := []byte("d1:md11:ut_metadatai1ee13:metadata_sizei16384e3:zzz" + deepValue(depth) + "e")
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		go func() {
			// Drain our own handshake, then send the hostile one.
			buf := make([]byte, 64)
			_, _ = b.Read(buf)
			_, _ = b.Write(frame(0, hs))
		}()
		l := &Leech{conn: a}
		_ = a.SetDeadline(time.Now().Add(10 * time.Second))
		err := l.doExHandshake()
		fmt.Printf("RESULT rejected=%v err=%v\n", err != nil, err)
	case "umheader":
		hdr := []byte("d8:msg_typei1e5:piecei0e3:zzz" + deepValue(depth) + "e")
		_, _, err := decodeUmHeader(append(hdr, make([]byte, 100)...))
		fmt.Printf("RESULT rejected=%v err=%v\n", err != nil, err)
	}
}

func runPeerCase(t *testing.T, kind string, depth int) (out string, crashed bool, wall time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHostilePeerChild$", "-test.v")
	cmd.Env = append(os.Environ(), peerEnvCase+"="+kind, peerEnvDepth+"="+strconv.Itoa(depth))
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	start := time.Now()
	err := cmd.Run()
	return buf.String(), err != nil, time.Since(start)
}

func TestHostilePeerInput(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess hostile-input test")
	}
	cases := []struct {
		kind   string
		depth  int
		reject bool
	}{
		{"handshake", 2, false},
		{"handshake", 5_000, true}, // under the size cap: depth guard
		{"handshake", 100_000, true},
		// 8 MB: within the old 10 MiB cap; the unguarded decoder died with "fatal error: stack overflow".
		{"handshake", 4_000_000, true},
		// ~1 MiB: the largest message readMessage admits; used to be 10 MiB.
		{"handshake", 500_000, true},
		{"umheader", 2, false},
		{"umheader", 17, true},
		{"umheader", 8_000, true},
	}
	for _, c := range cases {
		out, crashed, wall := runPeerCase(t, c.kind, c.depth)
		line := ""
		if i := strings.Index(out, "RESULT"); i >= 0 {
			line = strings.SplitN(out[i:], "\n", 2)[0]
		}
		t.Logf("%-10s depth=%-8d crashed=%v wall=%v %.100s", c.kind, c.depth, crashed, wall.Round(time.Millisecond), line)
		if crashed {
			t.Errorf("%s depth=%d: child crashed\n%.400s", c.kind, c.depth, out)
			continue
		}
		if got := strings.Contains(line, "rejected=true"); got != c.reject {
			t.Errorf("%s depth=%d: rejected=%v, want %v (%s)", c.kind, c.depth, got, c.reject, line)
		}
	}
}

// TestPeerMessageSizeLimits covers the size bounds without a child process.
func TestPeerMessageSizeLimits(t *testing.T) {
	t.Parallel()

	// readMessage refuses a declared length above MaxPeerMessageSize without
	// allocating it.
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], MaxPeerMessageSize+1)
		_, _ = b.Write(n[:])
	}()
	l := &Leech{conn: a}
	_ = a.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := l.readMessage(); err == nil || !strings.Contains(err.Error(), "maximum peer message size") {
		t.Fatalf("oversized message: err = %v", err)
	}

	// An oversized ut_metadata message is rejected before decoding.
	big := append([]byte("d8:msg_typei1e5:piecei0ee"), make([]byte, metadataPieceSize+maxUmHeaderSize)...)
	if _, _, err := decodeUmHeader(big); err == nil {
		t.Error("oversized ut_metadata message accepted")
	}

	// A well-formed data message round-trips: header decoded, data untouched.
	data := bytes.Repeat([]byte{'l'}, metadataPieceSize) // looks like nesting; must not be scanned
	d, rest, err := decodeUmHeader(append([]byte("d8:msg_typei1e5:piecei3ee"), data...))
	if err != nil || d.MsgType != 1 || d.Piece != 3 || !bytes.Equal(rest, data) {
		t.Fatalf("valid header: %+v err=%v rest=%d", d, err, len(rest))
	}

	// Oversized extension handshake is rejected.
	a2, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	go func() {
		buf := make([]byte, 64)
		_, _ = b2.Read(buf)
		_, _ = b2.Write(frame(0, append([]byte("d1:md11:ut_metadatai1ee1:x"), bytes.Repeat([]byte("a"), maxExHandshakeSize)...)))
	}()
	l2 := &Leech{conn: a2}
	_ = a2.SetDeadline(time.Now().Add(5 * time.Second))
	if err := l2.doExHandshake(); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized handshake: err = %v", err)
	}
}
