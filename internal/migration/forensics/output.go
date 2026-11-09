package forensics

// Output safety (spec §5): the JSONL lands in a hidden temp file in the
// destination directory, written 0600 under an explicit chmod, flushed and
// fsynced, then published by HARD LINK + remove — atomic, no-clobber (an
// existing or racing final path fails with EEXIST instead of being
// overwritten; rename would silently clobber). Any failure removes the temp
// and leaves no final file, so a final path always names a COMPLETE artifact.

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// countingWriter tees every byte into the SHA-256 and the byte counter.
type countingWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func newCountingWriter(w io.Writer) *countingWriter {
	return &countingWriter{w: w, h: sha256.New()}
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.h.Write(p[:n])
		c.n += int64(n)
	}
	return n, err
}

func (c *countingWriter) sum() [32]byte {
	var out [32]byte
	copy(out[:], c.h.Sum(nil))
	return out
}

// jsonlSink emits one JSON object per line through the capped counting
// writer. It enforces BOTH caps (events, output bytes) as hard errors —
// never truncation. The encoder writes UNBUFFERED through the counting
// writer: byte counts and the checksum are exact after every event, and an
// underlying write failure (ENOSPC, EPIPE) surfaces on the event that hit
// it rather than hiding in a buffer until flush.
type jsonlSink struct {
	cw     *countingWriter
	enc    *json.Encoder
	events int64
	counts map[string]int64
	caps   Limits
}

func newJSONLSink(w io.Writer, caps Limits) *jsonlSink {
	cw := newCountingWriter(w)
	enc := json.NewEncoder(cw)
	enc.SetEscapeHTML(false)
	return &jsonlSink{
		cw: cw, enc: enc, caps: caps,
		counts: map[string]int64{},
	}
}

// emit encodes one event and returns the event's type. Caps are checked
// before AND after the write: a single event may push the stream past the
// byte cap, but the breach is always a hard error either way.
func (s *jsonlSink) emit(evType string, ev any) error {
	if s.cw.n >= s.caps.MaxOutputBytes {
		return capError("max-output-bytes", s.cw.n, s.caps.MaxOutputBytes)
	}
	if err := s.enc.Encode(ev); err != nil {
		return fmt.Errorf("export writer failed: %w", err)
	}
	s.events++
	s.counts[evType]++
	if s.events > s.caps.MaxEvents {
		return capError("max-events", s.events, s.caps.MaxEvents)
	}
	if s.cw.n > s.caps.MaxOutputBytes {
		return capError("max-output-bytes", s.cw.n, s.caps.MaxOutputBytes)
	}
	return nil
}

func capError(name string, got, allowed int64) error {
	return fmt.Errorf("%w: --%s %d exceeds the configured limit %d (raise the flag or narrow the export)",
		ErrExportCapExceeded, name, got, allowed)
}

// safeFile owns the temp-file lifecycle for one output path.
type safeFile struct {
	final     string
	tempPath  string
	f         *os.File
	sink      *jsonlSink
	published bool
}

// StdoutPath selects stdout mode (metadata-only; records refuse).
const StdoutPath = "-"

// openOutput creates the sink for path. File mode: Lstat refuses an existing
// final path (regular file or symlink alike) up front, then creates the
// hidden temp sibling O_EXCL.
func openOutput(path string, caps Limits) (*safeFile, error) {
	if path == StdoutPath {
		return &safeFile{sink: newJSONLSink(os.Stdout, caps)}, nil
	}
	dir := filepath.Dir(path)
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrOutputExists, filepath.Base(path))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	tempPath := filepath.Join(dir, fmt.Sprintf(".magnetico-export-%d-%s.tmp", os.Getpid(), randToken()))
	f, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Vanishingly unlikely token collision; never overwrite.
			return nil, fmt.Errorf("%w: %s", ErrOutputExists, filepath.Base(tempPath))
		}
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil { // umask-proof
		f.Close()
		os.Remove(tempPath)
		return nil, err
	}
	sf := &safeFile{final: path, tempPath: tempPath, f: f}
	sf.sink = newJSONLSink(f, caps)
	return sf, nil
}

// sink exposes the event emitter.
func (s *safeFile) sinkWriter() *jsonlSink { return s.sink }

// publish flushes, fsyncs and hard-links the temp to the final path.
// EEXIST from the link is ErrOutputExists — the no-clobber guarantee. The
// temp is removed either way; a crash between link and remove can orphan a
// .magnetico-export-*.tmp (documented, recognizable).
func (s *safeFile) publish() error {
	if s.f == nil {
		return nil // stdout: nothing to publish
	}
	if err := s.f.Sync(); err != nil {
		s.abort()
		return err
	}
	if err := s.f.Close(); err != nil {
		os.Remove(s.tempPath)
		return err
	}
	if err := os.Link(s.tempPath, s.final); err != nil {
		os.Remove(s.tempPath)
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrOutputExists, filepath.Base(s.final))
		}
		return err
	}
	if err := os.Remove(s.tempPath); err != nil {
		return fmt.Errorf("published %s but could not remove the temp link: %w",
			filepath.Base(s.final), err)
	}
	if err := syncDir(filepath.Dir(s.final)); err != nil {
		return fmt.Errorf("published %s but the directory fsync failed: %w",
			filepath.Base(s.final), err)
	}
	s.published = true
	return nil
}

// abort discards the temp file (idempotent). Stdout mode cannot be recalled
// — its final summary says the stream is not a complete artifact.
func (s *safeFile) abort() {
	if s.f == nil || s.published {
		return
	}
	_ = s.f.Close()
	_ = os.Remove(s.tempPath)
}

// syncDir fsyncs a directory so the published name survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// bytesReader wraps a body for strict JSON decoding.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// sha256File hashes a file streaming (bounded memory regardless of size).
func sha256File(path string) ([32]byte, error) {
	var out [32]byte
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return out, err
	}
	copy(out[:], h.Sum(nil))
	return out, nil
}
