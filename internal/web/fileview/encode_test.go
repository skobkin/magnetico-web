package fileview

import (
	"encoding/json"
	"math/rand/v2"
	"testing"
	"unicode/utf8"
)

func encodeStr(s string, max int) ([]byte, bool) {
	w := &jsonBuf{max: max}
	w.str(s)
	return w.b, w.over
}

func TestStrMatchesEncodingJSON(t *testing.T) {
	t.Parallel()
	cases := []string{
		"", "plain", "\x00", "a\x00b", "\a\b\t\n\v\f\r", "\x1f\x7f", "\"\\", "</script>&", "  ", "�",
		"日本語/ünï", "😀", "‮؜", string([]rune{0x10FFFF}),
	}
	for c := 0; c < 0x80; c++ {
		cases = append(cases, string([]byte{byte(c)}))
	}
	for _, s := range cases {
		got, over := encodeStr(s, 1<<20)
		want, _ := json.Marshal(s)
		if over || string(got) != string(want) {
			t.Errorf("str(%q) = %s, want %s", s, got, want)
		}
		if !json.Valid(got) {
			t.Errorf("str(%q) invalid JSON: %s", s, got)
		}
	}
}

func FuzzStr(f *testing.F) {
	for _, s := range []string{"", "a", "\x00", " ", "\"<>&\\", "\xff", "日本"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			// Invalid names never reach str; they take the base64 member.
			w := &jsonBuf{max: 1 << 20}
			w.name(s)
			doc := "{" + string(w.b) + "}"
			var n nameJSON
			if err := json.Unmarshal([]byte(doc), &n); err != nil || n.Name != nil || n.NameBase64 == "" {
				t.Fatalf("invalid UTF-8 %q encoded as %s (%v)", s, doc, err)
			}
			if n.bytes(t) != s {
				t.Fatalf("base64 round trip lost bytes for %q", s)
			}
			return
		}
		got, over := encodeStr(s, 1<<20)
		want, _ := json.Marshal(s)
		if over || string(got) != string(want) {
			t.Fatalf("str(%q) = %s, want %s", s, got, want)
		}
	})
}

func TestNameRandomBytesRoundTrip(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 5000; i++ {
		b := make([]byte, rng.IntN(40))
		for j := range b {
			b[j] = byte(rng.IntN(256))
		}
		w := &jsonBuf{max: 1 << 20}
		w.name(string(b))
		doc := "{" + string(w.b) + "}"
		if !json.Valid([]byte(doc)) {
			t.Fatalf("invalid JSON for %x: %s", b, doc)
		}
		var n nameJSON
		if err := json.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatal(err)
		}
		if n.bytes(t) != string(b) {
			t.Fatalf("bytes lost for %x", b)
		}
	}
}

func TestJSONBufCapIsExact(t *testing.T) {
	t.Parallel()
	for max := 0; max < 40; max++ {
		w := &jsonBuf{max: max}
		w.raw(`{"a":`)
		w.str("hello\n")
		w.raw("}")
		full := `{"a":"hello\n"}`
		if wantOver := len(full) > max; w.over != wantOver {
			t.Fatalf("max %d: over=%v, want %v", max, w.over, wantOver)
		}
		if !w.over && len(w.b) > max {
			t.Fatalf("max %d: buffer grew to %d without overflow", max, len(w.b))
		}
	}
}
