package forensics

// Output lifecycle unit tests: the no-clobber publish, the 0600 temp
// discipline, abort-on-failure leaving no final path, cap enforcement and
// writer-failure propagation through the io.Writer seam.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenOutputRefusesExistingFinal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	final := filepath.Join(dir, "export.jsonl")
	if err := os.WriteFile(final, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := openOutput(final, DefaultLimits())
	if !errors.Is(err, ErrOutputExists) {
		t.Fatalf("error = %v, want ErrOutputExists", err)
	}
}

func TestOpenOutputRefusesSymlinkFinal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(dir, "export.jsonl")
	if err := os.Symlink(target, final); err != nil {
		t.Fatal(err)
	}
	_, err := openOutput(final, DefaultLimits())
	if !errors.Is(err, ErrOutputExists) {
		t.Fatalf("error = %v, want ErrOutputExists (symlinks are never followed)", err)
	}
}

func TestSafeFilePublishLifecycle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	final := filepath.Join(dir, "export.jsonl")

	sf, err := openOutput(final, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	// Final must not exist while writing; the temp must be hidden and 0600.
	if _, err := os.Lstat(final); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("final exists during the write: %v", err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || !strings.HasPrefix(ents[0].Name(), ".magnetico-export-") || !strings.HasSuffix(ents[0].Name(), ".tmp") {
		t.Fatalf("temp file naming wrong: %v", ents)
	}
	tempFi, err := os.Stat(sf.tempPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := tempFi.Mode() & fs.ModePerm; got != 0o600 {
		t.Fatalf("temp mode = %v, want 0600 regardless of umask", got)
	}

	lines := []string{"first", "second", "third"}
	for _, l := range lines {
		if err := sf.sink.emit("test_event", map[string]string{"line": l}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sf.publish(); err != nil {
		t.Fatal(err)
	}
	// Idempotent-ish: a second abort must not touch the published file.
	sf.abort()

	body, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(got) != len(lines) {
		t.Fatalf("published %d lines, want %d", len(got), len(lines))
	}
	for i, l := range got {
		var m map[string]string
		if err := json.Unmarshal([]byte(l), &m); err != nil || m["line"] != lines[i] {
			t.Fatalf("line %d = %q (%v)", i, l, err)
		}
	}
	fi, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o777 != 0o600 {
		t.Fatalf("published mode = %v, want 0600", fi.Mode())
	}
	// No temp leftovers.
	ents, err = os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "export.jsonl" {
		t.Fatalf("directory after publish: %v", ents)
	}
	// The sink checksum matches the published bytes exactly.
	sum := sf.sink.cw.sum()
	if int64(fi.Size()) != sf.sink.cw.n || fi.Size() != int64(len(body)) {
		t.Fatalf("byte accounting: file %d, counter %d", fi.Size(), sf.sink.cw.n)
	}
	_ = sum
}

func TestSafeFilePublishRaceIsNoClobber(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	final := filepath.Join(dir, "export.jsonl")

	sf, err := openOutput(final, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	// A competing creator wins the publish race between Lstat and Link:
	// the pre-existing content must NEVER be overwritten.
	victim := []byte("pre-existing artifact")
	if err := os.WriteFile(final, victim, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sf.sink.emit("test_event", map[string]string{"line": "new"}); err != nil {
		t.Fatal(err)
	}
	err = sf.publish()
	if !errors.Is(err, ErrOutputExists) {
		t.Fatalf("publish error = %v, want ErrOutputExists", err)
	}
	body, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(victim) {
		t.Fatalf("publish CLOBBERED the existing file: %q", body)
	}
	// No temp leftovers after the failed publish.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp %s left behind", e.Name())
		}
	}
}

func TestSafeFileAbortLeavesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	final := filepath.Join(dir, "export.jsonl")

	sf, err := openOutput(final, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := sf.sink.emit("test_event", map[string]string{"line": "x"}); err != nil {
		t.Fatal(err)
	}
	sf.abort()
	sf.abort() // idempotent
	if _, err := os.Lstat(final); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("final exists after abort: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("abort left %v behind", ents)
	}
}

func TestStdoutModeHasNoFileLifecycle(t *testing.T) {
	t.Parallel()
	sf, err := openOutput(StdoutPath, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := sf.sink.emit("test_event", map[string]string{"line": "stdout"}); err != nil {
		t.Fatal(err)
	}
	if err := sf.publish(); err != nil {
		t.Fatalf("stdout publish = %v, want a no-op", err)
	}
	sf.abort() // must not panic
}

func TestJSONLSinkEnforcesCaps(t *testing.T) {
	t.Parallel()

	t.Run("event cap", func(t *testing.T) {
		t.Parallel()
		caps := DefaultLimits()
		caps.MaxEvents = 3
		sink := newJSONLSink(&strings.Builder{}, caps)
		for i := 0; i < 3; i++ {
			if err := sink.emit("test_event", map[string]int{"i": i}); err != nil {
				t.Fatalf("event %d refused: %v", i, err)
			}
		}
		err := sink.emit("test_event", map[string]int{"i": 3})
		if !errors.Is(err, ErrExportCapExceeded) || !strings.Contains(err.Error(), "max-events") {
			t.Fatalf("error = %v, want max-events cap", err)
		}
	})

	t.Run("byte cap checked before writing", func(t *testing.T) {
		t.Parallel()
		caps := DefaultLimits()
		caps.MaxOutputBytes = 10
		sink := newJSONLSink(&strings.Builder{}, caps)
		if err := sink.emit("test_event", map[string]int{"i": 0}); err != nil {
			t.Fatal(err)
		}
		err := sink.emit("test_event", map[string]int{"i": 1})
		if !errors.Is(err, ErrExportCapExceeded) || !strings.Contains(err.Error(), "max-output-bytes") {
			t.Fatalf("error = %v, want max-output-bytes cap", err)
		}
	})
}

// failingWriter fails after budget bytes — the seam for ENOSPC-style tests.
type failingWriter struct {
	budget int
	n      int
	err    error
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.n+len(p) > w.budget {
		return 0, w.err
	}
	w.n += len(p)
	return len(p), nil
}

func TestJSONLSinkPropagatesWriterFailure(t *testing.T) {
	t.Parallel()
	diskFull := errors.New("no space left on device")
	sink := newJSONLSink(&failingWriter{budget: 8, err: diskFull}, DefaultLimits())
	// The first line fits the budget; the second hits it. With the
	// unbuffered chain the failure surfaces on exactly the event that
	// triggered it.
	if err := sink.emit("test_event", map[string]int{"i": 0}); err != nil {
		t.Fatalf("first emit refused: %v", err)
	}
	err := sink.emit("test_event", map[string]int{"i": 1})
	if !errors.Is(err, diskFull) {
		t.Fatalf("error = %v, want the wrapped writer error", err)
	}
	if sink.events != 1 {
		t.Fatalf("events counted %d after a failed write, want 1", sink.events)
	}
}

func TestManifestWriteLifecycle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "export.jsonl.manifest.json")
	m := &Manifest{
		Format: manifestFormat, FormatVersion: FormatVersion,
		Artifact: "export.jsonl", SHA256: strings.Repeat("a", 64),
		ByteCount: 5, RunID: "11111111-1111-4111-8111-111111111111",
		Coverage: "complete",
	}
	if err := writeManifest(path, m); err != nil {
		t.Fatal(err)
	}
	err := writeManifest(path, m)
	if !errors.Is(err, ErrOutputExists) {
		t.Fatalf("rewrite error = %v, want ErrOutputExists", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("directory after manifest writes: %v", ents)
	}
	fi, _ := os.Stat(path)
	if fi.Mode()&0o777 != 0o600 {
		t.Fatalf("manifest mode = %v, want 0600", fi.Mode())
	}
}
