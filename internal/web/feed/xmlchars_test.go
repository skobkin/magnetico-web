package feed

import (
	"encoding/xml"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/skobkin/magnetico-web/persistence"
)

// xml10Char is the XML 1.0 Char production: #x9 | #xA | #xD | [#x20-#xD7FF] |
// [#xE000-#xFFFD] | [#x10000-#x10FFFF].
func xml10Char(r rune) bool {
	return r == 0x9 || r == 0xA || r == 0xD || (r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF)
}

// The feed keeps every character XML 1.0 allows, bidirectional formatting
// characters included, and replaces only the characters it forbids (with
// U+FFFD). It never strips, reorders or "sanitises" anything else; the HTML
// pages (visible markers) and the JSON API (lossless name/name_base64) have
// their own, separate rules.
func TestXMLKeepsAllowedCharactersAndReplacesOnlyProhibitedOnes(t *testing.T) {
	bidi := "\u200e\u200f\u061c\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069"
	names := []string{
		"plain",
		"bidi " + bidi + " end",
		"override \u202eexe.txt",
		"zero width \u200b\u200c\u200d\u2060 bom \ufeff done",
		"whitespace \t tab \n newline \r return",
		"C1 \u0080\u0085\u009f DEL \u007f",
		"prohibited \x01\x02\x08\x0b\x0c\x0e\x1f end",
		"noncharacters \ufffe\uffff end",
		"astral 😀 \U0001F1FA\U0001F1F8 \U000e0001 \U0010ffff",
		`markup <b>&amp;</b> "q" 'a' ]]> <![CDATA[x]]>`,
		"日本語 Кириллица العربية עברית",
	}
	for _, name := range names {
		if !utf8.ValidString(name) {
			t.Fatalf("test name is not valid UTF-8: %q", name)
		}
		row := persistence.Torrent{ID: 1, InfoHash: []byte(strings.Repeat("\xab", 20)), Name: name, TotalSize: 5, DiscoveredOn: 1_700_000_000}
		for _, legacy := range []bool{true, false} {
			doc, err := render([]persistence.Torrent{row}, []string{"udp://t.example:1/announce"}, &url.URL{Scheme: "https", Host: "magnet.example.org"}, V2Path, legacy, time.Unix(1_800_000_000, 0))
			if err != nil {
				t.Fatalf("%q: %v", name, err)
			}
			var parsed struct {
				Channel struct {
					Items []struct {
						Title string `xml:"title"`
						Link  string `xml:"link"`
					} `xml:"item"`
				} `xml:"channel"`
			}
			if err := xml.Unmarshal(doc, &parsed); err != nil {
				t.Fatalf("%q: the document is not well-formed XML: %v\n%s", name, err, doc)
			}
			if len(parsed.Channel.Items) != 1 {
				t.Fatalf("%q: %d items", name, len(parsed.Channel.Items))
			}
			var want strings.Builder
			for _, r := range name {
				if xml10Char(r) {
					want.WriteRune(r)
				} else {
					want.WriteRune('�')
				}
			}
			// encoding/xml normalises a literal CR in character data; the parser
			// reads back "\r" for "&#xD;", so the title must come back as written.
			if got := parsed.Channel.Items[0].Title; got != want.String() {
				t.Errorf("title %q came back as %q, want %q", name, got, want.String())
			}
			// The magnet link carries the name percent-encoded, whatever it holds.
			if !strings.HasPrefix(parsed.Channel.Items[0].Link, "magnet:?xt=urn:btih:"+strings.Repeat("ab", 20)) {
				t.Errorf("%q: link %q", name, parsed.Channel.Items[0].Link)
			}
		}
	}
	// A bidi character survives byte for byte in the document itself.
	doc, _ := render([]persistence.Torrent{{ID: 2, InfoHash: []byte(strings.Repeat("\xcd", 20)), Name: "x\u202ey", TotalSize: 1, DiscoveredOn: 1}}, nil, &url.URL{Scheme: "https", Host: "h"}, V2Path, false, time.Unix(1, 0))
	if !strings.Contains(string(doc), "<title>x\u202ey</title>") {
		t.Errorf("the bidi override was altered in the document:\n%s", doc)
	}
}
