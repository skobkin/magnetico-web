package pathfmt

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/name-vectors.json")

const vectorsFile = "testdata/name-vectors.json"

type vectorSegment struct {
	Text   string `json:"text"`
	Marker bool   `json:"marker,omitempty"`
}

type vector struct {
	Bytes    string          `json:"bytes_base64"`
	Segments []vectorSegment `json:"segments"`
}

// vectorInputs are the shared Go/JavaScript name cases: the browser's name
// renderer (internal/web/ui/static/app.js) must produce exactly these
// segments for the same bytes.
func vectorInputs() []string {
	return []string{
		"", "plain.txt", "tab\there", "nul\x00byte", "bell\a", "del\x7f", "c1\u0085", "c1\u009f",
		"\xff", "\xff\xfe", "a\xe3\x81", "valid\xc3\xa9\xff", "日本語/ünï", "😀 emoji",
		"  ", "‮evil‬", "‎ltr‏", "؜mark", "⁦iso⁩",
		"real�fffd", `\x41 literal backslash`, `\u{0041}`, "a\x00\x01\x02b",
		"\xf0\x9f\x98", "\xed\xa0\x80", "\xc0\xaf", "zwj‍ok", "vs️ok",
	}
}

func buildVectors() []vector {
	var out []vector
	for _, in := range vectorInputs() {
		v := vector{Bytes: base64.StdEncoding.EncodeToString([]byte(in)), Segments: []vectorSegment{}}
		for _, s := range Display(in) {
			v.Segments = append(v.Segments, vectorSegment{Text: s.Text, Marker: s.Marker})
		}
		out = append(out, v)
	}
	return out
}

func TestNameVectorsMatchDisplay(t *testing.T) {
	want := buildVectors()
	if *update {
		b, err := json.MarshalIndent(want, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vectorsFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(vectorsFile)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/web/pathfmt -run NameVectors -update)", err)
	}
	var got []vector
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("testdata/name-vectors.json is stale; regenerate with -update")
	}
}
