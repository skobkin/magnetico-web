package record

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/metadata"
	"github.com/skobkin/magnetico-web/persistence"
)

var limits = codec.DefaultLimits()

func hash(b byte) [20]byte {
	var h [20]byte
	for i := range h {
		h[i] = b + byte(i)
	}
	return h
}

func mk(name string, files ...persistence.File) Record {
	var sum int64
	for _, f := range files {
		sum += f.Size
	}
	return Record{InfoHash: hash(1), Name: name, TotalSize: sum, DiscoveredOn: 1700000000, Files: files}
}

func roundTrip(t *testing.T, r Record) []byte {
	t.Helper()
	data, err := Encode(r, limits)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data, limits)
	if err != nil {
		t.Fatalf("Decode(%s): %v", data, err)
	}
	if !reflect.DeepEqual(got, r) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, r)
	}
	return data
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	cases := map[string]Record{
		"ordinary": mk("dir", persistence.File{Path: "dir/a.txt", Size: 10}, persistence.File{Path: "dir/b/c.bin", Size: 20}),
		"single":   mk("one.iso", persistence.File{Path: "one.iso", Size: 5}),
		"duplicate paths": mk("d",
			persistence.File{Path: "d/x", Size: 1}, persistence.File{Path: "d/x", Size: 2}),
		"zero size member": mk("d",
			persistence.File{Path: "d/empty", Size: 0}, persistence.File{Path: "d/full", Size: 7}),
		"non-ascii": mk("Ünïcödé 日本語 🎬 <&>",
			persistence.File{Path: "Ünïcödé 日本語 🎬 <&>/файл.txt", Size: 3}),
		"empty name": mk("", persistence.File{Path: "x", Size: 1}),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			data := roundTrip(t, r)
			if bytes.HasSuffix(data, []byte("\n")) {
				t.Error("encoded record has a trailing newline")
			}
		})
	}
}

func TestEncodeDoesNotEscapeHTML(t *testing.T) {
	t.Parallel()
	data, err := Encode(mk("a<b>&c", persistence.File{Path: "a<b>&c", Size: 1}), limits)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("\\u003c")) || !bytes.Contains(data, []byte("a<b>&c")) {
		t.Errorf("HTML escaping was applied: %s", data)
	}
}

func TestEncodeRejectsInvalidText(t *testing.T) {
	t.Parallel()
	cases := map[string]Record{
		"NUL in name":      mk("a\x00b", persistence.File{Path: "x", Size: 1}),
		"NUL in path":      mk("a", persistence.File{Path: "x\x00", Size: 1}),
		"bad utf8 name":    mk("a\xffb", persistence.File{Path: "x", Size: 1}),
		"bad utf8 path":    mk("a", persistence.File{Path: "\xc3(", Size: 1}),
		"no files":         {InfoHash: hash(1), Name: "a", TotalSize: 1, DiscoveredOn: 1},
		"size mismatch":    {InfoHash: hash(1), Name: "a", TotalSize: 9, DiscoveredOn: 1, Files: []persistence.File{{Path: "x", Size: 1}}},
		"zero discovered":  {InfoHash: hash(1), Name: "a", TotalSize: 1, Files: []persistence.File{{Path: "x", Size: 1}}},
		"non-positive tot": {InfoHash: hash(1), Name: "a", DiscoveredOn: 1, Files: []persistence.File{{Path: "x", Size: 0}}},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := Encode(r, limits)
			if err == nil {
				t.Fatalf("expected an error, got %s", data)
			}
			if !errors.Is(err, ErrInvalidRecord) || !errors.Is(err, persistence.ErrInvalidMetadata) {
				t.Errorf("error %v should wrap ErrInvalidRecord and ErrInvalidMetadata", err)
			}
			if strings.Contains(string(data), "�") {
				t.Error("U+FFFD substitution leaked into output")
			}
		})
	}
}

const good = `{"v":1,"info_hash":"0102030405060708090a0b0c0d0e0f1011121314","name":"n","total_size":1,"discovered_on":5,"files":[{"path":"n","size":1}]}`

func TestDecodeStrict(t *testing.T) {
	t.Parallel()
	if _, err := Decode([]byte(good), limits); err != nil {
		t.Fatalf("baseline record rejected: %v", err)
	}
	bad := map[string]string{
		"unknown field":     strings.Replace(good, `"v":1,`, `"v":1,"extra":1,`, 1),
		"unknown file key":  strings.Replace(good, `"size":1}]`, `"size":1,"x":2}]`, 1),
		"trailing data":     good + ` {}`,
		"trailing garbage":  good + `x`,
		"uppercase hash":    strings.Replace(good, "0a0b0c", "0A0B0C", 1),
		"short hash":        strings.Replace(good, "11121314", "1112", 1),
		"non-hex hash":      strings.Replace(good, "01020304", "0102030z", 1),
		"missing name":      strings.Replace(good, `"name":"n",`, ``, 1),
		"null name":         strings.Replace(good, `"name":"n"`, `"name":null`, 1),
		"missing files":     `{"v":1,"info_hash":"0102030405060708090a0b0c0d0e0f1011121314","name":"n","total_size":1,"discovered_on":5}`,
		"missing file size": strings.Replace(good, `,"size":1}]`, `}]`, 1),
		"not an object":     `[1]`,
		"empty":             ``,
		"NUL escape":        strings.Replace(good, `"name":"n"`, `"name":"a\u0000b"`, 1),
		"raw bad utf8":      strings.Replace(good, `"name":"n"`, "\"name\":\"a\xffb\"", 1),
		"lone high escape":  strings.Replace(good, `"name":"n"`, `"name":"a\ud800b"`, 1),
		"lone low escape":   strings.Replace(good, `"name":"n"`, `"name":"a\udc00b"`, 1),
		"high then non-low": strings.Replace(good, `"name":"n"`, `"name":"a\ud800\u0041"`, 1),
		"float size":        strings.Replace(good, `"total_size":1`, `"total_size":1.5`, 1),
		"version string":    strings.Replace(good, `"v":1`, `"v":"1"`, 1),
		"missing version":   strings.Replace(good, `"v":1,`, ``, 1),
		"sum mismatch":      strings.Replace(good, `"total_size":1`, `"total_size":2`, 1),
	}
	for name, in := range bad {
		t.Run(name, func(t *testing.T) {
			r, err := Decode([]byte(in), limits)
			if err == nil {
				t.Fatalf("accepted: %+v", r)
			}
			if !errors.Is(err, ErrInvalidRecord) {
				t.Errorf("error %v should be ErrInvalidRecord", err)
			}
		})
	}
}

func TestDecodeAcceptsValidSurrogatePair(t *testing.T) {
	t.Parallel()
	in := strings.Replace(good, `"name":"n"`, `"name":"a🎬b"`, 1)
	in = strings.Replace(in, `"path":"n"`, `"path":"a🎬b"`, 1)
	r, err := Decode([]byte(in), limits)
	if err != nil {
		t.Fatalf("valid surrogate pair rejected: %v", err)
	}
	if r.Name != "a🎬b" {
		t.Errorf("name = %q", r.Name)
	}
}

func TestEscapedBackslashIsNotASurrogateEscape(t *testing.T) {
	t.Parallel()
	in := strings.Replace(good, `"name":"n"`, `"name":"\\ud800"`, 1)
	in = strings.Replace(in, `"path":"n"`, `"path":"\\ud800"`, 1)
	r, err := Decode([]byte(in), limits)
	if err != nil {
		t.Fatalf("literal backslash text rejected: %v", err)
	}
	if r.Name != `\ud800` {
		t.Errorf("name = %q", r.Name)
	}
}

func TestDecodeUnsupportedVersion(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		strings.Replace(good, `"v":1`, `"v":2`, 1),
		strings.Replace(good, `"v":1`, `"v":0`, 1),
		// A future version may add fields: still "unsupported", not "invalid".
		strings.Replace(good, `"v":1,`, `"v":2,"new":{"a":1},`, 1),
	} {
		_, err := Decode([]byte(in), limits)
		if !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("%s: err = %v, want ErrUnsupportedVersion", in, err)
		}
		if errors.Is(err, ErrInvalidRecord) {
			t.Errorf("unsupported version must not be reported as invalid: %v", err)
		}
	}
}

func TestDecodeOversize(t *testing.T) {
	t.Parallel()
	big := make([]byte, MaxRecordBytes+1)
	if _, err := Decode(big, limits); !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("err = %v, want ErrRecordTooLarge", err)
	}
	// Precedence: oversize beats everything else, including a bad version.
	big = append([]byte(`{"v":99,`), make([]byte, MaxRecordBytes)...)
	if _, err := Decode(big, limits); !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("err = %v, want ErrRecordTooLarge", err)
	}
}

// TestEscapeExpansionNearLimit builds a record whose bencode-sized content is
// well below 16 MiB but whose JSON form crosses it: control characters escape
// to six bytes each.
func TestEscapeExpansionNearLimit(t *testing.T) {
	t.Parallel()
	// 0x01 is valid text for ValidateIngest but encodes as \u0001 (6 bytes).
	rawLen := MaxRecordBytes/6 + 1024 // ≈ 2.8 MiB raw, ≈ 16 MiB+ escaped
	path := strings.Repeat("\x01", rawLen)
	r := mk("n", persistence.File{Path: path, Size: 1})
	lim := limits
	lim.MaxPathBytes = rawLen + 1

	data, err := Encode(r, lim)
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("err = %v, want ErrRecordTooLarge", err)
	}
	if len(data) <= MaxRecordBytes {
		t.Fatalf("payload not preserved: %d bytes", len(data))
	}
	if !bytes.Contains(data, []byte(`\u0001`)) {
		t.Error("returned payload should be the intact JSON")
	}

	// Just under the limit still works and round-trips.
	r2 := mk("n", persistence.File{Path: strings.Repeat("\x01", MaxRecordBytes/6-1024), Size: 1})
	lim.MaxPathBytes = MaxRecordBytes
	roundTrip2(t, r2, lim)
}

func roundTrip2(t *testing.T, r Record, lim codec.Limits) {
	t.Helper()
	data, err := Encode(r, lim)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(data) > MaxRecordBytes {
		t.Fatalf("size %d over limit", len(data))
	}
	got, err := Decode(data, lim)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(got, r) {
		t.Fatal("round trip mismatch")
	}
}

func TestFromMetadata(t *testing.T) {
	t.Parallel()
	h1 := hash(1)
	md := &metadata.Metadata{
		InfoHash: h1[:], Name: "n", TotalSize: 3, DiscoveredOn: 9,
		Files: []persistence.File{{Path: "n", Size: 3}},
	}
	r, err := FromMetadata(md)
	if err != nil {
		t.Fatal(err)
	}
	if r.InfoHash != hash(1) || r.TotalSize != 3 || r.Name != "n" || r.DiscoveredOn != 9 {
		t.Errorf("bad conversion: %+v", r)
	}

	md.TotalSize = math.MaxInt64 + 1
	if _, err := FromMetadata(md); !errors.Is(err, persistence.ErrInvalidMetadata) {
		t.Errorf("uint64 overflow: err = %v, want ErrInvalidMetadata", err)
	}
	md.TotalSize = math.MaxInt64
	if _, err := FromMetadata(md); err != nil {
		t.Errorf("MaxInt64 must convert (validation catches sum mismatch later): %v", err)
	}
	md.InfoHash = []byte{1, 2}
	if _, err := FromMetadata(md); !errors.Is(err, persistence.ErrInvalidMetadata) {
		t.Errorf("short infohash: err = %v", err)
	}
}

func TestTorrentView(t *testing.T) {
	t.Parallel()
	r := mk("n", persistence.File{Path: "n", Size: 3})
	tor := r.Torrent()
	tor.InfoHash[0] ^= 0xff
	if r.InfoHash[0] == tor.InfoHash[0] {
		t.Error("Torrent() must copy the infohash")
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte(good))
	f.Add([]byte(`{"v":2}`))
	f.Add([]byte(`{"v":1,"name":"\ud800"}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		r, err := Decode(in, limits)
		if err != nil {
			return
		}
		// Anything accepted must re-encode and decode to the same record.
		out, err := Encode(r, limits)
		if err != nil {
			t.Fatalf("accepted record does not re-encode: %v", err)
		}
		r2, err := Decode(out, limits)
		if err != nil || !reflect.DeepEqual(r, r2) {
			t.Fatalf("re-decode mismatch: %v", err)
		}
	})
}
