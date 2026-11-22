package server

import (
	"net/netip"
	"testing"
)

func prefixes(t *testing.T, ss ...string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, s := range ss {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestResolveClientIP(t *testing.T) {
	proxies := prefixes(t, "10.0.0.0/8", "192.0.2.1/32", "2001:db8:ffff::/48")
	cases := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		// Untrusted peers can never choose their identity.
		{"untrusted peer ignores XFF", "203.0.113.9:4000", []string{"1.1.1.1"}, "203.0.113.9"},
		{"untrusted peer spoofing a trusted address", "203.0.113.9:4000", []string{"10.0.0.5"}, "203.0.113.9"},
		{"no proxies configured", "10.0.0.2:4000", []string{"1.1.1.1"}, "10.0.0.2"},
		// Trusted peer.
		{"single hop", "10.0.0.2:4000", []string{"198.51.100.7"}, "198.51.100.7"},
		{"no header", "10.0.0.2:4000", nil, "10.0.0.2"},
		{"empty header", "10.0.0.2:4000", []string{""}, "10.0.0.2"},
		{"multi hop skips trusted hops from the right", "10.0.0.2:4000", []string{"198.51.100.7, 10.1.1.1, 192.0.2.1"}, "198.51.100.7"},
		{"client-supplied left entries are ignored", "10.0.0.2:4000", []string{"6.6.6.6, 198.51.100.7, 10.1.1.1"}, "198.51.100.7"},
		{"multiple header lines are joined in order", "10.0.0.2:4000", []string{"6.6.6.6", "198.51.100.7, 10.1.1.1"}, "198.51.100.7"},
		{"all hops trusted uses the peer", "10.0.0.2:4000", []string{"10.5.5.5, 192.0.2.1"}, "10.0.0.2"},
		{"ports and brackets tolerated", "10.0.0.2:4000", []string{`198.51.100.7:5555`}, "198.51.100.7"},
		{"bracketed ipv6", "10.0.0.2:4000", []string{`[2001:db8::1]`}, "2001:db8::1"},
		{"ipv6 with port", "10.0.0.2:4000", []string{`[2001:db8::1]:99`}, "2001:db8::1"},
		{"trusted ipv6 proxy", "[2001:db8:ffff::5]:4000", []string{"198.51.100.7"}, "198.51.100.7"},
		{"ipv4-mapped peer matches ipv4 prefix", "[::ffff:10.0.0.2]:4000", []string{"198.51.100.7"}, "198.51.100.7"},
		{"ipv4-mapped hop is normalized", "10.0.0.2:4000", []string{"::ffff:10.9.9.9"}, "10.0.0.2"},
		// Malformed input breaks the chain: fall back to the peer.
		{"garbage nearest hop", "10.0.0.2:4000", []string{"198.51.100.7, garbage"}, "10.0.0.2"},
		{"garbage in the middle", "10.0.0.2:4000", []string{"6.6.6.6, garbage, 10.1.1.1"}, "10.0.0.2"},
		{"empty entry between commas", "10.0.0.2:4000", []string{"198.51.100.7,,10.1.1.1"}, "10.0.0.2"},
		{"zoned address rejected", "10.0.0.2:4000", []string{"fe80::1%eth0"}, "10.0.0.2"},
		{"unterminated bracket", "10.0.0.2:4000", []string{"[2001:db8::1"}, "10.0.0.2"},
		{"overlong chain", "10.0.0.2:4000", []string{longChain(65)}, "10.0.0.2"},
		{"unparsable peer", "not-an-addr", []string{"1.1.1.1"}, "invalid IP"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps := proxies
			if c.name == "no proxies configured" {
				ps = nil
			}
			if got := ResolveClientIP(c.remote, c.xff, ps).String(); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

func longChain(n int) string {
	s := "198.51.100.7"
	for i := 0; i < n; i++ {
		s += ", 10.1.1.1"
	}
	return s
}
