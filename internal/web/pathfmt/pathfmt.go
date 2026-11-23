// Package pathfmt is the pure, lossless presentation of torrent file paths
// (web-v1 §6.4.5–6.4.6). Paths are opaque byte strings: they may hold invalid
// UTF-8, empty components, leading separators and literal "." / "..". Nothing
// here normalises, merges, drops or repairs bytes, and U+FFFD is never
// substituted for an invalid byte.
//
// The package knows nothing about HTTP, templates or the file index. A
// presentation is a list of Segments; a template later renders Text segments
// as ordinary (escaped) text and Marker segments in a visibly marked span.
// Every Text segment is valid UTF-8 free of controls, bidi controls and
// U+FFFD, so it is safe for html/template escaping.
package pathfmt

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Segment is a run of a displayed name. Marker segments are explicit
// notation (\xNN, \u{XXXX}, an empty-component or truncation note), never
// content: the Marker flag, not the characters, tells them apart from a name
// that happens to contain a backslash.
type Segment struct {
	Text   string
	Marker bool
}

// EmptyComponentMarker is shown for a zero-length path component.
const EmptyComponentMarker = "(empty)"

const hexDigits = "0123456789abcdef"

// needsMarker reports whether a valid rune is shown as a \u{XXXX} marker:
// C0 and C1 controls (incl. NUL, tab, DEL), U+2028/2029, the bidi controls
// (U+061C, U+200E/F, U+202A–202E, U+2066–2069) and a real U+FFFD. Other
// format characters (ZWJ, variation selectors, …) are left alone so emoji and
// scripts still render.
func needsMarker(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r <= 0x9f:
		return true
	case r == 0x061c, r == 0x200e, r == 0x200f, r == 0x2028, r == 0x2029,
		r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == utf8.RuneError:
		return true
	}
	return false
}

func byteMarker(b byte) string { return `\x` + string([]byte{hexDigits[b>>4], hexDigits[b&15]}) }

func runeMarker(r rune) string {
	h := strings.ToUpper(strconv.FormatInt(int64(r), 16))
	if len(h) < 4 {
		h = strings.Repeat("0", 4-len(h)) + h
	}
	return `\u{` + h + `}`
}

// Display presents s. It is total and lossless: ParseMarkers (the inverse,
// used by tests) recovers s exactly from the segments.
func Display(s string) []Segment { return display(s, 0) }

// Component presents one path component: an empty one becomes a single
// EmptyComponentMarker; otherwise as Display. If maxBytes > 0 and the
// component is longer, the presentation stops at the first unit boundary at or
// beyond maxBytes input bytes and ends with a marker "…(N more bytes)", so the
// omission is explicit. maxBytes <= 0 disables truncation.
func Component(s string, maxBytes int) []Segment {
	if s == "" {
		return []Segment{{Text: EmptyComponentMarker, Marker: true}}
	}
	return display(s, maxBytes)
}

func display(s string, maxBytes int) []Segment {
	var out []Segment
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			out = append(out, Segment{Text: run.String()})
			run.Reset()
		}
	}
	mark := func(t string) {
		flush()
		out = append(out, Segment{Text: t, Marker: true})
	}
	for i := 0; i < len(s); {
		if maxBytes > 0 && i >= maxBytes {
			mark("…(" + strconv.Itoa(len(s)-i) + " more bytes)")
			return out
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1: // invalid byte
			mark(byteMarker(s[i]))
		case needsMarker(r):
			mark(runeMarker(r))
		default:
			run.WriteString(s[i : i+n])
		}
		i += n
	}
	flush()
	return out
}

// Plain concatenates the displayed text of segments (markers included) for
// logs-free plain-text contexts such as a title attribute.
func Plain(segs []Segment) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Text)
	}
	return b.String()
}
