package pathfmt

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// parseMarkers is the test-side inverse of Display: it proves losslessness.
func parseMarkers(t testing.TB, segs []Segment) string {
	t.Helper()
	var b strings.Builder
	for _, s := range segs {
		if !s.Marker {
			b.WriteString(s.Text)
			continue
		}
		switch {
		case strings.HasPrefix(s.Text, `\x`) && len(s.Text) == 4:
			v, err := strconv.ParseUint(s.Text[2:], 16, 8)
			if err != nil {
				t.Fatalf("bad byte marker %q", s.Text)
			}
			b.WriteByte(byte(v))
		case strings.HasPrefix(s.Text, `\u{`) && strings.HasSuffix(s.Text, "}"):
			v, err := strconv.ParseUint(s.Text[3:len(s.Text)-1], 16, 32)
			if err != nil {
				t.Fatalf("bad rune marker %q", s.Text)
			}
			b.WriteRune(rune(v))
		default:
			t.Fatalf("unexpected marker %q", s.Text)
		}
	}
	return b.String()
}

func checkSafe(t testing.TB, segs []Segment) {
	t.Helper()
	for _, s := range segs {
		if s.Text == "" {
			t.Fatal("empty segment")
		}
		if !utf8.ValidString(s.Text) {
			t.Fatalf("segment %q is not valid UTF-8", s.Text)
		}
		if s.Marker {
			continue
		}
		for _, r := range s.Text {
			if needsMarker(r) {
				t.Fatalf("unmarked unsafe rune %U in %q", r, s.Text)
			}
		}
	}
}

func TestDisplayTable(t *testing.T) {
	m := func(s string) Segment { return Segment{s, true} }
	x := func(s string) Segment { return Segment{s, false} }
	tests := []struct {
		name, in string
		want     []Segment
	}{
		{"empty input", "", nil},
		{"ascii", "movie.mkv", []Segment{x("movie.mkv")}},
		{"valid unicode kept", "日本語/ñ/😀", []Segment{x("日本語/ñ/😀")}},
		{"zwj emoji kept", "👨‍👩", []Segment{x("👨‍👩")}},
		{"invalid byte", "a\xffb", []Segment{x("a"), m(`\xff`), x("b")}},
		{"leading invalid", "\x80abc", []Segment{m(`\x80`), x("abc")}},
		{"two invalid", "\xfe\xff", []Segment{m(`\xfe`), m(`\xff`)}},
		{"truncated multibyte", "a\xe3\x81", []Segment{x("a"), m(`\xe3`), m(`\x81`)}},
		{"overlong", "\xc0\xaf", []Segment{m(`\xc0`), m(`\xaf`)}},
		{"surrogate bytes", "\xed\xa0\x80", []Segment{m(`\xed`), m(`\xa0`), m(`\x80`)}},
		{"real U+FFFD", "a�b", []Segment{x("a"), m(`\u{FFFD}`), x("b")}},
		{"NUL", "a\x00b", []Segment{x("a"), m(`\u{0000}`), x("b")}},
		{"tab newline", "a\tb\n", []Segment{x("a"), m(`\u{0009}`), x("b"), m(`\u{000A}`)}},
		{"DEL", "\x7f", []Segment{m(`\u{007F}`)}},
		{"C1", "\u0085\u009f", []Segment{m(`\u{0085}`), m(`\u{009F}`)}},
		{"bidi override", "a‮b", []Segment{x("a"), m(`\u{202E}`), x("b")}},
		{"bidi isolate", "⁦x⁩", []Segment{m(`\u{2066}`), x("x"), m(`\u{2069}`)}},
		{"lrm rlm alm", "‎‏؜", []Segment{m(`\u{200E}`), m(`\u{200F}`), m(`\u{061C}`)}},
		{"line sep", "a b ", []Segment{x("a"), m(`\u{2028}`), x("b"), m(`\u{2029}`)}},
		{"literal backslash", `a\xffb`, []Segment{x(`a\xffb`)}},
		{"dots and slashes untouched", "../a//./b/", []Segment{x("../a//./b/")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Display(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("seg %d: got %#v want %#v", i, got[i], tc.want[i])
				}
			}
			checkSafe(t, got)
			if back := parseMarkers(t, got); back != tc.in {
				t.Fatalf("not lossless: %q -> %q", tc.in, back)
			}
		})
	}
}

func TestNoReplacementCharacterEmitted(t *testing.T) {
	for _, in := range []string{"\xff", "a\xc3", "\xf4\x90\x80\x80", "�", "x\xe2\x82"} {
		if strings.ContainsRune(Plain(Display(in)), '�') {
			t.Errorf("%q: displayed text contains U+FFFD", in)
		}
	}
}

func TestComponent(t *testing.T) {
	if got := Component("", 0); len(got) != 1 || !got[0].Marker || got[0].Text != EmptyComponentMarker {
		t.Fatalf("empty component: %#v", got)
	}
	// Dot components are real names, not special.
	for _, n := range []string{".", ".."} {
		if got := Component(n, 0); len(got) != 1 || got[0].Marker || got[0].Text != n {
			t.Fatalf("%q: %#v", n, got)
		}
	}
	long := strings.Repeat("a", 100)
	got := Component(long, 10)
	if len(got) != 2 || got[0].Text != strings.Repeat("a", 10) || !got[1].Marker || got[1].Text != "…(90 more bytes)" {
		t.Fatalf("truncated: %#v", got)
	}
	if got := Component(long, 0); len(got) != 1 || got[0].Text != long {
		t.Fatalf("no limit must not truncate")
	}
	if got := Component(long, 100); len(got) != 1 {
		t.Fatalf("exact length is not truncated: %#v", got)
	}
	// Truncation never splits an escape and never cuts a multibyte rune.
	got = Component("日本語日本語", 4)
	checkSafe(t, got)
	if got[0].Text != "日本" { // 3-byte units: the first boundary >= 4 bytes is 6
		t.Fatalf("rune boundary: %#v", got)
	}
}

func TestEncodeDirTable(t *testing.T) {
	for _, d := range []string{
		"a/", "a//b/", "/", "//", "/a/", "./", "../", "a/../b/", "..", ".",
		"100%/", "a+b/", "a b/", "a&b=c/", "a#b/", "a?b/", "a;b/", "a\x00b/",
		"\xff\xfe/", "日本/", "a‮b/", "%2f/", "%/", "\\/", strings.Repeat("x", 5000) + "/",
	} {
		enc := EncodeDir(d)
		if strings.ContainsAny(enc, "/&#?;= \x00") {
			t.Errorf("%q: encoding %q holds a reserved byte", d, enc)
		}
		got, err := DecodeDir(enc)
		if err != nil || got != d {
			t.Errorf("%q: DecodeDir -> %q, %v", d, got, err)
		}
		// Through a real query parser, as a handler would.
		vals, err := url.ParseQuery(DirQuery(d))
		if err != nil {
			t.Errorf("%q: ParseQuery: %v", d, err)
			continue
		}
		if d == "" {
			continue
		}
		if vals.Get(DirParam) != d {
			t.Errorf("%q: ParseQuery round trip gave %q", d, vals.Get(DirParam))
		}
		// And through a full URL parse.
		u, err := url.Parse("/torrents/1/files?" + DirQuery(d))
		if err != nil || u.Query().Get(DirParam) != d || u.Path != "/torrents/1/files" {
			t.Errorf("%q: URL round trip failed: %v", d, err)
		}
	}
	if DirQuery("") != "" {
		t.Error("root must omit the parameter")
	}
	if _, err := DecodeDir("%zz"); err == nil {
		t.Error("malformed escape must error")
	}
}

func FuzzDisplayLossless(f *testing.F) {
	for _, s := range []string{"", "a", "\xff", "a‮b", "\x00", "日本", "�", `\x41`, "a\xe3\x81"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		segs := Display(s)
		checkSafe(t, segs)
		if back := parseMarkers(t, segs); back != s {
			t.Fatalf("not lossless: %q -> %q", s, back)
		}
		if strings.ContainsRune(Plain(segs), '�') {
			t.Fatal("U+FFFD in output")
		}
		// Truncated presentation stays safe.
		checkSafe(t, Component(s, 3))
	})
}

func FuzzDirRoundTrip(f *testing.F) {
	for _, s := range []string{"", "a/", "\xff/", "%/", "+&#?/", "../"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		enc := EncodeDir(s)
		for i := 0; i < len(enc); i++ {
			if c := enc[i]; c >= 0x80 || c < 0x20 || strings.IndexByte("/&#?;= \"<>\\", c) >= 0 {
				t.Fatalf("encoding %q holds unsafe byte %#x", enc, c)
			}
		}
		if got, err := DecodeDir(enc); err != nil || got != s {
			t.Fatalf("%q -> %q (%v)", s, got, err)
		}
		if s == "" {
			return
		}
		vals, err := url.ParseQuery(DirQuery(s))
		if err != nil || vals.Get(DirParam) != s {
			t.Fatalf("ParseQuery %q -> %q (%v)", s, vals.Get(DirParam), err)
		}
	})
}
