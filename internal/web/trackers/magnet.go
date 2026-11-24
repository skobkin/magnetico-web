package trackers

import (
	"encoding/hex"
	"net/url"
	"strings"
)

// InfoHashHex returns the lower-case 40-digit form of a 20-byte info hash.
func InfoHashHex(h []byte) string { return hex.EncodeToString(h) }

// ValidInfoHash reports whether s is exactly 40 hexadecimal characters, either
// case, and returns its canonical lower-case form.
func ValidInfoHash(s string) (string, bool) {
	if len(s) != 40 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return "", false
		}
	}
	return strings.ToLower(s), true
}

// MagnetURI builds the magnet link of a torrent: xt, then dn (only when name
// is not nil), then one tr per tracker in the given order. The legacy
// application built links the same way with PHP's urlencode; url.QueryEscape
// produces the same form encoding (a space becomes "+") except that it leaves
// "~" unescaped, where urlencode writes %7E. The hash must already be the
// canonical lower-case hex.
func MagnetURI(hashHex string, name *string, trackers []string) string {
	var b strings.Builder
	b.Grow(64 + len(trackers)*48)
	b.WriteString("magnet:?xt=urn:btih:")
	b.WriteString(hashHex)
	if name != nil {
		b.WriteString("&dn=")
		b.WriteString(url.QueryEscape(*name))
	}
	for _, t := range trackers {
		b.WriteString("&tr=")
		b.WriteString(url.QueryEscape(t))
	}
	return b.String()
}
