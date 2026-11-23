package codec_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/synth"
)

// BlobVariant is one forced (serializer, compressor) pair.
type blobVariant struct {
	Name string
	Ser  codec.SerializerID
	Comp codec.CompressorID
}

var blobVariants = []blobVariant{
	{"flat-none", codec.Flat, codec.None},
	{"prefix-none", codec.PrefixDelta, codec.None},
	{"prefix-zstd", codec.PrefixDelta, codec.Zstd},
	{"prefix-brotli9", codec.PrefixDelta, codec.Brotli},
	{"trie-none", codec.DirectoryTrie, codec.None},
	{"trie-brotli9", codec.DirectoryTrie, codec.Brotli},
}

type blobMeta struct {
	Dataset   string            `json:"dataset"`
	Files     int               `json:"files"`
	PathBytes int               `json:"path_bytes"`
	TotalSize int64             `json:"total_size"`
	Variants  map[string]int    `json:"stored_bytes"`
	Skipped   map[string]string `json:"skipped"`
}

// TestDumpBenchBlobs writes the synthetic corpus, encoded under every forced
// (serializer, compressor) pair, to $MAGNETICO_BLOB_DIR for the explicit C1
// benchmarks. It is skipped unless that variable is set, so the ordinary suite
// never pays for it:
//
//	MAGNETICO_BLOB_DIR=/path go test -run TestDumpBenchBlobs -timeout 1h ./codec
func TestDumpBenchBlobs(t *testing.T) {
	dir := os.Getenv("MAGNETICO_BLOB_DIR")
	if dir == "" {
		t.Skip("set MAGNETICO_BLOB_DIR to write the benchmark blobs")
	}
	lim := codec.DefaultLimits()
	for _, sp := range synth.Corpus() {
		files := synth.Generate(sp)
		st := synth.Measure(files)
		meta := blobMeta{Dataset: sp.Name, Files: st.Files, PathBytes: st.PathBytes, TotalSize: st.TotalSize,
			Variants: map[string]int{}, Skipped: map[string]string{}}
		out := filepath.Join(dir, sp.Name)
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, v := range blobVariants {
			fp := filepath.Join(out, v.Name+".blob")
			if b, err := os.ReadFile(fp); err == nil { // resumable
				meta.Variants[v.Name] = len(b)
				continue
			}
			start := time.Now()
			blob, err := codec.EncodeForced(v.Ser, v.Comp, files, lim)
			var le *codec.LimitError
			switch {
			case errors.As(err, &le):
				meta.Skipped[v.Name] = "limit " + le.Limit
			case errors.Is(err, codec.ErrEmptyPathComponent):
				meta.Skipped[v.Name] = "trie cannot represent this list"
			case err != nil:
				t.Fatalf("%s/%s: %v", sp.Name, v.Name, err)
			default:
				if err := os.WriteFile(fp, blob, 0o644); err != nil {
					t.Fatal(err)
				}
				meta.Variants[v.Name] = len(blob)
			}
			t.Logf("%-16s %-15s stored=%-9d skipped=%q encode=%v", sp.Name, v.Name, meta.Variants[v.Name], meta.Skipped[v.Name], time.Since(start).Round(time.Millisecond))
		}
		b, _ := json.MarshalIndent(meta, "", " ")
		if err := os.WriteFile(filepath.Join(out, "meta.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
